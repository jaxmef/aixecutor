package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully-resolved, typed configuration for aixecutor. It mirrors
// the canonical schema in CLAUDE.md §5 exactly: every field, default value, and
// nested key documented there is reproduced here and in Default(). The two are
// kept in sync by hand; if you change one, change the other (and the README).
//
// The yaml tags are the public configuration keys users write in
// ~/.aixecutor/config.yaml and <repo>/.aixecutor/config.yaml. Decoding uses
// strict (KnownFields) mode, so an unknown key is a typo and an error.
type Config struct {
	Version   int                `yaml:"version"`
	Paths     Paths              `yaml:"paths"`
	Harnesses map[string]Harness `yaml:"harnesses"`
	Roles     Roles              `yaml:"roles"`
	Pipeline  Pipeline           `yaml:"pipeline"`
	Git       Git                `yaml:"git"`
	Backlog   Backlog            `yaml:"backlog"`
	Workspace Workspace          `yaml:"workspace"`
	Update    Update             `yaml:"update"`
}

// Paths controls where the tool writes its per-project artifacts. CONFIGURABLE.
type Paths struct {
	// RunsDir is the base path for run artifacts, relative to the repo root.
	RunsDir string `yaml:"runsDir"`
	// DocsSubdir is where planning docs live, under <runsDir>/<run-id>/.
	DocsSubdir string `yaml:"docsSubdir"`
}

// Harness describes how to drive a single AI coding agent as a CLI subprocess.
// Harnesses are declarative so most new agents are config-only (CLAUDE.md §4.2).
type Harness struct {
	// Type selects the harness implementation. Currently only "cli".
	Type string `yaml:"type"`
	// Command is the executable to run (e.g. "claude", "pi").
	Command string `yaml:"command"`
	// PromptDelivery is how the rendered prompt reaches the command:
	// "arg" (inline in args), "stdin", or "file" (a temp file path).
	PromptDelivery string `yaml:"promptDelivery"`
	// Args are the command arguments, each a Go text/template with access to
	// {{.Prompt}}, {{.Model}}, {{.PermissionMode}}, {{.WorkDir}}, etc.
	Args []string `yaml:"args"`
	// Output is the format of the command's stdout: "text" or "json".
	Output string `yaml:"output"`
	// ResultPath, for output: json, is the JSON field holding the final text.
	ResultPath string `yaml:"resultPath,omitempty"`
	// Timeout bounds a single invocation.
	Timeout Duration `yaml:"timeout"`
	// Retry bounds how a transient invocation failure is retried (AIX-0014). Only
	// transient failures (process spawn, timeout, empty/unparseable output) are
	// retried; a successful run — even one whose reviewer said "not approved" — and
	// any unambiguous hard error are never retried.
	Retry Retry `yaml:"retry"`
	// Env is extra environment passed to the subprocess.
	Env map[string]string `yaml:"env"`
}

// Retry is the harness-level retry policy for transient invocation failures
// (AIX-0014). It is intentionally small: a total attempt count and a base delay.
// The classification of "transient vs hard" lives in the harness layer
// (internal/harness/retry.go); this struct only carries the bounds.
type Retry struct {
	// MaxAttempts is the total number of attempts INCLUDING the first, so 1 means
	// "no retry" and 2 means "one retry". Must be >= 1.
	MaxAttempts int `yaml:"maxAttempts"`
	// Backoff is the base delay applied between attempts. Must be >= 0; 0 retries
	// immediately. A fixed (non-exponential) delay keeps the behavior predictable
	// and easy to reason about for a CLI.
	Backoff Duration `yaml:"backoff"`
}

// Roles binds each pipeline role to a harness, model, prompt and timeout. Every
// role is independently configurable (invariant #4).
type Roles struct {
	Planner         Role `yaml:"planner"`
	Executor        Role `yaml:"executor"`
	SubtaskReviewer Role `yaml:"subtaskReviewer"`
	SeniorReviewer  Role `yaml:"seniorReviewer"`
}

// Role is the configuration for one pipeline role (planner/executor/reviewers).
type Role struct {
	// Harness is the key of an entry in Config.Harnesses.
	Harness string `yaml:"harness"`
	// Model is the model alias or full ID handed to the harness.
	Model string `yaml:"model"`
	// PermissionMode is harness-specific (e.g. claude: plan|acceptEdits).
	PermissionMode string `yaml:"permissionMode"`
	// PromptTemplate is the name of the prompt template for this role.
	PromptTemplate string `yaml:"promptTemplate"`
	// Timeout bounds this role's harness invocation.
	Timeout Duration `yaml:"timeout"`
}

// Pipeline controls the orchestrator: when execution starts, how subtasks are
// scheduled, and the review-loop bounds.
type Pipeline struct {
	// AutostartExecution begins executing while the user reviews the docs.
	AutostartExecution bool `yaml:"autostartExecution"`
	// Execution controls subtask scheduling and isolation.
	Execution Execution `yaml:"execution"`
	// SubtaskReview is the per-subtask executor↔reviewer loop.
	SubtaskReview ReviewLoop `yaml:"subtaskReview"`
	// SeniorReview is the final whole-diff review loop.
	SeniorReview ReviewLoop `yaml:"seniorReview"`
}

// Execution controls how ready subtasks are scheduled and isolated.
type Execution struct {
	// Parallel enables concurrent subtask execution.
	Parallel bool `yaml:"parallel"`
	// MaxParallel caps concurrently-running subtasks (>= 1).
	MaxParallel int `yaml:"maxParallel"`
	// Isolation is the parallelism safety model:
	// "non-overlapping" (default), "worktree", or "none".
	Isolation string `yaml:"isolation"`
}

// ReviewLoop configures an executor↔reviewer remediation loop.
type ReviewLoop struct {
	// Enabled toggles the loop on or off.
	Enabled bool `yaml:"enabled"`
	// MaxLoops bounds the cycles; -1 means unlimited (so the minimum is -1).
	MaxLoops int `yaml:"maxLoops"`
}

// Git holds the git access policy. Mutating git (commit/push/add/reset/...) is
// NEVER permitted regardless of this setting; the only opt-in is worktrees.
type Git struct {
	// Policy is "read-only" (default) or "allow-worktree".
	Policy string `yaml:"policy"`
}

// Backlog configures the backlog runner / driver mode (AIX-0018): which directory
// of ticket files it reads and how it gates advancement between tickets. The
// runner never commits — each ticket leaves its changes in the working tree.
type Backlog struct {
	// Dir is the default backlog directory (a folder of ticket *.md files). Empty
	// means none; the `backlog run [dir]` argument overrides it.
	Dir string `yaml:"dir"`
	// Gate is the gating mode between tickets: "manual" (process one ticket then
	// pause — the safe default), "stop-on-finding" (advance through clean reviews,
	// stop on unresolved findings), or "auto" (run the whole backlog unattended).
	Gate string `yaml:"gate"`
}

// Workspace configures multi-root operation (AIX-0020): aixecutor can run in a
// single git repo (the default), a plain non-git directory, or a parent workspace
// containing several git repos and/or plain dirs, with one task spanning them.
// Single-repo is the degenerate 1-root case and needs no configuration.
type Workspace struct {
	// Root is the workspace root to operate over. Empty means "auto": the current
	// repo (or the cwd when not in a repo). The --workspace flag overrides it. When
	// set (or auto-detected as a non-repo dir), git repos beneath it are discovered.
	Root string `yaml:"root"`
	// MaxDepth bounds how deep beneath the root git repos are discovered (a guard
	// against scanning a huge org tree). >= 1; the root itself is depth 1.
	MaxDepth int `yaml:"maxDepth"`
	// Ignore is the set of directory names skipped when walking NON-git areas of the
	// workspace (where there is no .gitignore to lean on). `.git` and paths.runsDir
	// are always ignored regardless. These names are matched at any depth.
	Ignore []string `yaml:"ignore"`
}

// Update configures the startup update check (AIX-0022): a best-effort, non-blocking
// poll of the latest GitHub release that prints a notice when a newer aixecutor is
// available. It never delays or fails a run; a hanging GitHub is simply ignored.
type Update struct {
	// Check enables the startup update check. On by default; set false to opt out.
	Check bool `yaml:"check"`
	// Interval is the minimum time between network checks; within it the cached
	// result is reused. Must be >= 0 (0 checks every run).
	Interval Duration `yaml:"interval"`
}

// Duration is a time.Duration that marshals to and from the human-friendly
// Go duration strings used throughout the schema (e.g. "30m", "20m"). The
// stdlib yaml decoder does not understand these strings, so we wrap the field.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the duration the same way time.Duration does ("30m0s").
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration from a YAML scalar. Strings are parsed with
// time.ParseDuration ("30m", "1h30m"); a bare integer is treated as seconds for
// convenience. An empty value decodes to zero.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		// Fall back to an integer number of seconds.
		var n int64
		if err2 := value.Decode(&n); err2 != nil {
			return fmt.Errorf("duration %q: must be a string like \"30m\" or an integer number of seconds", value.Value)
		}
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration as its Go string form so it round-trips
// cleanly through the layered map[string]any merge and back into YAML.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}
