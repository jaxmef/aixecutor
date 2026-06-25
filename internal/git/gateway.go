package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// allowedReadCmds is the allowlist of git subcommands the gateway will execute.
// It is the in-code enforcement of CLAUDE.md §2 invariant 1: only read commands
// are permitted, so even a programming mistake elsewhere cannot drive a mutating
// git operation through this gateway. Anything not in this set is refused by
// read before any process is spawned (defense in depth, not mere convention).
//
// Worktree add/remove are the project's sole permitted mutating git commands and
// are deliberately NOT in this set: they live only on a WorktreeManager that the
// git.policy gate has already authorized (see worktree.go). They never route
// through read; they call the runner directly, behind the policy check.
var allowedReadCmds = map[string]bool{
	"status":    true,
	"diff":      true,
	"log":       true,
	"show":      true,
	"rev-parse": true,
	"ls-files":  true,
	"cat-file":  true,
}

// runnerFunc executes `git <args...>` in dir and returns captured stdout and
// stderr separately, plus any process error. It is a field on Gateway (and is
// shared with the worktree manager) so tests can inject a fake that records args
// and returns canned output without spawning git — mirroring the injectable
// runner pattern in internal/harness. The default is execRunner.
type runnerFunc func(ctx context.Context, dir string, args ...string) (stdout, stderr []byte, err error)

// RunnerFunc is the exported form of runnerFunc, used with NewGatewayWithRunner
// by tests in OTHER packages that need to observe (or intercept) the exact git
// invocations a gateway makes — for example the orchestrator's belt-and-suspenders
// "no mutating git ran" assertion (AIX-0013), which wraps execRunner to record
// every `git <subcommand>` driven through the gateway during a full pipeline.
type RunnerFunc = runnerFunc

// ExecRunner is the exported default RunnerFunc: it runs git as a real subprocess
// (the same runner Open installs). Cross-package tests compose it — e.g. a
// recording wrapper that delegates to ExecRunner over a real temp repo — when they
// want REAL git behavior while still observing the subcommands invoked.
var ExecRunner RunnerFunc = execRunner

// NewGatewayWithRunner builds a Gateway rooted at repoRoot whose git invocations go
// through run. It is the exported seam for cross-package tests; production code
// uses Open (which discovers the repo root and installs execRunner). The returned
// gateway is still read-only by construction — run is only ever called via the read
// allowlist (and the worktree manager's policy gate), exactly as for an Open'd one.
// A nil runner defaults to execRunner.
func NewGatewayWithRunner(repoRoot string, run RunnerFunc) *Gateway {
	if run == nil {
		run = execRunner
	}
	return newGatewayWithRunner(repoRoot, run)
}

// Gateway is the single chokepoint for all git access in aixecutor. Every git
// invocation in the application goes through a Gateway (read commands via read)
// or through a WorktreeManager obtained from one (the only mutating exception).
// No other package shells out to git; a guard test enforces that.
//
// A Gateway is read-only by construction: its public surface exposes only read
// helpers, and read refuses any subcommand outside allowedReadCmds.
type Gateway struct {
	// repoRoot is the absolute path to the repository root (the directory git
	// commands run in). It is discovered once in Open via rev-parse.
	repoRoot string
	// run executes git; injectable for hermetic tests. Defaults to execRunner.
	run runnerFunc
	// excludePrefixes is a set of cleaned, repo-relative directory prefixes whose
	// contents are filtered out of BOTH the run-start baseline (CaptureBaseline)
	// and the full diff's current-tree snapshot (FullDiff). It exists so the tool
	// never snapshots its own output directory (the configured paths.runsDir) into
	// a run's .baseline or surfaces it in the senior-review diff, even when the user
	// has not gitignored it. Because the SAME set gates both the baseline and the
	// full diff's current side, the two stay symmetric by construction — neither
	// captures runsDir, so the diff is clean of it.
	//
	// The git package cannot import config, so the prefixes are supplied by the
	// caller (the CLI relativizes cfg.Paths.RunsDir against repoRoot). Excluding
	// ".aixecutor/runs" also covers every run dir and its .baseline beneath it,
	// which is the recursion guard: a run dir can never be snapshotted into its own
	// .baseline. nil/empty means no exclusion (the historical behavior).
	excludePrefixes []string
}

// Open discovers the repository containing dir and returns a Gateway rooted at
// its top level. Discovery itself uses only a read command
// (`git rev-parse --show-toplevel`). If dir is not inside a git work tree, Open
// returns an error explaining that, wrapping the underlying git failure with %w.
func Open(dir string) (*Gateway, error) {
	g := &Gateway{repoRoot: dir, run: execRunner}
	root, err := g.topLevel(context.Background())
	if err != nil {
		return nil, fmt.Errorf("git: %q is not inside a git repository: %w", dir, err)
	}
	g.repoRoot = root
	return g, nil
}

// newGatewayWithRunner builds a Gateway with an explicit repoRoot and runner. It
// is the seam tests use to construct a gateway without discovery and with a fake
// runner; production code uses Open.
func newGatewayWithRunner(repoRoot string, run runnerFunc) *Gateway {
	return &Gateway{repoRoot: repoRoot, run: run}
}

// RepoRoot returns the absolute repository root the gateway operates on.
func (g *Gateway) RepoRoot() string { return g.repoRoot }

// SetExcludePrefixes configures the repo-relative directory prefixes whose
// contents are skipped by CaptureBaseline and FullDiff (see the excludePrefixes
// field). Each prefix is cleaned and any entry that is empty, ".", absolute, or
// escapes the repo root is dropped, so a runsDir that lives OUTSIDE the repo (a
// caller passing ".." after relativizing) simply yields no exclusion rather than
// an over-broad filter. Calling it again replaces the set.
//
// The CLI calls this once on the shared gateway, right after Open, so the SAME
// set gates the baseline and the full diff and the two stay symmetric.
func (g *Gateway) SetExcludePrefixes(prefixes ...string) {
	g.excludePrefixes = cleanPrefixes(prefixes)
}

// IsRepo reports whether the gateway's repoRoot is inside a git work tree. It
// performs a read-only `git rev-parse --is-inside-work-tree`. A gateway returned
// by Open is always a repo; this is useful when a Gateway is constructed by
// other means or to re-check after directory changes.
func (g *Gateway) IsRepo() bool {
	out, err := g.read(context.Background(), "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// read executes an allowlisted git subcommand in repoRoot and returns its
// stdout. It is the ONLY path through which the gateway runs git for reads, and
// the choke point that enforces invariant 1: if args is empty or args[0] is not
// in allowedReadCmds, read refuses with a clear, actionable error and never
// spawns a process. On a non-zero git exit it returns an error that includes a
// bounded tail of stderr so callers get an actionable message.
func (g *Gateway) read(ctx context.Context, args ...string) ([]byte, error) {
	if err := checkAllowed(args); err != nil {
		return nil, err
	}
	stdout, stderr, err := g.run(ctx, g.repoRoot, args...)
	if err != nil {
		return stdout, fmt.Errorf("git %s: %w%s", strings.Join(args, " "), err, stderrTail(stderr))
	}
	return stdout, nil
}

// checkAllowed enforces the read allowlist: it returns a clear, actionable error
// (and the caller must not spawn git) unless args[0] is an allowlisted read
// subcommand. Extracted so both read and the diff engine — which needs the same
// gate but different exit-code handling for `git diff --no-index` — share one
// authoritative refusal point.
func checkAllowed(args []string) error {
	if len(args) == 0 {
		return errors.New("git: no command given (read-only gateway)")
	}
	if !allowedReadCmds[args[0]] {
		return fmt.Errorf("git: command %q is not permitted (read-only gateway)", args[0])
	}
	return nil
}

// topLevel returns the absolute repository root via `git rev-parse
// --show-toplevel` (a read command). Used by Open for discovery.
func (g *Gateway) topLevel(ctx context.Context) (string, error) {
	out, err := g.read(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", errors.New("git: empty repository root from rev-parse --show-toplevel")
	}
	return root, nil
}

// Status returns the porcelain v1 status of the work tree (`git status
// --porcelain`). It is read-only and used by baseline enumeration to find
// untracked-but-not-ignored files.
func (g *Gateway) Status(ctx context.Context) ([]byte, error) {
	return g.read(ctx, "status", "--porcelain")
}

// TrackedFiles returns the repo-relative paths of all tracked files (`git
// ls-files`). Read-only.
func (g *Gateway) TrackedFiles(ctx context.Context) ([]string, error) {
	out, err := g.read(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// UntrackedFiles returns repo-relative paths of files that are untracked but not
// ignored (`git ls-files --others --exclude-standard`). Respecting
// --exclude-standard is how the gateway honors .gitignore: ignored build
// artifacts (node_modules, dist, …) are never returned, so the baseline never
// snapshots them. Read-only.
func (g *Gateway) UntrackedFiles(ctx context.Context) ([]string, error) {
	out, err := g.read(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// splitNUL splits NUL-delimited git output (the -z form) into a slice, dropping
// a trailing empty element. The -z form is used so paths with spaces or unusual
// characters are not mangled.
func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, string(p))
	}
	return out
}

// stderrTailBytes bounds how much trailing git stderr is quoted in errors so a
// chatty failure does not produce an unreadable message.
const stderrTailBytes = 2048

// stderrTail returns a quoted, length-bounded tail of stderr suitable for
// appending to an error, or "" when stderr is empty.
func stderrTail(stderr []byte) string {
	trimmed := bytes.TrimSpace(stderr)
	if len(trimmed) == 0 {
		return ""
	}
	if len(trimmed) > stderrTailBytes {
		trimmed = trimmed[len(trimmed)-stderrTailBytes:]
	}
	return fmt.Sprintf("\nstderr: %s", trimmed)
}
