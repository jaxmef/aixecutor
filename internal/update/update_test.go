package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mockFetcher struct {
	version string
	err     error
	calls   int
}

func (m *mockFetcher) LatestVersion(ctx context.Context) (string, error) {
	m.calls++
	return m.version, m.err
}

type failingFetcher struct{ t *testing.T }

func (f failingFetcher) LatestVersion(ctx context.Context) (string, error) {
	f.t.Helper()
	f.t.Fatal("fetcher must not be called")
	return "", nil
}

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestParseSemverCompare(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		newer   bool
	}{
		{"older patch", "v0.2.0", "v0.2.1", true},
		{"multi-digit minor", "v0.2.1", "v0.10.0", true},
		{"equal", "v1.0.0", "v1.0.0", false},
		{"downgrade", "v1.2.0", "v1.1.9", false},
		{"no leading v", "1.0.0", "1.0.1", true},
		{"mixed leading v", "1.0.0", "v1.0.1", true},
		{"prerelease metadata ignored", "v1.0.0", "v1.0.0-rc1", false},
		{"build metadata ignored", "v1.0.0", "v1.0.0+build5", false},
		{"prerelease still newer", "v1.0.0", "v1.0.1-beta", true},
		{"unparsable current", "dev", "v1.0.0", false},
		{"unparsable latest", "v1.0.0", "garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNewer(tt.current, tt.latest); got != tt.newer {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.newer)
			}
		})
	}
}

func TestCheckColdMissFetcherError(t *testing.T) {
	dir := t.TempDir()
	c := &Checker{
		Current:   "v1.0.0",
		Fetcher:   &mockFetcher{err: context.DeadlineExceeded},
		CachePath: filepath.Join(dir, ".update-check"),
		Interval:  time.Hour,
		Now:       fixedNow(time.Unix(1000, 0)),
	}
	latest, newer := c.Check(context.Background())
	if latest != "" || newer {
		t.Errorf("got (%q, %v), want (\"\", false)", latest, newer)
	}
}

func TestCheckFetchesNewer(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".update-check")
	now := time.Unix(2000, 0)
	c := &Checker{
		Current:   "v1.0.0",
		Fetcher:   &mockFetcher{version: "v1.2.0"},
		CachePath: cachePath,
		Interval:  time.Hour,
		Now:       fixedNow(now),
	}
	latest, newer := c.Check(context.Background())
	if latest != "v1.2.0" || !newer {
		t.Fatalf("got (%q, %v), want (v1.2.0, true)", latest, newer)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("bad cache json: %v", err)
	}
	if entry.Latest != "v1.2.0" || !entry.CheckedAt.Equal(now) {
		t.Errorf("cache = %+v, want latest v1.2.0 at %v", entry, now)
	}
}

func TestCheckCacheHitSkipsFetcher(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".update-check")
	checkedAt := time.Unix(3000, 0)
	writeTestCache(t, cachePath, cacheEntry{CheckedAt: checkedAt, Latest: "v2.0.0"})

	c := &Checker{
		Current:   "v1.0.0",
		Fetcher:   failingFetcher{t},
		CachePath: cachePath,
		Interval:  time.Hour,
		Now:       fixedNow(checkedAt.Add(30 * time.Minute)),
	}
	latest, newer := c.Check(context.Background())
	if latest != "v2.0.0" || !newer {
		t.Errorf("got (%q, %v), want (v2.0.0, true)", latest, newer)
	}
}

func TestCheckCacheExpiryRefetches(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".update-check")
	checkedAt := time.Unix(4000, 0)
	writeTestCache(t, cachePath, cacheEntry{CheckedAt: checkedAt, Latest: "v2.0.0"})

	fetcher := &mockFetcher{version: "v3.0.0"}
	c := &Checker{
		Current:   "v1.0.0",
		Fetcher:   fetcher,
		CachePath: cachePath,
		Interval:  time.Hour,
		Now:       fixedNow(checkedAt.Add(2 * time.Hour)),
	}
	latest, newer := c.Check(context.Background())
	if latest != "v3.0.0" || !newer {
		t.Errorf("got (%q, %v), want (v3.0.0, true)", latest, newer)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetcher calls = %d, want 1", fetcher.calls)
	}
}

func TestCheckFetchErrorFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".update-check")
	checkedAt := time.Unix(5000, 0)
	writeTestCache(t, cachePath, cacheEntry{CheckedAt: checkedAt, Latest: "v2.0.0"})

	c := &Checker{
		Current:   "v1.0.0",
		Fetcher:   &mockFetcher{err: context.DeadlineExceeded},
		CachePath: cachePath,
		Interval:  time.Hour,
		Now:       fixedNow(checkedAt.Add(2 * time.Hour)),
	}
	latest, newer := c.Check(context.Background())
	if latest != "v2.0.0" || !newer {
		t.Errorf("got (%q, %v), want (v2.0.0, true)", latest, newer)
	}
}

func TestCheckNoCacheDisabled(t *testing.T) {
	fetcher := &mockFetcher{version: "v1.5.0"}
	c := &Checker{
		Current:  "v1.0.0",
		Fetcher:  fetcher,
		Interval: time.Hour,
		Now:      fixedNow(time.Unix(6000, 0)),
	}
	latest, newer := c.Check(context.Background())
	if latest != "v1.5.0" || !newer {
		t.Errorf("got (%q, %v), want (v1.5.0, true)", latest, newer)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetcher calls = %d, want 1", fetcher.calls)
	}
}

func TestNotice(t *testing.T) {
	got := Notice("v1.0.0", "v1.2.0")
	want := "A newer aixecutor is available: v1.2.0 (you have v1.0.0). Update: go install github.com/jaxmef/aixecutor@latest"
	if got != want {
		t.Errorf("Notice = %q, want %q", got, want)
	}
}

func writeTestCache(t *testing.T, path string, entry cacheEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
