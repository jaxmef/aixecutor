package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes content to <dir>/<rel>, creating parent directories. Test
// helper that uses ONLY raw file I/O — never git — so these tests never run a
// mutating git command (CLAUDE.md §7).
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
}

// realGateway returns a gateway wired to the production execRunner. `git diff
// --no-index` is read-only and works outside a repo, so an empty repoRoot is
// fine — these tests exercise the real diff binary without any repo or mutation.
func realGateway() *Gateway { return newGatewayWithRunner("", execRunner) }

// TestDiffTreesReportsAddModDelAndExitCode1 is the core of acceptance criteria
// 2/3: it drives the REAL `git diff --no-index` over two directory trees built
// with raw file writes, asserting the unified diff is correct and that the
// exit-code-1 ("differences found") path is handled as SUCCESS, not an error.
func TestDiffTreesReportsAddModDelAndExitCode1(t *testing.T) {
	before := t.TempDir()
	after := t.TempDir()

	// before: unchanged.txt, modified.txt, deleted.txt
	writeFile(t, before, "unchanged.txt", "same\n")
	writeFile(t, before, "modified.txt", "old line\n")
	writeFile(t, before, "deleted.txt", "gone\n")
	// after: unchanged.txt (same), modified.txt (changed), added.txt (new)
	writeFile(t, after, "unchanged.txt", "same\n")
	writeFile(t, after, "modified.txt", "new line\n")
	writeFile(t, after, "added.txt", "brand new\n")

	d, err := realGateway().DiffTrees(context.Background(), before, after)
	if err != nil {
		t.Fatalf("DiffTrees: %v (exit code 1 must be treated as success)", err)
	}
	if !d.HasChanges {
		t.Fatal("HasChanges = false; want true (trees differ)")
	}

	// Modification shows both sides.
	if !strings.Contains(d.Patch, "-old line") || !strings.Contains(d.Patch, "+new line") {
		t.Errorf("patch missing modification hunk:\n%s", d.Patch)
	}
	// Addition and deletion appear.
	if !strings.Contains(d.Patch, "added.txt") || !strings.Contains(d.Patch, "+brand new") {
		t.Errorf("patch missing addition:\n%s", d.Patch)
	}
	if !strings.Contains(d.Patch, "deleted.txt") || !strings.Contains(d.Patch, "-gone") {
		t.Errorf("patch missing deletion:\n%s", d.Patch)
	}
	// Unchanged file must NOT appear.
	if strings.Contains(d.Patch, "unchanged.txt") {
		t.Errorf("patch should not mention unchanged file:\n%s", d.Patch)
	}
}

// TestDiffTreesHeadersAreRepoRelative guards the patch-path cleanup: headers must
// read as repo-relative paths (a/foo, b/foo), never leak the absolute snapshot
// temp-dir prefix that `git diff --no-index` would otherwise emit.
func TestDiffTreesHeadersAreRepoRelative(t *testing.T) {
	before := t.TempDir()
	after := t.TempDir()
	writeFile(t, before, "calc.go", "package calc\n")
	writeFile(t, after, "calc.go", "package calc\n\nfunc Add() {}\n")

	d, err := realGateway().DiffTrees(context.Background(), before, after)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	for _, dir := range []string{before, after} {
		if strings.Contains(d.Patch, strings.TrimPrefix(dir, "/")) {
			t.Errorf("patch leaks snapshot dir %q:\n%s", dir, d.Patch)
		}
	}
	for _, want := range []string{"diff --git a/calc.go b/calc.go", "--- a/calc.go", "+++ b/calc.go"} {
		if !strings.Contains(d.Patch, want) {
			t.Errorf("patch missing repo-relative header %q:\n%s", want, d.Patch)
		}
	}
}

// TestDiffTreesIdenticalIsNoChange asserts identical trees yield exit 0, empty
// patch, HasChanges=false.
func TestDiffTreesIdenticalIsNoChange(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeFile(t, a, "f.txt", "x\n")
	writeFile(t, b, "f.txt", "x\n")

	d, err := realGateway().DiffTrees(context.Background(), a, b)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	if d.HasChanges {
		t.Errorf("HasChanges = true; want false for identical trees")
	}
	if strings.TrimSpace(d.Patch) != "" {
		t.Errorf("patch = %q; want empty", d.Patch)
	}
}

// TestDiffTreesMissingBeforeDirIsAllAdditions proves the empty-before case: when
// the "before" directory does not exist (a fresh baseline with no prior
// content), the diff is all-additions rather than an error.
func TestDiffTreesMissingBeforeDirIsAllAdditions(t *testing.T) {
	after := t.TempDir()
	writeFile(t, after, "only.txt", "hello\n")
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	d, err := realGateway().DiffTrees(context.Background(), missing, after)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	if !d.HasChanges || !strings.Contains(d.Patch, "+hello") {
		t.Errorf("expected all-additions diff, got HasChanges=%v patch:\n%s", d.HasChanges, d.Patch)
	}
}

// TestFullDiffExcludesPreExistingChanges is acceptance criterion 2's subtle
// requirement: pre-existing uncommitted changes are excluded from the run diff.
// We construct a baseline snapshot (the "starting point") via raw writes, then
// simulate "current tree" raw writes where ONE file matches the baseline
// (representing a pre-existing change captured at run start) and ANOTHER is new
// (a change the run made). Diffing baseline->current must show only the run's
// change. This proves the property by construction: anything already present at
// baseline-capture time is in both trees and cancels out.
func TestFullDiffExcludesPreExistingChanges(t *testing.T) {
	baseline := t.TempDir() // stands in for <run>/.baseline captured at run start
	current := t.TempDir()  // stands in for the working tree after the run

	// A file the user had already modified before the run started: its modified
	// content is what the baseline captured, so it is identical in both trees.
	writeFile(t, baseline, "preexisting.txt", "user already changed this\n")
	writeFile(t, current, "preexisting.txt", "user already changed this\n")

	// A file the run created/changed AFTER baseline capture.
	writeFile(t, current, "run-made.txt", "the run did this\n")

	d, err := realGateway().DiffTrees(context.Background(), baseline, current)
	if err != nil {
		t.Fatalf("DiffTrees: %v", err)
	}
	if strings.Contains(d.Patch, "preexisting.txt") {
		t.Errorf("pre-existing change leaked into run diff:\n%s", d.Patch)
	}
	if !strings.Contains(d.Patch, "run-made.txt") || !strings.Contains(d.Patch, "+the run did this") {
		t.Errorf("run's own change missing from diff:\n%s", d.Patch)
	}
}

// TestSubtaskDiffReflectsOnlyDeclaredPaths is acceptance criterion 3: a
// per-subtask diff reflects only the subtask's declared paths. We snapshot two
// before/after dirs that contain ONLY the declared file (because SnapshotPaths
// upstream copies only declared globs), and assert the diff is scoped to it.
func TestSubtaskDiffReflectsOnlyDeclaredPaths(t *testing.T) {
	before := t.TempDir()
	after := t.TempDir()
	// Only the declared path "src/feature.go" is present in either snapshot.
	writeFile(t, before, "src/feature.go", "package x\n")
	writeFile(t, after, "src/feature.go", "package x\n\nfunc New() {}\n")

	d, err := realGateway().SubtaskDiff(context.Background(),
		Snapshot{Dir: before}, Snapshot{Dir: after})
	if err != nil {
		t.Fatalf("SubtaskDiff: %v", err)
	}
	if !d.HasChanges {
		t.Fatal("expected changes")
	}
	if !strings.Contains(d.Patch, "src/feature.go") || !strings.Contains(d.Patch, "+func New()") {
		t.Errorf("subtask diff missing declared-path change:\n%s", d.Patch)
	}
	// Nothing outside the declared path can appear because nothing else was
	// snapshotted; assert no other file name sneaks in.
	if strings.Contains(d.Patch, "other") {
		t.Errorf("subtask diff mentions a non-declared path:\n%s", d.Patch)
	}
}
