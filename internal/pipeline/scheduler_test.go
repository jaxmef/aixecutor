package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// ---------------------------------------------------------------------------
// Hermetic test doubles. None of these run git, a real agent, or the network.
// ---------------------------------------------------------------------------

// fakeGit is a hermetic gitGateway. SnapshotPaths copies the listed (literal) paths
// from root with raw file I/O — exactly like the real gateway's literal path, but
// without filepath.Glob (expansion already happened in expandFiles). DiffTrees
// synthesizes a deterministic patch from the after-snapshot so a test can assert the
// persisted diff.patch reflects the files the mock executor wrote, WITHOUT invoking
// `git diff`. Worktree returns a fakeWorktreeManager only under allow-worktree,
// mirroring the real gateway's policy gate.
type fakeGit struct {
	root string

	// fullDiffs is the scripted sequence of full-diff patches returned by FullDiff,
	// consumed front-to-back; once exhausted the LAST entry repeats. It lets a
	// senior-review test prove the full diff is RECOMPUTED each round by returning a
	// different patch per round and asserting the reviewer saw the latest. When nil,
	// FullDiff synthesizes a patch from the current tree (the baselineDir is unused
	// by the fake) so callers that do not care about scripted diffs still get one.
	fullDiffs    []string
	fullDiffCall int
	// fullDiffBaselineDirs records the baselineDir passed to each FullDiff call, so a
	// test can assert the phase diffs against the persisted run baseline.
	fullDiffBaselineDirs []string

	// worktreePolicySeen records the policy passed to Worktree, for assertions.
	mu                 sync.Mutex
	worktreePolicySeen string
	// managers records every WorktreeManager handed out (one per Worktree call,
	// mirroring the real gateway), so a test can inspect Add/Remove across them.
	managers []*fakeWorktreeManager
	// failAddOnce, when set, makes the FIRST manager's Add fail (to exercise the
	// worktree-removed-even-on-error path).
	failAddErr error

	// restoreCalls records each RestoreTree invocation (the AIX-0016 revert) so an
	// amend test can assert the revert ran against the run baseline. restoreFn, when
	// set, performs the actual tree mutation for an end-to-end amend test.
	restoreCalls []restoreCall
	restoreFn    func(snapshotDir string, excludes []string) (git.RestoreResult, error)
}

// restoreCall captures the arguments of one fakeGit.RestoreTree invocation.
type restoreCall struct {
	snapshotDir string
	excludes    []string
}

func newFakeGit(root string) *fakeGit { return &fakeGit{root: root} }

func (f *fakeGit) RepoRoot() string { return f.root }

func (f *fakeGit) SnapshotPaths(dstDir string, paths []string, _ func(bytes int64)) (git.Snapshot, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return git.Snapshot{}, err
	}
	for _, rel := range paths {
		src := filepath.Join(f.root, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // declared-but-not-created.
			}
			return git.Snapshot{}, err
		}
		dst := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return git.Snapshot{}, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return git.Snapshot{}, err
		}
	}
	return git.Snapshot{Dir: dstDir}, nil
}

// DiffTrees produces a deterministic, assertable patch: for each file in afterDir
// whose content differs from beforeDir (or is new), emit a "+++ <rel>" header and
// the file body. This is enough for tests to assert the diff reflects known files;
// the REAL unified-diff engine is covered by internal/git's tests.
func (f *fakeGit) DiffTrees(_ context.Context, beforeDir, afterDir string) (git.Diff, error) {
	var b strings.Builder
	changed := false
	err := filepath.WalkDir(afterDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(afterDir, path)
		afterData, _ := os.ReadFile(path)
		beforeData, berr := os.ReadFile(filepath.Join(beforeDir, rel))
		if berr == nil && string(beforeData) == string(afterData) {
			return nil // unchanged: not in the diff.
		}
		changed = true
		fmt.Fprintf(&b, "+++ %s\n%s", filepath.ToSlash(rel), afterData)
		return nil
	})
	if err != nil {
		return git.Diff{}, err
	}
	return git.Diff{Patch: b.String(), HasChanges: changed}, nil
}

// FullDiff returns the next scripted full-diff patch (the last repeats once the
// script is exhausted), or, when no script is set, synthesizes a patch from the
// current tree by diffing the baselineDir against root via SnapshotPaths-style
// reads. It records the baselineDir it was handed so a test can assert the phase
// passes the persisted run baseline. No real git is run.
func (f *fakeGit) FullDiff(ctx context.Context, baselineDir string) (git.Diff, error) {
	f.mu.Lock()
	f.fullDiffBaselineDirs = append(f.fullDiffBaselineDirs, baselineDir)
	idx := f.fullDiffCall
	f.fullDiffCall++
	var scripted *string
	if len(f.fullDiffs) > 0 {
		s := f.fullDiffs[len(f.fullDiffs)-1]
		if idx < len(f.fullDiffs) {
			s = f.fullDiffs[idx]
		}
		scripted = &s
	}
	f.mu.Unlock()

	if scripted != nil {
		return git.Diff{Patch: *scripted, HasChanges: *scripted != ""}, nil
	}
	// No script: synthesize a patch from the current tree so the reviewer still
	// sees something derived from the real files. Diff the baseline snapshot dir
	// against the repo root.
	return f.DiffTrees(ctx, baselineDir, f.root)
}

// fullDiffCalls returns how many times FullDiff was invoked, for assertions.
func (f *fakeGit) fullDiffCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fullDiffCall
}

func (f *fakeGit) Worktree(policy string) (worktreeManager, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.worktreePolicySeen = policy
	if policy != git.PolicyAllowWorktree {
		// Mirror the real gateway's actionable refusal.
		return nil, fmt.Errorf("git worktree isolation is disabled: git.policy is %q; set git.policy: %q to opt in",
			policy, git.PolicyAllowWorktree)
	}
	// One manager per call, exactly like *git.Gateway.Worktree, so each subtask owns
	// its worktree(s) and its RemoveAll only tears down its own.
	base, err := os.MkdirTemp("", "aixecutor-fake-wt-*")
	if err != nil {
		return nil, err
	}
	m := &fakeWorktreeManager{base: base, seedFrom: f.root, failAdd: f.failAddErr}
	f.managers = append(f.managers, m)
	return m, nil
}

// RestoreTree records the AIX-0016 revert call and, if restoreFn is set, performs
// the actual tree mutation; otherwise it is a no-op that just satisfies the seam.
func (f *fakeGit) RestoreTree(_ context.Context, snapshotDir string, excludes []string) (git.RestoreResult, error) {
	f.mu.Lock()
	f.restoreCalls = append(f.restoreCalls, restoreCall{snapshotDir: snapshotDir, excludes: excludes})
	fn := f.restoreFn
	f.mu.Unlock()
	if fn != nil {
		return fn(snapshotDir, excludes)
	}
	return git.RestoreResult{}, nil
}

// restoreTreeCalls returns a snapshot of the recorded RestoreTree invocations.
func (f *fakeGit) restoreTreeCalls() []restoreCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]restoreCall(nil), f.restoreCalls...)
}

// allManagers returns a snapshot of every worktree manager handed out.
func (f *fakeGit) allManagers() []*fakeWorktreeManager {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeWorktreeManager(nil), f.managers...)
}

// fakeWorktreeManager records Add/Remove/RemoveAll without running git. Add creates
// a real temp directory seeded with a copy of the repo (so the executor edits a copy
// and reconcile can compare), mimicking `git worktree add` checking out content.
type fakeWorktreeManager struct {
	base     string
	seedFrom string
	failAdd  error

	mu        sync.Mutex
	live      []string // worktrees added but not yet removed
	adds      int      // total successful Add calls
	removes   int      // total worktree removals (via Remove or RemoveAll)
	removeAll int      // total RemoveAll calls
}

func (m *fakeWorktreeManager) Add(_ context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAdd != nil {
		return "", m.failAdd
	}
	path := filepath.Join(m.base, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	// Seed the worktree with a copy of the repo so edits start from current content.
	if m.seedFrom != "" {
		if err := copyTree(m.seedFrom, path); err != nil {
			return "", err
		}
	}
	m.live = append(m.live, path)
	m.adds++
	return path, nil
}

func (m *fakeWorktreeManager) Remove(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropLive(path)
	m.removes++
	return os.RemoveAll(path)
}

func (m *fakeWorktreeManager) RemoveAll(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeAll++
	var errs []error
	for _, p := range m.live {
		m.removes++
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	m.live = nil
	return errors.Join(errs...)
}

// dropLive removes path from the live list (caller holds mu).
func (m *fakeWorktreeManager) dropLive(path string) {
	for i, p := range m.live {
		if p == path {
			m.live = append(m.live[:i], m.live[i+1:]...)
			return
		}
	}
}

func (m *fakeWorktreeManager) addCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adds
}

func (m *fakeWorktreeManager) removeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removes
}

func (m *fakeWorktreeManager) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

// writingHarness wraps the recording harness.Mock so that, on each Run, it first
// writes a set of files into req.WorkDir (simulating an executor's edits) and tracks
// concurrency, then delegates to the Mock for recording, delay, and error scripting.
// This gives tests the real Mock's behaviors PLUS a deterministic file side-effect
// and a max-concurrency counter — without any real agent.
type writingHarness struct {
	mock *harness.Mock

	// filesByCall[i] are the files (rel→content) written on the i-th Run; the last
	// entry repeats once exhausted. nil means "write nothing".
	filesByCall []map[string]string

	mu          sync.Mutex
	calls       int
	cur         int32
	maxConcurr  int32
	workDirsLog []string
}

func newWritingHarness(name string) *writingHarness {
	return &writingHarness{mock: harness.NewMock(name)}
}

func (h *writingHarness) Name() string { return h.mock.Name() }

func (h *writingHarness) Run(ctx context.Context, req harness.Request) (harness.Result, error) {
	// Track concurrency at entry.
	n := atomic.AddInt32(&h.cur, 1)
	for {
		old := atomic.LoadInt32(&h.maxConcurr)
		if n <= old || atomic.CompareAndSwapInt32(&h.maxConcurr, old, n) {
			break
		}
	}
	defer atomic.AddInt32(&h.cur, -1)

	h.mu.Lock()
	idx := h.calls
	h.calls++
	h.workDirsLog = append(h.workDirsLog, req.WorkDir)
	var files map[string]string
	if len(h.filesByCall) > 0 {
		if idx < len(h.filesByCall) {
			files = h.filesByCall[idx]
		} else {
			files = h.filesByCall[len(h.filesByCall)-1]
		}
	}
	h.mu.Unlock()

	// Write the configured files into the working directory BEFORE delegating, so
	// the side-effect is visible to the after-snapshot.
	for rel, content := range files {
		p := filepath.Join(req.WorkDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return harness.Result{}, err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return harness.Result{}, err
		}
	}

	// Delegate to the Mock for recording + scripted delay/error.
	return h.mock.Run(ctx, req)
}

func (h *writingHarness) maxConcurrent() int32 {
	return atomic.LoadInt32(&h.maxConcurr)
}

func (h *writingHarness) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// ---------------------------------------------------------------------------
// Scheduler test fixtures.
// ---------------------------------------------------------------------------

// fixedClock returns a clock fixed at a stable instant so run IDs are deterministic.
func fixedClock() run.Clock {
	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	return run.ClockFunc(func() time.Time { return at })
}

// newSchedRun builds a hermetic Store (fake baseliner, fixed clock) rooted in a temp
// dir, creates a run, and replaces its subtasks with the given set (already pending).
// It returns the store and the run, ready to schedule.
func newSchedRun(t *testing.T, cfg config.Config, subtasks []run.Subtask) (*run.Store, *run.Run) {
	t.Helper()
	store, err := run.NewStoreFromConfig(cfg, t.TempDir(),
		run.WithBaseliner(fakeBaseliner{}),
		run.WithClock(fixedClock()),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	r, err := store.Create("do the work", cfg)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	r.Subtasks = subtasks
	if err := store.Save(r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	return store, r
}

// registryWith builds a harness.Registry whose executor harness name resolves to h.
// The executor role in config.Default uses "claude"; we register h under that name.
func registryWith(t *testing.T, cfg config.Config, h harness.Harness) *harness.Registry {
	t.Helper()
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Executor.Harness: func(string, config.Harness) (harness.Harness, error) {
				return h, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

// pending builds a pending subtask with the given id, deps, and files.
func pending(id string, deps []string, files ...string) run.Subtask {
	return run.Subtask{
		ID:          id,
		Title:       "subtask " + id,
		Description: "implement " + id,
		Deps:        deps,
		Files:       files,
		Status:      run.SubtaskPending,
	}
}

// runScheduler wires and runs a Scheduler over r with the given config, fake git,
// and executor harness. It returns the scheduler error (if any).
func runScheduler(t *testing.T, cfg config.Config, store *run.Store, r *run.Run, fg gitGateway, h harness.Harness, opts ...SchedulerOption) error {
	t.Helper()
	reg := registryWith(t, cfg, h)
	base := []SchedulerOption{WithSchedulerOutput(&strings.Builder{})}
	s, err := NewScheduler(r, cfg, reg, fg, store, prompt.NewRenderer(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	return s.Run(context.Background())
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestTopologicalScheduling proves a subtask runs only after its deps are done: the
// executor records the order of WorkDir requests, and the dependency must appear in
// the run's done set before the dependent's executor is invoked. We capture order by
// having the harness append the subtask's file to the workdir and checking the run's
// subtask statuses are all done at the end, plus that the dependent's prompt names
// its own id and the dep ran first via call ordering on a serial config.
func TestTopologicalScheduling(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false // serial: call order is deterministic.

	// st-03 depends on st-01 and st-02; st-02 depends on st-01.
	subs := []run.Subtask{
		pending("st-01", nil, "a/x.go"),
		pending("st-02", []string{"st-01"}, "b/y.go"),
		pending("st-03", []string{"st-01", "st-02"}, "c/z.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{
		{"a/x.go": "package a\n"},
		{"b/y.go": "package b\n"},
		{"c/z.go": "package c\n"},
	}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	// All subtasks done.
	for _, st := range r.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("subtask %s status = %q; want done", st.ID, st.Status)
		}
	}

	// Order: the executor was invoked in dependency order. The prompts embed each
	// subtask's id/title, so we can read the order off the recorded requests.
	reqs := h.mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("executor invoked %d times; want 3", len(reqs))
	}
	order := []string{"st-01", "st-02", "st-03"}
	for i, want := range order {
		if !strings.Contains(reqs[i].Prompt, want) {
			t.Errorf("call %d prompt does not mention %q (out-of-order scheduling?):\n%s", i, want, reqs[i].Prompt)
		}
	}
}

// TestDisjointSubtasksRunConcurrently proves non-overlapping isolation lets
// disjoint-file subtasks run AT THE SAME TIME: two independent subtasks with
// disjoint file sets and a delayed executor must reach max-concurrency 2.
func TestDisjointSubtasksRunConcurrently(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4
	cfg.Pipeline.Execution.Isolation = isolationNonOverlapping

	subs := []run.Subtask{
		pending("st-01", nil, "internal/a/**"),
		pending("st-02", nil, "internal/b/**"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	// Each call delays so concurrency is observable; the Mock honors the delay.
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if got := h.maxConcurrent(); got != 2 {
		t.Errorf("max concurrent executors = %d; want 2 (disjoint subtasks must run together)", got)
	}
}

// TestOverlappingSubtasksSerialize proves the converse: two ready subtasks whose
// file sets OVERLAP must never run concurrently (max-concurrency 1), even with ample
// maxParallel.
func TestOverlappingSubtasksSerialize(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4
	cfg.Pipeline.Execution.Isolation = isolationNonOverlapping

	// Both declare the same subtree → overlap → must serialize.
	subs := []run.Subtask{
		pending("st-01", nil, "internal/shared/**"),
		pending("st-02", nil, "internal/shared/x.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if got := h.maxConcurrent(); got != 1 {
		t.Errorf("max concurrent executors = %d; want 1 (overlapping subtasks must serialize)", got)
	}
}

// TestNoneIsolationSkipsOverlapChecks proves the advanced/unsafe `none` policy runs
// everything in the main tree WITHOUT overlap checks: two subtasks whose files
// overlap still run concurrently (max-concurrency 2), unlike under non-overlapping.
// (none is documented as unsafe; this only asserts the scheduler does not serialize.)
func TestNoneIsolationSkipsOverlapChecks(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4
	cfg.Pipeline.Execution.Isolation = isolationNone

	// Overlapping files — under non-overlapping these would serialize.
	subs := []run.Subtask{
		pending("st-01", nil, "internal/shared/**"),
		pending("st-02", nil, "internal/shared/x.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})
	h.mock.PushDelay(60*time.Millisecond, harness.Result{Text: "ok"})

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if got := h.maxConcurrent(); got != 2 {
		t.Errorf("max concurrent under none = %d; want 2 (no overlap checks)", got)
	}
}

// TestMaxParallelRespected proves the concurrency cap: with 5 disjoint ready
// subtasks and maxParallel=2, no more than 2 executors run at once.
func TestMaxParallelRespected(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 2
	cfg.Pipeline.Execution.Isolation = isolationNonOverlapping

	var subs []run.Subtask
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("st-%02d", i)
		subs = append(subs, pending(id, nil, fmt.Sprintf("internal/p%02d/**", i)))
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.mock.DefaultResult = harness.Result{Text: "ok"}
	for i := 0; i < 5; i++ {
		h.mock.PushDelay(40*time.Millisecond, harness.Result{Text: "ok"})
	}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if got := h.maxConcurrent(); got > 2 {
		t.Errorf("max concurrent executors = %d; want <= 2 (maxParallel cap)", got)
	}
	if h.callCount() != 5 {
		t.Errorf("executor called %d times; want 5", h.callCount())
	}
}

// TestParallelWritesDoNotCorruptRunYAML proves the serialized-write guarantee: many
// disjoint subtasks run in parallel, all mutating run state through the run-state
// owner goroutine, and afterward run.yaml reloads cleanly with EVERY subtask done.
// Combined with the -race run of the suite, this demonstrates concurrent state
// transitions (and Store.Save) cannot corrupt run.yaml or race on r.Subtasks.
func TestParallelWritesDoNotCorruptRunYAML(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 8
	cfg.Pipeline.Execution.Isolation = isolationNonOverlapping

	const n = 12
	var subs []run.Subtask
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("st-%02d", i)
		subs = append(subs, pending(id, nil, fmt.Sprintf("pkg/p%02d/file.go", i)))
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.mock.DefaultResult = harness.Result{Text: "ok"}
	// A tiny delay so workers genuinely overlap and contend on the save lock.
	for i := 0; i < n; i++ {
		h.mock.PushDelay(10*time.Millisecond, harness.Result{Text: "ok"})
	}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	// run.yaml reloads cleanly and shows every subtask done — no corruption, no lost
	// transition.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload after parallel writes: %v", err)
	}
	if len(reloaded.Subtasks) != n {
		t.Fatalf("reloaded %d subtasks; want %d", len(reloaded.Subtasks), n)
	}
	for _, st := range reloaded.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("reloaded subtask %s = %q; want done", st.ID, st.Status)
		}
	}
}

// TestDiffCapturedAndPersisted proves each subtask's diff is computed and written to
// subtasks/<id>/diff.patch, and the patch reflects the files the executor wrote.
func TestDiffCapturedAndPersisted(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{
		pending("st-01", nil, "feature/new.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{
		{"feature/new.go": "package feature\n\nfunc New() int { return 42 }\n"},
	}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	patch, err := os.ReadFile(layout.SubtaskDiffFile("st-01"))
	if err != nil {
		t.Fatalf("reading diff.patch: %v", err)
	}
	if !strings.Contains(string(patch), "feature/new.go") {
		t.Errorf("diff.patch does not mention the changed file:\n%s", patch)
	}
	if !strings.Contains(string(patch), "func New() int { return 42 }") {
		t.Errorf("diff.patch does not reflect the executor's content:\n%s", patch)
	}
}

// TestResumeSkipsDoneSubtasks proves resume never re-runs a done subtask: pre-seed
// st-01 as done and st-02 as pending; scheduling must execute ONLY st-02 (proven by
// the recording mock's call count and the recorded prompt).
func TestResumeSkipsDoneSubtasks(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{
		func() run.Subtask { s := pending("st-01", nil, "a/x.go"); s.Status = run.SubtaskDone; return s }(),
		pending("st-02", []string{"st-01"}, "b/y.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{"b/y.go": "package b\n"}}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if h.callCount() != 1 {
		t.Fatalf("executor invoked %d times; want 1 (done subtask must not re-run)", h.callCount())
	}
	if reqs := h.mock.Requests(); !strings.Contains(reqs[0].Prompt, "st-02") {
		t.Errorf("the single execution was not st-02:\n%s", reqs[0].Prompt)
	}
	// st-01 stayed done; st-02 reached done.
	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done (unchanged)", s.Status)
	}
	if s, _ := r.SubtaskByID("st-02"); s.Status != run.SubtaskDone {
		t.Errorf("st-02 = %q; want done", s.Status)
	}
}

// TestResumeRestartsInterruptedSubtask proves an interrupted subtask
// (implementing) is rewound to pending and re-executed on resume.
func TestResumeRestartsInterruptedSubtask(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{
		func() run.Subtask {
			s := pending("st-01", nil, "a/x.go")
			s.Status = run.SubtaskImplementing // interrupted mid-step
			return s
		}(),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if h.callCount() != 1 {
		t.Errorf("interrupted subtask executed %d times; want 1 (restart)", h.callCount())
	}
	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done after restart", s.Status)
	}
}

// TestFailedSubtaskBlocksDependents proves a failing subtask is marked failed, its
// dependents never execute, and the scheduler reports which subtasks could not run.
func TestFailedSubtaskBlocksDependents(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{
		pending("st-01", nil, "a/x.go"),
		pending("st-02", []string{"st-01"}, "b/y.go"), // depends on the failing one
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	// st-01's executor errors; st-02 should then never be invoked.
	h.mock.PushError(harness.Result{}, errors.New("boom"))

	err := runScheduler(t, cfg, store, r, newFakeGit(root), h)
	if err == nil {
		t.Fatal("scheduler should fail when a subtask fails and strands a dependent")
	}
	if !strings.Contains(err.Error(), "st-01") {
		t.Errorf("error should name the failed subtask st-01; got: %v", err)
	}
	if !strings.Contains(err.Error(), "st-02") {
		t.Errorf("error should report st-02 could not run; got: %v", err)
	}
	// The failure cause is surfaced (actionable error), not just the id.
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include the executor failure cause %q; got: %v", "boom", err)
	}

	if h.callCount() != 1 {
		t.Errorf("executor invoked %d times; want 1 (the dependent must not run)", h.callCount())
	}
	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskFailed {
		t.Errorf("st-01 = %q; want failed", s.Status)
	}
	if s, _ := r.SubtaskByID("st-02"); s.Status != run.SubtaskPending {
		t.Errorf("st-02 = %q; want pending (never ran)", s.Status)
	}
	// Run marked failed and persisted.
	reloaded, lerr := store.Load(r.ID)
	if lerr != nil {
		t.Fatalf("reload: %v", lerr)
	}
	if reloaded.Status != run.StatusFailed {
		t.Errorf("reloaded run status = %q; want failed", reloaded.Status)
	}
}

// TestStatePersistedPerTransition proves run.yaml is written across transitions: on
// success the reloaded run shows the subtask done and the run executing.
func TestStatePersistedPerTransition(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusExecuting {
		t.Errorf("reloaded run status = %q; want executing (the phase transition is a later ticket)", reloaded.Status)
	}
	if s, _ := reloaded.SubtaskByID("st-01"); s.Status != run.SubtaskDone {
		t.Errorf("reloaded st-01 = %q; want done", s.Status)
	}
}

// TestReviewHookSeam proves the review-hook seam: a custom hook is invoked per
// subtask after the diff is captured, and its decision (here, marking blocked then
// done) is what advances the subtask — confirming AIX-0011 can slot its loop in
// without touching the scheduler.
func TestReviewHookSeam(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}

	var hookCalls int32
	hook := func(_ context.Context, snap run.Subtask, commit CommitFunc) error {
		atomic.AddInt32(&hookCalls, 1)
		// Prove the hook controls the transition and that the diff already exists.
		layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
		if _, err := os.Stat(layout.SubtaskDiffFile(snap.ID)); err != nil {
			t.Errorf("review hook ran before the diff was persisted: %v", err)
		}
		// Mutate + persist atomically through commit (the race-safe contract).
		return commit(func(st *run.Subtask) { st.Status = run.SubtaskDone })
	}

	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h, WithReviewHook(hook)); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if hookCalls != 1 {
		t.Errorf("review hook called %d times; want 1", hookCalls)
	}
	if s, _ := r.SubtaskByID("st-01"); s.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done (set by the hook)", s.Status)
	}
}

// blockingHarness signals once it is entered, then blocks until the context is
// cancelled. It lets a test cancel a run while a worker is mid-flight (and has
// already round-tripped subtaskSnapshot + commit through the state owner).
type blockingHarness struct {
	name    string
	entered chan struct{}
}

func (h *blockingHarness) Name() string { return h.name }

func (h *blockingHarness) Run(ctx context.Context, _ harness.Request) (harness.Result, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return harness.Result{}, ctx.Err()
}

// TestCancelDuringExecutionDoesNotDeadlock proves the owner-goroutine refactor keeps
// a cancelled run from deadlocking: workers query the state owner over channels, and
// when the context is cancelled mid-execution Run must still return promptly (the
// done-guarded send/receive never blocks forever). Run under -race, it also exercises
// the worker→owner round-trips concurrently with teardown.
func TestCancelDuringExecutionDoesNotDeadlock(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4

	subs := []run.Subtask{
		pending("st-01", nil, "a/f.go"),
		pending("st-02", nil, "b/f.go"),
		pending("st-03", nil, "c/f.go"),
	}
	store, r := newSchedRun(t, cfg, subs)

	h := &blockingHarness{name: cfg.Roles.Executor.Harness, entered: make(chan struct{}, len(subs))}
	reg := registryWith(t, cfg, h)
	s, err := NewScheduler(r, cfg, reg, newFakeGit(t.TempDir()), store, prompt.NewRenderer(),
		WithSchedulerOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-h.entered // a worker is now blocked inside the executor, past the owner queries.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil; want a cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run deadlocked after context cancellation")
	}
}

// TestUnknownExecutorHarnessErrors proves NewScheduler fails clearly when the
// executor role references a harness that is not registered.
func TestUnknownExecutorHarnessErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Roles.Executor.Harness = "does-not-exist"
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	store, r := newSchedRun(t, config.Default(), subs)

	// Registry built from the (valid) default harnesses; the executor role points
	// elsewhere.
	reg, err := harness.NewRegistry(config.Default(), harness.Options{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	_, err = NewScheduler(r, cfg, reg, newFakeGit(t.TempDir()), store, prompt.NewRenderer())
	if err == nil {
		t.Fatal("NewScheduler should fail for an unknown executor harness")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing harness; got: %v", err)
	}
}
