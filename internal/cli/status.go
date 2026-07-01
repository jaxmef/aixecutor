package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

func newStatusCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status [run-id]",
		Short: "Show a run's phase, per-subtask progress, and senior-review status",
		Long: "Show the current state of a run: its pipeline phase, elapsed time, each\n" +
			"subtask's status and review-loop count, the senior-review status (clean or\n" +
			"unresolved findings), and where the planning docs live. With no run-id (or\n" +
			"'latest'), the most recent run is shown.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			id := run.LatestSentinel
			if len(args) == 1 {
				id = args[0]
			}
			r, err := store.Load(id)
			if err != nil {
				// An unknown id / no-runs is a "not found" failure, with the store's
				// already-actionable message.
				return withExit(exitNotFound, err)
			}
			renderRunStatus(c.OutOrStdout(), r, store.DocsDir(r.ID))
			return nil
		},
	}
}

// renderRunStatus writes a human-readable status report for a single run: a
// header (id / status / task / timestamps / elapsed / baseline), the subtask
// table with loop counts, the senior-review line (clean vs N unresolved), any
// unresolved findings, and the docs path. docsDir is the run's planning docs
// directory (resolved by the store), so the user can open the plan/context docs.
func renderRunStatus(w io.Writer, r *run.Run, docsDir string) {
	fmt.Fprintf(w, "Run:     %s\n", r.ID)
	fmt.Fprintf(w, "Status:  %s\n", r.Status)
	fmt.Fprintf(w, "Task:    %s\n", excerpt(r.Task, taskExcerptLen))
	fmt.Fprintf(w, "Created: %s\n", formatTime(r.CreatedAt))
	fmt.Fprintf(w, "Updated: %s\n", formatTime(r.UpdatedAt))
	fmt.Fprintf(w, "Elapsed: %s\n", run.FormatElapsed(r.CreatedAt, r.UpdatedAt))
	if r.Baseline.Dir != "" {
		fmt.Fprintf(w, "Baseline: %s (%d files)\n", r.Baseline.Dir, r.Baseline.Files)
	}
	if docsDir != "" {
		fmt.Fprintf(w, "Docs:    %s\n", docsDir)
	}

	done, total := r.CountSubtasks()
	fmt.Fprintf(w, "\nSubtasks (%d/%d done):\n", done, total)
	if total == 0 {
		fmt.Fprintln(w, "  (none yet — planning has not produced subtasks)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tSTATUS\tLOOPS\tUNRESOLVED\tTITLE")
		for i := range r.Subtasks {
			st := &r.Subtasks[i]
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\n",
				st.ID, st.Status, st.Loops, len(st.Unresolved), excerpt(st.Title, taskExcerptLen))
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(w, "\nSenior review: %s\n", seniorReviewLine(r.SeniorReview))
	writeUnresolvedFindings(w, r)
}

// seniorReviewLine renders the senior-review phase as a one-line summary,
// distinguishing disabled from the various phase states, showing the round count,
// and — when done — whether the change is clean or has N unresolved findings (read
// from SeniorReview.Unresolved, the structured cap-reached signal).
func seniorReviewLine(sr run.SeniorReview) string {
	if !sr.Enabled {
		return "disabled"
	}
	switch sr.Status {
	case run.SeniorReviewRunning:
		return fmt.Sprintf("%s (%d rounds)", sr.Status, sr.Rounds)
	case run.SeniorReviewDone:
		if n := len(sr.Unresolved); n > 0 {
			return fmt.Sprintf("done — %d unresolved finding(s) after %d round(s) (cap reached)", n, sr.Rounds)
		}
		return fmt.Sprintf("done — clean after %d round(s)", sr.Rounds)
	default:
		return sr.Status.String()
	}
}

// writeUnresolvedFindings lists every open finding the run carries — from any
// subtask whose review loop ended flagged AND from the senior review at the cap —
// so `status` shows exactly what is still open without the user opening the
// artifact files. Nothing is printed when the run is clean.
func writeUnresolvedFindings(w io.Writer, r *run.Run) {
	type located struct {
		where string
		f     run.Finding
	}
	var all []located
	for i := range r.Subtasks {
		st := &r.Subtasks[i]
		for _, f := range st.Unresolved {
			all = append(all, located{where: "subtask " + st.ID, f: f})
		}
	}
	for _, f := range r.SeniorReview.Unresolved {
		all = append(all, located{where: "senior review", f: f})
	}
	if len(all) == 0 {
		return
	}
	fmt.Fprintf(w, "\nUnresolved findings (%d):\n", len(all))
	for i, it := range all {
		fmt.Fprintf(w, "  %d. (%s) %s\n", i+1, it.where, formatStatusFinding(it.f))
	}
}

// formatStatusFinding renders one finding compactly for `status`:
// "[severity] file:line — message". Empty location/line are elided.
func formatStatusFinding(f run.Finding) string {
	loc := f.File
	if f.File != "" && f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc != "" {
		return fmt.Sprintf("[%s] %s — %s", f.Severity, loc, f.Message)
	}
	return fmt.Sprintf("[%s] %s", f.Severity, f.Message)
}
