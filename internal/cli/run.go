package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/pipeline"
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newRunCmd builds the `run` command: execute the WHOLE pipeline (plan → execute
// → review) for a task (AIX-0013). The task is resolved from a positional argument,
// --task-file/@file (AIX-0017), piped stdin, or an interactive editor (AIX-0019)
// by resolveTaskForCommand. With --dry-run it exercises the full pipeline without
// invoking real agents. SIGINT/SIGTERM are translated into a canceled context so
// the run stops gracefully and resumably (the orchestrator persists `aborted`).
func newRunCmd(opts *GlobalOptions) *cobra.Command {
	var taskFile string
	cmd := &cobra.Command{
		Use:   "run [task]",
		Short: "Run the full pipeline (plan → execute → review) for a task",
		Long: "Run the full pipeline for a task: plan it into a subtask DAG, execute the\n" +
			"subtasks with per-subtask review loops, then run a senior review over the\n" +
			"full diff. Nothing is ever committed — the working tree is left for you.\n" +
			"Interrupting with Ctrl-C stops the run resumably (resume with `aixecutor resume`).\n\n" +
			"The task may be given as an argument, read from a file with --task-file <path>\n" +
			"(or the @<path> shorthand), piped on stdin, or composed in your editor when run\n" +
			"interactively with no task and no --task-file.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			task, aborted, err := resolveTaskForCommand(c, args, taskFile)
			if err != nil || aborted {
				return err
			}
			return runRun(c, opts, task)
		},
	}
	cmd.Flags().StringVar(&taskFile, "task-file", "",
		"read the task from a file instead of a positional argument")
	return cmd
}

// runRun wires and runs the full pipeline for task. It loads config, opens the
// read-only git gateway, runs a best-effort planner-binary preflight (skipped under
// --dry-run), builds the orchestrator, installs signal handling, then Starts the
// run and prints the end-of-run summary. Abort (Ctrl-C) is surfaced as an
// actionable, non-failure message with a resume hint.
func runRun(c *cobra.Command, opts *GlobalOptions, task string) error {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return err
	}

	// Resolve the execution environment: a single git repo (default, worktree
	// isolation available) or a multi-root workspace (--workspace / a non-git dir,
	// AIX-0020). It gives the root (agents' cwd + runsDir anchor), the read-only
	// gateway, and the baseline/summary source — all read-only; runsDir excluded.
	env, err := resolveExecEnv(opts, cfg)
	if err != nil {
		return err
	}

	// Best-effort preflight: when not in dry-run, surface a missing planner binary
	// with the preset's install hint before creating a run or invoking anything.
	if !opts.DryRun {
		if err := preflightHarness(cfg.Roles.Planner.Harness); err != nil {
			return err
		}
	}

	// Observability (AIX-0014): concise human progress to stdout, structured logs to
	// stderr + the run's logs/ dir (attached by the orchestrator once the run exists).
	progress := newProgress(c.OutOrStdout())
	logger := newLogger(opts, c.ErrOrStderr())
	defer logger.Close()

	orch, err := newOrchestrator(opts, cfg, env, progress, logger)
	if err != nil {
		return err
	}

	ctx, stop := signalContext(c.Context())
	defer stop()

	r, runErr := orch.Start(ctx, task)
	return finishPipeline(c, opts, cfg, r, runErr)
}

// finishPipeline renders the end-of-run summary and maps the orchestrator's error
// onto the command's result, shared by `run` and `resume`. A nil error prints the
// full summary and succeeds. An abort (ErrAborted) prints the summary plus a clear
// "resume with …" line and returns a concise error (so the exit code is non-zero
// but the message is not a scary stack of wraps). Any other error prints the
// summary (so the user sees how far the run got) and returns the error.
//
// r may be nil if the run could not even be created (Start failed before Create);
// in that case there is no summary to print and the error is returned as-is.
func finishPipeline(c *cobra.Command, opts *GlobalOptions, cfg config.Config, r *run.Run, runErr error) error {
	if r == nil {
		return runErr
	}

	pipeline.WriteSummary(c.OutOrStdout(), r, docsDirFor(cfg, r))

	switch {
	case runErr == nil:
		return nil
	case errors.Is(runErr, pipeline.ErrPaused):
		// A clean, resumable stop for review (AIX-0016): print the next-step options.
		c.Printf("\nRun paused for review. Edit the docs in %s, then:\n", docsDirFor(cfg, r))
		printReviewOptions(c, r.ID)
		return nil
	case errors.Is(runErr, pipeline.ErrAborted):
		// Resumable, user-initiated stop. Report it plainly with the resume hint;
		// return a terse error so Execute sets a non-zero exit without dumping the
		// wrapped chain.
		c.Printf("\nRun aborted. Resume it with: aixecutor resume %s\n", r.ID)
		return errors.New("run aborted")
	default:
		return runErr
	}
}

// docsDirFor resolves a run's docs directory for the summary using the run's
// configured runs/docs layout, falling back to the repo root when the runs dir is
// relative (matching how the store resolves it). It builds a read-only store from
// config so the path matches exactly where planning wrote the docs.
func docsDirFor(cfg config.Config, r *run.Run) string {
	store, err := run.NewStoreFromConfig(cfg, repoRoot())
	if err != nil {
		return ""
	}
	return store.DocsDir(r.ID)
}

// signalContext derives a context that is canceled on SIGINT or SIGTERM, so a
// Ctrl-C (or a `kill`) during the pipeline cancels the orchestrator's context. The
// orchestrator treats that cancellation as an ABORT (persist `aborted`, resumable)
// rather than a failure. The returned stop func releases the signal handler and
// must be deferred. signal.NotifyContext restores default signal behavior after
// the first signal, so a second Ctrl-C force-quits if a phase ignores cancellation.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
