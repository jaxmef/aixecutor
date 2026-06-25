package run

import (
	"strings"
	"testing"
	"time"
)

// fixedClock is a deterministic Clock for tests: every Now() returns the same
// instant, so run IDs and timestamps are reproducible.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fixedAt is the canonical test instant. It is in a non-UTC offset on purpose so
// the tests also confirm NewID normalizes to UTC before formatting.
var fixedAt = time.Date(2026, 6, 23, 14, 5, 1, 0, time.FixedZone("UTC+2", 2*60*60))

func TestNewIDDeterministic(t *testing.T) {
	clk := fixedClock{t: fixedAt}

	got := NewID("Add OAuth2 login", clk)
	// 14:05:01 at UTC+2 is 12:05:01 UTC.
	want := "20260623T120501-add-oauth2-login"
	if got != want {
		t.Fatalf("NewID = %q, want %q", got, want)
	}

	// Stable across calls with the same clock + task.
	if again := NewID("Add OAuth2 login", clk); again != got {
		t.Errorf("NewID not deterministic: %q != %q", again, got)
	}
}

func TestNewIDSortableByTime(t *testing.T) {
	earlier := NewID("task", fixedClock{t: fixedAt})
	later := NewID("task", fixedClock{t: fixedAt.Add(time.Hour)})
	if !(earlier < later) {
		t.Errorf("IDs not lexically time-sortable: %q should sort before %q", earlier, later)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Add OAuth2 login with Google", "add-oauth2-login-with-google"},
		{"  leading/trailing  ", "leading-trailing"},
		{"Mixed---Dashes__and  spaces", "mixed-dashes-and-spaces"},
		{"UPPER", "upper"},
		{"emoji 🚀 and ünïcödé", "emoji-and-n-c-d"}, // non-ascii → separators
		{"!!!", ""},
		{"", ""},
		{"a", "a"},
	}
	for _, tc := range cases {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlugifyTruncates(t *testing.T) {
	long := strings.Repeat("word ", 50) // far longer than maxSlugLen
	got := Slugify(long)
	if len(got) > maxSlugLen {
		t.Errorf("Slugify result length %d exceeds cap %d: %q", len(got), maxSlugLen, got)
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("Slugify result has dangling dash: %q", got)
	}
}

func TestNewIDFallbackSlug(t *testing.T) {
	// A task with no slug-able characters must not produce a trailing-dash id.
	got := NewID("!!!", fixedClock{t: fixedAt})
	if !strings.HasSuffix(got, "-run") {
		t.Errorf("NewID(%q) = %q, want fallback slug %q", "!!!", got, "run")
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("NewID produced a trailing dash: %q", got)
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	if loc := (SystemClock{}).Now().Location(); loc != time.UTC {
		t.Errorf("SystemClock.Now location = %v, want UTC", loc)
	}
}
