package git

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// manifestPaths returns a manifest's keys as sorted, slash-separated paths so
// assertions are order- and OS-independent.
func manifestPaths(m Manifest) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, filepath.ToSlash(p))
	}
	sort.Strings(out)
	return out
}

type manifestFile struct {
	rel     string
	content string
}

// TestManifestEnumerationAndExclusion exercises Manifest over a temp working tree
// with a fake enumeration runner (the same seam baseline/full-diff tests use). The
// fake stands in for `git ls-files [--others --exclude-standard]`: whatever it omits
// is exactly what real git would omit for a .gitignored path, so the ".gitignored
// excluded" case is honest without running git. Files are written with raw I/O.
func TestManifestEnumerationAndExclusion(t *testing.T) {
	tests := []struct {
		name            string
		files           []manifestFile
		tracked         []string
		untracked       []string
		excludePrefixes []string
		want            []string
	}{
		{
			name: "tracked and untracked included",
			files: []manifestFile{
				{"keep.go", "package x\n"},
				{"new.txt", "hi\n"},
			},
			tracked:   []string{"keep.go"},
			untracked: []string{"new.txt"},
			want:      []string{"keep.go", "new.txt"},
		},
		{
			name: "gitignored path excluded",
			files: []manifestFile{
				{"keep.go", "package x\n"},
				{"ignored.log", "should never appear\n"},
			},
			// ignored.log is omitted from the listing, as --exclude-standard would.
			tracked:   []string{"keep.go"},
			untracked: nil,
			want:      []string{"keep.go"},
		},
		{
			name: "excluded prefix paths absent",
			files: []manifestFile{
				{"keep.go", "package x\n"},
				{".aixecutor/runs/r1/run.yaml", "id: r1\n"},
				{".aixecutor/runs/r1/diff.patch", "diff\n"},
			},
			tracked:         []string{"keep.go"},
			untracked:       []string{".aixecutor/runs/r1/run.yaml", ".aixecutor/runs/r1/diff.patch"},
			excludePrefixes: []string{".aixecutor/runs"},
			want:            []string{"keep.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			sizes := map[string]int64{}
			for _, f := range tt.files {
				writeFile(t, repo, f.rel, f.content)
				sizes[f.rel] = int64(len(f.content))
			}
			er := &enumRunner{tracked: tt.tracked, untracked: tt.untracked}
			g := newGatewayWithRunner(repo, er.run)
			if tt.excludePrefixes != nil {
				g.SetExcludePrefixes(tt.excludePrefixes...)
			}

			m, err := g.Manifest(context.Background(), repo)
			if err != nil {
				t.Fatalf("Manifest: %v", err)
			}
			got := manifestPaths(m)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("manifest paths = %v; want %v", got, tt.want)
			}
			for _, rel := range tt.want {
				meta, ok := m[filepath.FromSlash(rel)]
				if !ok {
					t.Fatalf("expected %q in manifest", rel)
				}
				if meta.Size != sizes[rel] {
					t.Errorf("%q size = %d; want %d", rel, meta.Size, sizes[rel])
				}
				if meta.ModTime.IsZero() {
					t.Errorf("%q mod-time is zero", rel)
				}
			}
		})
	}
}

// TestManifestSurfacesEnumerationError proves an enumeration (git) failure is
// returned rather than swallowed — the caller decides whether to log-and-continue.
func TestManifestSurfacesEnumerationError(t *testing.T) {
	repo := t.TempDir()
	g := newGatewayWithRunner(repo, func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("fatal: not a git repository"), errUnexpectedGit
	})
	if _, err := g.Manifest(context.Background(), repo); err == nil {
		t.Fatal("expected Manifest to surface the enumeration error")
	}
}

// TestManifestDefaultsRootToRepoRoot confirms an empty root falls back to repoRoot.
func TestManifestDefaultsRootToRepoRoot(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "a.go", "package a\n")
	er := &enumRunner{tracked: []string{"a.go"}}
	g := newGatewayWithRunner(repo, er.run)

	m, err := g.Manifest(context.Background(), "")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if _, ok := m["a.go"]; !ok {
		t.Errorf("expected a.go in manifest, got %v", manifestPaths(m))
	}
}

// TestManifestChanged covers the Changed helper: additions, removals, and
// size/mod-time modifications are all reported; unchanged files are not.
func TestManifestChanged(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	t1 := time.Unix(1_700_000_100, 0)

	before := Manifest{
		"same.go":    {ModTime: t0, Size: 10},
		"edited.go":  {ModTime: t0, Size: 10},
		"resized.go": {ModTime: t0, Size: 10},
		"removed.go": {ModTime: t0, Size: 10},
	}
	// after: same.go unchanged; edited.go mod-time changed; resized.go size
	// changed; removed.go gone; added.go is new.
	after := Manifest{
		"same.go":    {ModTime: t0, Size: 10},
		"edited.go":  {ModTime: t1, Size: 10},
		"resized.go": {ModTime: t0, Size: 20},
		"added.go":   {ModTime: t1, Size: 5},
	}

	got := before.Changed(after)
	want := []string{"added.go", "edited.go", "removed.go", "resized.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Changed = %v; want %v", got, want)
	}
}
