package run

import (
	"path/filepath"
	"testing"
)

func TestStopRequestRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const id = "run-1"

	if s.StopRequested(id) {
		t.Fatal("stop should not be requested before RequestStop")
	}
	if err := s.RequestStop(id); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	if !s.StopRequested(id) {
		t.Fatal("stop should be requested after RequestStop")
	}
	want := filepath.Join(s.layoutFor(id).ControlDir(), StopRequestFileName)
	if got := s.layoutFor(id).StopRequestFile(); got != want {
		t.Fatalf("StopRequestFile() = %q, want %q", got, want)
	}
	if err := s.ClearStop(id); err != nil {
		t.Fatalf("ClearStop: %v", err)
	}
	if s.StopRequested(id) {
		t.Fatal("stop should not be requested after ClearStop")
	}
	if err := s.ClearStop(id); err != nil {
		t.Fatalf("ClearStop on missing marker should be nil: %v", err)
	}
}

func TestStopAndPauseAreIndependent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const id = "run-1"

	if err := s.RequestStop(id); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	if s.PauseRequested(id) {
		t.Fatal("requesting stop must not set the pause marker")
	}

	if err := s.RequestPause(id); err != nil {
		t.Fatalf("RequestPause: %v", err)
	}
	if err := s.ClearStop(id); err != nil {
		t.Fatalf("ClearStop: %v", err)
	}
	if !s.PauseRequested(id) {
		t.Fatal("clearing stop must not clear the pause marker")
	}
	if s.StopRequested(id) {
		t.Fatal("stop should be cleared")
	}
}
