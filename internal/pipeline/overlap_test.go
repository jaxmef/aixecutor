package pipeline

import (
	"testing"

	"github.com/jaxmef/aixecutor/internal/run"
)

// TestGlobsOverlap is the table test for the conservative/sound overlap detector.
// The critical property is no false "disjoint": the detector returns false (safe to
// parallelize) ONLY when the two globs provably cannot share a path. Every uncertain
// case must be true (overlap → serialize).
func TestGlobsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		why  string
	}{
		// Provably disjoint: static prefixes diverge at a concrete segment.
		{"internal/a/**", "internal/b/**", false, "disjoint sibling dirs"},
		{"internal/a/x.go", "internal/b/y.go", false, "disjoint literal files in different dirs"},
		{"src/a/**", "src/b/**", false, "disjoint subtrees"},
		{"a/b/c.go", "a/b/d.go", false, "same dir, different files (prefixes diverge at file segment)"},

		// Must overlap (cannot prove disjoint).
		{"internal/x/**", "internal/x/y.go", true, "literal inside the ** subtree"},
		{"internal/x/**", "internal/x/sub/**", true, "nested subtree under the same prefix"},
		{"**/*.go", "internal/a/x.go", true, "leading ** matches anything"},
		{"**/*.go", "anything/else.txt", true, "leading ** has no literal anchor → conservative overlap"},
		{"internal/a/**", "internal/a/**", true, "identical globs overlap"},
		{"internal/a/x.go", "internal/a/x.go", true, "identical literals overlap"},
		{"a/*/c.go", "a/b/c.go", true, "single-segment wildcard could match b"},
		{"internal/**", "internal/a/x.go", true, "broad ** prefix contains the literal"},
		{"a/b", "a/b/c.go", true, "a/b prefix-of a/b/c (one reaches into the other)"},
	}
	for _, c := range cases {
		t.Run(c.a+"~"+c.b, func(t *testing.T) {
			if got := globsOverlap(c.a, c.b); got != c.want {
				t.Errorf("globsOverlap(%q, %q) = %v; want %v (%s)", c.a, c.b, got, c.want, c.why)
			}
			// Overlap is symmetric: swapping arguments must agree.
			if got := globsOverlap(c.b, c.a); got != c.want {
				t.Errorf("globsOverlap is not symmetric for (%q, %q): %v vs %v", c.a, c.b, c.want, got)
			}
		})
	}
}

// TestSubtasksOverlap checks the subtask-level overlap: any overlapping pair of
// globs makes the subtasks overlap, identical sets overlap, disjoint sets do not,
// and an empty file set is treated conservatively as overlapping everything.
func TestSubtasksOverlap(t *testing.T) {
	st := func(files ...string) run.Subtask { return run.Subtask{Files: files} }

	cases := []struct {
		name string
		a, b run.Subtask
		want bool
	}{
		{"disjoint dirs", st("internal/a/**"), st("internal/b/**"), false},
		{"disjoint literals", st("a/x.go", "a/y.go"), st("b/z.go"), false},
		{"one overlapping pair among many", st("internal/a/**", "internal/c/x.go"), st("internal/b/**", "internal/c/y.go"), false},
		{"overlap via shared subtree", st("internal/a/**"), st("internal/a/x.go", "other/z.go"), true},
		{"identical sets overlap", st("internal/a/**"), st("internal/a/**"), true},
		{"empty set on left overlaps", st(), st("internal/a/**"), true},
		{"empty set on right overlaps", st("internal/a/**"), st(), true},
		{"both empty overlap", st(), st(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := subtasksOverlap(c.a, c.b); got != c.want {
				t.Errorf("subtasksOverlap = %v; want %v", got, c.want)
			}
		})
	}
}
