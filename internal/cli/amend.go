package cli

import (
	"fmt"

	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newAmendCmd builds the `amend` command: the revert-and-restart side of the
// AIX-0016 review checkpoint. From a PAUSED run, after the user has edited the
// planning docs, it reverts the working tree to the exact pre-execution state
// (Option B baseline restore — no mutating git; pre-existing uncommitted changes
// preserved; the amended docs kept) and restarts execution from the amended plan.
func newAmendCmd(opts *GlobalOptions) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "amend [run-id]",
		Short: "Revert a paused run and restart execution from the amended plan",
		Long: "Revert a paused run's execution back to the pre-execution state and restart\n" +
			"from the amended docs/subtasks.yaml. This DISCARDS the changes execution made\n" +
			"so far (your pre-run uncommitted changes and your doc edits are preserved) and\n" +
			"re-runs the subtasks from a clean baseline. The run must be paused first with\n" +
			"`aixecutor review <id>`. Requires --confirm because it rewrites the working\n" +
			"tree. Nothing is committed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runAmend(c, opts, idArg(args), confirm)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"confirm the revert + restart (required; it rewrites the working tree)")
	return cmd
}

// runAmend previews or performs the amend. It resolves the run read-only first
// (validating it is paused and gating on --confirm before any destructive action),
// then opens the git gateway and drives the orchestrator's Amend lifecycle.
func runAmend(c *cobra.Command, opts *GlobalOptions, id string, confirm bool) error {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return err
	}

	roStore, err := openStore(opts)
	if err != nil {
		return err
	}
	loaded, err := roStore.Load(id)
	if err != nil {
		return withExit(exitNotFound, err)
	}
	if loaded.Status != run.StatusPaused {
		return withExit(exitUsage, fmt.Errorf(
			"run %s is %s, not paused; pause it with `aixecutor review %s` before amending",
			loaded.ID, loaded.Status, loaded.ID))
	}

	if !confirm {
		c.Printf("amend will REVERT run %s to the pre-execution state (undoing execution's\n", loaded.ID)
		c.Printf("changes; your pre-run uncommitted work and doc edits are kept) and restart\n")
		c.Printf("execution from the amended plan in %s.\n", roStore.DocsDir(loaded.ID))
		c.Printf("Re-run to proceed: aixecutor amend %s --confirm\n", loaded.ID)
		return nil
	}

	// Confirmed: resolve the execution environment (single repo or workspace — the
	// revert + restart need it) and drive the amend lifecycle through the orchestrator.
	env, err := resolveExecEnv(opts, cfg)
	if err != nil {
		return err
	}

	if !opts.DryRun {
		if err := preflightHarness(cfg.Roles.Executor.Harness); err != nil {
			return err
		}
	}

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

	r, amendErr := orch.Amend(ctx, loaded.ID)
	// Stop the live region before the summary prints so it lands on a clean line.
	progress.Close()
	return finishPipeline(c, opts, cfg, r, amendErr)
}
