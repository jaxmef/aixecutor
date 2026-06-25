package cli

import (
	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

// newReviewCmd builds the `review` command (alias `pause`): the interactive review
// checkpoint request side of AIX-0016. It asks a currently-running execution to
// pause at the next safe subtask boundary so the user can read (and optionally
// amend) the planning docs, then continue (`resume`) or revert+restart (`amend`).
func newReviewCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "review [run-id]",
		Aliases: []string{"pause"},
		Short:   "Pause a running run at a safe boundary to review/amend the plan",
		Long: "Request a pause-to-review of a run. A currently-running execution stops at\n" +
			"the next subtask boundary (run.yaml left consistent). Then read the docs and\n" +
			"either continue with `aixecutor resume <id>` or revert and restart from the\n" +
			"amended plan with `aixecutor amend <id> --confirm`. With no run-id, the most\n" +
			"recent run is targeted. Nothing is committed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runReview(c, opts, idArg(args))
		},
	}
}

// runReview resolves the target run (read-only; no git needed) and, unless it is
// terminal, writes a pause request to its control channel. A run already paused is
// reported with the same options. The pause is honored by the process running the
// execution; this command only signals it.
func runReview(c *cobra.Command, opts *GlobalOptions, id string) error {
	store, err := openStore(opts)
	if err != nil {
		return err
	}
	r, err := store.Load(id)
	if err != nil {
		return withExit(exitNotFound, err)
	}

	docsDir := store.DocsDir(r.ID)
	switch {
	case r.Status.IsTerminal():
		c.Printf("Run %s is %s — nothing to review.\n", r.ID, r.Status)
		return nil
	case r.Status == run.StatusPaused:
		c.Printf("Run %s is already paused for review. Edit the docs in %s, then:\n", r.ID, docsDir)
		printReviewOptions(c, r.ID)
		return nil
	default:
		if err := store.RequestPause(r.ID); err != nil {
			return err
		}
		if r.Status == run.StatusExecuting {
			c.Printf("Pause requested for run %s — it will stop at the next subtask boundary.\n", r.ID)
		} else {
			// created/planning/planned: nothing is executing yet, so the pause takes
			// effect at the first subtask boundary once execution starts.
			c.Printf("Pause requested for run %s (%s) — it will take effect at the first subtask boundary when execution starts.\n", r.ID, r.Status)
		}
		c.Printf("Then edit the docs in %s and choose:\n", docsDir)
		printReviewOptions(c, r.ID)
		return nil
	}
}

// idArg returns the optional positional run-id, defaulting to the latest sentinel
// when absent — the shared "[run-id]" resolution used by review/amend/resume.
func idArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return run.LatestSentinel
}

// printReviewOptions prints the two ways out of a paused run: continue or amend.
func printReviewOptions(c *cobra.Command, id string) {
	c.Printf("  aixecutor resume %s            # clarify only: continue from where it paused\n", id)
	c.Printf("  aixecutor amend %s --confirm   # amend: revert execution and restart from the amended plan\n", id)
}
