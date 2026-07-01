package pipeline

import (
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/run"
)

func sampleRun() *run.Run {
	return &run.Run{
		ID:     "run-1",
		Status: run.StatusCompleted,
		Subtasks: []run.Subtask{
			{ID: "st-01", Title: "schema", Status: run.SubtaskDone, Loops: 1},
		},
		SeniorReview: run.SeniorReview{Enabled: true, Status: run.SeniorReviewDone, Rounds: 1},
	}
}

// TestWriteSummaryColorDisabled proves the plain summary carries no ANSI escapes and
// keeps the committed-nothing reminder byte-identical.
func TestWriteSummaryColorDisabled(t *testing.T) {
	var buf strings.Builder
	WriteSummary(&buf, sampleRun(), "/runs/run-1/docs", false)
	out := buf.String()

	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain summary must contain no ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, "Status: completed") {
		t.Errorf("plain summary missing status word:\n%s", out)
	}
	const reminder = "NOTE: Nothing was committed. aixecutor never runs git write commands —"
	if !strings.Contains(out, reminder) {
		t.Errorf("summary missing the committed-nothing reminder:\n%s", out)
	}
}

// TestWriteSummaryColorEnabled proves colour wraps the status word, the senior
// verdict and the subtask status, while the reminder text stays uncoloured.
func TestWriteSummaryColorEnabled(t *testing.T) {
	var buf strings.Builder
	WriteSummary(&buf, sampleRun(), "/runs/run-1/docs", true)
	out := buf.String()

	if !strings.Contains(out, "Status: "+log.AnsiGreen+"completed"+log.AnsiReset) {
		t.Errorf("coloured summary missing coloured status word:\n%q", out)
	}
	if !strings.Contains(out, log.AnsiGreen+"clean"+log.AnsiReset) {
		t.Errorf("coloured summary missing coloured senior verdict:\n%q", out)
	}
	if !strings.Contains(out, log.AnsiGreen+"done") {
		t.Errorf("coloured summary missing coloured subtask status:\n%q", out)
	}
	// The reminder is never coloured.
	const reminder = "NOTE: Nothing was committed. aixecutor never runs git write commands —"
	if !strings.Contains(out, reminder) {
		t.Errorf("coloured summary altered the committed-nothing reminder:\n%s", out)
	}
}
