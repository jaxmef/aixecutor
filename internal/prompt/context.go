package prompt

// This file defines the typed render contexts handed to each role's prompt
// template. They are the contract between this package and the pipeline phases
// that build prompts (planner: AIX-0009, executor: AIX-0010, subtask reviewer:
// AIX-0011, senior reviewer: AIX-0012). The field names are referenced verbatim
// in the templates under internal/prompt/prompts/, and the renderer runs with
// missingkey=error, so a template that names a field absent from its context is
// a hard render error. Treat these field names as stable: renaming one is a
// breaking change to both a template and a phase. Each struct documents what its
// caller must populate.

// Finding is one issue raised by a reviewer. It mirrors the machine-readable
// findings schema the reviewer prompts enforce and the findings parser in
// AIX-0011 (internal/pipeline/findings.go) produces. It appears in render
// contexts so findings can be fed back into worker prompts: into the executor on
// a remediation loop (ExecutorContext.PriorFindings) and into the senior
// reviewer as carried-forward items (SeniorReviewerContext.CarriedFindings).
//
// It is defined here, in the leaf prompt package, so this package stays free of
// pipeline imports (the dependency rule in CLAUDE.md §3.1); the pipeline maps its
// own findings type onto this one when rendering.
type Finding struct {
	// Severity is one of: blocker, major, minor, nit.
	Severity string
	// File is the path the finding concerns, relative to the repo root. May be
	// empty when the finding is not tied to a specific file.
	File string
	// Line is the line number in the new file, or 0 when not applicable.
	Line int
	// Message states the problem concretely. Always set.
	Message string
	// Suggestion is an optional concrete remedy. May be empty.
	Suggestion string
}

// PlannerContext is the render context for the planner template. The planner is
// invoked once per run, in plan mode, to produce the human docs and the subtask
// DAG.
//
// # Output contract (the bundle)
//
// The planner is READ-ONLY on the repository: it does NOT write files. Instead it
// returns all four documents in a single text response using the delimited bundle
// format the planner template defines and the planning phase parses (AIX-0009,
// internal/pipeline/planning.go). Each document is introduced by a marker line of
// its own — `@@AIXECUTOR_DOC:<filename>@@` — and aixecutor writes the parsed
// documents under <run>/<docsSubdir>/. Because the docs themselves contain
// Markdown code fences and YAML, the marker is deliberately distinctive so it
// cannot collide with document content; the template and the parser must stay in
// lockstep. No on-disk paths are passed in — aixecutor owns where the docs land.
type PlannerContext struct {
	// Task is the task the user asked the pipeline to accomplish.
	Task string
	// RepoSummary is a compact orientation blob (e.g. file tree plus a README
	// excerpt) so the planner can ground its plan in the actual repository,
	// without being so large it dominates the prompt.
	RepoSummary string
	// PriorError carries a validation error from a previous planning attempt so
	// the planner can correct it on a single re-prompt (AIX-0009). It is empty on
	// the first attempt; a non-empty value switches the prompt into its "your
	// previous output was rejected, fix it" mode. See planning.go's retry path.
	PriorError string
}

// SubtaskSpec is the planner-declared specification of a single subtask, as
// rendered into worker prompts. It is the prompt-facing view of a subtask (a
// subset of the run model's Subtask in AIX-0007/0009): the fields an executor or
// reviewer needs to see. It is duplicated here rather than imported so the prompt
// package has no pipeline/run dependency.
type SubtaskSpec struct {
	// ID is the subtask's stable identifier (e.g. "st-01").
	ID string
	// Title is the short imperative title.
	Title string
	// Description says what the subtask must accomplish.
	Description string
	// Files are the declared ownership globs for the subtask.
	Files []string
	// Acceptance lists the concrete, checkable acceptance criteria.
	Acceptance []string
	// ManualTest is an optional note on manually verifying the subtask; empty
	// when none was provided.
	ManualTest string
}

// BaselineInfo describes what a worker's diff is measured against, so the prompt
// can tell the agent how its change will be judged. The pipeline derives this
// from the run-start baseline captured by the git gateway (AIX-0006/0007).
type BaselineInfo struct {
	// Description is a human-readable phrase naming the diff baseline, e.g.
	// "the working tree as it was when this run started". It is interpolated
	// into a sentence in the executor prompt.
	Description string
}

// ExecutorContext is the render context for the executor template. It is built
// once per executor invocation: on the first attempt PriorFindings is empty, and
// on each remediation loop (AIX-0011) it is populated with the reviewer's
// findings so the executor can address them.
type ExecutorContext struct {
	// Task is the overall task, for context.
	Task string
	// Subtask is the specification of the subtask to implement.
	Subtask SubtaskSpec
	// ContextExcerpt is the relevant slice of docs/context.md for this subtask.
	ContextExcerpt string
	// PriorFindings are the reviewer findings to fix on a remediation pass. It is
	// empty on the first attempt; a non-empty value switches the prompt into its
	// "address these findings" mode.
	PriorFindings []Finding
	// Baseline describes what the executor's diff is measured against.
	Baseline BaselineInfo
}

// SubtaskReviewerContext is the render context for the subtask-reviewer template.
// It is built per review round (AIX-0011) with the subtask spec and the diff for
// just that subtask.
type SubtaskReviewerContext struct {
	// Subtask is the specification of the subtask being reviewed.
	Subtask SubtaskSpec
	// Diff is the unified diff (patch text) for this subtask only.
	Diff string
}

// SeniorReviewerContext is the render context for the senior-reviewer template.
// It is built per senior-review round (AIX-0012) with the full baseline→current
// diff plus any findings carried forward from subtask review.
type SeniorReviewerContext struct {
	// Task is the overall task.
	Task string
	// PlanSummary summarizes the plan (e.g. an excerpt of docs/plan.md).
	PlanSummary string
	// FullDiff is the full diff from the run-start baseline to the current tree.
	FullDiff string
	// CarriedFindings are unresolved findings carried forward from subtask
	// review; empty when there are none.
	CarriedFindings []Finding
}
