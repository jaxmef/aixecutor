package cli

import (
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newResumeCmd builds the `resume` command: continue a previous run from its last
// persisted state (AIX-0013). With no run-id (or "latest"), the most recent run is
// resumed. Finished work is never redone (the resume contract on run.Store.Load):
// done subtasks are skipped, interrupted ones re-run from execution, and the senior
// review re-enters at its persisted round. SIGINT/SIGTERM aborts resumably.
func newResumeCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "resume [run-id]",
		Short: "Resume a previous run from its last persisted state",
		Long: "Resume a previous run, continuing the pipeline from where it stopped.\n" +
			"Finished work is not redone. With no run-id (or 'latest'), the most recent\n" +
			"run is resumed. A completed or failed run has nothing to resume. Nothing is\n" +
			"ever committed; interrupting with Ctrl-C stops the run resumably.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runResume(c, opts, args)
		},
	}
}

// runResume loads the target run and re-drives it through the orchestrator. It
// first loads the run through the READ-ONLY store (no git needed) so a completed or
// failed run can be reported as "nothing to resume" even outside a git repository;
// only a genuinely resumable run opens the git gateway and builds the orchestrator.
func runResume(c *cobra.Command, opts *GlobalOptions, args []string) error {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return err
	}

	id := run.LatestSentinel
	if len(args) == 1 {
		id = args[0]
	}

	// Read-only load first: resolves the id (defaulting to latest) and lets us
	// short-circuit a finished run without requiring a git repo. Aborted runs are
	// resumable, so they are NOT short-circuited here.
	roStore, err := openStore(opts)
	if err != nil {
		return err
	}
	loaded, err := roStore.Load(id)
	if err != nil {
		return withExit(exitNotFound, err)
	}
	if loaded.Status == run.StatusCompleted || loaded.Status == run.StatusFailed {
		renderRunStatus(c.OutOrStdout(), loaded, roStore.DocsDir(loaded.ID))
		c.Printf("\nRun %s is %s; nothing to resume.\n", loaded.ID, loaded.Status)
		return nil
	}

	// Resumable: resolve the execution environment (single repo or workspace) — the
	// pipeline needs it for the baseline/diff/summary — and drive from the persisted
	// state. runsDir exclusion is applied inside resolveExecEnv.
	env, err := resolveExecEnv(opts, cfg)
	if err != nil {
		return err
	}

	// Best-effort preflight on the planner binary (skipped under --dry-run), so a
	// resume that re-enters planning fails fast with an install hint.
	if !opts.DryRun {
		if err := preflightHarness(cfg.Roles.Planner.Harness); err != nil {
			return err
		}
	}

	// Observability (AIX-0014): concise human progress to stdout, structured logs to
	// stderr + the run's logs/ dir (attached by the orchestrator).
	progress := newProgress(opts, c.OutOrStdout())
	defer progress.Close()
	logger := newLogger(opts, c.ErrOrStderr())
	defer logger.Close()

	orch, err := newOrchestrator(opts, cfg, env, progress, logger)
	if err != nil {
		return err
	}

	ctx, stop := signalContext(c.Context())
	defer stop()

	r, runErr := orch.Resume(ctx, loaded.ID)
	// Stop the live region before the summary prints so it lands on a clean line.
	progress.Close()
	return finishPipeline(c, opts, cfg, r, runErr)
}
