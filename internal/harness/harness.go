// Package harness defines the Harness interface, a generic declarative CLI
// adapter, a registry, a mock, and a dry-run wrapper. The pipeline depends on
// this abstraction rather than on any specific agent, which is the
// harness-agnostic seam required by CLAUDE.md invariant #5. Adding a new agent
// should be config-only (CLAUDE.md §4.2); concrete presets (claude in AIX-0004,
// pi in AIX-0005) are thin layers over the generic adapter.
//
// # Git-safety boundary
//
// A Harness spawns whatever command it is configured with; it deliberately does
// NOT police what the sub-agent does with git. Two separate mechanisms cover the
// two distinct concerns:
//
//   - The application's own git access is read-only and flows through the git
//     gateway (internal/git, AIX-0006). This layer never runs git at all.
//   - Sub-agent git safety ("do not commit / push") is enforced by the prompts
//     handed to the agent (internal/prompt, AIX-0008), not by inspecting the
//     subprocess here.
//
// Keeping that boundary explicit means the adapter stays strictly declarative
// and agent-neutral.
package harness

import (
	"context"
	"time"
)

// Harness drives a single AI coding agent. The pipeline talks only to this
// interface, so swapping or adding an agent never requires touching pipeline
// code (CLAUDE.md §3.2, invariant #5).
type Harness interface {
	// Name returns the harness's identifier (its key in config.Harnesses).
	Name() string
	// Run executes a single prompt in req.WorkDir and returns the agent's
	// result. Implementations MUST NOT perform write git operations; see the
	// package-level git-safety boundary note.
	Run(ctx context.Context, req Request) (Result, error)
}

// Request is a single invocation of a harness: one prompt, with the model,
// working directory, and execution parameters for this call. It mirrors the
// target shape in CLAUDE.md §3.2.
type Request struct {
	// Prompt is the fully-rendered prompt text handed to the agent.
	Prompt string
	// Role labels which pipeline role this invocation serves (e.g. "planner",
	// "executor", "subtask-reviewer", "senior-reviewer"). It is purely for
	// observability (AIX-0014): the logging wrapper tags every log line and the
	// persisted raw-output filename with it. It never affects how the command runs,
	// so an empty Role is harmless (it falls back to a generic label).
	Role string
	// Model is the model alias or full ID the harness should use.
	Model string
	// WorkDir is the directory the agent runs in (the subprocess's cwd).
	WorkDir string
	// PermissionMode is harness-specific (e.g. claude: plan|acceptEdits).
	PermissionMode string
	// Env is extra environment for this invocation; it is layered on top of the
	// process environment and the harness's configured env, and wins over both.
	Env map[string]string
	// Timeout bounds this single invocation. Zero means fall back to the
	// harness's configured timeout, and then to no deadline.
	Timeout time.Duration
}

// Result is the outcome of a single harness invocation (CLAUDE.md §3.2).
type Result struct {
	// Text is the final assistant text / summary, extracted per the harness's
	// output format (raw stdout for text, the resultPath field for json).
	Text string
	// Raw is the raw stdout, always retained for logging and debugging.
	Raw []byte
	// ExitCode is the subprocess exit code (0 on success).
	ExitCode int
	// Duration is the wall-clock time the invocation took. Always set, even on
	// error.
	Duration time.Duration
}
