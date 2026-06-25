package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/pipeline"
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/jaxmef/aixecutor/internal/workspace"
)

// execSource is the read-only backing a pipeline command needs from its execution
// environment: snapshot the tree for the run baseline AND list its files for the
// planner orientation summary. Both the single-repo *git.Gateway and the multi-root
// *workspace.Workspace satisfy it, so the two modes share one wiring path.
type execSource interface {
	run.BaselineSource
	TrackedFiles(ctx context.Context) ([]string, error)
}

// execEnv is the resolved execution environment for a pipeline command: the root
// (the agents' working dir + the runsDir anchor), the pipeline's read-only
// git/workspace gateway, and the baseline/summary source. It abstracts "single repo
// vs. workspace" so run/plan/resume/backlog/amend wire identically (AIX-0020).
type execEnv struct {
	root    string
	gateway pipeline.Gateway
	source  execSource
}

// resolveExecEnv decides single-repo vs. workspace mode and builds the environment:
//
//   - explicit --workspace flag or workspace.root in config → workspace mode over
//     that root (the multi-repo / org case);
//   - otherwise, if the working dir is inside a git repo → single-repo mode (the
//     unchanged default; worktree isolation stays available);
//   - otherwise (cwd is not a git repo) → workspace mode over the cwd, which handles
//     a plain non-git directory.
//
// In every mode the tool's own runsDir is excluded from the baseline/diff/restore.
func resolveExecEnv(opts *GlobalOptions, cfg config.Config) (execEnv, error) {
	wsRoot := opts.Workspace
	if wsRoot == "" {
		wsRoot = cfg.Workspace.Root
	}

	if wsRoot == "" {
		// No explicit workspace: prefer the single-repo path when in a git repo, so
		// existing single-repo behavior (including worktree isolation) is unchanged.
		if gw, err := git.Open(workingDir()); err == nil {
			configureGitExclusions(cfg, gw)
			return execEnv{root: gw.RepoRoot(), gateway: pipeline.NewGitGateway(gw), source: gw}, nil
		}
		// Not a git repo → run over the cwd as a (single-root) workspace.
		wsRoot = workingDir()
	}

	ws, err := workspace.Discover(wsRoot, workspace.Options{
		MaxDepth:        cfg.Workspace.MaxDepth,
		Ignore:          cfg.Workspace.Ignore,
		ExcludePrefixes: workspaceExcludes(cfg, wsRoot),
	})
	if err != nil {
		return execEnv{}, err
	}
	return execEnv{root: ws.Root(), gateway: pipeline.NewWorkspaceGateway(ws), source: ws}, nil
}

// workspaceExcludes returns the workspace-relative exclusion prefixes (the tool's
// runsDir) so run artifacts never enter the baseline/diff/revert. It reuses the
// single-repo runsDir relativization, anchored at the workspace root.
func workspaceExcludes(cfg config.Config, root string) []string {
	if rel := run.RepoRelRunsDir(cfg, root); rel != "" {
		return []string{rel}
	}
	return nil
}

// newCreateStoreEnv builds a create-capable run.Store from the resolved environment:
// the runsDir is anchored at env.root and the baseliner snapshots env.source (a git
// repo or the whole workspace). It replaces the single-repo newCreateStore for the
// env-based commands.
func newCreateStoreEnv(cfg config.Config, env execEnv) (*run.Store, error) {
	baseliner := run.NewGitBaseliner(env.source, context.Background(), warnLargeSnapshot)
	return run.NewStoreFromConfig(cfg, env.root, run.WithBaseliner(baseliner))
}

// warnLargeSnapshot surfaces the snapshot soft-size-ceiling warning (the scope
// guard) to the user once, so pointing --workspace at a huge tree is flagged rather
// than silently snapshotted. The depth bound on discovery + this warn together
// satisfy AIX-0020's scope/size guard.
func warnLargeSnapshot(bytes int64) {
	fmt.Fprintf(os.Stderr,
		"aixecutor: warning: the workspace snapshot is large (%d bytes) — consider narrowing --workspace or the workspace.ignore set\n",
		bytes)
}
