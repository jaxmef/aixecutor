package run

import (
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
)

// TestGitPolicyStringsAcceptedByConfig is a carry-forward cross-check. The git
// package and the config package each define the policy strings ("read-only",
// "allow-worktree") independently (git as exported constants, config as its
// schema values). This test wires them together so the two definitions cannot
// silently drift: it feeds the git package's constants into config.Validate and
// asserts the semantics line up.
//
//   - git.PolicyAllowWorktree + isolation: worktree must VALIDATE (the opt-in
//     path), and
//   - git.PolicyReadOnly + isolation: worktree must FAIL (worktree requires the
//     opt-in).
//
// If either package renames or re-spells its policy constant, this test breaks,
// surfacing the divergence at build/test time rather than as a confusing runtime
// validation error.
func TestGitPolicyStringsAcceptedByConfig(t *testing.T) {
	// allow-worktree + worktree isolation: valid.
	allow := config.Default()
	allow.Git.Policy = git.PolicyAllowWorktree
	allow.Pipeline.Execution.Isolation = "worktree"
	if err := config.Validate(allow); err != nil {
		t.Errorf("config.Validate rejected git.PolicyAllowWorktree (%q) with worktree isolation: %v",
			git.PolicyAllowWorktree, err)
	}

	// read-only + worktree isolation: must be rejected (no opt-in).
	readonly := config.Default()
	readonly.Git.Policy = git.PolicyReadOnly
	readonly.Pipeline.Execution.Isolation = "worktree"
	if err := config.Validate(readonly); err == nil {
		t.Errorf("config.Validate accepted git.PolicyReadOnly (%q) with worktree isolation; worktree must require the opt-in",
			git.PolicyReadOnly)
	}

	// read-only is otherwise a valid policy (default isolation).
	def := config.Default()
	def.Git.Policy = git.PolicyReadOnly
	if err := config.Validate(def); err != nil {
		t.Errorf("config.Validate rejected the default git.PolicyReadOnly (%q): %v", git.PolicyReadOnly, err)
	}
}
