package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/run"
)

// stateActor is the single goroutine that exclusively owns the mutable run state
// (s.run — notably Status and Subtasks) and failCauses, and is the ONLY caller of
// store.Save. It replaces the former saveMu (CLAUDE.md §7: coordinate through
// channels, not locks; one goroutine owns each piece of mutable state). Parallel
// subtask workers and the main Run loop are CLIENTS: every read or write is a query
// round-trip onto reqs, answered by the actor's select loop. run.yaml therefore
// serializes without any lock, and no two goroutines ever touch run state at once.
//
// Reads of IMMUTABLE run fields (ID, Task) stay direct on the *run.Run — they are
// set at planning and never written during execution, so they cannot race the actor.
type stateActor struct {
	run        *run.Run
	failCauses map[string]error
	store      *run.Store
	cfg        config.Config

	reqs chan stateMsg
	done chan struct{}
}

// stateMsg is a query/command carrying its own reply channel. apply runs INSIDE the
// owner goroutine, so it may freely touch the owned state and then answer on its
// reply channel ({args, reply chan T} — CLAUDE.md §7).
type stateMsg interface{ apply(*stateActor) }

// errActorStopped is returned by a client wrapper when the owner goroutine has
// already stopped (its done channel is closed) and so cannot service the request.
// In normal flow this never happens: the actor stops only after Run has joined every
// worker, so no client is in flight. It exists so a blocked client unblocks (never
// deadlocks) if the run is cancelled — the done select in send/ask fires.
var errActorStopped = errors.New("pipeline: run-state owner stopped")

func newStateActor(r *run.Run, store *run.Store, cfg config.Config) *stateActor {
	return &stateActor{
		run:        r,
		failCauses: make(map[string]error),
		store:      store,
		cfg:        cfg,
		reqs:       make(chan stateMsg),
		done:       make(chan struct{}),
	}
}

// loop is the owner goroutine: it services one request at a time until done is
// closed. Because every state access funnels through here, there is no shared
// mutable state left to lock.
func (a *stateActor) loop() {
	for {
		select {
		case m := <-a.reqs:
			m.apply(a)
		case <-a.done:
			return
		}
	}
}

// stop closes done, ending the loop and releasing any client blocked in send/ask.
func (a *stateActor) stop() { close(a.done) }

// send delivers m to the owner, selecting on done so a stopped actor never blocks
// the caller. Returns false if the actor has stopped.
func (a *stateActor) send(m stateMsg) bool {
	select {
	case a.reqs <- m:
		return true
	case <-a.done:
		return false
	}
}

// ask is the generic client round-trip: send m, then await its reply, both guarded
// by done so a cancelled/stopped run can never deadlock a blocked client. reply must
// be the SAME buffered (cap 1) channel carried by m, so the owner's reply send never
// blocks even if this caller has already gone. ok is false iff the actor stopped.
func ask[T any](a *stateActor, reply chan T, m stateMsg) (T, bool) {
	var zero T
	if !a.send(m) {
		return zero, false
	}
	select {
	case r := <-reply:
		return r, true
	case <-a.done:
		return zero, false
	}
}

// --- messages -------------------------------------------------------------------

// runSaveReq applies a whole-run mutation (e.g. run-level status transitions) and
// persists. Used for beginExecuting and pauseAtBoundary.
type runSaveReq struct {
	mutate func(*run.Run)
	reply  chan error
}

func (m runSaveReq) apply(a *stateActor) {
	if m.mutate != nil {
		m.mutate(a.run)
	}
	m.reply <- a.store.Save(a.run)
}

// commitReq applies a mutation to the subtask identified by id and persists — the
// owner-side of CommitFunc. A nil mutate just persists; an unknown id persists
// without mutating (matching the prior saveLocked + SubtaskByID guard).
type commitReq struct {
	id     string
	mutate func(*run.Subtask)
	reply  chan error
}

func (m commitReq) apply(a *stateActor) {
	if m.mutate != nil {
		if cur, ok := a.run.SubtaskByID(m.id); ok {
			m.mutate(cur)
		}
	}
	m.reply <- a.store.Save(a.run)
}

// failReq folds a subtask's failure (status → failed) and the cause record into a
// single owner-side transition + persist, so the failCauses write no longer races.
type failReq struct {
	id    string
	cause error
	reply chan error
}

func (m failReq) apply(a *stateActor) {
	if cur, ok := a.run.SubtaskByID(m.id); ok {
		cur.Status = run.SubtaskFailed
	}
	a.failCauses[m.id] = m.cause
	m.reply <- a.store.Save(a.run)
}

type snapshotRes struct {
	st run.Subtask
	ok bool
}

// snapshotReq returns a value copy of a subtask (read-only for workers).
type snapshotReq struct {
	id    string
	reply chan snapshotRes
}

func (m snapshotReq) apply(a *stateActor) {
	if st, ok := a.run.SubtaskByID(m.id); ok {
		m.reply <- snapshotRes{st: *st, ok: true}
		return
	}
	m.reply <- snapshotRes{}
}

// batchReq runs the ready/pick computation over the owner's private state and
// returns the next concurrent batch of subtask ids.
type batchReq struct{ reply chan []string }

func (m batchReq) apply(a *stateActor) { m.reply <- a.selectBatch() }

type countsRes struct{ terminal, total int }

// countsReq returns (terminalCount, total).
type countsReq struct{ reply chan countsRes }

func (m countsReq) apply(a *stateActor) { m.reply <- a.counts() }

// finalizeReq builds the final outcome from the owner's private state: nil if every
// subtask is done, otherwise the run is marked failed (persisted) and an actionable
// error naming the failed + stranded subtasks is returned.
type finalizeReq struct{ reply chan error }

func (m finalizeReq) apply(a *stateActor) { m.reply <- a.finalize() }

// deadlockReq marks the run failed and builds the "stranded subtasks" error.
type deadlockReq struct{ reply chan error }

func (m deadlockReq) apply(a *stateActor) { m.reply <- a.deadlock() }

// --- owner-side state computations (run on the owner goroutine only) -------------

func (a *stateActor) counts() countsRes {
	res := countsRes{total: len(a.run.Subtasks)}
	for i := range a.run.Subtasks {
		if a.run.Subtasks[i].Status.IsTerminal() {
			res.terminal++
		}
	}
	return res
}

// selectBatch computes the next concurrent batch: the ready subtasks selected under
// the isolation policy and bounded by maxParallel. An empty result with non-terminal
// subtasks remaining signals a deadlock to Run.
func (a *stateActor) selectBatch() []string {
	ready := a.ready()
	if len(ready) == 0 {
		return nil
	}
	return a.pickConcurrent(ready)
}

// ready returns the IDs of subtasks that are pending and whose every dep is done. A
// dep that is failed/blocked keeps a dependent out of the ready set, which is how a
// failure strands its dependents (surfaced by the deadlock guard). IDs are returned
// in subtask declaration order for determinism.
func (a *stateActor) ready() []string {
	statusByID := make(map[string]run.SubtaskStatus, len(a.run.Subtasks))
	for i := range a.run.Subtasks {
		statusByID[a.run.Subtasks[i].ID] = a.run.Subtasks[i].Status
	}

	var ready []string
	for i := range a.run.Subtasks {
		st := a.run.Subtasks[i]
		if st.Status != run.SubtaskPending {
			continue
		}
		depsDone := true
		for _, dep := range st.Deps {
			if statusByID[dep] != run.SubtaskDone {
				depsDone = false
				break
			}
		}
		if depsDone {
			ready = append(ready, st.ID)
		}
	}
	return ready
}

// pickConcurrent greedily selects a non-overlapping batch from the ready set under
// the isolation policy, capped at maxParallel. For non-overlapping isolation a
// candidate joins only if it does not overlap any already-selected subtask;
// overlapping candidates are left for a later iteration (which serializes them). For
// worktree/none isolation any ready subtask may join. At least one subtask is always
// selected when ready is non-empty, so the loop cannot stall with work available.
func (a *stateActor) pickConcurrent(ready []string) []string {
	max := effectiveMaxParallel(a.cfg)

	byID := make(map[string]run.Subtask, len(a.run.Subtasks))
	for i := range a.run.Subtasks {
		byID[a.run.Subtasks[i].ID] = a.run.Subtasks[i]
	}

	checkOverlap := a.cfg.Pipeline.Execution.Isolation == isolationNonOverlapping ||
		a.cfg.Pipeline.Execution.Isolation == ""

	var batch []string
	for _, id := range ready {
		if len(batch) >= max {
			break
		}
		if checkOverlap {
			conflicts := false
			for _, chosen := range batch {
				if subtasksOverlap(byID[id], byID[chosen]) {
					conflicts = true
					break
				}
			}
			if conflicts {
				continue // serialize: leave it for a subsequent batch.
			}
		}
		batch = append(batch, id)
	}
	return batch
}

// finalize is invoked when every subtask is terminal. If any failed it marks the run
// failed (persisted) and returns an error naming the failed and any stranded
// subtasks; otherwise it returns nil, leaving the run executing for the next phase.
func (a *stateActor) finalize() error {
	var failed []string
	for i := range a.run.Subtasks {
		if a.run.Subtasks[i].Status == run.SubtaskFailed {
			failed = append(failed, a.run.Subtasks[i].ID)
		}
	}
	if len(failed) == 0 {
		return nil
	}

	a.run.Status = run.StatusFailed
	if err := a.store.Save(a.run); err != nil {
		return fmt.Errorf("pipeline: marking run %q failed: %w", a.run.ID, err)
	}

	blocked := a.stranded()
	msg := fmt.Sprintf("pipeline: %d subtask(s) failed: %s", len(failed), a.describeFailed(failed))
	if len(blocked) > 0 {
		msg += fmt.Sprintf("; %d dependent subtask(s) could not run: %s",
			len(blocked), strings.Join(blocked, ", "))
	}
	return errors.New(msg)
}

// deadlock is returned when non-terminal subtasks remain but none are ready and none
// are running. The DAG was validated acyclic at planning, so the only cause is a
// failed/blocked dependency stranding its dependents; the error names the stuck
// subtasks and the unmet deps. It also marks the run failed.
func (a *stateActor) deadlock() error {
	stuck := a.stranded()
	a.run.Status = run.StatusFailed
	_ = a.store.Save(a.run) // best-effort; the returned error is the primary signal.

	if len(stuck) == 0 {
		return errors.New("pipeline: execution stalled with no runnable subtasks")
	}

	msg := fmt.Sprintf(
		"pipeline: execution stalled — %d subtask(s) cannot run because a dependency failed or is blocked: %s",
		len(stuck), strings.Join(stuck, "; "))
	var failed []string
	for i := range a.run.Subtasks {
		if a.run.Subtasks[i].Status == run.SubtaskFailed {
			failed = append(failed, a.run.Subtasks[i].ID)
		}
	}
	if len(failed) > 0 {
		msg += fmt.Sprintf("; upstream failure(s): %s", a.describeFailed(failed))
	}
	return errors.New(msg)
}

// stranded lists the non-terminal subtasks that are not runnable, each annotated
// with the deps that are not done, so callers can build an actionable "stuck because
// X failed" message.
func (a *stateActor) stranded() []string {
	statusByID := make(map[string]run.SubtaskStatus, len(a.run.Subtasks))
	for i := range a.run.Subtasks {
		statusByID[a.run.Subtasks[i].ID] = a.run.Subtasks[i].Status
	}

	var stuck []string
	for i := range a.run.Subtasks {
		st := a.run.Subtasks[i]
		if st.Status.IsTerminal() {
			continue
		}
		var unmet []string
		for _, dep := range st.Deps {
			if statusByID[dep] != run.SubtaskDone {
				unmet = append(unmet, fmt.Sprintf("%s=%s", dep, statusByID[dep]))
			}
		}
		sort.Strings(unmet)
		if len(unmet) > 0 {
			stuck = append(stuck, fmt.Sprintf("%s (waiting on %s)", st.ID, strings.Join(unmet, ", ")))
		} else {
			stuck = append(stuck, st.ID)
		}
	}
	return stuck
}

// describeFailed renders the failed subtask ids annotated with their recorded cause
// (e.g. "st-01 (executor failed: …)"), so the run-level error explains WHY each
// subtask failed, not merely that it did.
func (a *stateActor) describeFailed(failed []string) string {
	parts := make([]string, 0, len(failed))
	for _, id := range failed {
		if cause := a.failCauses[id]; cause != nil {
			parts = append(parts, fmt.Sprintf("%s (%v)", id, cause))
		} else {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, "; ")
}

// resetInterrupted rewinds subtasks left mid-step by an interrupted run
// (SubtaskImplementing/SubtaskReviewing) back to SubtaskPending so the scheduler
// re-runs that step from the persisted diff/baseline. Loops is preserved so the
// maxLoops bound still applies across the interruption.
func resetInterrupted(r *run.Run) {
	for i := range r.Subtasks {
		if r.Subtasks[i].Status.IsInterrupted() {
			r.Subtasks[i].Status = run.SubtaskPending
		}
	}
}

// effectiveMaxParallel is the concurrency cap: 1 when parallelism is disabled
// (strictly serial), otherwise the configured maxParallel floored at 1 (validation
// guarantees >= 1, floored defensively so a zero never yields a zero-capacity
// semaphore that would deadlock).
func effectiveMaxParallel(cfg config.Config) int {
	if !cfg.Pipeline.Execution.Parallel {
		return 1
	}
	if cfg.Pipeline.Execution.MaxParallel < 1 {
		return 1
	}
	return cfg.Pipeline.Execution.MaxParallel
}
