// Package update implements a best-effort, fail-silent check for newer
// aixecutor releases. It uses the GitHub HTTP API (not the git gateway) and
// must never block or fail a run: every error degrades to "no notice".
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type releaseFetcher interface {
	LatestVersion(ctx context.Context) (string, error)
}

// Checker decides whether a newer release exists, consulting an on-disk cache
// before reaching the network.
type Checker struct {
	Current   string
	Fetcher   releaseFetcher
	CachePath string // "" disables caching
	Interval  time.Duration
	Now       func() time.Time // nil → time.Now
}

type cacheEntry struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
}

// Check returns the latest version and whether it is strictly newer than
// Current. It yields ("", false) on any skip, error, or cold cache miss, never
// panics, and never blocks beyond the fetcher's own timeout.
func (c *Checker) Check(ctx context.Context) (latest string, newer bool) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	if cached, ok := c.readCache(); ok {
		if now().Sub(cached.CheckedAt) < c.Interval {
			return cached.Latest, isNewer(c.Current, cached.Latest)
		}
		latest = cached.Latest // fallback if the fetch fails
	}

	if c.Fetcher == nil {
		if latest == "" {
			return "", false
		}
		return latest, isNewer(c.Current, latest)
	}

	fetched, err := c.Fetcher.LatestVersion(ctx)
	if err != nil {
		if latest == "" {
			return "", false
		}
		return latest, isNewer(c.Current, latest)
	}

	c.writeCache(cacheEntry{CheckedAt: now(), Latest: fetched})
	return fetched, isNewer(c.Current, fetched)
}

func (c *Checker) readCache() (cacheEntry, bool) {
	if c.CachePath == "" {
		return cacheEntry{}, false
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.Latest == "" {
		return cacheEntry{}, false
	}
	return entry, true
}

func (c *Checker) writeCache(entry cacheEntry) {
	if c.CachePath == "" {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.CachePath, data, 0o644)
}

// isNewer reports whether latest is a strictly higher semver than current.
// Equal or unparsable versions are not newer.
func isNewer(current, latest string) bool {
	cur, ok1 := parseSemver(current)
	lat, ok2 := parseSemver(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// GitHubReleaseFetcher fetches the latest release tag via the GitHub API.
type GitHubReleaseFetcher struct {
	Repo      string // "owner/name"
	Client    *http.Client
	UserAgent string
}

func (f *GitHubReleaseFetcher) LatestVersion(ctx context.Context) (string, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", f.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github release check: status %d", resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.TagName == "" {
		return "", fmt.Errorf("github release check: empty tag_name")
	}
	return payload.TagName, nil
}

// Notice renders the one-line update message.
func Notice(current, latest string) string {
	return fmt.Sprintf(
		"A newer aixecutor is available: %s (you have %s). Update: go install github.com/jaxmef/aixecutor@latest",
		latest, current,
	)
}
