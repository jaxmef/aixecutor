package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureBaselineExcludesRunsDir is the core robustness fix: when the tool's
// own output dir (paths.runsDir, here ".aixecutor/runs") is NOT gitignored — so
// git's enumeration lists files under it — the run-start baseline must NOT
// snapshot anything under runsDir, while still capturing the real project files.
// Without the exclusion the baseline would copy the tool's run artifacts into the
// new run's .baseline (bloat + latent recursion). No git is executed for
// enumeration (faked); nothing mutates git.
func TestCaptureBaselineExcludesRunsDir(t *testing.T) {
	repo := t.TempDir()
	// Real project files (must be captured) plus tool output that git happens to
	// track because runsDir is not gitignored (must be excluded).
	writeFile(t, repo, "main.go", "package main\n")
	writeFile(t, repo, "pkg/util.go", "package pkg\n")
	writeFile(t, repo, ".aixecutor/runs/20240101-x/run.yaml", "id: x\n")
	writeFile(t, repo, ".aixecutor/runs/20240101-x/.baseline/main.go", "package main\n")
	writeFile(t, repo, ".aixecutor/runs/20240101-x/logs/run.log", "log line\n")
	// A sibling dir whose name merely shares the runsDir prefix string must NOT be
	// excluded (segment-boundary matching, not substring).
	writeFile(t, repo, ".aixecutor/runs-archive/keep.txt", "keep me\n")

	er := &enumRunner{
		tracked: []string{
			"main.go",
			"pkg/util.go",
			".aixecutor/runs/20240101-x/run.yaml",
			".aixecutor/runs/20240101-x/.baseline/main.go",
			".aixecutor/runs/20240101-x/logs/run.log",
			".aixecutor/runs-archive/keep.txt",
		},
	}
	g := newGatewayWithRunner(repo, er.run)
	g.SetExcludePrefixes(filepath.FromSlash(".aixecutor/runs"))

	dst := filepath.Join(t.TempDir(), ".baseline")
	b, err := g.CaptureBaseline(context.Background(), dst, nil)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	got := snapshotNames(b.Snapshot) // "/"-separated, sorted
	want := []string{".aixecutor/runs-archive/keep.txt", "main.go", "pkg/util.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("baseline files = %v; want %v", got, want)
	}

	// Belt-and-suspenders: nothing under runsDir was copied to disk...
	for _, rel := range []string{
		".aixecutor/runs/20240101-x/run.yaml",
		".aixecutor/runs/20240101-x/.baseline/main.go",
		".aixecutor/runs/20240101-x/logs/run.log",
	} {
		if fileExists(filepath.Join(dst, filepath.FromSlash(rel))) {
			t.Errorf("runsDir path %q must not be snapshotted into the baseline", rel)
		}
	}
	// ...while the real project files were.
	for _, rel := range []string{"main.go", "pkg/util.go", ".aixecutor/runs-archive/keep.txt"} {
		if !fileExists(filepath.Join(dst, filepath.FromSlash(rel))) {
			t.Errorf("project file %q must be snapshotted into the baseline", rel)
		}
	}
}

// TestFullDiffExcludesRunsDirBothSides proves the senior-review full diff
// (baseline -> current) is symmetric: the configured runsDir is excluded from
// BOTH sides, so changes the tool writes under runsDir between baseline capture
// and the diff (growing run.yaml, new logs/diffs, even a whole new run dir) do
// NOT appear in the diff, while a real project-file change DOES.
//
// Enumeration is faked; the comparison uses the REAL `git diff --no-index`
// (read-only, works outside a repo), so the diff assertions are honest. No
// mutating git runs.
func TestFullDiffExcludesRunsDirBothSides(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "app.go", "package app\nconst V = 1\n")
	writeFile(t, repo, ".aixecutor/runs/run-1/run.yaml", "status: created\n")

	er := &enumRunner{
		tracked: []string{"app.go", ".aixecutor/runs/run-1/run.yaml"},
	}
	g := newGatewayWithRunner(repo, er.run)
	g.SetExcludePrefixes(filepath.FromSlash(".aixecutor/runs"))

	// Baseline: captured at run start. runsDir is excluded here.
	baselineDir := filepath.Join(t.TempDir(), ".baseline")
	baseline, err := g.CaptureBaseline(context.Background(), baselineDir, nil)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The run proceeds: it makes a REAL project change (app.go) AND, as a side
	// effect, the tool writes a lot under runsDir — grows run.yaml, adds logs, and
	// even creates an entirely new run dir. None of the runsDir churn must surface.
	writeFile(t, repo, "app.go", "package app\nconst V = 2\n")
	writeFile(t, repo, ".aixecutor/runs/run-1/run.yaml", "status: completed\nsubtasks: many\n")
	writeFile(t, repo, ".aixecutor/runs/run-1/logs/run.log", "lots of new log output\n")
	writeFile(t, repo, ".aixecutor/runs/run-2/run.yaml", "status: created\n")
	er.tracked = []string{
		"app.go",
		".aixecutor/runs/run-1/run.yaml",
		".aixecutor/runs/run-1/logs/run.log",
		".aixecutor/runs/run-2/run.yaml",
	}

	d, err := g.FullDiff(context.Background(), baseline, nil)
	if err != nil {
		t.Fatalf("FullDiff: %v", err)
	}
	if !d.HasChanges {
		t.Fatal("FullDiff HasChanges = false; want true (app.go changed)")
	}
	// The real project change is present.
	if !strings.Contains(d.Patch, "app.go") ||
		!strings.Contains(d.Patch, "-const V = 1") || !strings.Contains(d.Patch, "+const V = 2") {
		t.Errorf("project-file change missing from full diff:\n%s", d.Patch)
	}
	// NONE of the runsDir churn appears (clean of the tool's own artifacts).
	for _, leak := range []string{"run.yaml", "run.log", "run-2", "status: completed", "lots of new log output"} {
		if strings.Contains(d.Patch, leak) {
			t.Errorf("runsDir artifact %q leaked into the senior-review full diff:\n%s", leak, d.Patch)
		}
	}
}

// TestExcludePrefixMatchingAndNormalization unit-tests the prefix logic directly:
// segment-boundary matching (no substring false-positives), exact-prefix match,
// nested match, and that invalid prefixes (empty, ".", absolute, "..") are dropped
// so they never become an over-broad "exclude everything" filter.
func TestExcludePrefixMatchingAndNormalization(t *testing.T) {
	g := newGatewayWithRunner(t.TempDir(), nil)
	g.SetExcludePrefixes(
		filepath.FromSlash(".aixecutor/runs"),
		"", ".", "..", filepath.FromSlash("../outside"), // all dropped
		string(filepath.Separator)+"abs",      // absolute → dropped
		filepath.FromSlash(".aixecutor/runs"), // duplicate → collapsed
	)
	if got, want := len(g.excludePrefixes), 1; got != want {
		t.Fatalf("excludePrefixes = %v; want exactly the one valid prefix", g.excludePrefixes)
	}

	excluded := []string{
		filepath.FromSlash(".aixecutor/runs"),                // exact
		filepath.FromSlash(".aixecutor/runs/id/run.yaml"),    // nested
		filepath.FromSlash(".aixecutor/runs/id/.baseline/x"), // nested deeper (recursion guard)
	}
	for _, p := range excluded {
		if !g.isExcluded(p) {
			t.Errorf("isExcluded(%q) = false; want true", p)
		}
	}
	notExcluded := []string{
		filepath.FromSlash(".aixecutor/runs-archive/keep"), // shares prefix string only
		filepath.FromSlash(".aixecutor/config.yaml"),       // sibling
		"main.go",
	}
	for _, p := range notExcluded {
		if g.isExcluded(p) {
			t.Errorf("isExcluded(%q) = true; want false", p)
		}
	}
}
