package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFile returns a file's contents as a string, failing the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(b)
}

// TestRestoreTreeRevertsModifyAddDelete is the heart of AIX-0016: restoring to the
// baseline snapshot undoes a run's modifications (revert content), additions
// (delete the new file), and deletions (re-create the removed file) — all via raw
// file I/O, no mutating git. The enumeration runner stands in for read-only
// ls-files; the actual revert touches the filesystem directly.
func TestRestoreTreeRevertsModifyAddDelete(t *testing.T) {
	repo := t.TempDir()
	// Pre-execution tree (the baseline). dirty.txt is a PRE-EXISTING uncommitted
	// change the user had before the run — it must come back byte-for-byte.
	writeFile(t, repo, "keep.go", "package x\nconst A = 1\n")
	writeFile(t, repo, "dirty.txt", "uncommitted work\nLINE2\n")
	writeFile(t, repo, "pkg/del.go", "package pkg\n// will be deleted by the run\n")

	er := &enumRunner{tracked: []string{"keep.go", "dirty.txt", "pkg/del.go"}}
	g := newGatewayWithRunner(repo, er.run)

	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The run executes: modify keep.go, add new.go, delete pkg/del.go.
	writeFile(t, repo, "keep.go", "package x\nconst A = 999\n// mangled by the run\n")
	writeFile(t, repo, "new.go", "package x\nfunc Added() {}\n")
	if err := os.Remove(filepath.Join(repo, "pkg/del.go")); err != nil {
		t.Fatal(err)
	}
	// Current enumeration reflects the post-run tree (new.go present, del.go gone).
	er.tracked = []string{"keep.go", "dirty.txt", "new.go"}

	res, err := g.RestoreTree(context.Background(), snapDir, nil)
	if err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}

	// keep.go reverted, pkg/del.go re-created → 3 restored (keep, dirty, del).
	if res.Restored != 3 {
		t.Errorf("Restored = %d, want 3", res.Restored)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (new.go)", res.Deleted)
	}
	if got := readFile(t, filepath.Join(repo, "keep.go")); got != "package x\nconst A = 1\n" {
		t.Errorf("keep.go not reverted: %q", got)
	}
	if fileExists(filepath.Join(repo, "new.go")) {
		t.Error("new.go (run addition) should have been deleted")
	}
	if got := readFile(t, filepath.Join(repo, "pkg/del.go")); got != "package pkg\n// will be deleted by the run\n" {
		t.Errorf("pkg/del.go not restored: %q", got)
	}
	// Criterion #6: the pre-existing uncommitted file is byte-for-byte identical.
	if got := readFile(t, filepath.Join(repo, "dirty.txt")); got != "uncommitted work\nLINE2\n" {
		t.Errorf("pre-existing uncommitted file not preserved: %q", got)
	}
}

// TestRestoreTreePreservesExcludedPaths proves the exclude set (runsDir + an extra
// docs dir) is never deleted or overwritten by a revert — so run artifacts and the
// just-amended planning docs survive.
func TestRestoreTreePreservesExcludedPaths(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "code.go", "package x\n")

	er := &enumRunner{tracked: []string{"code.go"}}
	g := newGatewayWithRunner(repo, er.run)
	g.SetExcludePrefixes(".aixecutor/runs")

	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The run produces artifacts under runsDir and the user amended docs under a
	// custom docs dir; both are "new" files absent from the baseline.
	writeFile(t, repo, ".aixecutor/runs/r1/run.yaml", "status: paused\n")
	writeFile(t, repo, "planning-docs/subtasks.yaml", "amended: true\n")
	// Enumeration would list neither (runsDir excluded; assume docs untracked but we
	// pass it as an extra exclude), but include them to prove the guard deletes
	// nothing excluded even if it surfaces.
	er.tracked = []string{"code.go"}
	er.untracked = []string{".aixecutor/runs/r1/run.yaml", "planning-docs/subtasks.yaml"}

	if _, err := g.RestoreTree(context.Background(), snapDir, []string{"planning-docs"}); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}

	if !fileExists(filepath.Join(repo, ".aixecutor/runs/r1/run.yaml")) {
		t.Error("runsDir artifact must survive a revert")
	}
	if got := readFile(t, filepath.Join(repo, "planning-docs/subtasks.yaml")); got != "amended: true\n" {
		t.Errorf("amended docs must survive a revert, got %q", got)
	}
}

// TestRestoreTreePrunesEmptiedDirs proves a directory emptied by deleting a run's
// added file is pruned (no stray empty dirs left behind).
func TestRestoreTreePrunesEmptiedDirs(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "root.go", "package x\n")

	er := &enumRunner{tracked: []string{"root.go"}}
	g := newGatewayWithRunner(repo, er.run)

	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	writeFile(t, repo, "fresh/dir/added.go", "package y\n")
	er.tracked = []string{"root.go", "fresh/dir/added.go"}

	if _, err := g.RestoreTree(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}
	if fileExists(filepath.Join(repo, "fresh")) {
		t.Error("emptied directory tree 'fresh/' should have been pruned")
	}
}

// TestRestoreTreePreservesSymlinks proves a pre-existing tracked symlink (which the
// baseline snapshot does not capture — it copies regular files only) is NOT deleted
// by a revert, while a regular run-added file still is. Without the regular-file
// guard the symlink would be wrongly removed (it is present-now, absent-from-snapshot).
func TestRestoreTreePreservesSymlinks(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "real.txt", "real content\n")
	if err := os.Symlink("real.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	// ls-files lists the symlink too; the baseline snapshot will skip it (non-regular).
	er := &enumRunner{tracked: []string{"real.txt", "link.txt"}}
	g := newGatewayWithRunner(repo, er.run)

	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The run adds a regular file; the symlink is untouched.
	writeFile(t, repo, "added.txt", "new\n")
	er.tracked = []string{"real.txt", "link.txt", "added.txt"}

	if _, err := g.RestoreTree(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}

	fi, err := os.Lstat(filepath.Join(repo, "link.txt"))
	if err != nil {
		t.Fatalf("symlink must survive the revert: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link.txt should still be a symlink after the revert")
	}
	if fileExists(filepath.Join(repo, "added.txt")) {
		t.Error("the run-added regular file should have been deleted")
	}
}

// TestRestoreTreeUsesOnlyReadOnlyGit asserts the revert (invariant #1) issues only
// read-only git subcommands — the enumeration — and otherwise touches the tree with
// raw file I/O. A recording runner captures every subcommand for the assertion.
func TestRestoreTreeUsesOnlyReadOnlyGit(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "a.go", "package x\n")

	var subs []string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		subs = append(subs, args[0])
		switch strings.Join(args, " ") {
		case "ls-files -z":
			return nulList("a.go"), nil, nil
		case "ls-files --others --exclude-standard -z":
			return nil, nil, nil
		default:
			return nil, nil, errUnexpectedGit
		}
	}
	g := newGatewayWithRunner(repo, runner)

	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	writeFile(t, repo, "added.go", "package y\n")

	subs = nil // focus the assertion on the restore's git usage.
	if _, err := g.RestoreTree(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}
	if len(subs) == 0 {
		t.Fatal("expected the restore to enumerate the tree via read-only git")
	}
	for _, s := range subs {
		if !allowedReadCmds[s] {
			t.Errorf("revert issued a non-read-only git subcommand %q (invariant #1)", s)
		}
	}
}

// TestRestoreTreeMissingSnapshotErrors guards against a revert with no baseline.
func TestRestoreTreeMissingSnapshotErrors(t *testing.T) {
	g := newGatewayWithRunner(t.TempDir(), (&enumRunner{}).run)
	if _, err := g.RestoreTree(context.Background(), filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Error("expected an error restoring from a missing snapshot dir")
	}
}
