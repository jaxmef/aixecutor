package run

import "time"

// CurrentSchemaVersion is the version stamped into newly-created run.yaml files.
// It is recorded so a future change to the persisted shape can be migrated
// rather than silently misread: Load rejects a run.yaml whose SchemaVersion is
// newer than this binary understands, with an actionable error. Bump this (and
// add migration handling) whenever the on-disk Run shape changes incompatibly.
const CurrentSchemaVersion = 1

// Run is a single pipeline execution, persisted to <Dir>/run.yaml. It is the
// durable, resumable record of a run: the orchestrator (AIX-0013) saves it after
// every state transition, and Store.Load reconstructs it to decide what work
// remains (see the resume contract on Store.Load). The shape mirrors CLAUDE.md
// §3.2; every field is YAML-serialized in a human-inspectable form.
//
// Field order here is also the field order in run.yaml (yaml.v3 preserves struct
// order), arranged so the most useful identifying fields appear first.
type Run struct {
	// SchemaVersion is the run.yaml format version (see CurrentSchemaVersion).
	SchemaVersion int `yaml:"schemaVersion"`
	// ID is the run identifier: "<timestamp>-<slug>" (see id.go). Stable,
	// filesystem-safe, and sortable by creation time.
	ID string `yaml:"id"`
	// Task is the original task description the user passed to `aixecutor run`.
	// It is also written verbatim to <Dir>/task.md for easy reading.
	Task string `yaml:"task"`
	// Status is the run-level state (state machine in CLAUDE.md §3.3).
	Status Status `yaml:"status"`
	// CreatedAt is when Store.Create made the run (from the injected clock).
	CreatedAt time.Time `yaml:"createdAt"`
	// UpdatedAt is the time of the last Save (from the injected clock). Each
	// Save refreshes it, so it doubles as a "last checkpoint" timestamp.
	UpdatedAt time.Time `yaml:"updatedAt"`
	// Baseline records where the run-start working-tree snapshot lives, so diffs
	// (per-subtask and the senior-review full diff) are taken relative to the
	// user's starting point rather than HEAD (CLAUDE.md §4.4).
	Baseline Baseline `yaml:"baseline"`
	// Subtasks is the planned subtask DAG with per-subtask state. It is empty
	// until planning populates it (from docs/subtasks.yaml).
	Subtasks []Subtask `yaml:"subtasks"`
	// SeniorReview tracks the final whole-diff review phase.
	SeniorReview SeniorReview `yaml:"seniorReview"`
	// Dir is the absolute run directory (<runsDir>/<id>). It is derived from the
	// layout, not authoritative on disk, but persisted for convenience; Load
	// always recomputes it from the store's runsDir so a moved runs tree still
	// resolves correctly.
	Dir string `yaml:"dir"`
}

// Subtask is one unit of work in the execution DAG (CLAUDE.md §3.2). Its fields
// originate in the planner's docs/subtasks.yaml (ID, Title, Deps, Files,
// Description, Acceptance) and are augmented with runtime state (Status, Loops)
// as the pipeline progresses. Carrying Description/Acceptance here lets resume
// rebuild an executor's context without re-reading the planning docs, and keeps
// the field set aligned with AIX-0009's subtasks schema.
type Subtask struct {
	// ID is the subtask identifier, unique within the run; it names the
	// subtasks/<id>/ artifact directory.
	ID string `yaml:"id"`
	// Title is a short human-readable summary of the subtask.
	Title string `yaml:"title"`
	// Description is the fuller statement of the work (optional). Carried so
	// resume can reconstruct executor context without re-invoking the planner.
	Description string `yaml:"description,omitempty"`
	// Acceptance is the subtask's acceptance criteria (optional), used by the
	// subtask reviewer.
	Acceptance string `yaml:"acceptance,omitempty"`
	// Deps lists the IDs of subtasks that must reach SubtaskDone before this one
	// becomes ready. Drives the DAG scheduler.
	Deps []string `yaml:"deps,omitempty"`
	// Files are the declared file-ownership globs for this subtask. They drive
	// non-overlapping parallelism (two ready subtasks run together only if their
	// file sets are disjoint) and bound the per-subtask diff (CLAUDE.md §4.3/4.4).
	Files []string `yaml:"files,omitempty"`
	// Status is the per-subtask state (CLAUDE.md §3.3).
	Status SubtaskStatus `yaml:"status"`
	// Loops is the number of executor↔reviewer cycles spent on this subtask, for
	// enforcing subtaskReview.maxLoops and for reporting in `status`.
	Loops int `yaml:"loops"`
	// Unresolved carries the reviewer findings that were still open when the
	// subtask review loop hit its maxLoops bound without the reviewer approving.
	// The subtask is then marked done-but-flagged (the default proceed-flagged
	// behaviour, CLAUDE.md §3.3), and these findings are persisted so the senior
	// review (AIX-0012) can pick them up and the user can see what was left open.
	// Empty for a subtask whose review converged cleanly. It is omitempty so a
	// clean run.yaml stays uncluttered and the field is backward-compatible with
	// run.yaml files written before it existed (schemaVersion covers the shape).
	Unresolved []Finding `yaml:"unresolved,omitempty"`
	// Mirrors Unresolved's rationale: omitempty keeps a clean run.yaml
	// uncluttered and stays backward-compatible with run.yaml files written before
	// the field existed.
	Undeclared []string `yaml:"undeclared,omitempty"`
}

// Finding is one reviewer finding persisted on the run (CLAUDE.md §3.3 review
// loops). It mirrors the machine-readable findings schema the reviewer prompts
// enforce (see internal/prompt.Finding, the leaf render shape) and the pipeline's
// findings parser. It is recorded here only when findings are carried forward —
// e.g. a subtask whose review loop hit maxLoops without approval stores its open
// findings in Subtask.Unresolved so the senior-review phase can adjudicate them.
// Defining it in the run package keeps the persisted shape independent of the
// pipeline and prompt packages while staying human-inspectable in run.yaml.
type Finding struct {
	// Severity is one of: blocker, major, minor, nit.
	Severity string `yaml:"severity"`
	// File is the path the finding concerns, relative to the repo root. May be
	// empty when the finding is not tied to a specific file.
	File string `yaml:"file,omitempty"`
	// Line is the line number in the new file, or 0 when not applicable.
	Line int `yaml:"line,omitempty"`
	// Message states the problem concretely. Always set.
	Message string `yaml:"message"`
	// Suggestion is an optional concrete remedy. May be empty.
	Suggestion string `yaml:"suggestion,omitempty"`
}

// SeniorReview tracks the final senior-review phase: whether it is enabled, its
// phase status, how many remediation rounds have run (against
// seniorReview.maxLoops), and any findings left unresolved at the cap. It is
// enough state for resume to know whether the phase is unstarted, mid-round, or
// finished, and for the end-of-run summary to report the outcome WITHOUT reading
// an artifact filename.
type SeniorReview struct {
	// Enabled mirrors pipeline.seniorReview.enabled at run start. When false the
	// phase is skipped and Status is SeniorReviewSkipped.
	Enabled bool `yaml:"enabled"`
	// Status is the senior-review phase state.
	Status SeniorReviewStatus `yaml:"status"`
	// Rounds is the number of senior reviewer → remediation cycles completed,
	// bounded by seniorReview.maxLoops.
	Rounds int `yaml:"rounds"`
	// Unresolved carries the senior reviewer's findings that were still open when
	// the phase hit its maxLoops bound without the change becoming clean. It is
	// the structured counterpart to the senior-review/unresolved.md report: with
	// Status=done, an empty Unresolved means the change converged clean, while a
	// non-empty Unresolved means the phase completed report-and-proceed with these
	// findings open for the user to adjudicate. This lets the end-of-run summary
	// distinguish "clean" from "cap-reached" by reading state, not a filename
	// (mirrors Subtask.Unresolved; see AIX-0012). Empty on a clean convergence.
	// omitempty keeps a clean run.yaml uncluttered and is backward-compatible with
	// run.yaml files written before the field existed.
	Unresolved []Finding `yaml:"unresolved,omitempty"`
}

// Baseline records the run-start working-tree snapshot. It wraps the location of
// the snapshot directory produced by the git gateway so run.yaml stays
// human-inspectable (a reader sees the path) and resume can re-open the baseline
// for diffing without re-capturing it.
//
// The snapshot itself is created by a Baseliner (see store.go), which in
// production adapts *git.Gateway.CaptureBaseline; the run package does not import
// git's Baseline type into its persisted shape, keeping the on-disk format
// independent of git internals.
type Baseline struct {
	// Dir is the directory holding the snapshotted files (e.g. <Dir>/.baseline).
	// It is relative to nothing in particular as written, but Store records an
	// absolute path; resume reads it back as-is.
	Dir string `yaml:"dir"`
	// Files is the number of files captured in the baseline (informational, for
	// `status` and debugging). Zero is valid (empty working tree).
	Files int `yaml:"files,omitempty"`
	// Bytes is the total size of the captured baseline (informational).
	Bytes int64 `yaml:"bytes,omitempty"`
}

// RunSummary is the lightweight projection of a run that Store.List returns: the
// identity and headline state, without loading every subtask. It backs the
// `list` table.
type RunSummary struct {
	// ID is the run identifier.
	ID string
	// Task is the original task description (rendered truncated by the CLI).
	Task string
	// Status is the run-level status.
	Status Status
	// CreatedAt / UpdatedAt are the run timestamps.
	CreatedAt time.Time
	UpdatedAt time.Time
	// Dir is the absolute run directory.
	Dir string
}

// SubtaskByID returns a pointer to the subtask with the given id and whether it
// was found. Callers (the orchestrator, status rendering) use it to update or
// inspect a single subtask's state.
func (r *Run) SubtaskByID(id string) (*Subtask, bool) {
	for i := range r.Subtasks {
		if r.Subtasks[i].ID == id {
			return &r.Subtasks[i], true
		}
	}
	return nil, false
}

// CountSubtasks tallies subtasks by status, returning (done, total). It is a
// small helper for progress reporting in `status`/`list`.
func (r *Run) CountSubtasks() (done, total int) {
	total = len(r.Subtasks)
	for i := range r.Subtasks {
		if r.Subtasks[i].Status == SubtaskDone {
			done++
		}
	}
	return done, total
}
