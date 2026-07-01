package config

import "time"

// Default returns the hardcoded, fully-populated configuration baseline. It is
// the single source of truth in code and MUST mirror the canonical schema in
// CLAUDE.md §5 exactly (invariant #2: no required configuration — the tool runs
// end-to-end with zero config files because this struct is complete and valid).
//
// Every call returns a fresh value with its own maps and slices, so callers may
// mutate the result (e.g. during merge) without corrupting a shared instance.
func Default() Config {
	min := func(d time.Duration) Duration { return Duration(d) }

	return Config{
		Version: 1,
		Paths: Paths{
			RunsDir:    ".aixecutor/runs",
			DocsSubdir: "docs",
		},
		Harnesses: map[string]Harness{
			// Claude Code, driven headless via `claude -p`.
			"claude": {
				Type:           "cli",
				Command:        "claude",
				PromptDelivery: "arg",
				Args: []string{
					"-p",
					"{{.Prompt}}",
					"--output-format",
					"json",
					"--model",
					"{{.Model}}",
					"--permission-mode",
					"{{.PermissionMode}}",
				},
				Output:     "json",
				ResultPath: "result",
				Timeout:    min(30 * time.Minute),
				Retry: Retry{
					MaxAttempts: 2,
					Backoff:     min(2 * time.Second),
				},
				Env: map[string]string{},
			},
			// pi coding agent. Headless contract verified against pi v0.79.10
			// (AIX-0005): `--print`/`-p` runs a single prompt and exits, the
			// prompt is a POSITIONAL argument (not stdin), `--model` selects the
			// model, and `--mode text` (the default) prints the final text to
			// stdout. `--mode json` exists but the JSON field holding the final
			// text is unverified, so json output is deferred (TODO: confirm the
			// resultPath with a live `pi -p --mode json` run before switching).
			// Empty-model caveat: defaults always set a role model, so {{.Model}}
			// is non-empty in practice; if a role using pi omits a model, an empty
			// `--model ""` is passed — acceptable for v1.
			"pi": {
				Type:           "cli",
				Command:        "pi",
				PromptDelivery: "arg",
				Args: []string{
					"--print",
					"--model",
					"{{.Model}}",
					"{{.Prompt}}",
				},
				Output:  "text",
				Timeout: min(30 * time.Minute),
				Retry: Retry{
					MaxAttempts: 2,
					Backoff:     min(2 * time.Second),
				},
				Env: map[string]string{},
			},
		},
		Roles: Roles{
			Planner: Role{
				Harness:        "claude",
				Model:          "opus",
				PermissionMode: "plan",
				PromptTemplate: "planner",
				Timeout:        min(30 * time.Minute),
			},
			Executor: Role{
				Harness:        "claude",
				Model:          "sonnet",
				PermissionMode: "acceptEdits",
				PromptTemplate: "executor",
				Timeout:        min(30 * time.Minute),
			},
			SubtaskReviewer: Role{
				Harness:        "claude",
				Model:          "sonnet",
				PermissionMode: "plan",
				PromptTemplate: "subtask-reviewer",
				Timeout:        min(20 * time.Minute),
			},
			SeniorReviewer: Role{
				Harness:        "claude",
				Model:          "opus",
				PermissionMode: "plan",
				PromptTemplate: "senior-reviewer",
				Timeout:        min(30 * time.Minute),
			},
		},
		Pipeline: Pipeline{
			AutostartExecution: true,
			Execution: Execution{
				Parallel:    true,
				MaxParallel: 4,
				Isolation:   "non-overlapping",
			},
			SubtaskReview: ReviewLoop{
				Enabled:  true,
				MaxLoops: 3,
			},
			SeniorReview: ReviewLoop{
				Enabled:  true,
				MaxLoops: 3,
			},
		},
		Git: Git{
			Policy: "read-only",
		},
		Backlog: Backlog{
			Dir:  "",
			Gate: "manual",
		},
		Workspace: Workspace{
			Root:     "",
			MaxDepth: 4,
		},
		Update: Update{
			Check:    true,
			Interval: min(24 * time.Hour),
		},
		Ignore: []string{".idea", ".vscode", ".DS_Store", "node_modules", "vendor", "dist", "build", ".next", "target"},
	}
}
