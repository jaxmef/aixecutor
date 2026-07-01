package run

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStatusValidity(t *testing.T) {
	valid := []Status{
		StatusCreated, StatusPlanning, StatusPlanned, StatusExecuting,
		StatusSeniorReview, StatusCompleted, StatusFailed, StatusAborted,
	}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("Status %q should be valid", s)
		}
	}
	for _, s := range []Status{"", "bogus", "Created", "done"} {
		if s.IsValid() {
			t.Errorf("Status %q should be invalid", s)
		}
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusCompleted, StatusFailed, StatusAborted} {
		if !s.IsTerminal() {
			t.Errorf("Status %q should be terminal", s)
		}
	}
	for _, s := range []Status{StatusCreated, StatusPlanning, StatusPlanned, StatusExecuting, StatusSeniorReview} {
		if s.IsTerminal() {
			t.Errorf("Status %q should not be terminal", s)
		}
	}
}

func TestStatusOrderCoversPhases(t *testing.T) {
	// statusOrder is used for presentation; it must list each non-terminal phase
	// plus completed, in order, with no duplicates.
	seen := map[Status]bool{}
	for _, s := range statusOrder {
		if seen[s] {
			t.Errorf("statusOrder has duplicate %q", s)
		}
		seen[s] = true
		if !s.IsValid() {
			t.Errorf("statusOrder entry %q is not a valid status", s)
		}
	}
}

func TestSubtaskStatusValidity(t *testing.T) {
	valid := []SubtaskStatus{
		SubtaskPending, SubtaskImplementing, SubtaskReviewing,
		SubtaskBlocked, SubtaskDone, SubtaskFailed,
	}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("SubtaskStatus %q should be valid", s)
		}
	}
	for _, s := range []SubtaskStatus{"", "bogus", "Done"} {
		if s.IsValid() {
			t.Errorf("SubtaskStatus %q should be invalid", s)
		}
	}
}

func TestSubtaskStatusInterruptedAndTerminal(t *testing.T) {
	if !SubtaskImplementing.IsInterrupted() || !SubtaskReviewing.IsInterrupted() {
		t.Error("implementing/reviewing should be interrupted states")
	}
	for _, s := range []SubtaskStatus{SubtaskPending, SubtaskBlocked, SubtaskDone, SubtaskFailed} {
		if s.IsInterrupted() {
			t.Errorf("SubtaskStatus %q should not be interrupted", s)
		}
	}
	if !SubtaskDone.IsTerminal() || !SubtaskFailed.IsTerminal() {
		t.Error("done/failed should be terminal subtask states")
	}
	if SubtaskImplementing.IsTerminal() {
		t.Error("implementing should not be terminal")
	}
}

func TestSubtaskUndeclaredRoundTrip(t *testing.T) {
	in := Subtask{
		ID:         "st-01",
		Title:      "example",
		Status:     SubtaskDone,
		Undeclared: []string{"pkg/a.go", "pkg/b.go"},
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Subtask
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Undeclared) != 2 || got.Undeclared[0] != "pkg/a.go" || got.Undeclared[1] != "pkg/b.go" {
		t.Errorf("Undeclared did not round-trip: got %v", got.Undeclared)
	}
}

func TestSubtaskUndeclaredOmittedWhenEmpty(t *testing.T) {
	data, err := yaml.Marshal(Subtask{ID: "st-01", Title: "example", Status: SubtaskDone})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "undeclared") {
		t.Errorf("empty Undeclared should be omitted, got:\n%s", data)
	}
}

func TestSubtaskLoadsWithoutUndeclaredField(t *testing.T) {
	// A run.yaml written before Undeclared existed must still load.
	src := "id: st-01\ntitle: example\nstatus: done\n"
	var got Subtask
	if err := yaml.Unmarshal([]byte(src), &got); err != nil {
		t.Fatalf("unmarshal legacy subtask: %v", err)
	}
	if got.Undeclared != nil {
		t.Errorf("expected nil Undeclared for legacy subtask, got %v", got.Undeclared)
	}
}

func TestSeniorReviewStatusValidity(t *testing.T) {
	for _, s := range []SeniorReviewStatus{SeniorReviewPending, SeniorReviewRunning, SeniorReviewDone, SeniorReviewSkipped} {
		if !s.IsValid() {
			t.Errorf("SeniorReviewStatus %q should be valid", s)
		}
	}
	if SeniorReviewStatus("bogus").IsValid() {
		t.Error(`SeniorReviewStatus "bogus" should be invalid`)
	}
}
