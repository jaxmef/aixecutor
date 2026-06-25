package pipeline

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestMatchGlob is the table test for the `**`-aware matcher (the AIX-0006
// carry-forward). It pins the documented semantics: `**` spans segments, `*` stays
// within a segment, `?` is one non-slash char.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		// `**` spans any number of segments, including zero.
		{"internal/x/**", "internal/x/a/b.go", true},
		{"internal/x/**", "internal/x/a.go", true},
		{"internal/x/**", "internal/x", true},       // ** matches zero segments
		{"internal/x/**", "internal/y/a.go", false}, // different subtree
		{"**/*.go", "a/b/c.go", true},               // leading ** then segment glob
		{"**/*.go", "main.go", true},                // ** matches zero leading segs
		{"**", "anything/at/all.go", true},          // bare ** matches everything
		{"a/**/d.go", "a/b/c/d.go", true},           // ** in the middle
		{"a/**/d.go", "a/d.go", true},               // middle ** matches zero
		{"a/**/d.go", "a/b/c/e.go", false},          // tail mismatch
		// `*` stays within one segment.
		{"*.go", "main.go", true},
		{"*.go", "a/main.go", false}, // * does not cross a slash
		{"a/*/c.go", "a/b/c.go", true},
		{"a/*/c.go", "a/b/x/c.go", false}, // exactly one intervening segment
		{"a/*/c.go", "a/c.go", false},     // * needs a segment
		{"internal/*/file.go", "internal/pkg/file.go", true},
		// `?` is exactly one non-slash char.
		{"file?.go", "file1.go", true},
		{"file?.go", "file.go", false},
		{"file?.go", "file12.go", false},
		// literals.
		{"internal/x/y.go", "internal/x/y.go", true},
		{"internal/x/y.go", "internal/x/z.go", false},
		// trailing/leading slash normalization via filepath separators.
		{"a/b", "a/b", true},
	}
	for _, c := range cases {
		t.Run(c.pattern+"~"+c.name, func(t *testing.T) {
			if got := matchGlob(c.pattern, c.name); got != c.want {
				t.Errorf("matchGlob(%q, %q) = %v; want %v", c.pattern, c.name, got, c.want)
			}
		})
	}
}

// TestStaticPrefix checks the static-prefix extraction that underpins overlap
// detection: the leading literal segments and whether the whole pattern is literal.
func TestStaticPrefix(t *testing.T) {
	cases := []struct {
		pattern   string
		want      []string
		allStatic bool
	}{
		{"internal/x/**", []string{"internal", "x"}, false},
		{"internal/x/y.go", []string{"internal", "x", "y.go"}, true},
		{"**/*.go", nil, false},
		{"*.go", nil, false},
		{"internal/a*/b", []string{"internal"}, false},
		{"a/b/c", []string{"a", "b", "c"}, true},
		{"**", nil, false},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			got, all := staticPrefix(c.pattern)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("staticPrefix(%q) prefix = %v; want %v", c.pattern, got, c.want)
			}
			if all != c.allStatic {
				t.Errorf("staticPrefix(%q) allStatic = %v; want %v", c.pattern, all, c.allStatic)
			}
		})
	}
}

// TestExpandFiles proves the tree expansion used for snapshotting: literal paths
// pass through (even when missing), globs match existing files via the `**` matcher,
// and .git is skipped.
func TestExpandFiles(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, root, "internal/x/a.go", "package x\n")
	writeTreeFile(t, root, "internal/x/sub/b.go", "package sub\n")
	writeTreeFile(t, root, "internal/y/c.go", "package y\n")
	writeTreeFile(t, root, "main.go", "package main\n")
	writeTreeFile(t, root, ".git/config", "[core]\n") // must be skipped

	t.Run("double-star matches subtree", func(t *testing.T) {
		got, err := expandFiles(root, []string{"internal/x/**"})
		if err != nil {
			t.Fatalf("expandFiles: %v", err)
		}
		want := []string{
			filepath.FromSlash("internal/x/a.go"),
			filepath.FromSlash("internal/x/sub/b.go"),
		}
		assertPathSet(t, got, want)
	})

	t.Run("top-level star only", func(t *testing.T) {
		got, err := expandFiles(root, []string{"*.go"})
		if err != nil {
			t.Fatalf("expandFiles: %v", err)
		}
		assertPathSet(t, got, []string{"main.go"})
	})

	t.Run("literal missing path passes through", func(t *testing.T) {
		got, err := expandFiles(root, []string{"internal/x/new.go"})
		if err != nil {
			t.Fatalf("expandFiles: %v", err)
		}
		assertPathSet(t, got, []string{filepath.FromSlash("internal/x/new.go")})
	})

	t.Run("git dir is skipped", func(t *testing.T) {
		got, err := expandFiles(root, []string{"**/*"})
		if err != nil {
			t.Fatalf("expandFiles: %v", err)
		}
		for _, p := range got {
			if filepath.ToSlash(p) == ".git/config" {
				t.Errorf("expandFiles returned a .git path: %q", p)
			}
		}
	})

	t.Run("escaping path is rejected", func(t *testing.T) {
		if _, err := expandFiles(root, []string{"../escape.go"}); err == nil {
			t.Error("expandFiles should reject a path escaping the root")
		}
	})
}

// writeTreeFile writes content to <root>/<rel> with raw I/O (no git), creating
// parent dirs. Shared test helper for the pipeline package.
func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
}

// assertPathSet asserts two path slices are equal as sets (order-independent).
func assertPathSet(t *testing.T, got, want []string) {
	t.Helper()
	gc := append([]string(nil), got...)
	wc := append([]string(nil), want...)
	sort.Strings(gc)
	sort.Strings(wc)
	if !reflect.DeepEqual(gc, wc) {
		t.Errorf("path set = %v; want %v", gc, wc)
	}
}
