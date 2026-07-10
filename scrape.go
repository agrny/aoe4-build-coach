package main

// scrape.go — network I/O: fetching civ list via chromedp and build data via REST API.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/chromedp/chromedp"
)

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

// fetchBuilds calls the REST API for the given civ and returns all raw builds
// (including full step data), sorted descending by scoreAllTime.
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
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].ScoreAllTime > builds[j].ScoreAllTime
	})
	return builds, nil
}

// newHTTPClient returns a shared HTTP client with a sensible timeout.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}
