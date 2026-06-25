package pipeline

import "github.com/jaxmef/aixecutor/internal/run"

// overlap.go implements the CONSERVATIVE, SOUND file-ownership overlap test that
// drives non-overlapping parallelism (CLAUDE.md §4.3). The scheduler runs two
// ready subtasks concurrently only when their declared `files` globs are provably
// disjoint; whenever disjointness cannot be PROVEN, the subtasks must serialize.
//
// Soundness contract (the property tests pin down):
//
//	subtasksOverlap(a, b) == false  ⇒  no path can match both a and b.
//
// The dangerous direction is a false negative (claiming disjoint when a shared
// path exists), because that would let two executors edit the same file in
// parallel. So every analysis below is biased to return true (overlapping) the
// moment it is unsure. Returning true too often only costs parallelism, never
// correctness.

// subtasksOverlap reports whether two subtasks' declared file sets might intersect,
// and therefore whether they must NOT run concurrently under non-overlapping
// isolation. It is true unless EVERY pair of globs (one from each subtask) is
// provably disjoint.
//
// Empty file sets are treated conservatively as "could touch anything": a subtask
// that declares no ownership has not promised to stay anywhere, so it overlaps
// every other subtask (it serializes against them). This is the safe reading of an
// under-specified plan; the alternative — assuming it touches nothing — could let
// it race a sibling that edits the same files.
func subtasksOverlap(a, b run.Subtask) bool {
	if len(a.Files) == 0 || len(b.Files) == 0 {
		return true
	}
	for _, ga := range a.Files {
		for _, gb := range b.Files {
			if globsOverlap(ga, gb) {
				return true
			}
		}
	}
	return false
}

// globsOverlap reports whether two single globs might match a common path. It
// returns false ONLY when it can prove the two are disjoint; in every uncertain
// case it returns true.
//
// The proof of disjointness rests entirely on the globs' STATIC PREFIXES (the
// leading literal segments before any wildcard, from staticPrefix):
//
//   - If either glob has an EMPTY static prefix while still being a pattern (e.g.
//     `**/*.go`, `*.go`, `**`), it can match paths anywhere under the root with no
//     literal anchor we can compare, so we cannot prove disjointness → overlap.
//   - Otherwise compare the two prefixes segment-by-segment over their common
//     length. If any aligned pair of CONCRETE segments differs, the globs are
//     anchored into divergent subtrees and can never meet → disjoint (false).
//   - If one prefix is a (proper or full) prefix of the other — they agree on every
//     compared segment — the shorter, more general glob can reach into the longer's
//     subtree, so they may overlap → true. This is also where a literal path that
//     lies inside the other's `**` subtree (e.g. `internal/x/y.go` vs
//     `internal/x/**`) is correctly reported as overlapping.
//
// Because the only escape hatch from "overlap" is a concrete divergence in the
// static prefixes, a pattern that can dodge its prefix (a `**`) never produces a
// false "disjoint": its prefix is still literal up to the `**`, and divergence
// there is real, while agreement there keeps it overlapping.
func globsOverlap(a, b string) bool {
	pa, _ := staticPrefix(a)
	pb, _ := staticPrefix(b)

	// A pattern with no literal anchor at all can match arbitrarily; we cannot
	// prove it disjoint from anything. (A fully-empty prefix only arises for a
	// pattern beginning with a wildcard, e.g. `**/...` or `*.go` at the root.)
	if len(pa) == 0 || len(pb) == 0 {
		return true
	}

	// Compare aligned concrete segments. The first divergence proves disjointness.
	n := len(pa)
	if len(pb) < n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		if pa[i] != pb[i] {
			return false // anchored into different subtrees: provably disjoint.
		}
	}

	// One static prefix is a prefix of the other (they never diverged). The shorter
	// glob is broad enough to reach into the longer one's subtree, so we cannot rule
	// out a shared path. Be conservative: overlap.
	return true
}
