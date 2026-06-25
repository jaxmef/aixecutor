package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestFullDiffEndToEndWithFakeEnumeration exercises FullDiff with a fake
// enumeration runner for the ls-files reads but the REAL `git diff --no-index`
// for the comparison. It proves the full-diff path (baseline -> current tree)
// produces a correct unified diff and excludes ignored paths (the fake omits
// them, as real git would).
//
// Flow: capture a baseline from the repo's current contents, then edit a file on
// disk and add a new one, then FullDiff. Because the same enumeration listing is
// used for both baseline and current snapshots, only the post-baseline edits
// appear. An ignored file present on disk but absent from the listing never shows
// up. No mutating git runs.
func TestFullDiffEndToEndWithFakeEnumeration(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "keep.go", "package x\nconst A = 1\n")
	writeFile(t, repo, "ignored.log", "should never be diffed\n")

	er := &enumRunner{
		tracked: []string{"keep.go"}, // ignored.log intentionally not listed
	}
	g := newGatewayWithRunner(repo, er.run)

	baselineDir := filepath.Join(t.TempDir(), ".baseline")
	baseline, err := g.CaptureBaseline(context.Background(), baselineDir, nil)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// Now the "run" edits keep.go and creates a new tracked file. Update the
	// enumeration to include the new file (as git would once it exists).
	writeFile(t, repo, "keep.go", "package x\nconst A = 2\n")
	writeFile(t, repo, "added.go", "package x\nfunc New() {}\n")
	er.tracked = []string{"keep.go", "added.go"}

	d, err := g.FullDiff(context.Background(), baseline, nil)
	if err != nil {
		t.Fatalf("FullDiff: %v", err)
	}
	if !d.HasChanges {
		t.Fatal("FullDiff HasChanges = false; want true")
	}
	if !strings.Contains(d.Patch, "-const A = 1") || !strings.Contains(d.Patch, "+const A = 2") {
		t.Errorf("modification missing from full diff:\n%s", d.Patch)
	}
	if !strings.Contains(d.Patch, "added.go") || !strings.Contains(d.Patch, "+func New()") {
		t.Errorf("addition missing from full diff:\n%s", d.Patch)
	}
	if strings.Contains(d.Patch, "ignored.log") {
		t.Errorf("ignored file leaked into full diff:\n%s", d.Patch)
	}
}

// TestSnapshotPathsGlobMatching checks that SnapshotPaths expands globs against
// the working tree and snapshots only matching files (the basis for scoping a
// per-subtask diff to its declared paths). Filesystem-only; no git.
func TestSnapshotPathsGlobMatching(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "src/a.go", "a\n")
	writeFile(t, repo, "src/b.go", "b\n")
	writeFile(t, repo, "src/c.txt", "c\n")
	writeFile(t, repo, "other/d.go", "d\n")

	g := newGatewayWithRunner(repo, func(context.Context, string, ...string) ([]byte, []byte, error) {
		t.Fatal("SnapshotPaths must not run git")
		return nil, nil, nil
	})

	dst := filepath.Join(t.TempDir(), "snap")
	snap, err := g.SnapshotPaths(dst, []string{"src/*.go"}, nil)
	if err != nil {
		t.Fatalf("SnapshotPaths: %v", err)
	}
	want := []string{"src/a.go", "src/b.go"}
	if strings.Join(snapshotNames(snap), ",") != strings.Join(want, ",") {
		t.Errorf("globbed snapshot = %v; want %v", snapshotNames(snap), want)
	}
}

// TestSnapshotPathsLiteralNonexistent confirms a literal (non-glob) declared path
// that does not yet exist contributes nothing but is not an error — so an
// after-snapshot can show it as a pure addition.
func TestSnapshotPathsLiteralNonexistent(t *testing.T) {
	repo := t.TempDir()
	g := newGatewayWithRunner(repo, func(context.Context, string, ...string) ([]byte, []byte, error) {
		t.Fatal("SnapshotPaths must not run git")
		return nil, nil, nil
	})
	snap, err := g.SnapshotPaths(filepath.Join(t.TempDir(), "snap"), []string{"will/be/created.go"}, nil)
	if err != nil {
		t.Fatalf("SnapshotPaths: %v", err)
	}
	if len(snap.Files) != 0 {
		t.Errorf("expected empty snapshot for nonexistent literal path, got %v", snap.Files)
	}
}

// TestResolvePatternsRejectsEscape ensures a glob/path that escapes the repo is
// refused at resolution time.
func TestResolvePatternsRejectsEscape(t *testing.T) {
	g := newGatewayWithRunner(t.TempDir(), nil)
	if _, err := g.resolvePatterns([]string{"../../etc/*"}); err == nil {
		t.Fatal("expected escape rejection")
	}
}
