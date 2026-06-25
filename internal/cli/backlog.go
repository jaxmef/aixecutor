package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jaxmef/aixecutor/internal/backlog"
	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// backlogStateFile is the runner-state filename under the runs directory. It tracks
// per-ticket lifecycle for the active backlog so a multi-ticket run is resumable.
const backlogStateFile = "backlog-state.yaml"

// newBacklogCmd builds the `backlog` command group (AIX-0018). Its `run`
// subcommand discovers a directory of ticket files, resolves their dependency DAG,
// and drives each ready ticket end-to-end through the pipeline, gating advancement
// on the run's structured outcome. It never commits — each ticket leaves its
// changes in the working tree for the user.
func newBacklogCmd(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backlog",
		Short: "Drive a backlog of ticket files through the pipeline",
	}
	cmd.AddCommand(newBacklogRunCmd(opts))
	return cmd
}

// newBacklogRunCmd builds `backlog run [dir]`. The directory comes from the
// argument or backlog.dir in config; --gate overrides backlog.gate.
func newBacklogRunCmd(opts *GlobalOptions) *cobra.Command {
	var gate string
	cmd := &cobra.Command{
		Use:   "run [dir]",
		Short: "Run the next ready ticket(s) from a backlog directory",
		Long: "Discover ticket files (Markdown with `id`/`dependsOn`/`status` frontmatter)\n" +
			"in a backlog directory, resolve their dependency DAG, and run the next ready\n" +
			"ticket end-to-end through the plan → execute → review pipeline. Advancement is\n" +
			"gated: 'manual' (default) runs one ticket then pauses; 'stop-on-finding' advances\n" +
			"through clean reviews but stops on unresolved findings; 'auto' runs unattended.\n" +
			"The run is resumable — already-done tickets are not re-run. Nothing is committed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var dirArg string
			if len(args) == 1 {
				dirArg = args[0]
			}
			return runBacklog(c, opts, dirArg, gate)
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "",
		"gating mode: manual | stop-on-finding | auto (overrides backlog.gate)")
	return cmd
}

// runBacklog wires and drives the backlog runner: it resolves the directory and
// gate, opens the read-only git gateway, builds the orchestrator (reused per
// ticket), discovers + validates the ticket DAG, loads resumable runner state, and
// runs until the backlog pauses, stops on a gate, is aborted, or is exhausted.
func runBacklog(c *cobra.Command, opts *GlobalOptions, dirArg, gateFlag string) error {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return err
	}

	dir, err := resolveBacklogDir(dirArg, cfg)
	if err != nil {
		return err
	}
	gate, err := resolveGate(gateFlag, cfg)
	if err != nil {
		return err
	}

	// Resolve the execution environment (single repo or workspace) once and reuse it
	// for every ticket's run — it gives the root, the read-only gateway, and the
	// baseline source; runsDir exclusion is applied inside.
	env, err := resolveExecEnv(opts, cfg)
	if err != nil {
		return err
	}

	if !opts.DryRun {
		if err := preflightHarness(cfg.Roles.Planner.Harness); err != nil {
			return err
		}
	}

	tickets, err := backlog.Discover(dir)
	if err != nil {
		return withExit(exitUsage, err)
	}
	graph, err := backlog.BuildGraph(tickets)
	if err != nil {
		return withExit(exitConfig, err)
	}

	statePath, err := backlogStatePath(cfg)
	if err != nil {
		return err
	}
	state, err := loadBacklogState(statePath, dir)
	if err != nil {
		return err
	}

	progress := newProgress(c.OutOrStdout())
	logger := newLogger(opts, c.ErrOrStderr())
	defer logger.Close()

	orch, err := newOrchestrator(opts, cfg, env, progress, logger)
	if err != nil {
		return err
	}

	ctx, stop := signalContext(c.Context())
	defer stop()

	ticketRun := func(ctx context.Context, t backlog.Ticket) (backlog.Outcome, error) {
		r, runErr := orch.Start(ctx, t.Task)
		out := backlog.Outcome{}
		if r != nil {
			out.RunID = r.ID
		}
		if runErr != nil {
			return out, runErr
		}
		out.Completed = r.Status == run.StatusCompleted
		out.Unresolved = len(r.SeniorReview.Unresolved)
		out.Clean = out.Completed && out.Unresolved == 0
		return out, nil
	}

	runner := backlog.NewRunner(graph, state, statePath, gate, ticketRun, c.OutOrStdout())
	sum, runErr := runner.Run(ctx)
	return finishBacklog(c, dir, sum, runErr)
}

// resolveBacklogDir picks the backlog directory from the argument or config and
// requires one to be set.
func resolveBacklogDir(dirArg string, cfg config.Config) (string, error) {
	dir := dirArg
	if dir == "" {
		dir = cfg.Backlog.Dir
	}
	if dir == "" {
		return "", withExit(exitUsage, errors.New(
			"no backlog directory given: pass it as an argument or set backlog.dir in config"))
	}
	return dir, nil
}

// resolveGate picks the gating mode from the --gate flag or config and validates it.
func resolveGate(gateFlag string, cfg config.Config) (backlog.GateMode, error) {
	g := gateFlag
	if g == "" {
		g = cfg.Backlog.Gate
	}
	gate := backlog.GateMode(g)
	if !backlog.ValidGate(gate) {
		return "", withExit(exitUsage, fmt.Errorf(
			"invalid gate %q: must be %q, %q, or %q",
			g, backlog.GateManual, backlog.GateStopOnFinding, backlog.GateAuto))
	}
	return gate, nil
}

// backlogStatePath resolves where the runner state file lives: under the configured
// runs directory (anchored at the repo root), alongside the run artifacts.
func backlogStatePath(cfg config.Config) (string, error) {
	store, err := run.NewStoreFromConfig(cfg, repoRoot())
	if err != nil {
		return "", err
	}
	return filepath.Join(store.RunsDir(), backlogStateFile), nil
}

// loadBacklogState loads the resumable runner state and guards against reusing a
// state file that tracks a different backlog directory (which would mix unrelated
// progress). The directory is compared by absolute path.
func loadBacklogState(statePath, dir string) (*backlog.State, error) {
	state, err := backlog.LoadState(statePath)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if state.Dir != "" && state.Dir != abs {
		return nil, withExit(exitUsage, fmt.Errorf(
			"backlog state at %q tracks a different directory (%q); finish that backlog or remove the state file to start %q",
			statePath, state.Dir, abs))
	}
	state.Dir = abs
	return state, nil
}

// finishBacklog renders the end-of-run summary and maps the runner's error onto the
// command result. An abort is reported with a resume hint and a terse non-failure
// error (non-zero exit, no scary wrap); a ticket failure is returned as-is; a clean
// stop (pause / needs-review / exhausted) succeeds with an informative message.
func finishBacklog(c *cobra.Command, dir string, sum backlog.Summary, runErr error) error {
	if len(sum.Completed) > 0 {
		c.Printf("\nCompleted this run: %v\n", sum.Completed)
	}

	switch {
	case errors.Is(runErr, backlog.ErrAborted):
		c.Printf("Backlog run aborted. Resume it with: aixecutor backlog run %s\n", dir)
		return errors.New("backlog run aborted")
	case runErr != nil:
		// A ticket failed; its dependents did not run. Report what remains.
		if len(sum.Remaining) > 0 {
			c.Printf("Remaining (not run): %v\n", sum.Remaining)
		}
		return runErr
	}

	switch {
	case sum.NeedsReview != "":
		c.Printf("Stopped: ticket %s completed with unresolved findings — review the working tree, then re-run.\n", sum.NeedsReview)
		if len(sum.Remaining) > 0 {
			c.Printf("Remaining: %v\n", sum.Remaining)
		}
	case sum.Exhausted:
		c.Printf("Backlog complete — all tickets are done. The working tree holds the changes (nothing was committed).\n")
	case sum.Paused:
		c.Printf("Paused (manual gate). Remaining: %v — re-run `aixecutor backlog run %s` to continue.\n", sum.Remaining, dir)
	default:
		// No ready ticket and not exhausted: the rest is blocked by an unfinished
		// (failed/parked) dependency.
		c.Printf("No ready tickets. Blocked/remaining: %v\n", sum.Remaining)
	}
	return nil
}
