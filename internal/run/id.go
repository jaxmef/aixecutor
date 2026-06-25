package run

import (
	"strings"
	"time"
)

// Clock is the time source for run identity and timestamps. It is an interface
// so tests inject a fixed clock and get deterministic, reproducible run IDs and
// createdAt/updatedAt values (CLAUDE.md §7: inject clock/IDs for determinism).
// Production code uses SystemClock.
type Clock interface {
	// Now returns the current time. Implementations should return a consistent
	// location; SystemClock returns UTC so IDs are timezone-stable.
	Now() time.Time
}

// SystemClock is the production Clock: it returns the wall-clock time in UTC.
// UTC is used so run IDs (which embed the timestamp) sort and read consistently
// regardless of the machine's local timezone.
type SystemClock struct{}

// Now returns time.Now() in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ClockFunc adapts a plain func() time.Time to the Clock interface, for callers
// (and tests) that prefer a function literal over a named type.
type ClockFunc func() time.Time

// Now calls the wrapped function.
func (f ClockFunc) Now() time.Time { return f() }

// idTimestampLayout is the timestamp prefix format for run IDs. It is chosen to
// be filesystem-safe (no ':' which is illegal on Windows and awkward elsewhere)
// and lexicographically sortable: a plain string sort of IDs orders them by
// time. Format: YYYYMMDDTHHMMSS (e.g. 20260623T140501).
const idTimestampLayout = "20060102T150405"

// maxSlugLen bounds the slug portion of a run ID so a long task description does
// not produce an unwieldy directory name. The timestamp prefix is fixed-width,
// so the whole ID stays comfortably within filesystem name limits.
const maxSlugLen = 40

// NewID builds a run ID of the form "<timestamp>-<slug>" from the task, using
// clk for the timestamp. The timestamp is fixed-width and sortable; the slug is
// a sanitized, kebab-cased, length-capped rendering of the task (see Slugify) so
// the ID is human-recognizable while remaining filesystem-safe. When the task
// has no slug-able characters, the slug falls back to "run" so the ID is never
// just a trailing dash.
func NewID(task string, clk Clock) string {
	ts := clk.Now().UTC().Format(idTimestampLayout)
	slug := Slugify(task)
	if slug == "" {
		slug = "run"
	}
	return ts + "-" + slug
}

// Slugify converts an arbitrary string to a filesystem-safe, URL-ish kebab-case
// slug: it lowercases, transliterates any run of non-alphanumeric ASCII to a
// single '-', trims leading/trailing '-', and caps the length at maxSlugLen
// (trimming any '-' the cut leaves dangling). Non-ASCII bytes are treated as
// separators, so the result is pure [a-z0-9-]. An input with no usable
// characters yields "".
func Slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			// Any separator/punctuation/space/non-ASCII collapses to a single
			// dash; consecutive separators do not produce repeated dashes.
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxSlugLen {
		slug = strings.Trim(slug[:maxSlugLen], "-")
	}
	return slug
}
