package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/run"
)

// worktreeConfig returns a default config switched to worktree isolation with the
// allow-worktree git policy (the opt-in worktree requires).
func worktreeConfig() config.Config {
	cfg := config.Default()
	cfg.Pipeline.Execution.Isolation = isolationWorktree
	cfg.Pipeline.Execution.Parallel = false // serial keeps the worktree assertions simple.
	cfg.Git.Policy = git.PolicyAllowWorktree
	return cfg
}

// TestWorktreeProvisionsReconcilesAndRemoves is acceptance criterion 5 (the happy
// path): under allow-worktree the scheduler provisions a worktree, the executor edits
// a file inside it, the change is RECONCILED (copied) back into the main tree, and the
// worktree is removed — all WITHOUT any mutating git (the fake records Add/Remove).
func TestWorktreeProvisionsReconcilesAndRemoves(t *testing.T) {
	cfg := worktreeConfig()

	root := t.TempDir()
	// The main tree starts with an old version of the file the subtask owns.
	writeTreeFile(t, root, "feature/x.go", "package feature\n// old\n")

	subs := []run.Subtask{pending("st-01", nil, "feature/x.go")}
	store, r := newSchedRun(t, cfg, subs)

	fg := newFakeGit(root)
	h := newWritingHarness("executor")
	// The executor (running inside the worktree) rewrites the file.
	h.filesByCall = []map[string]string{
		{"feature/x.go": "package feature\n// NEW from worktree\n"},
	}

	if err := runScheduler(t, cfg, store, r, fg, h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	// The executor ran in the worktree, not the repo root.
	if len(h.workDirsLog) != 1 {
		t.Fatalf("executor workdir log = %v; want one entry", h.workDirsLog)
	}
	if h.workDirsLog[0] == root {
		t.Errorf("executor ran in the repo root %q; expected a worktree dir", root)
	}

	// Reconcile copied the worktree change back into the MAIN tree.
	got, err := os.ReadFile(filepath.Join(root, "feature", "x.go"))
	if err != nil {
		t.Fatalf("reading reconciled main-tree file: %v", err)
	}
	if !strings.Contains(string(got), "NEW from worktree") {
		t.Errorf("main tree was not reconciled with the worktree change; got:\n%s", got)
	}

	// Worktree manager: one Add, fully removed (RemoveAll on defer), nothing live.
	mgrs := fg.allManagers()
	if len(mgrs) != 1 {
		t.Fatalf("worktree managers handed out = %d; want 1", len(mgrs))
	}
	m := mgrs[0]
	if m.addCount() != 1 {
		t.Errorf("worktree Add count = %d; want 1", m.addCount())
	}
	if m.removeCount() < 1 {
		t.Errorf("worktree was not removed (removeCount=%d)", m.removeCount())
	}
	if m.liveCount() != 0 {
		t.Errorf("worktree leaked: %d still live", m.liveCount())
	}
	// The diff was still captured and reflects the change.
	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	patch, _ := os.ReadFile(layout.SubtaskDiffFile("st-01"))
	if !strings.Contains(string(patch), "NEW from worktree") {
		t.Errorf("worktree subtask diff did not capture the change:\n%s", patch)
	}
}

// TestWorktreeRemovedEvenOnExecutorError is acceptance criterion 5/6: when the
// executor errors mid-subtask, the worktree is STILL removed (defer cleanup), and the
// subtask is marked failed. No worktree leaks.
func TestWorktreeRemovedEvenOnExecutorError(t *testing.T) {
	cfg := worktreeConfig()
	root := t.TempDir()
	writeTreeFile(t, root, "feature/x.go", "package feature\n")
	subs := []run.Subtask{pending("st-01", nil, "feature/x.go")}
	store, r := newSchedRun(t, cfg, subs)

	fg := newFakeGit(root)
	h := newWritingHarness("executor")
	h.mock.PushError(harness.Result{}, errors.New("executor blew up inside the worktree"))

	err := runScheduler(t, cfg, store, r, fg, h)
	if err == nil {
		t.Fatal("scheduler should fail when the only subtask's executor errors")
	}

	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskFailed {
		t.Errorf("st-01 = %q; want failed", s.Status)
	}
	mgrs := fg.allManagers()
	if len(mgrs) != 1 {
		t.Fatalf("worktree managers = %d; want 1", len(mgrs))
	}
	if m := mgrs[0]; m.liveCount() != 0 || m.removeCount() < 1 {
		t.Errorf("worktree leaked on executor error: live=%d removed=%d", m.liveCount(), m.removeCount())
	}
}

// TestWorktreeRefusedUnderReadOnlyPolicy is acceptance criterion 5 (refusal half):
// isolation=worktree with the default read-only policy must fail with an actionable
// error pointing at allow-worktree, and the subtask must not be marked done.
func TestWorktreeRefusedUnderReadOnlyPolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Isolation = isolationWorktree
	cfg.Pipeline.Execution.Parallel = false
	cfg.Git.Policy = git.PolicyReadOnly // NOT opted in

	root := t.TempDir()
	writeTreeFile(t, root, "feature/x.go", "package feature\n")
	subs := []run.Subtask{pending("st-01", nil, "feature/x.go")}
	store, r := newSchedRun(t, cfg, subs)

	fg := newFakeGit(root)
	h := newWritingHarness("executor")

	err := runScheduler(t, cfg, store, r, fg, h)
	if err == nil {
		t.Fatal("worktree isolation under read-only policy must fail")
	}
	if !strings.Contains(err.Error(), git.PolicyAllowWorktree) {
		t.Errorf("error should tell the user to set %q; got: %v", git.PolicyAllowWorktree, err)
	}
	// The executor must never have run (refusal happens before invocation).
	if h.callCount() != 0 {
		t.Errorf("executor invoked %d times; want 0 (refused before running)", h.callCount())
	}
	if s, _ := r.SubtaskByID("st-01"); s.Status == run.SubtaskDone {
		t.Errorf("st-01 reached done despite the worktree refusal")
	}
}

// TestWorktreeAddFailureIsCleanedUp proves that if `worktree add` fails, the subtask
// fails and no worktree is left live (the failing-Add path still runs cleanup).
func TestWorktreeAddFailureIsCleanedUp(t *testing.T) {
	cfg := worktreeConfig()
	root := t.TempDir()
	writeTreeFile(t, root, "feature/x.go", "package feature\n")
	subs := []run.Subtask{pending("st-01", nil, "feature/x.go")}
	store, r := newSchedRun(t, cfg, subs)

	fg := newFakeGit(root)
	fg.failAddErr = errors.New("worktree add failed")
	h := newWritingHarness("executor")

	err := runScheduler(t, cfg, store, r, fg, h)
	if err == nil {
		t.Fatal("scheduler should fail when worktree provisioning fails")
	}
	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskFailed {
		t.Errorf("st-01 = %q; want failed", s.Status)
	}
	if h.callCount() != 0 {
		t.Errorf("executor invoked %d times; want 0 (worktree add failed first)", h.callCount())
	}
	for _, m := range fg.allManagers() {
		if m.liveCount() != 0 {
			t.Errorf("worktree leaked after a failed add: %d live", m.liveCount())
		}
	}
}

// compile-time assertion that *git.WorktreeManager satisfies the scheduler's
// worktreeManager interface, so the production adapter (gitGatewayAdapter) wires up.
var _ worktreeManager = (*git.WorktreeManager)(nil)

// compile-time assertion that the production adapter satisfies gitGateway.
var _ gitGateway = gitGatewayAdapter{}

// silence unused in case a build tag drops a test referencing context.
var _ = context.Background
