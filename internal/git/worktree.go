package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// Git policy values, mirroring config.Git.Policy (CLAUDE.md §5). Duplicated as
// local constants so this package does not import config solely for two strings
// and so the worktree gate reads explicitly; the values must stay in sync with
// the schema. Worktree returns a manager only under PolicyAllowWorktree.
const (
	PolicyReadOnly      = "read-only"
	PolicyAllowWorktree = "allow-worktree"
)

// WorktreeManager creates and tears down git worktrees for opt-in worktree
// isolation. `git worktree add` and `git worktree remove` are the ONLY mutating
// git commands permitted anywhere in aixecutor (CLAUDE.md §2 invariant 1), and
// they are reachable solely through a manager that the git.policy gate has
// already authorized — a Gateway alone cannot mutate anything. The manager
// deliberately bypasses the read allowlist (worktree is not a read command) for
// exactly these two operations, and nothing else.
//
// Every worktree the manager creates is tracked so it can be removed on
// teardown, including on error paths, so a failed run never leaks worktrees into
// the user's repository.
type WorktreeManager struct {
	gw *Gateway

	mu      sync.Mutex
	created []string // absolute worktree paths this manager added, for cleanup
}

// Worktree returns a WorktreeManager for the gateway's repository, but ONLY when
// policy is PolicyAllowWorktree. Under any other policy (notably the default
// PolicyReadOnly) the constructor refuses with a clear, actionable error and no
// manager is produced — so without the explicit opt-in there is no object in the
// program capable of running a mutating git command. This constructor-level gate
// is the in-code enforcement of the worktree opt-in.
func (g *Gateway) Worktree(policy string) (*WorktreeManager, error) {
	if policy != PolicyAllowWorktree {
		return nil, fmt.Errorf(
			"git worktree isolation is disabled: git.policy is %q; set git.policy: %q to opt in",
			policy, PolicyAllowWorktree)
	}
	return &WorktreeManager{gw: g}, nil
}

// Add creates a new git worktree named name and returns its absolute path. To
// avoid writing inside the repo's working tree (which would itself show up in
// diffs) or inside .git, the worktree is created as a sibling directory of the
// repo root: <repoRoot>/../<repo>-wt-<name>. The path is recorded for cleanup
// before git runs, so even a partial failure is still torn down by RemoveAll.
//
// This runs `git worktree add <path>`, one of the two permitted mutating git
// commands, via the gateway's runner (so tests inject a fake and assert the
// command surface without creating real worktrees).
func (m *WorktreeManager) Add(ctx context.Context, name string) (string, error) {
	path, err := m.worktreePath(name)
	if err != nil {
		return "", err
	}

	// Record the intended path BEFORE running git, so if `worktree add` partially
	// succeeds (creates the dir, then fails registering) RemoveAll still targets
	// it. A path that git never created will simply fail removal harmlessly.
	m.mu.Lock()
	m.created = append(m.created, path)
	m.mu.Unlock()

	// `git worktree add <path>` — PERMITTED mutating git (the only kind), gated
	// by the allow-worktree policy enforced in the Worktree constructor. It does
	// NOT go through the read allowlist by design.
	_, stderr, runErr := m.gw.run(ctx, m.gw.repoRoot, "worktree", "add", path)
	if runErr != nil {
		return "", fmt.Errorf("git worktree add %q: %w%s", path, runErr, stderrTail(stderr))
	}
	return path, nil
}

// Remove tears down a single worktree previously created by this manager via
// `git worktree remove <path>` (the second of the two permitted mutating git
// commands) and forgets it. --force is passed so a worktree containing the
// agent's uncommitted edits — the normal case, since we never commit — is still
// removable. Removing a path the manager did not create returns an error.
func (m *WorktreeManager) Remove(ctx context.Context, path string) error {
	if !m.forget(path) {
		return fmt.Errorf("git worktree remove: %q was not created by this manager", path)
	}
	return m.removeNow(ctx, path)
}

// RemoveAll removes every worktree this manager created, in reverse order of
// creation, and is safe to defer immediately after obtaining the manager so
// worktrees are cleaned up on ALL exit paths (success or error). It attempts to
// remove every tracked worktree even if some removals fail, and returns the
// joined errors so a caller can log them; a leaked-on-error worktree is thus the
// exception, surfaced loudly, not the silent default.
func (m *WorktreeManager) RemoveAll(ctx context.Context) error {
	m.mu.Lock()
	paths := m.created
	m.created = nil
	m.mu.Unlock()

	var errs []error
	// Reverse order: nested/dependent worktrees (if any) are removed before
	// ancestors.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := m.removeNow(ctx, paths[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeNow runs `git worktree remove --force <path>`. It does not touch the
// manager's tracking list (callers handle that), so it is reusable by both
// Remove and RemoveAll.
func (m *WorktreeManager) removeNow(ctx context.Context, path string) error {
	_, stderr, runErr := m.gw.run(ctx, m.gw.repoRoot, "worktree", "remove", "--force", path)
	if runErr != nil {
		return fmt.Errorf("git worktree remove %q: %w%s", path, runErr, stderrTail(stderr))
	}
	return nil
}

// forget drops path from the tracking list, reporting whether it was present.
func (m *WorktreeManager) forget(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range m.created {
		if p == path {
			m.created = append(m.created[:i], m.created[i+1:]...)
			return true
		}
	}
	return false
}

// Created returns a copy of the worktree paths currently tracked by the manager.
// Exposed for observability and tests; mutating the result does not affect the
// manager.
func (m *WorktreeManager) Created() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.created))
	copy(out, m.created)
	return out
}

// worktreePath derives an absolute worktree path for name as a sibling of the
// repo root (never inside .git), validating that name is a single, safe path
// segment so a crafted name cannot escape into an arbitrary location.
func (m *WorktreeManager) worktreePath(name string) (string, error) {
	if name == "" {
		return "", errors.New("git worktree add: name must not be empty")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("git worktree add: name %q must be a single path segment", name)
	}
	root := m.gw.repoRoot
	parent := filepath.Dir(root)
	base := filepath.Base(root)
	return filepath.Join(parent, fmt.Sprintf("%s-wt-%s", base, name)), nil
}
