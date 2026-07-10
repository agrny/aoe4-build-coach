package main

// output.go — all file/console output formatters.

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Console printers
// ---------------------------------------------------------------------------

// printCivList prints all civilizations in a compact list.
func printCivList(civs []Civ) {
	for _, c := range civs {
		fmt.Printf("  %-6s %s\n", c.Code, c.Name)
	}
}

// printBuildList prints the top builds for a single civ with index numbers.
func printBuildList(civ Civ, builds []RawBuild) {
	fmt.Printf("%s — %s  (top %d builds)\n", civ.Code, civ.Name, len(builds))
	fmt.Println(strings.Repeat("─", 60))
	for i, b := range builds {
		fmt.Printf("  %d  [%.2f]  %s\n", i+1, b.ScoreAllTime, b.Title)
	}
}

// printBuildsTable prints a full results table for one or more civs.
func printBuildsTable(records []BuildRecord) {
	if len(records) == 0 {
		fmt.Println("No builds found.")
		return
	}
	// Column widths
	const titleW = 45
	fmt.Printf("%-4s  %-25s  %s  %-*s  %s\n",
		"CIV", "FULL NAME", "RANK", titleW, "TITLE", "URL")
	fmt.Println(strings.Repeat("─", 120))
	for _, r := range records {
		title := r.Title
		if len(title) > titleW {
			title = title[:titleW-1] + "…"
		}
		fmt.Printf("%-4s  %-25s  %4d  %-*s  %s\n",
			r.CivCode, r.CivName, r.Rank, titleW, title, r.URL)
	}
}

// ---------------------------------------------------------------------------
// File writers
// ---------------------------------------------------------------------------

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

// writeDB creates (or overwrites) a SQLite database.
func writeDB(path string, civs []Civ, records []BuildRecord) error {
	_ = os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}

	schema := `
CREATE TABLE IF NOT EXISTS civilizations (
    code TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS builds (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    civ_code TEXT    NOT NULL REFERENCES civilizations(code),
    rank     INTEGER NOT NULL CHECK (rank BETWEEN 1 AND 5),
    title    TEXT    NOT NULL,
    score    REAL    NOT NULL,
    url      TEXT    NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_builds_civ   ON builds(civ_code);
CREATE INDEX IF NOT EXISTS idx_builds_score ON builds(score DESC);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

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

// ensureDir creates a directory if it does not exist.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// defaultOutputName returns a timestamped filename for a given extension.
func defaultOutputName(civ, ext string) string {
	if civ == "" {
		return fmt.Sprintf("aoe4builds.%s", ext)
	}
	return fmt.Sprintf("aoe4builds_%s.%s", strings.ToLower(civ), ext)
}

// writePerCivTextFiles writes one text file per civ under the builds/ directory.
func writePerCivTextFiles(dir string, civBuilds map[string][]BuildRecord) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	for civ, records := range civBuilds {
		if err := writeTextFile(filepath.Join(dir, civ), civ, records); err != nil {
			return fmt.Errorf("%s: %w", civ, err)
		}
	}
	return nil
}
