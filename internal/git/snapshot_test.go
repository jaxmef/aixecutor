package git

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// nulList encodes paths as git's -z (NUL-delimited) output for fake runners.
func nulList(paths ...string) []byte {
	if len(paths) == 0 {
		return nil
	}
	return []byte(strings.Join(paths, "\x00") + "\x00")
}

// enumRunner is a fake runnerFunc that answers the two enumeration commands the
// baseline uses (`ls-files -z` and `ls-files --others --exclude-standard -z`)
// with canned listings. It lets the .gitignore test feed a listing that OMITS
// ignored paths — exactly what real git with --exclude-standard would do —
// without running git for enumeration.
//
// `diff --no-index` is delegated to the real execRunner: it is read-only, works
// outside a repo, and is what FullDiff legitimately needs. Delegating it (rather
// than faking diff output) keeps the diff assertions honest while the
// enumeration stays hermetic. Any other git call fails the runner.
type enumRunner struct {
	tracked   []string
	untracked []string
}

func (e *enumRunner) run(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "ls-files -z":
		return nulList(e.tracked...), nil, nil
	case joined == "ls-files --others --exclude-standard -z":
		return nulList(e.untracked...), nil, nil
	case len(args) >= 2 && args[0] == "diff" && args[1] == "--no-index":
		return execRunner(ctx, dir, args...)
	default:
		return nil, []byte("unexpected git call in test: " + joined), errUnexpectedGit
	}
}

var errUnexpectedGit = &gitTestError{"unexpected git call"}

type gitTestError struct{ msg string }

func (e *gitTestError) Error() string { return e.msg }

// snapshotNames returns the snapshot's captured repo-relative paths using "/" as
// the separator so assertions are OS-independent.
func snapshotNames(s Snapshot) []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = filepath.ToSlash(f)
	}
	sort.Strings(out)
	return out
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestCaptureBaselineRespectsGitignore is acceptance criterion 4: .gitignored
// paths are not snapshotted. The fake enumeration runner returns ONLY the
// non-ignored files (mirroring `git ls-files --exclude-standard`), even though
// ignored files (node_modules, build artifacts) also physically exist in the
// repo dir. We then assert the baseline copied exactly the listed files and did
// NOT copy the ignored ones. No git is executed.
func TestCaptureBaselineRespectsGitignore(t *testing.T) {
	repo := t.TempDir()
	// Real files on disk: some tracked, one untracked-non-ignored, and two that
	// would be .gitignored (present on disk but absent from the enumeration).
	writeFile(t, repo, "main.go", "package main\n")
	writeFile(t, repo, "pkg/util.go", "package pkg\n")
	writeFile(t, repo, "new.txt", "untracked but not ignored\n")
	writeFile(t, repo, "node_modules/dep/index.js", "ignored\n")
	writeFile(t, repo, "build/out.bin", "ignored binary\n")

	er := &enumRunner{
		tracked:   []string{"main.go", "pkg/util.go"},
		untracked: []string{"new.txt"}, // node_modules/build deliberately omitted
	}
	g := newGatewayWithRunner(repo, er.run)

	dst := filepath.Join(t.TempDir(), ".baseline")
	b, err := g.CaptureBaseline(context.Background(), dst, nil)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	gotNames := snapshotNames(b.Snapshot)
	want := []string{"main.go", "new.txt", "pkg/util.go"}
	if strings.Join(gotNames, ",") != strings.Join(want, ",") {
		t.Errorf("snapshotted files = %v; want %v", gotNames, want)
	}

	// The listed files were copied...
	for _, rel := range want {
		if !fileExists(filepath.Join(dst, rel)) {
			t.Errorf("expected %q to be copied into baseline", rel)
		}
	}
	// ...and the ignored ones were NOT, even though they exist in the repo dir.
	for _, rel := range []string{"node_modules/dep/index.js", "build/out.bin"} {
		if fileExists(filepath.Join(dst, rel)) {
			t.Errorf("ignored path %q must not be snapshotted", rel)
		}
	}
}

// TestCaptureBaselineCopiesContents checks that copied file contents match the
// originals (raw I/O fidelity).
func TestCaptureBaselineCopiesContents(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "a.txt", "hello world\n")
	er := &enumRunner{tracked: []string{"a.txt"}}
	g := newGatewayWithRunner(repo, er.run)

	dst := filepath.Join(t.TempDir(), ".baseline")
	if _, err := g.CaptureBaseline(context.Background(), dst, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(got) != "hello world\n" {
		t.Errorf("copied content = %q; want %q", got, "hello world\n")
	}
}

// TestSnapshotFilesSkipsMissingPaths verifies that a declared path which does not
// exist is skipped (not an error): a subtask may declare a file it will create.
func TestSnapshotFilesSkipsMissingPaths(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "exists.go", "package x\n")
	dst := filepath.Join(t.TempDir(), "snap")
	snap, err := snapshotFiles(repo, dst, []string{"exists.go", "missing.go"}, nil)
	if err != nil {
		t.Fatalf("snapshotFiles: %v", err)
	}
	if strings.Join(snapshotNames(snap), ",") != "exists.go" {
		t.Errorf("files = %v; want only exists.go", snapshotNames(snap))
	}
}

// TestSnapshotFilesRejectsEscapingPaths ensures a path trying to climb out of the
// repo root is refused rather than reading outside the tree.
func TestSnapshotFilesRejectsEscapingPaths(t *testing.T) {
	repo := t.TempDir()
	_, err := snapshotFiles(repo, filepath.Join(t.TempDir(), "snap"), []string{"../escape"}, nil)
	if err == nil || !strings.Contains(err.Error(), "escapes the repository root") {
		t.Fatalf("want escape error, got %v", err)
	}
}

// TestSnapshotFilesRecursesDirectories checks that declaring a directory copies
// the regular files beneath it, preserving structure.
func TestSnapshotFilesRecursesDirectories(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "dir/a.go", "a\n")
	writeFile(t, repo, "dir/sub/b.go", "b\n")
	dst := filepath.Join(t.TempDir(), "snap")
	snap, err := snapshotFiles(repo, dst, []string{"dir"}, nil)
	if err != nil {
		t.Fatalf("snapshotFiles: %v", err)
	}
	want := []string{"dir/a.go", "dir/sub/b.go"}
	if strings.Join(snapshotNames(snap), ",") != strings.Join(want, ",") {
		t.Errorf("files = %v; want %v", snapshotNames(snap), want)
	}
}

// TestSnapshotSizeGuardWarns confirms the soft size guard fires the warn callback
// exactly once when the cumulative copy crosses the ceiling. It uses the
// limit-injecting seam (snapshotFilesWithLimit) with a tiny limit so the guard
// can be exercised without writing hundreds of megabytes.
func TestSnapshotSizeGuardWarns(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "big.bin", strings.Repeat("x", 2048))
	writeFile(t, repo, "small.bin", "y")

	var warnedBytes int64
	warns := 0
	warn := func(b int64) { warns++; warnedBytes = b }

	// Use the limit-injecting helper so we do not have to write 256 MiB.
	_, err := snapshotFilesWithLimit(repo, filepath.Join(t.TempDir(), "snap"),
		[]string{"big.bin", "small.bin"}, 1024, warn)
	if err != nil {
		t.Fatalf("snapshotFilesWithLimit: %v", err)
	}
	if warns != 1 {
		t.Errorf("warn called %d times; want exactly 1", warns)
	}
	if warnedBytes < 1024 {
		t.Errorf("warnedBytes = %d; want >= 1024", warnedBytes)
	}
}
