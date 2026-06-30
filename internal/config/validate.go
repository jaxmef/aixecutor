package config

import (
	"fmt"
	"sort"
	"strings"
)

// Valid isolation modes for pipeline.execution.isolation.
const (
	isolationNonOverlapping = "non-overlapping"
	isolationWorktree       = "worktree"
	isolationNone           = "none"
)

// Valid git policies for git.policy.
const (
	policyReadOnly      = "read-only"
	policyAllowWorktree = "allow-worktree"
)

// Valid gating modes for backlog.gate (AIX-0018).
const (
	gateManual        = "manual"
	gateStopOnFinding = "stop-on-finding"
	gateAuto          = "auto"
)

// Validate checks a resolved Config against the rules in CLAUDE.md §5 and
// returns the first violation as an actionable error (what is wrong, where, and
// how to fix it). Unknown-key (typo) protection is handled by the strict YAML
// decode in Load; Validate covers the semantic constraints.
//
// Rules enforced:
//   - isolation must be one of non-overlapping|worktree|none, and
//     isolation: worktree requires git.policy: allow-worktree;
//   - git.policy must be read-only|allow-worktree;
//   - every roles.*.harness must name a defined entry in harnesses;
//   - pipeline.execution.maxParallel >= 1;
//   - pipeline.subtaskReview.maxLoops and pipeline.seniorReview.maxLoops >= -1;
//   - backlog.gate must be manual|stop-on-finding|auto.
func Validate(cfg Config) error {
	// git.policy
	switch cfg.Git.Policy {
	case policyReadOnly, policyAllowWorktree:
	default:
		return fmt.Errorf("git.policy: %q is invalid; must be %q or %q",
			cfg.Git.Policy, policyReadOnly, policyAllowWorktree)
	}

	// pipeline.execution.isolation
	switch cfg.Pipeline.Execution.Isolation {
	case isolationNonOverlapping, isolationNone:
	case isolationWorktree:
		if cfg.Git.Policy != policyAllowWorktree {
			return fmt.Errorf(
				"pipeline.execution.isolation: %q requires git.policy: %q (currently %q); set git.policy to %q to opt into worktree isolation",
				isolationWorktree, policyAllowWorktree, cfg.Git.Policy, policyAllowWorktree)
		}
	default:
		return fmt.Errorf(
			"pipeline.execution.isolation: %q is invalid; must be %q, %q, or %q",
			cfg.Pipeline.Execution.Isolation, isolationNonOverlapping, isolationWorktree, isolationNone)
	}

	// pipeline.execution.maxParallel
	if cfg.Pipeline.Execution.MaxParallel < 1 {
		return fmt.Errorf(
			"pipeline.execution.maxParallel: %d is invalid; must be >= 1",
			cfg.Pipeline.Execution.MaxParallel)
	}

	// review-loop bounds (-1 means unlimited)
	if cfg.Pipeline.SubtaskReview.MaxLoops < -1 {
		return fmt.Errorf(
			"pipeline.subtaskReview.maxLoops: %d is invalid; must be >= -1 (-1 = unlimited)",
			cfg.Pipeline.SubtaskReview.MaxLoops)
	}
	if cfg.Pipeline.SeniorReview.MaxLoops < -1 {
		return fmt.Errorf(
			"pipeline.seniorReview.maxLoops: %d is invalid; must be >= -1 (-1 = unlimited)",
			cfg.Pipeline.SeniorReview.MaxLoops)
	}

	// per-harness retry policy bounds (AIX-0014). Iterate in sorted order so the
	// first reported violation is deterministic across runs.
	for _, name := range sortedHarnessNames(cfg.Harnesses) {
		h := cfg.Harnesses[name]
		if h.Retry.MaxAttempts < 1 {
			return fmt.Errorf(
				"harnesses.%s.retry.maxAttempts: %d is invalid; must be >= 1 (1 = no retry)",
				name, h.Retry.MaxAttempts)
		}
		if h.Retry.Backoff < 0 {
			return fmt.Errorf(
				"harnesses.%s.retry.backoff: %s is invalid; must be >= 0",
				name, h.Retry.Backoff)
		}
	}

	// backlog.gate (AIX-0018)
	switch cfg.Backlog.Gate {
	case gateManual, gateStopOnFinding, gateAuto:
	default:
		return fmt.Errorf(
			"backlog.gate: %q is invalid; must be %q, %q, or %q",
			cfg.Backlog.Gate, gateManual, gateStopOnFinding, gateAuto)
	}

	// workspace.maxDepth (AIX-0020)
	if cfg.Workspace.MaxDepth < 1 {
		return fmt.Errorf(
			"workspace.maxDepth: %d is invalid; must be >= 1 (the root is depth 1)",
			cfg.Workspace.MaxDepth)
	}

	// update.interval (AIX-0022)
	if cfg.Update.Interval < 0 {
		return fmt.Errorf(
			"update.interval: %s is invalid; must be >= 0 (0 = check every run)",
			cfg.Update.Interval)
	}

	// every role's harness must be defined in harnesses
	for _, r := range rolesByName(cfg.Roles) {
		if r.role.Harness == "" {
			return fmt.Errorf("roles.%s.harness: must be set", r.name)
		}
		if _, ok := cfg.Harnesses[r.role.Harness]; !ok {
			return fmt.Errorf(
				"roles.%s.harness: %q is not defined in harnesses (defined: %s)",
				r.name, r.role.Harness, knownHarnesses(cfg.Harnesses))
		}
	}

	return nil
}

// namedRole pairs a role with its schema key, so validation errors can name the
// offending role precisely.
type namedRole struct {
	name string
	role Role
}

// rolesByName returns the four roles in a stable order for validation.
func rolesByName(r Roles) []namedRole {
	return []namedRole{
		{"planner", r.Planner},
		{"executor", r.Executor},
		{"subtaskReviewer", r.SubtaskReviewer},
		{"seniorReviewer", r.SeniorReviewer},
	}
}

// knownHarnesses renders the defined harness keys, sorted, for error messages.
func knownHarnesses(h map[string]Harness) string {
	names := sortedHarnessNames(h)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// sortedHarnessNames returns the harness keys in stable, sorted order so
// validation reports the first violation deterministically.
func sortedHarnessNames(h map[string]Harness) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
