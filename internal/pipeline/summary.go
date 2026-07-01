package pipeline

import (
	"fmt"
	"io"
	"strings"

	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/run"
)

// WriteSummary renders the end-of-run summary for r to w. It is the single,
// reusable report both `run` and `resume` print when the orchestrator returns, and
// is derived ENTIRELY from the persisted run state (statuses, loop counts,
// SeniorReview.Status/Unresolved), so it reads the same whether the run just
// completed or was loaded by resume. docsDir is the run's docs directory (the
// store knows it) so the summary can point the user at the plan/context/manual
// docs.
//
// The summary is plain-terminal readable (aligned columns kept simple) and ALWAYS
// ends with the explicit reminder that nothing was committed — the working tree is
// the user's to inspect and commit (CLAUDE.md §2 invariant 1 / §3.3 "Never commit").
// When color is true the status word, senior verdict, and per-subtask statuses are
// coloured via log.Colorize; the reminder text is never coloured.
func WriteSummary(w io.Writer, r *run.Run, docsDir string, color bool) {
	if w == nil || r == nil {
		return
	}

	fmt.Fprintf(w, "\n==================== Run summary ====================\n")
	fmt.Fprintf(w, "Run:    %s\n", r.ID)
	fmt.Fprintf(w, "Status: %s\n", log.Colorize(color, runStatusColor(r.Status), string(r.Status)))
	fmt.Fprintf(w, "Elapsed: %s\n", run.FormatElapsed(r.CreatedAt, r.UpdatedAt))

	writeSubtaskOutcomes(w, r, color)
	writeSeniorVerdict(w, r, color)

	if docsDir != "" {
		fmt.Fprintf(w, "\nDocs:   %s\n", docsDir)
	}

	// The load-bearing, always-present reminder. Phrased so it is unmissable on a
	// plain terminal and unambiguous about who owns the next step.
	fmt.Fprintf(w, "\nNOTE: Nothing was committed. aixecutor never runs git write commands —\n")
	fmt.Fprintf(w, "      the working tree holds all changes and is yours to review and commit.\n")
	fmt.Fprintf(w, "====================================================\n")
}

// writeSubtaskOutcomes prints the per-subtask outcome table: id, status, the
// executor↔reviewer loop count, and a count of any unresolved findings carried
// forward from a subtask whose review did not converge. An empty subtask list (a
// run that stopped at planning, or a dry-run before planning) is reported plainly.
func writeSubtaskOutcomes(w io.Writer, r *run.Run, color bool) {
	done, total := r.CountSubtasks()
	fmt.Fprintf(w, "\nSubtasks (%d/%d done):\n", done, total)
	if total == 0 {
		fmt.Fprintf(w, "  (none — planning produced no subtasks)\n")
		return
	}
	for i := range r.Subtasks {
		st := &r.Subtasks[i]
		status := log.Colorize(color, subtaskStatusColor(st.Status), fmt.Sprintf("%-12s", st.Status))
		line := fmt.Sprintf("  - %-8s %s loops=%d", st.ID, status, st.Loops)
		if n := len(st.Unresolved); n > 0 {
			line += fmt.Sprintf("  (%d unresolved finding(s) carried forward)", n)
		}
		if st.Title != "" {
			line += "  " + st.Title
		}
		fmt.Fprintln(w, line)
	}
}

// writeSeniorVerdict prints the senior-review outcome as state, not a filename: it
// distinguishes disabled, skipped, an unfinished phase, a CLEAN convergence
// (Status=done, no unresolved findings), and a report-and-proceed at the cap
// (Status=done WITH unresolved findings). When findings remain, it lists them so
// the user sees exactly what is open without opening unresolved.md.
func writeSeniorVerdict(w io.Writer, r *run.Run, color bool) {
	sr := r.SeniorReview
	fmt.Fprintf(w, "\nSenior review: ")
	switch {
	case !sr.Enabled:
		fmt.Fprintf(w, "disabled\n")
		return
	case sr.Status == run.SeniorReviewSkipped:
		fmt.Fprintf(w, "skipped\n")
		return
	case sr.Status == run.SeniorReviewPending:
		fmt.Fprintf(w, "not started\n")
		return
	case sr.Status == run.SeniorReviewRunning:
		fmt.Fprintf(w, "in progress (%d round(s) so far)\n", sr.Rounds)
		return
	}

	// Status is done. Clean vs. cap-reached is read off Unresolved.
	if len(sr.Unresolved) == 0 {
		fmt.Fprintf(w, "%s after %d remediation round(s)\n",
			log.Colorize(color, log.AnsiGreen, "clean"), sr.Rounds)
		return
	}
	fmt.Fprintf(w, "%s with %d unresolved finding(s) after %d round(s) (cap reached):\n",
		log.Colorize(color, log.AnsiYellow, "completed"), len(sr.Unresolved), sr.Rounds)
	for i, f := range sr.Unresolved {
		fmt.Fprintf(w, "    %d. %s\n", i+1, formatFinding(f))
	}
}

// runStatusColor maps a run status to its SGR code: green for a clean completion,
// red for the terminal failure off-ramps, yellow for everything in between.
func runStatusColor(s run.Status) string {
	switch s {
	case run.StatusCompleted:
		return log.AnsiGreen
	case run.StatusFailed, run.StatusAborted:
		return log.AnsiRed
	default:
		return log.AnsiYellow
	}
}

// subtaskStatusColor maps a subtask status to its SGR code: green when done, red on
// failure/blocked, yellow while in flight or pending.
func subtaskStatusColor(s run.SubtaskStatus) string {
	switch s {
	case run.SubtaskDone:
		return log.AnsiGreen
	case run.SubtaskFailed, run.SubtaskBlocked:
		return log.AnsiRed
	default:
		return log.AnsiYellow
	}
}

// formatFinding renders one persisted finding as a compact one-liner for the
// summary: "[severity] file:line — message". Empty location/line fields are
// elided so a finding not tied to a file still reads well.
func formatFinding(f run.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", f.Severity)
	loc := f.File
	if f.File != "" && f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc != "" {
		fmt.Fprintf(&b, " %s", loc)
	}
	fmt.Fprintf(&b, " — %s", f.Message)
	return b.String()
}
