package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jaxmef/aixecutor/internal/run"
	"github.com/spf13/cobra"
)

func newListCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List previous runs (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			summaries, err := store.List()
			if err != nil {
				return err
			}
			renderRunList(c.OutOrStdout(), summaries)
			return nil
		},
	}
}

// taskExcerptLen bounds the task column so the table stays readable; longer tasks
// are truncated with an ellipsis (the full task is in task.md / `status`).
const taskExcerptLen = 60

// renderRunList writes a newest-first table of runs to w, or a clear "no runs"
// line when there are none. The columns are: RUN ID, STATUS, CREATED, TASK.
func renderRunList(w io.Writer, summaries []run.RunSummary) {
	if len(summaries) == 0 {
		fmt.Fprintln(w, "No runs found. Start one with: aixecutor run \"<task>\"")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tSTATUS\tCREATED\tTASK")
	for _, s := range summaries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			s.ID,
			s.Status,
			formatTime(s.CreatedAt),
			excerpt(s.Task, taskExcerptLen),
		)
	}
	_ = tw.Flush()
}

// formatTime renders a timestamp for the run tables. Zero times (absent in
// older/foreign run.yaml) render as "-" rather than the Go zero date.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// excerpt collapses a task to a single line bounded at n runes, appending an
// ellipsis when truncated, so multi-line or long task descriptions fit one row.
func excerpt(s string, n int) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			s = s[:i]
			break
		}
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
