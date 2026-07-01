package cli

import (
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newStopCmd builds the `stop` command (alias `abort`): the immediate-stop request
// side of the run control channel. Unlike `review` (which pauses at the next subtask
// boundary), `stop` asks a currently-running execution to cancel its in-flight work
// at once. The run is left in a resumable `aborted` state.
func newStopCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "stop [run-id]",
		Aliases: []string{"abort"},
		Short:   "Stop a running run at the next safe point (cancels in-flight work)",
		Long: "Request an immediate stop of a run. A currently-running execution cancels its\n" +
			"in-flight work at the next safe point and persists an `aborted` state, leaving\n" +
			"the interrupted subtask re-runnable. Continue where it left off with\n" +
			"`aixecutor resume <id>`. With no run-id, the most recent run is targeted.\n" +
			"Nothing is committed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runStop(c, opts, idArg(args))
		},
	}
}

// runStop resolves the target run (read-only; no git needed) and, unless it is
// terminal, writes a stop request to its control channel. The stop is honored by the
// process running the execution (a watcher cancels the run context); this command
// only signals it.
func runStop(c *cobra.Command, opts *GlobalOptions, id string) error {
	store, err := openStore(opts)
	if err != nil {
		return err
	}
	r, err := store.Load(id)
	if err != nil {
		return withExit(exitNotFound, err)
	}

	if r.Status.IsTerminal() {
		c.Printf("Run %s is %s — nothing to stop.\n", r.ID, r.Status)
		return nil
	}
	if err := store.RequestStop(r.ID); err != nil {
		return err
	}
	if r.Status == run.StatusExecuting {
		c.Printf("Stop requested for run %s — cancelling in-flight work; it will stop shortly.\n", r.ID)
	} else {
		// created/planning/planned: nothing is executing yet, so the stop takes effect
		// as soon as execution starts.
		c.Printf("Stop requested for run %s (%s) — it will take effect once execution starts.\n", r.ID, r.Status)
	}
	c.Printf("  aixecutor resume %s   # continue from where it stopped\n", r.ID)
	return nil
}
