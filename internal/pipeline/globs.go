package pipeline

import (
	"path"
	"strings"
)

// This file is the project's own glob engine, written here because the carry-forward
// from AIX-0006 is explicit: filepath.Glob (used by the git gateway's SnapshotPaths)
// does NOT understand `**`, yet subtask file-ownership globs and overlap detection
// both need it (CLAUDE.md §4.3/§4.4). It deliberately adds no dependency — the
// invariant in §6 keeps the tree to cobra + yaml.v3.
//
// Semantics (matched against forward-slash paths; see normSlash):
//
//   - `**` matches any number of path segments, including zero, AND the slashes
//     between them. So `a/**` matches `a`, `a/b`, and `a/b/c`; `**/x.go` matches
//     `x.go` and `a/b/x.go`.
//   - `*` matches any run of characters WITHIN a single segment (it never crosses a
//     `/`). So `*.go` matches only top-level `.go` files, and `a/*/c` matches
//     exactly one intervening segment.
//   - `?` matches exactly one non-`/` character.
//   - `[...]` character classes are deliberately NOT special here: subtask globs use
//     `*`/`**`/`?`, and treating `[` literally keeps the matcher small and total
//     (no ErrBadPattern path). hasGlobMeta below reflects this so a pattern with `[`
//     is matched literally rather than mishandled.
//
// The matcher is used in two places: expanding a subtask's declared `files` against
// the working tree (executor.go) and computing static prefixes for the conservative
// overlap detector (overlap.go).

// normSlash converts an OS path (which may use `\` on Windows) to the forward-slash
// form the matcher and glob patterns use, and trims a leading `./`. Patterns from
// the planner are already slash-delimited; tree paths come from filepath, so they
// are normalized here so matching is platform-independent.
func normSlash(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return p
}

// matchGlob reports whether the forward-slash path name matches pattern under the
// `**`/`*`/`?` semantics documented above. Both inputs are normalized first, so
// callers may pass OS-separator paths. An empty pattern matches only the empty
// path; matching is anchored at both ends (the whole name must match the whole
// pattern), as is conventional for path globs.
func matchGlob(pattern, name string) bool {
	return matchSegments(
		strings.Split(normSlash(pattern), "/"),
		strings.Split(normSlash(name), "/"),
	)
}

// matchSegments matches a pattern split into `/`-segments against a name split the
// same way. A `**` segment is the only one that can consume a variable number of
// name segments (zero or more); every other segment must line up one-to-one with a
// name segment via matchSegment. It recurses on `**` so that, e.g., `a/**/d`
// correctly tries every split point.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive `**` (they are equivalent to one).
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			// `**` is the last pattern segment: it matches all remaining name
			// segments (including none).
			if len(pat) == 1 {
				return true
			}
			// Try to match the rest of the pattern after `**` against every suffix
			// of name (zero or more segments consumed by `**`).
			rest := pat[1:]
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}

		// A non-`**` segment needs a name segment to match against.
		if len(name) == 0 {
			return false
		}
		if !matchSegment(pat[0], name[0]) {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	// Pattern exhausted: it matches iff the name is also exhausted.
	return len(name) == 0
}

// matchSegment matches a single path segment pattern (containing `*` and `?` but no
// `/` and no `**`) against a single name segment. `*` matches any run of characters
// (greedily, with backtracking) and `?` matches exactly one character; everything
// else is literal. It is a small, allocation-free two-pointer matcher with a single
// backtrack point for `*`, the standard linear-time wildcard algorithm.
func matchSegment(pattern, name string) bool {
	var (
		px, nx       int // current indices into pattern and name
		starPat      = -1
		starNameNext int
	)
	for nx < len(name) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == name[nx]):
			px++
			nx++
		case px < len(pattern) && pattern[px] == '*':
			// Record the backtrack: `*` initially matches zero chars; if matching
			// later fails we resume here having `*` consume one more char.
			starPat = px
			starNameNext = nx
			px++
		case starPat >= 0:
			// Backtrack: let the last `*` absorb one more name character.
			px = starPat + 1
			starNameNext++
			nx = starNameNext
		default:
			return false
		}
	}
	// Trailing `*`s in the pattern can match the empty remainder.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// hasGlobMeta reports whether a (normalized) pattern contains any wildcard this
// matcher treats specially: `*` (covers `**`) or `?`. `[` is intentionally excluded
// because the matcher treats character classes literally (see the file header), so
// a pattern with only `[` is a literal path for our purposes.
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?")
}

// staticPrefix returns the longest leading run of path segments of pattern that
// contain NO wildcard, as a slice of segments. It is the part of the pattern that
// any matching path must share verbatim. For `internal/x/**` the prefix is
// ["internal", "x"]; for `**/*.go` it is empty; for `internal/a*/b` it is
// ["internal"] (the `a*` segment is where wildcards begin). The boolean reports
// whether the ENTIRE pattern is static (no wildcards at all), i.e. the pattern is a
// literal path equal to its prefix.
//
// This is the heart of the conservative overlap test: two patterns can only be
// disjoint if their static prefixes diverge at some concrete segment, so the
// detector compares these prefixes.
func staticPrefix(pattern string) (prefix []string, allStatic bool) {
	segs := strings.Split(normSlash(path.Clean(normSlash(pattern))), "/")
	for _, s := range segs {
		if s == "**" || strings.ContainsAny(s, "*?") {
			return prefix, false
		}
		prefix = append(prefix, s)
	}
	return prefix, true
}

// hasDoubleStar reports whether the pattern contains a `**` segment, which lets it
// escape its static prefix and match arbitrarily deep. The overlap detector treats
// such patterns as broad (potentially overlapping anything sharing the prefix).
func hasDoubleStar(pattern string) bool {
	for _, s := range strings.Split(normSlash(pattern), "/") {
		if s == "**" {
			return true
		}
	}
	return false
}
