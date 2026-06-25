// Package git is the read-only git gateway: baseline snapshots, the diff
// engine, and opt-in worktree isolation. No mutating git operations are ever
// performed here (the sole exception being opt-in worktree add/remove). See
// CLAUDE.md §2 invariant 1 and §4.3–§4.4.
package git
