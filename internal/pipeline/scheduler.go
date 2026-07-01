package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/jaxmef/aixecutor/internal/workspace"
)

// Gateway is the pipeline's read-only git/workspace seam: either the single-repo
// *git.Gateway (via NewGitGateway) or a multi-root workspace (via
// NewWorkspaceGateway). Exported as an alias so the CLI can resolve which backing
// to use and hand the result to NewOrchestrator. Both are read-only + raw file I/O;
// the only mutating exception is gated worktree add/remove (single-repo only).
type Gateway = gitGateway

// Isolation policy values (CLAUDE.md §4.3). Mirrored as local constants so the
// scheduler reads explicitly; they must match config.Execution.Isolation.
const (
	isolationNonOverlapping = "non-overlapping"
	isolationWorktree       = "worktree"
	isolationNone           = "none"
)

// gitGateway is the narrow, read-only-plus-gated-worktree slice of the git gateway
// the scheduler needs. Declaring it here (rather than depending on *git.Gateway
// directly) keeps the pipeline decoupled and lets tests inject a fake that creates
// snapshot dirs and records worktree calls WITHOUT running git — the same seam
// pattern used by RepoSummarizer/Baseliner. *git.Gateway satisfies the snapshot and
// diff methods directly; gitGatewayAdapter bridges Worktree's concrete return type
// to the worktreeManager interface for production wiring.
//
// All snapshot/diff operations are read-only file operations under the hood; the
// only mutation is worktree add/remove, which the gateway itself gates on
// git.policy: allow-worktree (CLAUDE.md §2 invariant 1).
type gitGateway interface {
	// RepoRoot is the absolute repository root; the scheduler walks it to expand
	// `**` globs and uses it as the default executor working directory.
	RepoRoot() string
	// SnapshotPaths copies the given (already-expanded, literal) paths into dstDir,
	// preserving structure, for before/after per-subtask diffing.
	SnapshotPaths(dstDir string, patterns []string, warn func(bytes int64)) (git.Snapshot, error)
	// DiffTrees computes `git diff --no-index` between two snapshot dirs (read-only;
	// exit 1 == "differs" == success).
	DiffTrees(ctx context.Context, beforeDir, afterDir string) (git.Diff, error)
	// FullDiff computes the full diff from the run-start baseline (whose snapshot
	// lives at baselineDir) to the CURRENT working tree (CLAUDE.md §4.4). It
	// snapshots the current tree and diffs baseline → current, read-only. The
	// senior-review phase (AIX-0012) calls it once per round so each review judges
	// the whole change as it stands. baselineDir is taken straight from the
	// persisted run.Baseline.Dir, so it works unchanged on resume.
	FullDiff(ctx context.Context, baselineDir string) (git.Diff, error)
	// Manifest returns a point-in-time path->FileMeta listing of the tree rooted at
	// root (mtime+size per file), enumerated through the SAME read-only pipeline as
	// CaptureBaseline/FullDiff (so runsDir, .gitignored, and editor-dir paths are
	// excluded by construction). A before/after pair drives best-effort detection of
	// executor edits outside any subtask's declared files. Read-only.
	Manifest(ctx context.Context, root string) (git.Manifest, error)
	// Worktree returns a worktree manager IFF policy is allow-worktree, else an
	// actionable error. Under non-worktree isolation it is never called.
	Worktree(policy string) (worktreeManager, error)
	// RestoreTree reverts the working tree to the snapshot at snapshotDir (the
	// AIX-0016 clean revert), deleting run-added files and copying baseline files
	// back, all via raw file I/O (no mutating git). extraExcludes augments the
	// gateway's configured excludes (e.g. a custom docs dir) so amended docs and run
	// artifacts are never touched.
	RestoreTree(ctx context.Context, snapshotDir string, extraExcludes []string) (git.RestoreResult, error)
}

// worktreeManager is the scheduler's view of git's WorktreeManager: provision a
// worktree, remove one, and tear down all (for defer-cleanup on every exit path).
// *git.WorktreeManager satisfies this; the worktree tests inject a fake that
// records Add/Remove without running git.
type worktreeManager interface {
	Add(ctx context.Context, name string) (string, error)
	Remove(ctx context.Context, path string) error
	RemoveAll(ctx context.Context) error
}

// gitGatewayAdapter adapts a concrete *git.Gateway to the gitGateway interface,
// bridging the one impedance mismatch: Gateway.Worktree returns the concrete
// *git.WorktreeManager, which already satisfies worktreeManager, but Go needs the
// return type widened to the interface. The CLI wires production runs through this.
type gitGatewayAdapter struct{ *git.Gateway }

// NewGitGateway wraps a read-only git gateway so it satisfies the scheduler's
// gitGateway seam. Production callers (internal/cli) use this; tests inject a fake.
func NewGitGateway(gw *git.Gateway) gitGateway { return gitGatewayAdapter{gw} }

// workspaceGateway adapts a multi-root *workspace.Workspace (AIX-0020) to the
// scheduler's gitGateway seam: snapshot/diff/restore span every repo and the plain
// area, rooted at the workspace root. Worktree isolation is not supported across a
// heterogeneous workspace in v1 (snapshot-based non-overlapping isolation is used),
// so Worktree returns an actionable error — the scheduler only calls it under the
// worktree policy, which workspace mode does not select.
type workspaceGateway struct{ ws *workspace.Workspace }

// NewWorkspaceGateway wraps a workspace so the pipeline drives it exactly like a
// single-repo gateway. The single-repo path (NewGitGateway) is unchanged.
func NewWorkspaceGateway(ws *workspace.Workspace) gitGateway { return workspaceGateway{ws} }

func (a workspaceGateway) RepoRoot() string { return a.ws.Root() }

func (a workspaceGateway) SnapshotPaths(dstDir string, patterns []string, warn func(bytes int64)) (git.Snapshot, error) {
	return a.ws.SnapshotPaths(dstDir, patterns, warn)
}

func (a workspaceGateway) DiffTrees(ctx context.Context, beforeDir, afterDir string) (git.Diff, error) {
	return a.ws.DiffTrees(ctx, beforeDir, afterDir)
}

func (a workspaceGateway) FullDiff(ctx context.Context, baselineDir string) (git.Diff, error) {
	return a.ws.FullDiff(ctx, baselineDir)
}

func (a workspaceGateway) Manifest(ctx context.Context, root string) (git.Manifest, error) {
	return a.ws.Manifest(ctx, root)
}

func (a workspaceGateway) RestoreTree(ctx context.Context, snapshotDir string, extraExcludes []string) (git.RestoreResult, error) {
	return a.ws.RestoreTree(ctx, snapshotDir, extraExcludes)
}

func (a workspaceGateway) Worktree(policy string) (worktreeManager, error) {
	return nil, fmt.Errorf("worktree isolation is not supported in workspace mode; use isolation: %q", isolationNonOverlapping)
}

// Worktree forwards to the embedded gateway and widens the concrete manager to the
// interface (or returns the gateway's policy-refusal error verbatim).
func (a gitGatewayAdapter) Worktree(policy string) (worktreeManager, error) {
	m, err := a.Gateway.Worktree(policy)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// FullDiff adapts the gateway's FullDiff to the dir-only seam: it reconstructs a
// git.Baseline pointing at baselineDir (the gateway's FullDiff uses only the
// baseline's snapshot directory) and diffs it against the current tree. This lets
// the pipeline depend on a baseline DIRECTORY (persisted in run.yaml, available on
// resume) rather than on a live git.Baseline value captured at run start.
func (a gitGatewayAdapter) FullDiff(ctx context.Context, baselineDir string) (git.Diff, error) {
	baseline := git.Baseline{Snapshot: git.Snapshot{Dir: baselineDir}}
	return a.Gateway.FullDiff(ctx, baseline, nil)
}

// RestoreTree forwards to the embedded gateway's scope-aware revert.
func (a gitGatewayAdapter) RestoreTree(ctx context.Context, snapshotDir string, extraExcludes []string) (git.RestoreResult, error) {
	return a.Gateway.RestoreTree(ctx, snapshotDir, extraExcludes)
}

// contextProvider supplies the per-subtask context excerpt fed into the executor
// prompt (the relevant slice of docs/context.md). It is an interface so the
// scheduler does not hard-code where context comes from and tests can stub it. The
// default (newRunContextProvider) reads docs/context.md once and hands the whole
// document to every subtask; a future ticket can make this per-subtask without
// touching the scheduler.
type contextProvider interface {
	// ContextExcerpt returns the context text for subtask st (may be "").
	ContextExcerpt(st run.Subtask) string
}

// ReviewHook is the seam AIX-0011 fills in: after a subtask's diff is captured, the
// scheduler calls the hook to run the executor↔reviewer loop. In THIS ticket the
// default (markDoneReviewHook) is a no-op that simply records the implementing→done
// transition, so the scheduler runs end-to-end and is fully testable now. AIX-0011
// will replace the default with the real loop; the transition points
// (implementing → [review] → done) are explicit and persisted on both sides so the
// contract is stable across that change.
//
// Contract (and why it is shaped this way for concurrency safety): the hook is given
// the subtask's id and a read-only SNAPSHOT of the subtask (a value copy produced by
// the run-state owner, safe to read freely). It MUST NOT mutate shared run state
// directly — under parallel execution the owner may be marshalling run.yaml at the
// same moment. Instead it drives the subtask to a terminal state (SubtaskDone on
// success) THROUGH the provided commit function, which routes the caller's mutation
// and the persist into the single owner goroutine. This is what keeps the engine
// -race clean: every subtask-state write is serialized with every Store.Save by the
// owner. Returning an error leaves the subtask non-done and fails its execution.
type ReviewHook func(ctx context.Context, snapshot run.Subtask, commit CommitFunc) error

// CommitFunc applies mutate to the live subtask identified at hook-call time and
// persists the run, all inside the run-state owner goroutine, so the mutation cannot
// race a concurrent marshal. mutate receives a pointer to the live subtask. A nil
// mutate just persists. It is the only safe way for a ReviewHook to change subtask
// state.
type CommitFunc func(mutate func(st *run.Subtask)) error

// Scheduler is the execution engine (CLAUDE.md §3.3 step 2): it schedules the
// subtask DAG, runs the executor per subtask under the configured isolation policy,
// captures each subtask's diff, and persists state after every transition. All
// collaborators are injected so the whole engine is hermetically testable with a
// mock harness and a fake git gateway — no real agent, network, or git.
type Scheduler struct {
	run        *run.Run
	cfg        config.Config
	executor   harness.Harness
	role       config.Role
	git        gitGateway
	store      *run.Store
	renderer   *prompt.Renderer
	ctxProv    contextProvider
	reviewHook ReviewHook

	// declaredGlobs is the static union of every subtask's declared Files globs,
	// collected once at construction. Files are immutable after planning (see
	// subtaskSnapshot), so this needs no lock and no owner round-trip. It drives
	// best-effort detection of executor edits outside ANY subtask's declared files:
	// a changed path matched by none of these is "undeclared-by-all".
	declaredGlobs []string

	// progress renders concise, semantic human progress. It is concurrency-safe
	// (the scheduler fans out subtask workers in parallel), so it needs no extra
	// lock here. Defaults to a stdout-backed Progress.
	progress *log.Progress

	// actor is the single goroutine that owns all mutable run state (s.run +
	// failCauses) and is the sole caller of store.Save. Workers and the Run loop
	// reach it only through query channels (see stateActor), so run.yaml serializes
	// without a lock. Started in Run, torn down when the DAG completes.
	actor *stateActor

	// pauseCheck, when set, is polled at each subtask boundary; returning true makes
	// the scheduler stop at that safe boundary, persist StatusPaused, and return
	// ErrPaused (AIX-0016). Default nil = never pause. Production wires it to the
	// run's control channel (store.PauseRequested); tests inject a closure.
	pauseCheck func() bool

	// dryRun marks that the executor/reviewer harnesses are the dry-run wrapper,
	// which returns a role-agnostic placeholder (not a parseable reviewer verdict).
	// The subtask review loop reads this to short-circuit to "approved" instead of
	// trying to parse the placeholder and failing — mirroring how the Planner
	// handles --dry-run (planning.go), so the WHOLE pipeline runs under --dry-run.
	dryRun bool
}

// SchedulerOption configures a Scheduler at construction.
type SchedulerOption func(*Scheduler)

// WithReviewHook overrides the per-subtask review step. AIX-0011 passes its real
// executor↔reviewer loop; the default is markDoneReviewHook.
func WithReviewHook(hook ReviewHook) SchedulerOption {
	return func(s *Scheduler) {
		if hook != nil {
			s.reviewHook = hook
		}
	}
}

// WithSchedulerOutput sets where the scheduler prints human progress, by building
// a Progress over w. Defaults to os.Stdout. Kept for tests that pass a buffer;
// WithSchedulerProgress is preferred when a shared Progress already exists.
func WithSchedulerOutput(w io.Writer) SchedulerOption {
	return func(s *Scheduler) {
		if w != nil {
			s.progress = log.NewProgress(w)
		}
	}
}

// WithSchedulerProgress sets the shared Progress the scheduler (and its review
// loop) emit semantic events through. Defaults to a stdout-backed Progress.
func WithSchedulerProgress(p *log.Progress) SchedulerOption {
	return func(s *Scheduler) {
		if p != nil {
			s.progress = p
		}
	}
}

// WithSchedulerDryRun marks the scheduler as running against dry-run-wrapped
// harnesses, so the per-subtask review loop short-circuits to "approved" rather
// than parsing the wrapper's placeholder result (which is not a verdict). The
// executor still "runs" (the wrapper no-ops), capturing an empty diff. This is how
// the full pipeline converges under --dry-run, symmetric with the Planner.
func WithSchedulerDryRun(dryRun bool) SchedulerOption {
	return func(s *Scheduler) { s.dryRun = dryRun }
}

// WithPauseCheck sets the predicate the scheduler polls at each subtask boundary
// to honor a pause-to-review request (AIX-0016). A nil check (the default) never
// pauses, so existing callers are unaffected.
func WithPauseCheck(check func() bool) SchedulerOption {
	return func(s *Scheduler) {
		if check != nil {
			s.pauseCheck = check
		}
	}
}

// WithContextProvider overrides the per-subtask context-excerpt source. Defaults to
// reading docs/context.md via the store layout.
func WithContextProvider(p contextProvider) SchedulerOption {
	return func(s *Scheduler) {
		if p != nil {
			s.ctxProv = p
		}
	}
}

// NewScheduler builds a Scheduler for r. The executor harness is resolved from the
// registry by the executor role's harness name; an unknown harness is a clear,
// actionable error (rather than a nil-deref later). git, store, and renderer are
// the injected collaborators; cfg drives every knob (isolation, maxParallel,
// timeouts) per invariant #4.
func NewScheduler(
	r *run.Run,
	cfg config.Config,
	reg *harness.Registry,
	gw gitGateway,
	store *run.Store,
	renderer *prompt.Renderer,
	opts ...SchedulerOption,
) (*Scheduler, error) {
	if r == nil {
		return nil, errors.New("pipeline: NewScheduler(nil run)")
	}
	if reg == nil {
		return nil, errors.New("pipeline: NewScheduler requires a harness registry")
	}
	role := cfg.Roles.Executor
	h, ok := reg.Get(role.Harness)
	if !ok {
		return nil, fmt.Errorf("pipeline: executor harness %q is not defined (known: %s)",
			role.Harness, strings.Join(reg.Names(), ", "))
	}

	s := &Scheduler{
		run:           r,
		cfg:           cfg,
		executor:      h,
		role:          role,
		git:           gw,
		store:         store,
		renderer:      renderer,
		ctxProv:       newRunContextProvider(store, r),
		reviewHook:    markDoneReviewHook,
		progress:      log.NewProgress(nil),
		declaredGlobs: collectDeclaredGlobs(r.Subtasks),
	}
	for _, o := range opts {
		o(s)
	}
	if s.progress == nil {
		s.progress = log.NewProgress(nil)
	}
	return s, nil
}

// Run schedules and executes the subtask DAG until every subtask is terminal
// (CLAUDE.md §3.3). It transitions the run to executing, then repeatedly: computes
// the ready set (pending subtasks whose deps are all done), selects a concurrent
// batch honoring the isolation policy and maxParallel, runs that batch, and loops.
//
// Resume falls out naturally: done subtasks are never ready (so never re-run), and
// the ready set is reconstructed purely from persisted statuses. Interrupted
// subtasks (implementing/reviewing) are reset to pending up front so their step
// restarts from the persisted diff/baseline (see resetInterrupted).
//
// It returns an error if the DAG strands non-terminal subtasks (deadlock guard) or
// if a subtask fails; on a subtask failure the run is marked failed and the error
// names which subtasks could not run. A clean completion leaves the run in
// executing with all subtasks done (the run-level transition to senior review is
// AIX-0012/0013's concern).
func (s *Scheduler) Run(ctx context.Context) error {
	if len(s.run.Subtasks) == 0 {
		return errors.New("pipeline: no subtasks to execute (planning must run first)")
	}

	// Start the run-state owner before any worker spawns, and stop it on every exit
	// path. Run joins every worker (runBatch waits) before returning, so by the time
	// stop runs no client is in flight.
	s.actor = newStateActor(s.run, s.store, s.cfg)
	go s.actor.loop()
	defer s.actor.stop()

	if err := s.beginExecuting(); err != nil {
		return err
	}

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("pipeline: execution canceled: %w", ctx.Err())
		}

		done, total := s.terminalCounts()
		if done == total {
			// Every subtask reached a terminal state. If any failed, surface it;
			// otherwise execution succeeded. Checked BEFORE the pause so a request
			// that lands as the final batch drains finalizes the run rather than
			// stranding it in `paused` with no work left.
			return s.finalize()
		}

		// Honor a pause-to-review request at this safe boundary: the previous batch
		// (if any) has fully drained and run.yaml is consistent (done subtasks done,
		// the rest pending), so persisting `paused` here is never mid-subtask-write.
		// Only reached when work remains.
		if s.pauseCheck != nil && s.pauseCheck() {
			return s.pauseAtBoundary()
		}

		batch := s.selectBatch()
		if len(batch) == 0 {
			// Non-terminal subtasks remain but none are ready and none are running:
			// the DAG is stranded (a dep failed/blocked, leaving dependents stuck).
			return s.deadlockError()
		}

		if err := s.runBatch(ctx, batch); err != nil {
			// A worker hit an unrecoverable (non per-subtask) error such as context
			// cancellation or a persistence failure. Per-subtask executor failures
			// are recorded as SubtaskFailed and do NOT abort the loop here.
			return err
		}
	}
}

// pauseAtBoundary persists the run as StatusPaused at a safe subtask boundary,
// acknowledges (clears) the pause request so a later resume does not immediately
// re-pause, and returns ErrPaused. The orchestrator treats ErrPaused as a clean,
// resumable stop (neither failure nor abort).
func (s *Scheduler) pauseAtBoundary() error {
	if err := s.runSave(func(r *run.Run) { r.Status = run.StatusPaused }); err != nil {
		return fmt.Errorf("pipeline: persisting paused state for run %q: %w", s.run.ID, err)
	}
	_ = s.store.ClearPause(s.run.ID)
	s.progress.Logf("Paused for review at a subtask boundary (run %s).", s.run.ID)
	return ErrPaused
}

// beginExecuting marks the run executing (persisted) and resets any interrupted
// subtasks to pending so their step restarts on resume. Done subtasks are left
// untouched and will simply never become ready again.
func (s *Scheduler) beginExecuting() error {
	if err := s.runSave(func(r *run.Run) {
		resetInterrupted(r)
		r.Status = run.StatusExecuting
	}); err != nil {
		return fmt.Errorf("pipeline: marking run %q executing: %w", s.run.ID, err)
	}
	return nil
}

// runSave routes a whole-run mutation + persist through the state owner. Used for
// run-level status transitions (begin executing, pause).
func (s *Scheduler) runSave(mutate func(*run.Run)) error {
	reply := make(chan error, 1)
	err, ok := ask(s.actor, reply, runSaveReq{mutate: mutate, reply: reply})
	if !ok {
		return errActorStopped
	}
	return err
}

// selectBatch asks the owner for the next concurrent batch (ready set under the
// isolation policy, bounded by maxParallel). An empty result with non-terminal
// subtasks remaining signals a deadlock to Run.
func (s *Scheduler) selectBatch() []string {
	reply := make(chan []string, 1)
	// ok is always true here: this is called only from the Run loop, which the actor
	// outlives (stop is deferred and fires after the loop returns).
	batch, _ := ask(s.actor, reply, batchReq{reply: reply})
	return batch
}

// maxParallel returns the effective concurrency cap for the worker semaphore,
// reading the immutable config (so it needs no owner round-trip).
func (s *Scheduler) maxParallel() int {
	return effectiveMaxParallel(s.cfg)
}

// runBatch executes a batch of subtasks concurrently, bounded by maxParallel via a
// semaphore (so even a large ready batch never exceeds the cap), and waits for all
// of them. Each subtask's per-subtask failure is recorded on its status by
// executeSubtask and does not abort the batch; only a fatal error (context
// cancellation surfacing through the executor, or a persistence failure) is
// returned, aborting the run.
func (s *Scheduler) runBatch(ctx context.Context, batch []string) error {
	sem := make(chan struct{}, s.maxParallel())
	var wg sync.WaitGroup
	errs := make([]error, len(batch))

	for i, id := range batch {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			errs[i] = s.executeSubtask(ctx, id)
		}(i, id)
	}
	wg.Wait()

	return errors.Join(errs...)
}

// terminalCounts asks the owner for (terminalCount, total). A subtask is terminal
// when its status is done or failed.
func (s *Scheduler) terminalCounts() (terminal, total int) {
	reply := make(chan countsRes, 1)
	// ok is always true here: called only from the Run loop, which the actor outlives.
	r, _ := ask(s.actor, reply, countsReq{reply: reply})
	return r.terminal, r.total
}

// finalize asks the owner to settle the run once every subtask is terminal: nil if
// all done, otherwise the run is marked failed (persisted) and an error naming the
// failed and any stranded subtasks is returned.
func (s *Scheduler) finalize() error {
	reply := make(chan error, 1)
	err, ok := ask(s.actor, reply, finalizeReq{reply: reply})
	if !ok {
		return errActorStopped
	}
	return err
}

// deadlockError asks the owner to mark the run failed and build the stranded-subtask
// error (returned when non-terminal subtasks remain but none are ready/running).
func (s *Scheduler) deadlockError() error {
	reply := make(chan error, 1)
	err, ok := ask(s.actor, reply, deadlockReq{reply: reply})
	if !ok {
		return errActorStopped
	}
	return err
}

// commitSubtask routes a subtask mutation + persist through the owner — the
// owner-side of CommitFunc. A nil mutate just persists.
func (s *Scheduler) commitSubtask(id string, mutate func(*run.Subtask)) error {
	reply := make(chan error, 1)
	err, ok := ask(s.actor, reply, commitReq{id: id, mutate: mutate, reply: reply})
	if !ok {
		return errActorStopped
	}
	return err
}

// subtaskSnapshot returns a VALUE COPY of the named subtask, read by the owner.
// Workers use the snapshot to read subtask data (Files/Deps/Title and the scalar
// Status/Loops) without sharing the live struct: the copied scalars are stable, and
// Files/Deps are never mutated after planning. Any WRITE must go through
// commitSubtask, never through this snapshot.
func (s *Scheduler) subtaskSnapshot(id string) (run.Subtask, bool) {
	reply := make(chan snapshotRes, 1)
	res, ok := ask(s.actor, reply, snapshotReq{id: id, reply: reply})
	if !ok {
		return run.Subtask{}, false
	}
	return res.st, res.ok
}

// markDoneReviewHook is the default ReviewHook for THIS ticket: a no-op review that
// simply transitions the subtask implementing → done and persists it, so the
// scheduler completes the DAG end-to-end and every test here exercises the full
// path. AIX-0011 replaces it (via WithReviewHook) with the real executor↔reviewer
// loop; until then this keeps the seam honest — the transition is real and
// persisted (atomically, via commit), only the reviewing work is absent.
func markDoneReviewHook(_ context.Context, _ run.Subtask, commit CommitFunc) error {
	return commit(func(st *run.Subtask) { st.Status = run.SubtaskDone })
}
