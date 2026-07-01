package cli

import (
	"fmt"

	claudeharness "github.com/jaxmef/aixecutor/internal/harness/claude"
	piharness "github.com/jaxmef/aixecutor/internal/harness/pi"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/pipeline"
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newPlanCmd builds the `plan` command: run only the planning phase for a task
// (AIX-0009). It creates a run, invokes the planner role, writes the docs +
// subtask DAG, and stops — execution is AIX-0010/0013. The task is resolved from a
// positional argument, --task-file/@file (AIX-0017), piped stdin, or an interactive
// editor (AIX-0019) by resolveTaskForCommand.
func newPlanCmd(opts *GlobalOptions) *cobra.Command {
	var taskFile string
	cmd := &cobra.Command{
		Use:   "plan [task]",
		Short: "Run only the planning phase for a task",
		Long: "Run only the planning phase for a task. The task may be given as an argument,\n" +
			"read from a file with --task-file <path> (or the @<path> shorthand), piped on\n" +
			"stdin, or composed in your editor when run interactively with no task.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			task, aborted, err := resolveTaskForCommand(c, args, taskFile)
			if err != nil || aborted {
				return err
			}
			return runPlan(c, opts, task)
		},
	}
	cmd.Flags().StringVar(&taskFile, "task-file", "",
		"read the task from a file instead of a positional argument")
	return cmd
}

// runPlan wires and runs the planning phase for task. It loads config, opens the
// read-only git gateway, creates a run (with a git baseline), builds the harness
// registry (claude + pi presets, honoring --dry-run) and prompt renderer, resolves
// the planner role's harness, runs a best-effort binary preflight, and invokes the
// Planner. It prints the docs path and stops after planning.
func runPlan(c *cobra.Command, opts *GlobalOptions, task string) error {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return err
	}

	// Resolve the execution environment (single repo or workspace, AIX-0020): it
	// gives the root (planner's working dir + runsDir anchor), the baseline source,
	// and the file lister for the repo summary. runsDir exclusion is applied inside.
	env, err := resolveExecEnv(opts, cfg)
	if err != nil {
		return err
	}

	store, err := newCreateStoreEnv(cfg, env)
	if err != nil {
		return err
	}

	// Observability (AIX-0014): human progress to stdout, structured logs to stderr
	// + the run's logs/ dir (attached once the run exists). The logger is handed to
	// the registry so the retry wrapper logs attempts/backoff too.
	progress := newProgress(opts, c.OutOrStdout())
	defer progress.Close()
	logger := newLogger(opts, c.ErrOrStderr())
	defer logger.Close()

	registry, err := newRegistry(cfg, opts.DryRun, logger)
	if err != nil {
		return err
	}

	plannerRole := cfg.Roles.Planner
	h, ok := registry.Get(plannerRole.Harness)
	if !ok {
		return fmt.Errorf("planner role references unknown harness %q (configured harnesses: %v)",
			plannerRole.Harness, registry.Names())
	}

	// Best-effort preflight: when not in dry-run, surface a missing planner binary
	// with the preset's actionable install hint before creating a run or invoking.
	if !opts.DryRun {
		if err := preflightHarness(plannerRole.Harness); err != nil {
			return err
		}
	}

	renderer, err := newRenderer(opts)
	if err != nil {
		return err
	}

	r, err := store.Create(task, cfg)
	if err != nil {
		return err
	}

	// Attach the run log file now that the run dir exists, and wrap the planner
	// harness so its single invocation is logged and its raw output persisted, just
	// like an orchestrated run.
	if err := logger.AttachRunFile(logsDirFor(store, r.ID)); err != nil {
		logger.Warn("could not attach run log file; continuing console-only", "error", err.Error())
	}
	h = log.WrapHarness(h, logger)

	summarizer := pipeline.NewGitRepoSummarizer(env.source, env.root)
	planner := pipeline.NewPlanner(h, renderer, store, summarizer, plannerRole, env.root,
		pipeline.WithDryRun(opts.DryRun),
		pipeline.WithProgress(progress),
	)

	return planner.Plan(c.Context(), r)
}

// logsDirFor returns the run's logs directory through the store layout, so the
// `plan` command attaches its log file exactly where the orchestrated runs do.
func logsDirFor(store *run.Store, id string) string {
	return run.Layout{RunsDir: store.RunsDir(), ID: id}.LogsDir()
}

// preflightHarness runs the PATH preflight for the known presets so a missing
// binary fails fast with an install hint. Unknown harness names (generic CLI
// adapters) have no preset preflight, so they are not checked here — their first
// invocation surfaces the exec error. Returns nil when the binary is present or no
// preset preflight applies; a missing binary is classified exitMissingBinary so
// the process exits with the stable "missing harness" code and the preset's
// actionable install hint.
func preflightHarness(name string) error {
	switch name {
	case claudeharness.Name:
		return withExit(exitMissingBinary, claudeharness.Available())
	case piharness.Name:
		return withExit(exitMissingBinary, piharness.Available())
	default:
		return nil
	}
}
