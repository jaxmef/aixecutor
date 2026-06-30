package config

import (
	"strings"
	"testing"
	"time"
)

// TestDefaultValidates is the headline guard: the hardcoded baseline must be a
// complete, valid config (invariant #2 — the tool runs with zero config files).
func TestDefaultValidates(t *testing.T) {
	if err := Validate(Default()); err != nil {
		t.Fatalf("Default() does not validate: %v", err)
	}
}

// TestDefaultMatchesSchema spot-checks that Default() reproduces the canonical
// CLAUDE.md §5 values precisely. It does not re-list every field (the struct and
// default.go already do), but pins the values most likely to drift.
func TestDefaultMatchesSchema(t *testing.T) {
	d := Default()

	if d.Version != 1 {
		t.Errorf("version = %d, want 1", d.Version)
	}
	if d.Paths.RunsDir != ".aixecutor/runs" {
		t.Errorf("paths.runsDir = %q, want %q", d.Paths.RunsDir, ".aixecutor/runs")
	}
	if d.Paths.DocsSubdir != "docs" {
		t.Errorf("paths.docsSubdir = %q, want %q", d.Paths.DocsSubdir, "docs")
	}

	claude, ok := d.Harnesses["claude"]
	if !ok {
		t.Fatal("harnesses.claude missing")
	}
	if claude.Type != "cli" || claude.Command != "claude" || claude.PromptDelivery != "arg" {
		t.Errorf("harnesses.claude header wrong: %+v", claude)
	}
	wantArgs := []string{"-p", "{{.Prompt}}", "--output-format", "json", "--model", "{{.Model}}", "--permission-mode", "{{.PermissionMode}}"}
	if strings.Join(claude.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("harnesses.claude.args = %v, want %v", claude.Args, wantArgs)
	}
	if claude.Output != "json" || claude.ResultPath != "result" {
		t.Errorf("harnesses.claude output/resultPath wrong: %+v", claude)
	}
	if claude.Timeout.Std() != 30*time.Minute {
		t.Errorf("harnesses.claude.timeout = %v, want 30m", claude.Timeout)
	}

	pi, ok := d.Harnesses["pi"]
	if !ok {
		t.Fatal("harnesses.pi missing")
	}
	// Verified pi headless contract (AIX-0005): positional-arg prompt delivery,
	// --print, --model {{.Model}}, text output.
	if pi.Type != "cli" || pi.Command != "pi" || pi.PromptDelivery != "arg" || pi.Output != "text" {
		t.Errorf("harnesses.pi wrong: %+v", pi)
	}
	wantPiArgs := []string{"--print", "--model", "{{.Model}}", "{{.Prompt}}"}
	if strings.Join(pi.Args, "\x00") != strings.Join(wantPiArgs, "\x00") {
		t.Errorf("harnesses.pi.args = %v, want %v", pi.Args, wantPiArgs)
	}

	roles := map[string]struct {
		harness, model, perm, tmpl string
		timeout                    time.Duration
	}{
		"planner":         {"claude", "opus", "plan", "planner", 30 * time.Minute},
		"executor":        {"claude", "sonnet", "acceptEdits", "executor", 30 * time.Minute},
		"subtaskReviewer": {"claude", "sonnet", "plan", "subtask-reviewer", 20 * time.Minute},
		"seniorReviewer":  {"claude", "opus", "plan", "senior-reviewer", 30 * time.Minute},
	}
	got := map[string]Role{
		"planner":         d.Roles.Planner,
		"executor":        d.Roles.Executor,
		"subtaskReviewer": d.Roles.SubtaskReviewer,
		"seniorReviewer":  d.Roles.SeniorReviewer,
	}
	for name, w := range roles {
		r := got[name]
		if r.Harness != w.harness || r.Model != w.model || r.PermissionMode != w.perm ||
			r.PromptTemplate != w.tmpl || r.Timeout.Std() != w.timeout {
			t.Errorf("roles.%s = %+v, want harness=%s model=%s perm=%s tmpl=%s timeout=%v",
				name, r, w.harness, w.model, w.perm, w.tmpl, w.timeout)
		}
	}

	if !d.Pipeline.AutostartExecution {
		t.Error("pipeline.autostartExecution = false, want true")
	}
	if !d.Pipeline.Execution.Parallel || d.Pipeline.Execution.MaxParallel != 4 ||
		d.Pipeline.Execution.Isolation != "non-overlapping" {
		t.Errorf("pipeline.execution wrong: %+v", d.Pipeline.Execution)
	}
	if !d.Pipeline.SubtaskReview.Enabled || d.Pipeline.SubtaskReview.MaxLoops != 3 {
		t.Errorf("pipeline.subtaskReview wrong: %+v", d.Pipeline.SubtaskReview)
	}
	if !d.Pipeline.SeniorReview.Enabled || d.Pipeline.SeniorReview.MaxLoops != 3 {
		t.Errorf("pipeline.seniorReview wrong: %+v", d.Pipeline.SeniorReview)
	}
	if d.Git.Policy != "read-only" {
		t.Errorf("git.policy = %q, want read-only", d.Git.Policy)
	}

	// Update check (AIX-0022): enabled by default, 24h interval.
	if !d.Update.Check {
		t.Error("update.check = false, want true")
	}
	if d.Update.Interval.Std() != 24*time.Hour {
		t.Errorf("update.interval = %v, want 24h", d.Update.Interval)
	}

	// Retry policy (AIX-0014): both default harnesses retry once (maxAttempts 2)
	// with a 2s base backoff. Keep this in lockstep with CLAUDE.md §5 and README.
	for _, name := range []string{"claude", "pi"} {
		h := d.Harnesses[name]
		if h.Retry.MaxAttempts != 2 {
			t.Errorf("harnesses.%s.retry.maxAttempts = %d, want 2", name, h.Retry.MaxAttempts)
		}
		if h.Retry.Backoff.Std() != 2*time.Second {
			t.Errorf("harnesses.%s.retry.backoff = %v, want 2s", name, h.Retry.Backoff)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; "" means expect success
	}{
		{
			name:    "default is valid",
			mutate:  func(*Config) {},
			wantErr: "",
		},
		{
			name:    "worktree isolation without allow-worktree policy",
			mutate:  func(c *Config) { c.Pipeline.Execution.Isolation = "worktree" },
			wantErr: "requires git.policy",
		},
		{
			name: "worktree isolation with allow-worktree policy is valid",
			mutate: func(c *Config) {
				c.Pipeline.Execution.Isolation = "worktree"
				c.Git.Policy = "allow-worktree"
			},
			wantErr: "",
		},
		{
			name:    "unknown isolation mode",
			mutate:  func(c *Config) { c.Pipeline.Execution.Isolation = "bananas" },
			wantErr: "isolation",
		},
		{
			name:    "unknown git policy",
			mutate:  func(c *Config) { c.Git.Policy = "read-write" },
			wantErr: "git.policy",
		},
		{
			name:    "role references undefined harness",
			mutate:  func(c *Config) { c.Roles.Executor.Harness = "ghost" },
			wantErr: "not defined in harnesses",
		},
		{
			name:    "empty role harness",
			mutate:  func(c *Config) { c.Roles.Planner.Harness = "" },
			wantErr: "must be set",
		},
		{
			name:    "maxParallel below 1",
			mutate:  func(c *Config) { c.Pipeline.Execution.MaxParallel = 0 },
			wantErr: "maxParallel",
		},
		{
			name:    "subtaskReview maxLoops below -1",
			mutate:  func(c *Config) { c.Pipeline.SubtaskReview.MaxLoops = -2 },
			wantErr: "subtaskReview.maxLoops",
		},
		{
			name:    "seniorReview maxLoops below -1",
			mutate:  func(c *Config) { c.Pipeline.SeniorReview.MaxLoops = -5 },
			wantErr: "seniorReview.maxLoops",
		},
		{
			name:    "maxLoops -1 (unlimited) is valid",
			mutate:  func(c *Config) { c.Pipeline.SeniorReview.MaxLoops = -1 },
			wantErr: "",
		},
		{
			name: "harness retry maxAttempts below 1",
			mutate: func(c *Config) {
				h := c.Harnesses["claude"]
				h.Retry.MaxAttempts = 0
				c.Harnesses["claude"] = h
			},
			wantErr: "retry.maxAttempts",
		},
		{
			name: "harness retry negative backoff",
			mutate: func(c *Config) {
				h := c.Harnesses["pi"]
				h.Retry.Backoff = Duration(-1)
				c.Harnesses["pi"] = h
			},
			wantErr: "retry.backoff",
		},
		{
			name: "harness retry maxAttempts 1 (no retry) is valid",
			mutate: func(c *Config) {
				h := c.Harnesses["claude"]
				h.Retry.MaxAttempts = 1
				c.Harnesses["claude"] = h
			},
			wantErr: "",
		},
		{
			name:    "negative update interval",
			mutate:  func(c *Config) { c.Update.Interval = Duration(-1) },
			wantErr: "update.interval",
		},
		{
			name:    "zero update interval is valid",
			mutate:  func(c *Config) { c.Update.Interval = 0 },
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := Validate(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
