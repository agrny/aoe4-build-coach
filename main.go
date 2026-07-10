package main

// main.go — CLI entry point for aoe4build.
//
// Commands
//   aoe4build -l                         list all civ codes
//   aoe4build <CIV> -l                   list top 5 builds for a civ (with index)
//   aoe4build <CIV> <N>                  print normalized JSON for build N  (pipeable)
//   aoe4build get <CIV> [-o csv|sql]     fetch + display/save builds for one civ
//   aoe4build get -a    [-o csv|sql]     fetch + display/save builds for ALL civs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Shared constants / types
// ---------------------------------------------------------------------------

const (
	baseURL        = "https://aoe4guides.com"
	buildsDir      = "builds"
	topN           = 5
)

// Civ holds a civilization's acronym code and full display name.
type Civ struct {
	Code string
	Name string
}

// BuildRecord is a fully enriched row used for CSV / SQL / table output.
type BuildRecord struct {
	Rank    int
	CivCode string
	CivName string
	Score   float64
	Title   string
	URL     string
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Root command  —  aoe4build [-l] [CIV] [N]
// ---------------------------------------------------------------------------

func rootCmd() *cobra.Command {
	var listCivs bool

	root := &cobra.Command{
		Use:   "aoe4build",
		Short: "Scrape and explore AoE4 build orders",
		Long: `aoe4build — Age of Empires IV build order tool

Examples:
  aoe4build -l                   list all civilization codes
  aoe4build CHI -l               list top 5 builds for Chinese (with index)
  aoe4build CHI 2                print normalized JSON for Chinese build #2
  aoe4build get CHI              fetch and display builds for Chinese
  aoe4build get CHI -o csv       fetch and save to CSV
  aoe4build get -a -o sql        fetch all civs and save to SQLite`,

		// Accept zero args (for -l) or 1-2 positional args (CIV and optional N)
		Args: cobra.MaximumNArgs(2),

		RunE: func(cmd *cobra.Command, args []string) error {
			// ── aoe4build -l ──────────────────────────────────────────────
			if listCivs && len(args) == 0 {
				return runListAllCivs()
			}

			// ── aoe4build <CIV> -l  or  aoe4build <CIV> <N> ─────────────
			if len(args) == 0 {
				return cmd.Help()
			}

			civCode := strings.ToUpper(args[0])
			client := newHTTPClient()

			builds, err := fetchBuilds(client, civCode)
			if err != nil {
				return fmt.Errorf("fetching builds for %s: %w", civCode, err)
			}
			if len(builds) == 0 {
				return fmt.Errorf("no builds found for civ %q", civCode)
			}
			if len(builds) > topN {
				builds = builds[:topN]
			}

			// Resolve full civ name once for both -l and N sub-commands
			civName := civCode
			if knownCivs, err := getCivs(); err == nil {
				for _, c := range knownCivs {
					if c.Code == civCode {
						civName = c.Name
						break
					}
				}
			}
			civ := Civ{Code: civCode, Name: civName}

			// ── aoe4build <CIV> -l ────────────────────────────────────────
			if listCivs {
				printBuildList(civ, builds)
				return nil
			}

			// ── aoe4build <CIV> <N> ──────────────────────────────────────
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 || n > len(builds) {
					return fmt.Errorf("index must be a number between 1 and %d", len(builds))
				}
				nb := NormalizeBuild(builds[n-1], civName)
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(nb)
			}

			return cmd.Help()
		},
	}

	root.Flags().BoolVarP(&listCivs, "list", "l", false, "list civs (global) or builds for a civ")
	root.AddCommand(getCmd())
	return root
}

// ---------------------------------------------------------------------------
// get subcommand  —  aoe4build get [CIV] [-a] [-o csv|sql]
// ---------------------------------------------------------------------------

func getCmd() *cobra.Command {
	var (
		allCivs  bool
		outputTo []string
	)

	cmd := &cobra.Command{
		Use:   "get [CIV]",
		Short: "Fetch and display/save build orders",
		Long: `Fetch top 5 build orders for one or all civilizations.

Without -o, results are printed to the console.
With -o, results are saved to files (csv and/or sql supported).

Examples:
  aoe4build get CHI              display builds for Chinese
  aoe4build get CHI -o csv       save to aoe4builds_chi.csv
  aoe4build get CHI -o sql       save to aoe4builds_chi.db
  aoe4build get CHI -o csv,sql   save to both
  aoe4build get -a               display builds for all civs
  aoe4build get -a -o csv,sql    save all civs to CSV + SQLite`,

		Args: cobra.MaximumNArgs(1),

		RunE: func(cmd *cobra.Command, args []string) error {
			if !allCivs && len(args) == 0 {
				return fmt.Errorf("provide a civ code or use -a for all civs")
			}

			client := newHTTPClient()

			// ── resolve civ list (always fetch names via chromedp) ───────
			fmt.Fprintln(os.Stderr, "Fetching civilization list from aoe4guides.com…")
			allKnownCivs, err := getCivs()
			if err != nil {
				return fmt.Errorf("fetching civ list: %w", err)
			}
			civNameLookup := make(map[string]string, len(allKnownCivs))
			for _, c := range allKnownCivs {
				civNameLookup[c.Code] = c.Name
			}

			var civs []Civ
			if allCivs {
				civs = allKnownCivs
			} else {
				code := strings.ToUpper(args[0])
				name := civNameLookup[code]
				if name == "" {
					name = code // unknown code — keep the code as display name
				}
				civs = []Civ{{Code: code, Name: name}}
			}

			// ── fetch builds for each civ ─────────────────────────────────
			var allRecords []BuildRecord
			var allNormalized []NormalizedBuild
			civBuilds := make(map[string][]BuildRecord)
			civMap := make(map[string]Civ)

			for i, civ := range civs {
				if allCivs {
					fmt.Fprintf(os.Stderr, "  [%d/%d] %s…\n", i+1, len(civs), civ.Code)
				}
				builds, err := fetchBuilds(client, civ.Code)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  WARN: %s: %v\n", civ.Code, err)
					continue
				}
				if len(builds) > topN {
					builds = builds[:topN]
				}

				// Patch civ name from lookup if still empty
				if civ.Name == "" {
					if n := civNameLookup[civ.Code]; n != "" {
						civ.Name = n
					} else {
						civ.Name = civ.Code
					}
				}
				civMap[civ.Code] = civ

				var records []BuildRecord
				for j, b := range builds {
					rec := BuildRecord{
						Rank:    j + 1,
						CivCode: civ.Code,
						CivName: civ.Name,
						Score:   b.ScoreAllTime,
						Title:   b.Title,
						URL:     fmt.Sprintf("%s/builds/%s", baseURL, b.ID),
					}
					records = append(records, rec)
					allRecords = append(allRecords, rec)
					allNormalized = append(allNormalized, NormalizeBuild(b, civ.Name))
				}
				civBuilds[civ.Code] = records
			}

			// ── output ────────────────────────────────────────────────────
			outputs := parseOutputFlags(outputTo)

			if len(outputs) == 0 {
				// Console print
				printBuildsTable(allRecords)
				return nil
			}

			civCode := ""
			if len(civs) == 1 {
				civCode = civs[0].Code
			}

			for _, out := range outputs {
				switch out {
				case "csv":
					path := defaultOutputName(civCode, "csv")
					if err := writeCSV(path, allRecords); err != nil {
						return fmt.Errorf("writing CSV: %w", err)
					}
					fmt.Fprintf(os.Stderr, "Saved CSV → %s\n", path)

				case "sql":
					path := defaultOutputName(civCode, "db")
					civSlice := civsFromMap(civMap)
					if err := writeDB(path, civSlice, allRecords); err != nil {
						return fmt.Errorf("writing SQLite: %w", err)
					}
					fmt.Fprintf(os.Stderr, "Saved SQLite → %s\n", path)

				case "json":
					path := defaultOutputName(civCode, "json")
					if err := writeNormalizedJSON(path, allNormalized); err != nil {
						return fmt.Errorf("writing JSON: %w", err)
					}
					fmt.Fprintf(os.Stderr, "Saved JSON → %s\n", path)

				default:
					fmt.Fprintf(os.Stderr, "WARN: unknown output format %q (supported: csv, sql, json)\n", out)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&allCivs, "all", "a", false, "fetch builds for all civilizations")
	cmd.Flags().StringSliceVarP(&outputTo, "output", "o", nil, "output format(s): csv, sql, json  (comma-separated)")
	return cmd
}

// ---------------------------------------------------------------------------
// runListAllCivs
// ---------------------------------------------------------------------------

func runListAllCivs() error {
	fmt.Fprintln(os.Stderr, "Fetching civilization list from aoe4guides.com…")
	civs, err := getCivs()
	if err != nil {
		return err
	}
	printCivList(civs)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseOutputFlags normalises the -o flag values (handles both "csv,sql" as a
// single string and multiple -o flags).
func parseOutputFlags(raw []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

// civsFromMap returns a slice of Civ values from a map, preserving no
// particular order (order doesn't matter for DB inserts).
func civsFromMap(m map[string]Civ) []Civ {
	out := make([]Civ, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// httpClientKey is used to allow tests to inject a client — unused in prod.
var _ *http.Client
