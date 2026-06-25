package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// recordingGitRunner is a HERMETIC git.RunnerFunc: it records every git subcommand
// the pipeline issues through the gateway and returns canned outputs for the
// read-only commands the pipeline needs — it NEVER execs the real git binary and
// never runs (or even requests) a mutating command. This keeps the test fully
// hermetic (CLAUDE.md §7: no test may run a mutating git command) while letting it
// faithfully assert what the pipeline ISSUES.
//
// Canned behavior, keyed on the subcommand:
//   - rev-parse  → echoes the repo root (only used if discovery ran; the gateway is
//     built with a known root here, so this is belt-and-suspenders).
//   - ls-files   → empty output (no tracked/untracked files), so the repo summary
//     and the full-diff enumeration both succeed with an empty file set.
//   - diff       → exit 0 (nil error) == "no differences", so `git diff --no-index`
//     returns an empty patch without touching anything.
//   - anything else read-only → empty output.
//
// If a MUTATING subcommand were ever requested it would still be recorded (and the
// gateway's own allowlist would have refused it before reaching here); the
// assertion below catches it either way. Safe for concurrent use (the scheduler may
// run subtasks in parallel).
type recordingGitRunner struct {
	repoRoot string

	mu   sync.Mutex
	subs []string   // the subcommand (args[0]) of each invocation, in order
	full [][]string // the full args of each invocation, for diagnostics
}

func (r *recordingGitRunner) run(_ context.Context, _ string, args ...string) (stdout, stderr []byte, err error) {
	r.mu.Lock()
	if len(args) > 0 {
		r.subs = append(r.subs, args[0])
	}
	r.full = append(r.full, append([]string(nil), args...))
	r.mu.Unlock()

	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "rev-parse":
		return []byte(r.repoRoot + "\n"), nil, nil
	case "ls-files":
		// No tracked/untracked files: the summarizer tolerates an empty tree, and
		// FullDiff snapshots nothing, then diffs an empty current tree.
		return nil, nil, nil
	case "diff":
		// `git diff --no-index`: nil error == exit 0 == "identical" (empty patch).
		return nil, nil, nil
	default:
		// Any other read-only command (status/log/show/cat-file) the pipeline might
		// issue: return empty output, success.
		return nil, nil, nil
	}
}

func (r *recordingGitRunner) subcommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.subs...)
}

// readOnlyGitSubcommands is the set of git subcommands that are read-only — the
// allowlist the gateway itself enforces (internal/git: allowedReadCmds). The
// belt-and-suspenders assertion below cross-checks every recorded invocation
// against this set, so a regression that somehow drove a mutating command through
// the gateway (or a future code path that bypassed the read helper) is caught.
var readOnlyGitSubcommands = map[string]bool{
	"status":    true,
	"diff":      true,
	"log":       true,
	"show":      true,
	"rev-parse": true,
	"ls-files":  true,
	"cat-file":  true,
}

// TestOrchestratorNoMutatingGit drives the WHOLE pipeline over a real *git.Gateway
// whose runner is a hermetic recorder (no real git binary, no real repo) and
// asserts that every git subcommand the pipeline ISSUED is read-only — i.e. no
// commit/add/push/reset/… was requested anywhere in planning, execution,
// per-subtask review, or senior review. This is the belt-and-suspenders proof of
// CLAUDE.md §2 invariant 1, layered over the gateway's own allowlist and the
// architectural guard test, and it is fully hermetic (CLAUDE.md §7).
func TestOrchestratorNoMutatingGit(t *testing.T) {
	repoRoot := t.TempDir() // a plain dir; no git is ever run in it.

	// A gateway whose runner records every invocation and canns read-only outputs.
	// The gateway stays read-only by construction regardless of the runner (its read
	// helper enforces the allowlist before the runner is ever called).
	rec := &recordingGitRunner{repoRoot: repoRoot}
	gw := git.NewGatewayWithRunner(repoRoot, rec.run)

	cfg := orchCfg()
	// A fake baseliner keeps the baseline path off git entirely (simpler than
	// canning its ls-files); the run still diffs against the persisted baseline dir.
	store, err := run.NewStoreFromConfig(cfg, repoRoot,
		run.WithBaseliner(fakeBaseliner{}),
		run.WithClock(fixedClock()),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Full converging pipeline with mock harnesses; the diffs come back empty from
	// the canned runner, which is fine — the test asserts what git is ISSUED, not
	// diff content.
	hs := fullPipelineHarnesses()
	reg := orchRegistry(t, cfg, hs)

	o, err := NewOrchestrator(store, cfg, reg,
		NewGitGateway(gw), // the read-only gateway, wrapped for the scheduler seam
		prompt.NewRenderer(),
		NewGitRepoSummarizer(gw, gw.RepoRoot()), // exercises ls-files (read-only)
		WithOrchestratorOutput(&strings.Builder{}),
	)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	r, err := o.Start(context.Background(), "add the example feature")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Fatalf("run status = %q; want completed", r.Status)
	}

	// THE ASSERTION: every git subcommand the whole pipeline issued is read-only.
	subs := rec.subcommands()
	if len(subs) == 0 {
		t.Fatal("expected the pipeline to issue at least one (read-only) git command")
	}
	for i, sub := range subs {
		if !readOnlyGitSubcommands[sub] {
			t.Errorf("git invocation #%d used a non-read-only subcommand %q (full: %v)",
				i, sub, rec.full[i])
		}
	}
	// Sanity: the read paths we expect DID run (so the test is actually exercising
	// the gateway, not vacuously passing on an empty run). The gateway is built with
	// a known root (no rev-parse discovery), so the issued reads are ls-files (repo
	// summary + full-diff enumeration) and diff (per-subtask + full diffs).
	for _, want := range []string{"ls-files", "diff"} {
		if !containsStr(subs, want) {
			t.Errorf("expected a read-only %q invocation during the pipeline; got %v", want, subs)
		}
	}
}

// containsStr reports whether xs contains s.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
