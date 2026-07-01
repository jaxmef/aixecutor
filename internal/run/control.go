package run

import (
	"fmt"
	"os"
)

// The control channel (AIX-0016) is a tiny file-based signalling mechanism: a
// second `aixecutor` invocation (e.g. `review`) writes a marker under the run's
// .control/ dir, and the process currently executing the run polls for it at a safe
// subtask boundary. aixecutor is a CLI, not a daemon, so a file is the simplest
// cross-process channel; it lives in the run dir so it is per-run and resumable.

// RequestPause writes the pause-request marker for a run, asking a currently-running
// execution to pause at its next subtask boundary. It is idempotent (a repeated
// request is a harmless rewrite) and creates the control dir as needed.
func (s *Store) RequestPause(id string) error {
	l := s.layoutFor(id)
	if err := os.MkdirAll(l.ControlDir(), dirPerm); err != nil {
		return fmt.Errorf("run control: creating control dir: %w", err)
	}
	if err := os.WriteFile(l.PauseRequestFile(), []byte("pause\n"), filePerm); err != nil {
		return fmt.Errorf("run control: writing pause request: %w", err)
	}
	return nil
}

// PauseRequested reports whether a pause has been requested for a run (the marker
// exists). Any stat error other than "not found" is treated as "not requested" so a
// transient FS error never wedges execution.
func (s *Store) PauseRequested(id string) bool {
	_, err := os.Stat(s.layoutFor(id).PauseRequestFile())
	return err == nil
}

// ClearPause removes the pause-request marker (acknowledging it). A missing marker
// is not an error, so clearing is safe to call unconditionally on resume/amend.
func (s *Store) ClearPause(id string) error {
	if err := os.Remove(s.layoutFor(id).PauseRequestFile()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("run control: clearing pause request: %w", err)
	}
	return nil
}

// RequestStop writes the stop-request marker for a run, asking a currently-running
// execution to stop as soon as it reaches a safe point. It is idempotent (a repeated
// request is a harmless rewrite) and creates the control dir as needed.
func (s *Store) RequestStop(id string) error {
	l := s.layoutFor(id)
	if err := os.MkdirAll(l.ControlDir(), dirPerm); err != nil {
		return fmt.Errorf("run control: creating control dir: %w", err)
	}
	if err := os.WriteFile(l.StopRequestFile(), []byte("stop\n"), filePerm); err != nil {
		return fmt.Errorf("run control: writing stop request: %w", err)
	}
	return nil
}

// StopRequested reports whether a stop has been requested for a run (the marker
// exists). Any stat error other than "not found" is treated as "not requested" so a
// transient FS error never wedges execution.
func (s *Store) StopRequested(id string) bool {
	_, err := os.Stat(s.layoutFor(id).StopRequestFile())
	return err == nil
}

// ClearStop removes the stop-request marker (acknowledging it). A missing marker
// is not an error, so clearing is safe to call unconditionally.
func (s *Store) ClearStop(id string) error {
	if err := os.Remove(s.layoutFor(id).StopRequestFile()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("run control: clearing stop request: %w", err)
	}
	return nil
}
