package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jaxmef/aixecutor/internal/log"
)

// This file holds the loop-control primitives shared by BOTH review loops: the
// per-subtask executor↔reviewer loop (AIX-0011, review_subtask.go) and the
// senior-review loop (AIX-0012, senior_review.go). Factoring them here keeps the
// two loops in lockstep on the three behaviours that MUST NOT diverge between
// them (CLAUDE.md §3.3): the maxLoops/`-1`-unlimited budget rule, the
// one-lenient-re-ask policy for malformed reviewer output, and how a review round
// is rendered to disk. The loops themselves stay separate (their control flow,
// diff scope, and persistence targets differ), but they call these helpers so a
// fix to, say, the re-ask policy lands in both places at once.

// reachedMaxLoops reports whether a remediation budget is exhausted. loops counts
// remediation cycles ALREADY spent; the first review is free and does not count.
// With maxLoops >= 0 the budget is exhausted once loops >= maxLoops. maxLoops ==
// -1 means unlimited, so it is never reached and the loop runs until the reviewer
// approves; any maxLoops < -1 (validation forbids it) is treated as unlimited
// rather than stopping immediately. Both review loops use this identical rule.
func reachedMaxLoops(loops, maxLoops int) bool {
	if maxLoops < 0 {
		return false
	}
	return loops >= maxLoops
}

// reviewOnceFunc runs the reviewer EXACTLY ONCE and returns its raw final text.
// It is the per-loop seam over invokeReviewerWithReask: the subtask loop renders
// the subtask-reviewer prompt for one subtask's diff; the senior loop renders the
// senior-reviewer prompt for the full diff. Either way the caller below parses the
// raw text with ParseVerdict and applies the shared re-ask policy.
type reviewOnceFunc func(ctx context.Context) (raw string, err error)

// invokeReviewerWithReask runs a reviewer and parses its verdict, applying the
// one-lenient-re-ask policy shared by both review loops (CLAUDE.md §3.3): on a
// PARSE failure it re-asks the reviewer EXACTLY ONCE (same prompt, on the theory
// the agent merely botched the trailing yaml block) and treats a second parse
// failure as a hard error. A harness/transport error is returned immediately and
// is NOT retried here. It returns the raw text of the LAST attempt (so the caller
// can persist whatever the reviewer produced, even on failure), the parsed
// verdict, and any error.
//
// label names the review target in log/error messages (e.g. `subtask "st-01"` or
// "senior review"); progress is the loop's shared Progress (so the "re-asking
// once" notice goes to the same sink); once runs the reviewer a single time.
func invokeReviewerWithReask(
	ctx context.Context,
	label string,
	progress *log.Progress,
	once reviewOnceFunc,
) (raw string, v Verdict, err error) {
	const maxAttempts = 2 // initial + one lenient re-ask on malformed output.
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err = once(ctx)
		if err != nil {
			return raw, Verdict{}, fmt.Errorf("invoking reviewer for %s: %w", label, err)
		}
		v, err = ParseVerdict(raw)
		if err == nil {
			return raw, v, nil
		}
		if attempt < maxAttempts {
			progress.Logf("%s: reviewer output was malformed (%v); re-asking once", label, err)
			continue
		}
		// Second failure: hard error, but return the raw so it is persisted.
		return raw, Verdict{}, fmt.Errorf(
			"reviewer for %s returned malformed output twice; cannot parse a verdict: %w", label, err)
	}
	// Unreachable (the loop always returns), but keep the compiler happy.
	return raw, v, err
}

// renderReviewRound formats one review round as Markdown: a heading, the parsed
// verdict summary (or the parse error), and the raw reviewer output verbatim in a
// fenced block so nothing the reviewer said is lost. Both loops use it so a
// subtask round file and a senior-review round file read identically; only the
// heading differs (the caller passes the full title, e.g. "Subtask st-01 — review
// round 2" or "Senior review — round 1").
func renderReviewRound(title, raw string, v Verdict, parseErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if parseErr != nil {
		fmt.Fprintf(&b, "**Verdict: UNPARSEABLE** — %s\n\n", parseErr)
	} else {
		approved := "not approved"
		if v.Approved {
			approved = "approved"
		}
		fmt.Fprintf(&b, "**Verdict: %s** (%d finding(s))\n\n", approved, len(v.Findings))
		for i, f := range v.Findings {
			loc := f.File
			if f.File != "" && f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(&b, "%d. [%s]", i+1, f.Severity)
			if loc != "" {
				fmt.Fprintf(&b, " `%s`", loc)
			}
			fmt.Fprintf(&b, " — %s\n", f.Message)
			if f.Suggestion != "" {
				fmt.Fprintf(&b, "   - Suggestion: %s\n", f.Suggestion)
			}
		}
		if len(v.Findings) > 0 {
			b.WriteString("\n")
		}
	}
	b.WriteString("## Raw reviewer output\n\n")
	b.WriteString("```\n")
	b.WriteString(raw)
	if !strings.HasSuffix(raw, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// renderExecutionRound formats one executor pass as Markdown, the human-readable
// sibling of renderReviewRound (execution/round-N pairs with reviews/round-N): a
// heading, the run's metadata (harness/model/permissionMode/timeout, duration,
// exit code), a relative link to that round's diff.patch, the files it touched
// (with a fallback when none), and the executor's own summary text. The summary is
// emitted UNFENCED — it is the agent's own Markdown, unlike the raw reviewer output
// renderReviewRound fences. It is pure so it is unit-testable in isolation.
func renderExecutionRound(
	subtaskID, subtaskTitle string,
	round int,
	harnessName, model, permissionMode string,
	timeout, duration time.Duration,
	exitCode int,
	files []string,
	summary string,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Subtask %s — execution round %d\n\n", subtaskID, round)
	if subtaskTitle != "" {
		fmt.Fprintf(&b, "**Title:** %s\n\n", subtaskTitle)
	}
	fmt.Fprintf(&b, "**Harness:** %s (model `%s`, permission `%s`)\n\n", harnessName, model, permissionMode)
	fmt.Fprintf(&b, "**Timeout:** %s\n\n", timeout)
	fmt.Fprintf(&b, "**Duration:** %s\n\n", duration)
	fmt.Fprintf(&b, "**Exit code:** %d\n\n", exitCode)
	b.WriteString("**Diff:** [diff.patch](../diff.patch)\n\n")

	b.WriteString("## Files changed\n\n")
	if len(files) == 0 {
		b.WriteString("_No files changed._\n")
	} else {
		for _, f := range files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}

	b.WriteString("\n## Summary\n\n")
	b.WriteString(summary)
	if !strings.HasSuffix(summary, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
