package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jaxmef/aixecutor/internal/git"
)

// writeFile writes content under dir/rel, creating parents.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// markRepo creates a `.git` marker dir so the directory is discovered as a repo
// root — no real git is ever run.
func markRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// diskLsRunner is a fake git runner that answers `ls-files` by enumerating the real
// regular files on disk under repoDir (skipping .git), mimicking a clean tracked
// tree without running git. Untracked enumeration returns empty.
func diskLsRunner(repoDir string) git.RunnerFunc {
	return func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "ls-files -z":
			var rels []string
			_ = filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return fs.SkipDir
					}
					return nil
				}
				if d.Type().IsRegular() {
					rel, _ := filepath.Rel(repoDir, p)
					rels = append(rels, filepath.ToSlash(rel))
				}
				return nil
			})
			sort.Strings(rels)
			var b strings.Builder
			for _, r := range rels {
				b.WriteString(r)
				b.WriteByte(0)
			}
			return []byte(b.String()), nil, nil
		case "ls-files --others --exclude-standard -z":
			return nil, nil, nil
		default:
			return nil, []byte("unexpected git call: " + strings.Join(args, " ")), errUnexpected
		}
	}
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

var errUnexpected = &testErr{"unexpected git call in workspace test"}

// fakeOpener returns repos as fake-runner gateways enumerating real disk files.
func fakeOpener() func(string) (*git.Gateway, error) {
	return func(dir string) (*git.Gateway, error) {
		return git.NewGatewayWithRunner(dir, diskLsRunner(dir)), nil
	}
}

func discover(t *testing.T, root string, excludes ...string) *Workspace {
	t.Helper()
	ws, err := Discover(root, Options{
		MaxDepth:        8,
		Ignore:          []string{"node_modules"},
		ExcludePrefixes: excludes,
		Opener:          fakeOpener(),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return ws
}

func relsOf(t *testing.T, ws *Workspace) []string {
	t.Helper()
	rels, err := ws.CurrentRels(context.Background())
	if err != nil {
		t.Fatalf("CurrentRels: %v", err)
	}
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = filepath.ToSlash(r)
	}
	sort.Strings(out)
	return out
}

func TestDiscoverSingleRepo(t *testing.T) {
	root := t.TempDir()
	markRepo(t, root)
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "pkg/util.go", "package pkg\n")

	ws := discover(t, root)
	if got := ws.Repos(); len(got) != 1 || got[0] != "." {
		t.Errorf("Repos() = %v, want [.]", got)
	}
	if got, want := relsOf(t, ws), []string{"main.go", "pkg/util.go"}; !eq(got, want) {
		t.Errorf("CurrentRels = %v, want %v", got, want)
	}
}

func TestDiscoverPlainDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "notes.md", "hi\n")
	writeFile(t, root, "src/app.py", "print()\n")
	writeFile(t, root, "node_modules/dep/index.js", "ignored\n")

	ws := discover(t, root)
	if got := ws.Repos(); len(got) != 0 {
		t.Errorf("Repos() = %v, want none (plain dir)", got)
	}
	if got, want := relsOf(t, ws), []string{"notes.md", "src/app.py"}; !eq(got, want) {
		t.Errorf("CurrentRels = %v, want %v (node_modules ignored)", got, want)
	}
}

func TestDiscoverMultiRepo(t *testing.T) {
	root := t.TempDir()
	// Two repos + a plain area + an ignored dir, all under the workspace root.
	markRepo(t, filepath.Join(root, "repoA"))
	writeFile(t, root, "repoA/a.go", "package a\n")
	markRepo(t, filepath.Join(root, "repoB"))
	writeFile(t, root, "repoB/b.go", "package b\n")
	writeFile(t, root, "shared/loose.txt", "plain\n")
	writeFile(t, root, "node_modules/x/i.js", "ignored\n")

	ws := discover(t, root)
	if got, want := ws.Repos(), []string{"repoA", "repoB"}; !eq(got, want) {
		t.Errorf("Repos() = %v, want %v", got, want)
	}
	got := relsOf(t, ws)
	want := []string{"repoA/a.go", "repoB/b.go", "shared/loose.txt"}
	if !eq(got, want) {
		t.Errorf("CurrentRels = %v, want %v", got, want)
	}
}

func TestCurrentRelsExcludesRunsDir(t *testing.T) {
	root := t.TempDir()
	markRepo(t, root)
	writeFile(t, root, "code.go", "package x\n")
	writeFile(t, root, ".aixecutor/runs/r1/run.yaml", "status: x\n")

	ws := discover(t, root, ".aixecutor/runs")
	if got, want := relsOf(t, ws), []string{"code.go"}; !eq(got, want) {
		t.Errorf("CurrentRels = %v, want %v (runsDir excluded)", got, want)
	}
}

// TestWorkspaceRestoreAcrossReposPreservesDirty is the headline AIX-0020 criterion:
// a revert restores the ENTIRE workspace to the pre-execution state across multiple
// repos and the plain area — including pre-existing uncommitted (dirty) changes in
// each repo, byte-for-byte — while deleting files the run added, with no mutating git.
func TestWorkspaceRestoreAcrossReposPreservesDirty(t *testing.T) {
	root := t.TempDir()
	markRepo(t, filepath.Join(root, "repoA"))
	markRepo(t, filepath.Join(root, "repoB"))
	// Pre-execution state, with a dirty (uncommitted) file in each repo + the plain area.
	writeFile(t, root, "repoA/keep.go", "package a\nconst A = 1\n")
	writeFile(t, root, "repoA/dirty.go", "package a\n// uncommitted work A\n")
	writeFile(t, root, "repoB/dirty.go", "package b\n// uncommitted work B\n")
	writeFile(t, root, "plain/notes.md", "original notes\n")

	ws := discover(t, root)
	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := ws.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The run spans both repos and the plain area: modify, add, and delete.
	writeFile(t, root, "repoA/keep.go", "package a\nconst A = 999 // mangled\n")
	writeFile(t, root, "repoA/added.go", "package a\nfunc New() {}\n")
	writeFile(t, root, "repoB/added.go", "package b\nfunc New() {}\n")
	writeFile(t, root, "plain/notes.md", "MANGLED notes\n")
	if err := os.Remove(filepath.Join(root, "repoB/dirty.go")); err != nil {
		t.Fatal(err)
	}

	if _, err := ws.RestoreTree(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}

	// Modifications reverted across repos + plain area.
	assertFile(t, root, "repoA/keep.go", "package a\nconst A = 1\n")
	assertFile(t, root, "plain/notes.md", "original notes\n")
	// Run additions deleted in both repos.
	if fileExists(filepath.Join(root, "repoA/added.go")) || fileExists(filepath.Join(root, "repoB/added.go")) {
		t.Error("run-added files should be deleted across repos")
	}
	// Pre-existing dirty files restored byte-for-byte (one re-created after deletion).
	assertFile(t, root, "repoA/dirty.go", "package a\n// uncommitted work A\n")
	assertFile(t, root, "repoB/dirty.go", "package b\n// uncommitted work B\n")
}

// TestWorkspaceUsesOnlyReadOnlyGitPerRepo asserts invariant #1 across the workspace:
// enumeration of every repo issues only read-only git subcommands (ls-files); the
// baseline/diff/restore are raw file I/O. A recording opener captures every git
// subcommand driven through any repo gateway.
func TestWorkspaceUsesOnlyReadOnlyGitPerRepo(t *testing.T) {
	root := t.TempDir()
	markRepo(t, filepath.Join(root, "repoA"))
	markRepo(t, filepath.Join(root, "repoB"))
	writeFile(t, root, "repoA/a.go", "package a\n")
	writeFile(t, root, "repoB/b.go", "package b\n")

	var mu sync.Mutex
	var subs []string
	recordingOpener := func(dir string) (*git.Gateway, error) {
		base := diskLsRunner(dir)
		rec := func(ctx context.Context, d string, args ...string) ([]byte, []byte, error) {
			mu.Lock()
			subs = append(subs, args[0])
			mu.Unlock()
			return base(ctx, d, args...)
		}
		return git.NewGatewayWithRunner(dir, rec), nil
	}

	ws, err := Discover(root, Options{MaxDepth: 8, Opener: recordingOpener})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	snapDir := filepath.Join(t.TempDir(), ".baseline")
	if _, err := ws.CaptureBaseline(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	writeFile(t, root, "repoA/added.go", "package a\n")
	if _, err := ws.RestoreTree(context.Background(), snapDir, nil); err != nil {
		t.Fatalf("RestoreTree: %v", err)
	}

	if len(subs) == 0 {
		t.Fatal("expected read-only git enumeration across repos")
	}
	for _, s := range subs {
		if s != "ls-files" {
			t.Errorf("workspace issued a non-read-only git subcommand %q (invariant #1)", s)
		}
	}
}

func TestManifestSpansReposAndPlainAreaWithExclusions(t *testing.T) {
	root := t.TempDir()
	markRepo(t, filepath.Join(root, "repoA"))
	writeFile(t, root, "repoA/a.go", "package a\n")
	markRepo(t, filepath.Join(root, "repoB"))
	writeFile(t, root, "repoB/b.go", "package b\n")
	writeFile(t, root, "shared/loose.txt", "plain\n")
	writeFile(t, root, "node_modules/x/i.js", "ignored\n")
	writeFile(t, root, ".aixecutor/runs/r1/run.yaml", "status: x\n")

	ws := discover(t, root, ".aixecutor/runs")
	m, err := ws.Manifest(context.Background(), "")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	got := make([]string, 0, len(m))
	for p := range m {
		got = append(got, filepath.ToSlash(p))
	}
	sort.Strings(got)
	want := []string{"repoA/a.go", "repoB/b.go", "shared/loose.txt"}
	if !eq(got, want) {
		t.Errorf("Manifest paths = %v, want %v (node_modules + runsDir excluded)", got, want)
	}

	// The manifest fingerprints real files: size must match the bytes on disk.
	if fm := m[filepath.FromSlash("shared/loose.txt")]; fm.Size != int64(len("plain\n")) {
		t.Errorf("Manifest size for shared/loose.txt = %d, want %d", fm.Size, len("plain\n"))
	}
}

// TestExcludeNamesDropsEditorDirsFromSnapshots verifies Options.ExcludeNames is wired
// into ws.snap so editor/tool dirs (matched by base name at any depth) never enter the
// workspace snapshots the review diffs are computed from, while a real edit survives.
func TestExcludeNamesDropsEditorDirsFromSnapshots(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, ".idea/workspace.xml", "<project/>\n")
	writeFile(t, root, ".DS_Store", "\x00\x01\n")
	writeFile(t, root, "pkg/.vscode/settings.json", "{}\n")

	ws, err := Discover(root, Options{
		MaxDepth:     8,
		ExcludeNames: []string{".idea", ".vscode", ".DS_Store"},
		Opener:       fakeOpener(),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	snapDir := filepath.Join(t.TempDir(), "snap")
	paths := []string{"main.go", ".idea/workspace.xml", ".DS_Store", "pkg/.vscode/settings.json"}
	if _, err := ws.SnapshotPaths(snapDir, paths, nil); err != nil {
		t.Fatalf("SnapshotPaths: %v", err)
	}
	if !fileExists(filepath.Join(snapDir, "main.go")) {
		t.Error("real file main.go missing from snapshot")
	}
	for _, ed := range []string{".idea/workspace.xml", ".DS_Store", "pkg/.vscode/settings.json"} {
		if fileExists(filepath.Join(snapDir, filepath.FromSlash(ed))) {
			t.Errorf("editor/tool path %q leaked into snapshot", ed)
		}
	}
}

func assertFile(t *testing.T, root, rel, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %q: %v", rel, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", rel, got, want)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestPlainWalkBoundedByMaxDepth proves the scope guard: the plain-area walk does
// not descend past MaxDepth, so a huge/deep tree is not enumerated without limit.
func TestPlainWalkBoundedByMaxDepth(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "top.txt", "shallow\n")       // depth 2 (under root)
	writeFile(t, root, "a/mid.txt", "mid\n")         // dir a = depth 2; file kept
	writeFile(t, root, "a/b/deep.txt", "too deep\n") // dir a/b = depth 3; skipped

	ws, err := Discover(root, Options{MaxDepth: 2, Opener: fakeOpener()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := relsOf(t, ws)
	want := []string{"a/mid.txt", "top.txt"}
	if !eq(got, want) {
		t.Errorf("CurrentRels = %v, want %v (a/b beyond MaxDepth skipped)", got, want)
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
