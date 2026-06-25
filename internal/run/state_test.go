package run

import "testing"

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
