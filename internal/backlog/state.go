package backlog

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RunnerStatus is a ticket's lifecycle in the runner's own persisted state,
// distinct from the author-declared TicketStatus in the file. It is what makes a
// multi-ticket run resumable: done tickets are never re-run.
type RunnerStatus string

const (
	// TicketReady is a ticket not yet started (or reset after an interruption).
	TicketReady RunnerStatus = "ready"
	// TicketInProgress is a ticket whose pipeline run is underway. On load it is
	// reset to ready (the interrupted run is abandoned and re-run fresh).
	TicketInProgress RunnerStatus = "in-progress"
	// TicketDone is a ticket whose run completed and passed the gate; never re-run.
	TicketDone RunnerStatus = "done"
	// TicketFailed is a ticket whose run did not complete (failed); it stops the
	// backlog and blocks its dependents.
	TicketFailed RunnerStatus = "failed"
	// TicketReview is a ticket that completed but left unresolved findings under a
	// gating mode that pauses on them; it stops the backlog pending human review.
	TicketReview RunnerStatus = "review"
)

// TicketState is the persisted per-ticket runner state.
type TicketState struct {
	ID         string       `yaml:"id"`
	Status     RunnerStatus `yaml:"status"`
	RunID      string       `yaml:"runId,omitempty"`
	Unresolved int          `yaml:"unresolved,omitempty"`
}

// State is the runner's persisted state for one backlog directory, enabling
// resume across invocations. It is written after every ticket transition.
type State struct {
	// Dir is the backlog directory this state tracks, so a stale state for a
	// different directory can be detected.
	Dir string `yaml:"dir"`
	// Tickets maps ticket id to its runner state.
	Tickets map[string]*TicketState `yaml:"tickets"`
}

// LoadState reads runner state from path. A missing file yields a fresh, empty
// state (the first run). Any InProgress ticket is reset to Ready, since its run
// was interrupted and will be re-run from scratch.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{Tickets: map[string]*TicketState{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read backlog state %q: %w", path, err)
	}

	var s State
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("cannot parse backlog state %q: %w", path, err)
	}
	if s.Tickets == nil {
		s.Tickets = map[string]*TicketState{}
	}
	for _, ts := range s.Tickets {
		if ts.Status == TicketInProgress {
			ts.Status = TicketReady
		}
	}
	return &s, nil
}

// SaveState writes s to path atomically (temp file + rename), creating the parent
// directory if needed.
func SaveState(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create backlog state dir: %w", err)
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("cannot encode backlog state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("cannot write backlog state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cannot finalize backlog state: %w", err)
	}
	return nil
}

// get returns the (possibly newly-created) runner state for a ticket id.
func (s *State) get(id string) *TicketState {
	if ts, ok := s.Tickets[id]; ok {
		return ts
	}
	ts := &TicketState{ID: id, Status: TicketReady}
	s.Tickets[id] = ts
	return ts
}

// isDone reports whether the runner has recorded this ticket as done.
func (s *State) isDone(id string) bool {
	ts, ok := s.Tickets[id]
	return ok && ts.Status == TicketDone
}
