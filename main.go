package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/chromedp/chromedp"
	_ "modernc.org/sqlite"
)

const (
	baseURL     = "https://aoe4guides.com"
	buildsDir   = "builds"
	csvFile     = "builds.csv"
	dbFile      = "builds.db"
	normalizedFile = "builds_normalized.json"
	topN        = 5
)

// Civ holds a civilization's acronym code and full display name.
type Civ struct {
	Code string
	Name string
}

// BuildRecord is a fully enriched row used for CSV/SQL output.
type BuildRecord struct {
	Rank     int
	CivCode  string
	CivName  string
	Score    float64
	Title    string
	URL      string
}

func main() {
	// --- Step 1: scrape civ names + codes from the homepage ---
	civs, err := getCivs()
	if err != nil {
		log.Fatalf("failed to get civs: %v", err)
	}
	fmt.Printf("Found %d civilizations\n\n", len(civs))

	// Build a code → name lookup
	civNames := make(map[string]string, len(civs))
	for _, c := range civs {
		civNames[c.Code] = c.Name
	}

	// --- Step 2: create per-civ files directory ---
	if err := os.MkdirAll(buildsDir, 0755); err != nil {
		log.Fatalf("failed to create builds dir: %v", err)
	}

	// --- Step 3: fetch builds for every civ and collect all records ---
	client := &http.Client{Timeout: 15 * time.Second}
	var allRecords []BuildRecord
	var allNormalized []NormalizedBuild

	for _, civ := range civs {
		fmt.Printf("Processing %-4s (%s)...", civ.Code, civ.Name)

		rawBuilds, err := fetchBuilds(client, civ.Code)
		if err != nil {
			fmt.Printf(" ERROR: %v\n", err)
			continue
		}

		sort.Slice(rawBuilds, func(i, j int) bool {
			return rawBuilds[i].ScoreAllTime > rawBuilds[j].ScoreAllTime
		})
		if len(rawBuilds) > topN {
			rawBuilds = rawBuilds[:topN]
		}
		if len(rawBuilds) == 0 {
			fmt.Printf(" no builds found, skipping\n")
			continue
		}

		var records []BuildRecord
		for i, b := range rawBuilds {
			rec := BuildRecord{
				Rank:    i + 1,
				CivCode: civ.Code,
				CivName: civ.Name,
				Score:   b.ScoreAllTime,
				Title:   b.Title,
				URL:     fmt.Sprintf("%s/builds/%s", baseURL, b.ID),
			}
			records = append(records, rec)
			allRecords = append(allRecords, rec)

			// Normalize for JSON output
			allNormalized = append(allNormalized, NormalizeBuild(b, civ.Name))
		}

		if err := writeTextFile(filepath.Join(buildsDir, civ.Code), civ.Code, records); err != nil {
			fmt.Printf(" ERROR writing text file: %v\n", err)
			continue
		}
		fmt.Printf(" saved %d builds\n", len(records))
	}

	// --- Step 4: write CSV ---
	fmt.Printf("\nWriting %s...", csvFile)
	if err := writeCSV(csvFile, allRecords); err != nil {
		log.Fatalf(" ERROR: %v", err)
	}
	fmt.Printf(" %d rows written\n", len(allRecords))

	// --- Step 5: write SQLite database ---
	fmt.Printf("Writing %s...", dbFile)
	if err := writeDB(dbFile, civs, allRecords); err != nil {
		log.Fatalf(" ERROR: %v", err)
	}
	fmt.Printf(" done\n")

	// --- Step 6: write normalized JSON ---
	fmt.Printf("Writing %s...", normalizedFile)
	if err := writeNormalizedJSON(normalizedFile, allNormalized); err != nil {
		log.Fatalf(" ERROR: %v", err)
	}
	fmt.Printf(" %d builds written\n", len(allNormalized))

	fmt.Printf("\nOutput:\n  ./%s/              — %d per-civ text files\n  ./%s          — CSV\n  ./%s           — SQLite\n  ./%s — normalized JSON\n",
		buildsDir, len(civs), csvFile, dbFile, normalizedFile)
}

// getCivs launches a headless browser, loads the homepage, and returns every
// civilization's code and full name by reading the tile href and label.
func getCivs() ([]Civ, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Grab parallel arrays: hrefs and names, same index = same tile
	var hrefs []string
	var names []string

	err := chromedp.Run(ctx,
		chromedp.Navigate(baseURL+"/"),
		chromedp.WaitVisible(`.civ-tile__name`, chromedp.ByQuery),
		chromedp.Evaluate(`
			Array.from(document.querySelectorAll('.civ-tile__name'))
				.map(el => el.closest('a') ? el.closest('a').getAttribute('href') : '')
		`, &hrefs),
		chromedp.Evaluate(`
			Array.from(document.querySelectorAll('.civ-tile__name'))
				.map(el => el.textContent.trim())
		`, &names),
	)
	if err != nil {
		return nil, fmt.Errorf("chromedp: %w", err)
	}

	var civs []Civ
	for i, href := range hrefs {
		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		code := u.Query().Get("civ")
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if code != "" {
			civs = append(civs, Civ{Code: code, Name: name})
		}
	}
	return civs, nil
}

// fetchBuilds calls the REST API for the given civ and returns full raw builds
// (including all step data needed for normalization).
func fetchBuilds(client *http.Client, civ string) ([]RawBuild, error) {
	resp, err := client.Get(fmt.Sprintf("%s/api/builds?civ=%s", baseURL, civ))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var builds []RawBuild
	if err := json.Unmarshal(body, &builds); err != nil {
		return nil, fmt.Errorf("JSON: %w", err)
	}
	return builds, nil
}

// writeNormalizedJSON serializes all normalized builds to a pretty-printed JSON file.
func writeNormalizedJSON(path string, builds []NormalizedBuild) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(builds)
}

// writeTextFile writes a human-readable per-civ build list.
func writeTextFile(path, civ string, records []BuildRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "# Top %d builds for %s — %s (sorted by score)\n", len(records), civ, records[0].CivName)
	for _, r := range records {
		fmt.Fprintf(f, "%d. [%.2f] %s\n   %s\n", r.Rank, r.Score, r.Title, r.URL)
	}
	return nil
}

// writeCSV writes all build records to a single CSV file.
func writeCSV(path string, records []BuildRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Header
	if err := w.Write([]string{"rank", "civ_code", "civ_name", "score", "title", "url"}); err != nil {
		return err
	}
	for _, r := range records {
		row := []string{
			strconv.Itoa(r.Rank),
			r.CivCode,
			r.CivName,
			strconv.FormatFloat(r.Score, 'f', 6, 64),
			r.Title,
			r.URL,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}

// writeDB creates (or overwrites) a SQLite database with two tables:
//
//	civilizations(code, name)
//	builds(id, civ_code, rank, title, score, url)
//
// with a foreign key from builds.civ_code → civilizations.code.
func writeDB(path string, civs []Civ, records []BuildRecord) error {
	// Remove existing file so we start fresh
	_ = os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	// Enable foreign key enforcement
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}

	schema := `
CREATE TABLE IF NOT EXISTS civilizations (
    code TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS builds (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    civ_code TEXT    NOT NULL REFERENCES civilizations(code),
    rank     INTEGER NOT NULL CHECK (rank BETWEEN 1 AND 5),
    title    TEXT    NOT NULL,
    score    REAL    NOT NULL,
    url      TEXT    NOT NULL UNIQUE
);

CREATE INDEX IF NOT EXISTS idx_builds_civ  ON builds(civ_code);
CREATE INDEX IF NOT EXISTS idx_builds_score ON builds(score DESC);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// Insert civilizations
	civStmt, err := tx.Prepare(`INSERT OR IGNORE INTO civilizations(code, name) VALUES (?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer civStmt.Close()

	seen := make(map[string]bool)
	for _, c := range civs {
		if seen[c.Code] {
			continue
		}
		seen[c.Code] = true
		if _, err := civStmt.Exec(c.Code, c.Name); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert civ %s: %w", c.Code, err)
		}
	}

	// Insert builds
	buildStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO builds(civ_code, rank, title, score, url)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer buildStmt.Close()

	for _, r := range records {
		if _, err := buildStmt.Exec(r.CivCode, r.Rank, r.Title, r.Score, r.URL); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert build %s: %w", r.URL, err)
		}
	}

	return tx.Commit()
}


