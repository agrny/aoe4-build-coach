package main

// normalize.go — transforms raw API build data into a clean, clock-ready JSON
// structure suitable for a real-time build order overlay web application.
//
// Output schema per build:
//
//   NormalizedBuild
//   ├── id, title, civ_code, civ_name, strategy, season, score, url, video_url
//   └── phases[]
//       ├── age (1–4), type ("age" | "ageUp"), gameplan (plain text)
//       └── steps[]
//           ├── time_seconds  — nil when step has no timestamp (followup only)
//           ├── time_display  — original string e.g. "4:30", nil when absent
//           ├── workers       — { food, wood, gold, stone } worker counts
//           ├── instruction   — plain-English text decoded from HTML icons
//           └── followups[]   — timeless steps that belong after this anchor
//               ├── workers
//               └── instruction

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// flexString is a JSON field that may be either a JSON string or a JSON number.
// The API is inconsistent — most fields are strings like "6" but some authors
// accidentally save them as numbers like 6.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	// Try as a plain string first
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	// Fall back: accept any JSON number and convert to string
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexString(n.String())
		return nil
	}
	// null / anything else → empty string
	*f = ""
	return nil
}

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// NormalizedBuild is the top-level output for one build order.
type NormalizedBuild struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	CivCode  string            `json:"civ_code"`
	CivName  string            `json:"civ_name"`
	Strategy string            `json:"strategy"`
	Season   string            `json:"season"`
	Score    float64           `json:"score"`
	URL      string            `json:"url"`
	VideoURL string            `json:"video_url,omitempty"`
	Phases   []NormalizedPhase `json:"phases"`
}

// NormalizedPhase corresponds to one age section in the original data.
type NormalizedPhase struct {
	Age      int             `json:"age"`
	Type     string          `json:"type"` // "age" | "ageUp"
	Gameplan string          `json:"gameplan,omitempty"`
	Steps    []NormalizedStep `json:"steps"`
}

// NormalizedStep is a timed anchor step, optionally followed by timeless steps.
type NormalizedStep struct {
	TimeSeconds *int          `json:"time_seconds"` // nil = no timestamp
	TimeDisplay *string       `json:"time_display"`  // nil = no timestamp
	Workers     Workers       `json:"workers"`
	Instruction string        `json:"instruction"`
	Followups   []FollowupStep `json:"followups,omitempty"`
}

// FollowupStep is a timeless action that belongs after the preceding anchor.
type FollowupStep struct {
	Workers     Workers `json:"workers"`
	Instruction string  `json:"instruction"`
}

// Workers holds villager assignment counts per resource.
type Workers struct {
	Food  int `json:"food"`
	Wood  int `json:"wood"`
	Gold  int `json:"gold"`
	Stone int `json:"stone"`
}

// ---------------------------------------------------------------------------
// Raw API types (as returned by /api/builds?civ=XXX)
// ---------------------------------------------------------------------------

// RawBuild mirrors the full JSON shape from the API.
type RawBuild struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Civ          string       `json:"civ"`
	Strategy     string       `json:"strategy"`
	Season       string       `json:"season"`
	Description  string       `json:"description"`
	Video        string       `json:"video"`
	ScoreAllTime float64      `json:"scoreAllTime"`
	Steps        []RawSection `json:"steps"`
}

// RawSection is one age phase in the raw data.
type RawSection struct {
	Age      int      `json:"age"`
	Type     string   `json:"type"`
	Gameplan string   `json:"gameplan"`
	Steps    []RawStep `json:"steps"`
}

// RawStep is one row in a phase's step list.
// All numeric fields use flexString because some build authors save them as
// JSON numbers instead of strings, causing standard string unmarshalling to fail.
type RawStep struct {
	Time        flexString `json:"time"`
	Description string     `json:"description"`
	Food        flexString `json:"food"`
	Wood        flexString `json:"wood"`
	Gold        flexString `json:"gold"`
	Stone       flexString `json:"stone"`
	Builders    flexString `json:"builders"`
	Villagers   flexString `json:"villagers"`
}

// ---------------------------------------------------------------------------
// Top-level normalizer
// ---------------------------------------------------------------------------

// NormalizeBuild converts one raw API build into the clean output format.
func NormalizeBuild(raw RawBuild, civName string) NormalizedBuild {
	nb := NormalizedBuild{
		ID:       raw.ID,
		Title:    strings.TrimSpace(raw.Title),
		CivCode:  raw.Civ,
		CivName:  civName,
		Strategy: raw.Strategy,
		Season:   raw.Season,
		Score:    raw.ScoreAllTime,
		URL:      baseURL + "/builds/" + raw.ID,
		VideoURL: raw.Video,
	}

	for _, section := range raw.Steps {
		phase := normalizePhase(section)
		if len(phase.Steps) > 0 {
			nb.Phases = append(nb.Phases, phase)
		}
	}
	return nb
}

// ---------------------------------------------------------------------------
// Phase + step normalization
// ---------------------------------------------------------------------------

func normalizePhase(section RawSection) NormalizedPhase {
	phase := NormalizedPhase{
		Age:      section.Age,
		Type:     section.Type,
		Gameplan: decodeDescription(section.Gameplan),
	}

	// Split raw steps into timed anchors + their trailing timeless followups.
	// A "timed anchor" is the first step that has a time, OR any step that has
	// a time. Timeless steps that appear before any anchor become an anchor with
	// nil time. Timeless steps after an anchor become its followups.

	var current *NormalizedStep

	flush := func() {
		if current != nil {
			phase.Steps = append(phase.Steps, *current)
			current = nil
		}
	}

	for _, raw := range section.Steps {
		tSec, tDisplay := parseTime(string(raw.Time))
		workers := parseWorkers(raw)
		instruction := decodeDescription(raw.Description)

		// Skip completely empty steps
		if instruction == "" && workers == (Workers{}) && tDisplay == nil {
			continue
		}

		if tDisplay != nil {
			// This step has a timestamp → start a new anchor
			flush()
			current = &NormalizedStep{
				TimeSeconds: tSec,
				TimeDisplay: tDisplay,
				Workers:     workers,
				Instruction: instruction,
			}
		} else {
			// No timestamp
			if current == nil {
				// No anchor yet — make this step an anchor with nil time
				current = &NormalizedStep{
					Workers:     workers,
					Instruction: instruction,
				}
			} else {
				// Append as a followup to the current anchor
				if instruction != "" || workers != (Workers{}) {
					current.Followups = append(current.Followups, FollowupStep{
						Workers:     workers,
						Instruction: instruction,
					})
				}
			}
		}
	}
	flush()

	return phase
}

// ---------------------------------------------------------------------------
// Time string parser
// ---------------------------------------------------------------------------

// parseTime normalizes the many time string formats found in the data into
// (seconds, display_string). Returns (nil, nil) if no valid time is present.
//
// Formats seen: "0:00", "00:00", "2.30", "2;00", "~04:30", "4:30 ish",
//               "‑‑:‑‑", "<br>2:10", "2:12<br>"
func parseTime(raw string) (*int, *string) {
	// Strip HTML tags and leading/trailing whitespace
	s := stripAllTags(raw)
	s = strings.TrimSpace(s)

	// Remove fuzzy prefixes/suffixes
	s = strings.TrimPrefix(s, "~")
	s = strings.TrimSuffix(s, " ish")
	s = strings.TrimSpace(s)

	// Reject placeholder strings like "--:--" or "‑‑:‑‑"
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) || r == ':' {
			return r
		}
		return -1
	}, s)
	if stripped == "" {
		return nil, nil
	}

	// Normalize separators: accept ':', '.', ';' between minutes and seconds
	normalized := regexp.MustCompile(`[.:;]`).ReplaceAllString(s, ":")

	// Expect M:SS or MM:SS
	parts := strings.SplitN(normalized, ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	mins, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	secs, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || secs > 59 {
		return nil, nil
	}

	total := mins*60 + secs
	// Format canonical display as M:SS (no leading zero on minutes)
	display := strconv.Itoa(mins) + ":" + leftPad(strconv.Itoa(secs), 2, '0')
	return &total, &display
}

// ---------------------------------------------------------------------------
// Worker count parser
// ---------------------------------------------------------------------------

func parseWorkers(s RawStep) Workers {
	return Workers{
		Food:  parseIntField(string(s.Food)),
		Wood:  parseIntField(string(s.Wood)),
		Gold:  parseIntField(string(s.Gold)),
		Stone: parseIntField(string(s.Stone)),
	}
}

// parseIntField extracts the leading integer from a field like "6", "8+", "14+".
func parseIntField(s string) int {
	s = strings.TrimSpace(s)
	// Take only leading digits
	numStr := ""
	for _, c := range s {
		if unicode.IsDigit(c) {
			numStr += string(c)
		} else {
			break
		}
	}
	if numStr == "" {
		return 0
	}
	n, _ := strconv.Atoi(numStr)
	return n
}

// ---------------------------------------------------------------------------
// HTML description decoder
// ---------------------------------------------------------------------------

// iconTitles maps common icon filenames to human-readable labels for cases
// where the img element has no title attribute.
var iconTitleOverrides = map[string]string{
	"resource_food":            "food",
	"resource_wood":            "wood",
	"resource_gold":            "gold",
	"resource_stone":           "stone",
	"gaiatreeprototypetree":    "straggler tree",
	"sheep":                    "sheep",
	"deer":                     "deer",
	"boar":                     "boar",
	"relics":                   "relic",
	"sacred_sites":             "sacred site",
	"rally":                    "→ rally",
	"repair":                   "repair",
	"villager":                 "villager",
	"villager-china":           "villager",
	"villager-mongols":         "villager",
}

var (
	reImg      = regexp.MustCompile(`(?i)<img\s[^>]*/?>`)
	reTitle    = regexp.MustCompile(`(?i)title="([^"]*)"`)
	reSrc      = regexp.MustCompile(`(?i)src="[^"]*?/([^/"]+)\.[a-z0-9]+"`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
	reBR       = regexp.MustCompile(`(?i)<br\s*/?>`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reArrow    = regexp.MustCompile(`-+>`)
)

// decodeDescription converts the raw HTML description into plain text.
//
// Strategy:
//  1. Replace <br> with ". "
//  2. Replace each <img> with its title (if set) or a label derived from filename
//  3. Strip remaining HTML tags
//  4. Decode HTML entities (&gt; → →, &amp; → &, etc.)
//  5. Collapse whitespace, fix punctuation
func decodeDescription(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}

	// 1. Convert <br> to sentence separator
	s := reBR.ReplaceAllString(raw, ". ")

	// 2. Replace each <img> with a readable token
	s = reImg.ReplaceAllStringFunc(s, func(tag string) string {
		// Try title attribute first
		if m := reTitle.FindStringSubmatch(tag); m != nil && strings.TrimSpace(m[1]) != "" {
			return "[" + strings.TrimSpace(m[1]) + "]"
		}
		// Fall back to filename
		if m := reSrc.FindStringSubmatch(tag); m != nil {
			name := m[1]
			// Check override table
			if label, ok := iconTitleOverrides[name]; ok {
				return "[" + label + "]"
			}
			// Convert filename to title case words
			label := filenameToLabel(name)
			if label != "" {
				return "[" + label + "]"
			}
		}
		return ""
	})

	// 3. Strip remaining HTML tags
	s = reTag.ReplaceAllString(s, "")

	// 4. Decode HTML entities
	s = html.UnescapeString(s)

	// 5. Replace ASCII arrows
	s = reArrow.ReplaceAllString(s, "→")

	// 6. Tidy whitespace and punctuation
	s = reSpaces.ReplaceAllString(s, " ")
	s = tidyPunctuation(s)

	return strings.TrimSpace(s)
}

// filenameToLabel converts an icon filename stem into a readable label.
// e.g. "town-center" → "Town Center", "archery-range" → "Archery Range"
func filenameToLabel(name string) string {
	// Drop numeric suffixes like "-2", "-3" (unit tiers)
	name = regexp.MustCompile(`-\d+$`).ReplaceAllString(name, "")

	// Replace separators with spaces
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)

	// Title-case each word, skipping very short words that are likely prefixes
	words := strings.Fields(name)
	for i, w := range words {
		if len(w) > 2 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// tidyPunctuation cleans up artifacts from the HTML-to-text conversion.
func tidyPunctuation(s string) string {
	// Remove orphaned brackets with nothing inside: "[]"
	s = strings.ReplaceAll(s, "[]", "")
	// Collapse multiple separators: ". . " → ". "
	s = regexp.MustCompile(`(\. )+`).ReplaceAllString(s, ". ")
	// Remove leading/trailing dots and spaces
	s = strings.Trim(s, ". \t\n\r")
	// Collapse multiple spaces again
	s = reSpaces.ReplaceAllString(s, " ")
	return s
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// stripAllTags removes all HTML tags from a string.
func stripAllTags(s string) string {
	return reTag.ReplaceAllString(s, "")
}

func leftPad(s string, length int, pad rune) string {
	for len(s) < length {
		s = string(pad) + s
	}
	return s
}
