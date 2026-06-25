package run

// This file defines the run state machine's enumerations. They are string-typed
// (rather than integer iota) for one deliberate reason: run.yaml is meant to be
// human-inspectable (CLAUDE.md §3.3 / the resume contract). A reader opening the
// file should see `status: executing`, not `status: 3`, and a value that drifts
// out of the known set is caught by the IsValid helpers rather than silently
// decoding to some integer.

// Status is the run-level state. The values are exactly the states in the
// pipeline state machine (CLAUDE.md §3.3):
//
//	created → planning → planned → executing → seniorReview → completed
//
// with failed and aborted as terminal off-ramps reachable from any state. The
// orchestrator (AIX-0013) persists the Run after every transition between these
// states, so a resumed run reads its last durable Status and continues from
// there.
type Status string

const (
	// StatusCreated is the initial state set by Store.Create, before planning
	// has run. The run dir, task.md, config.snapshot.yaml and baseline exist.
	StatusCreated Status = "created"
	// StatusPlanning means the planner role is running (or was interrupted mid
	// run). On resume, planning restarts from scratch — it is a single
	// idempotent step that rewrites the docs, so re-running is safe.
	StatusPlanning Status = "planning"
	// StatusPlanned means planning finished and the docs + subtasks DAG are on
	// disk. Execution has not started (or autostart is off).
	StatusPlanned Status = "planned"
	// StatusExecuting means subtasks are being scheduled and run. Per-subtask
	// progress lives in each Subtask.Status; resume consults those.
	StatusExecuting Status = "executing"
	// StatusSeniorReview means all subtasks are done and the senior-review phase
	// (whole-diff audit + remediation loop) is running.
	StatusSeniorReview Status = "seniorReview"
	// StatusPaused means execution stopped at a safe subtask boundary in response to
	// a review request (AIX-0016). It is NOT terminal: the run resumes (clarify →
	// continue) or is amended (revert + restart). run.yaml is consistent at this
	// state (done subtasks done, the rest pending — never mid-subtask-write).
	StatusPaused Status = "paused"
	// StatusCompleted is the terminal success state: the pipeline finished and a
	// summary was written. The working tree is left for the user (never
	// committed).
	StatusCompleted Status = "completed"
	// StatusFailed is a terminal off-ramp: an unrecoverable error stopped the
	// run. The persisted state still reflects how far it got.
	StatusFailed Status = "failed"
	// StatusAborted is a terminal off-ramp: the user (or a signal) stopped the
	// run deliberately.
	StatusAborted Status = "aborted"
)

// statusOrder records the canonical forward order of the non-terminal pipeline
// phases, used only for stable presentation/diagnostics. Terminal states are not
// part of the linear order and are handled separately.
var statusOrder = []Status{
	StatusCreated,
	StatusPlanning,
	StatusPlanned,
	StatusExecuting,
	StatusSeniorReview,
	StatusCompleted,
}

// allStatuses is the full set of valid run statuses (phases + terminal
// off-ramps), the authority for IsValid.
var allStatuses = map[Status]bool{
	StatusCreated:      true,
	StatusPlanning:     true,
	StatusPlanned:      true,
	StatusExecuting:    true,
	StatusSeniorReview: true,
	StatusPaused:       true,
	StatusCompleted:    true,
	StatusFailed:       true,
	StatusAborted:      true,
}

// IsValid reports whether s is one of the defined run statuses. Used when
// loading run.yaml to reject a corrupted/foreign status with a clear error
// rather than carrying an unknown state into the orchestrator.
func (s Status) IsValid() bool { return allStatuses[s] }

// IsTerminal reports whether s is a terminal state (completed/failed/aborted).
// A terminal run is not resumable as "more work"; resume on a terminal run is a
// no-op the orchestrator surfaces to the user.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusAborted:
		return true
	default:
		return false
	}
}

// String returns the status as its YAML/string form.
func (s Status) String() string { return string(s) }

// SubtaskStatus is the per-subtask state (CLAUDE.md §3.2/§3.3). The resume
// contract (see Store.Load) is expressed entirely in terms of these values:
// a `done` subtask is never re-run; an interrupted `implementing`/`reviewing`
// subtask restarts that step.
type SubtaskStatus string

const (
	// SubtaskPending means the subtask has not started; it becomes ready when
	// all its Deps are SubtaskDone.
	SubtaskPending SubtaskStatus = "pending"
	// SubtaskImplementing means the executor is running on this subtask (or was
	// interrupted there). On resume the executor step restarts for this subtask.
	SubtaskImplementing SubtaskStatus = "implementing"
	// SubtaskReviewing means the subtask reviewer is running on this subtask's
	// diff (or was interrupted there). On resume the review step restarts.
	SubtaskReviewing SubtaskStatus = "reviewing"
	// SubtaskBlocked means the subtask cannot proceed (e.g. it hit its review
	// loop bound with findings still open, or a dependency failed). The
	// orchestrator decides how to surface this; it is not "done".
	SubtaskBlocked SubtaskStatus = "blocked"
	// SubtaskDone is the terminal success state for a subtask: implemented and
	// its review loop converged. A done subtask is NEVER re-run on resume.
	SubtaskDone SubtaskStatus = "done"
	// SubtaskFailed is the terminal failure state for a subtask.
	SubtaskFailed SubtaskStatus = "failed"
)

// allSubtaskStatuses is the full set of valid subtask statuses, the authority
// for IsValid.
var allSubtaskStatuses = map[SubtaskStatus]bool{
	SubtaskPending:      true,
	SubtaskImplementing: true,
	SubtaskReviewing:    true,
	SubtaskBlocked:      true,
	SubtaskDone:         true,
	SubtaskFailed:       true,
}

// IsValid reports whether s is one of the defined subtask statuses.
func (s SubtaskStatus) IsValid() bool { return allSubtaskStatuses[s] }

// IsTerminal reports whether s is a terminal subtask state (done/failed).
func (s SubtaskStatus) IsTerminal() bool {
	return s == SubtaskDone || s == SubtaskFailed
}

// IsInterrupted reports whether s is a non-terminal "in-flight" state
// (implementing/reviewing) that a resume must restart from its last checkpoint.
// This encodes one half of the resume contract documented on Store.Load.
func (s SubtaskStatus) IsInterrupted() bool {
	return s == SubtaskImplementing || s == SubtaskReviewing
}

// String returns the subtask status as its YAML/string form.
func (s SubtaskStatus) String() string { return string(s) }

// SeniorReviewStatus tracks the senior-review phase independently of the run
// Status, so resume can tell an unstarted senior review from one that was
// interrupted mid-round.
type SeniorReviewStatus string

const (
	// SeniorReviewPending means the senior review has not started.
	SeniorReviewPending SeniorReviewStatus = "pending"
	// SeniorReviewRunning means the senior reviewer (or its remediation loop) is
	// in progress, or was interrupted. On resume the current round restarts.
	SeniorReviewRunning SeniorReviewStatus = "running"
	// SeniorReviewDone means the senior review converged (diff clean or loop
	// bound reached) — terminal for this phase.
	SeniorReviewDone SeniorReviewStatus = "done"
	// SeniorReviewSkipped means the phase was disabled in config
	// (pipeline.seniorReview.enabled: false) and was not run.
	SeniorReviewSkipped SeniorReviewStatus = "skipped"
)

// allSeniorReviewStatuses is the full set of valid senior-review statuses.
var allSeniorReviewStatuses = map[SeniorReviewStatus]bool{
	SeniorReviewPending: true,
	SeniorReviewRunning: true,
	SeniorReviewDone:    true,
	SeniorReviewSkipped: true,
}

// IsValid reports whether s is one of the defined senior-review statuses.
func (s SeniorReviewStatus) IsValid() bool { return allSeniorReviewStatuses[s] }

// String returns the senior-review status as its YAML/string form.
func (s SeniorReviewStatus) String() string { return string(s) }
