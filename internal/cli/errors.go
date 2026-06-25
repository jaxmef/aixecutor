package cli

import "errors"

// Exit codes (AIX-0014 error UX). They are stable and documented so scripts can
// branch on the failure class, and so the common failures each map to a
// consistent, non-zero code rather than a catch-all 1. The numbers are small and
// grouped by kind; 1 stays the generic fallback for anything unclassified.
const (
	// exitOK is success.
	exitOK = 0
	// exitGeneric is any error not mapped to a more specific code below.
	exitGeneric = 1
	// exitUsage is a CLI usage error (bad flags/args). Cobra-detected usage errors
	// also funnel here.
	exitUsage = 2
	// exitConfig is an invalid or unparseable configuration (a bad config file or a
	// failed Validate()).
	exitConfig = 3
	// exitMissingBinary is a required harness CLI not found on PATH (the preflight
	// failed). The message names the tool and how to install it.
	exitMissingBinary = 4
	// exitNotFound is an addressed resource that does not exist — most commonly an
	// unknown run id (or no runs at all).
	exitNotFound = 5
)

// exitError wraps an error with the process exit code Execute should use. It keeps
// the actionable message on the wrapped error while carrying the machine-readable
// code, so the two concerns stay separate. errors.Is/As traverse the cause.
type exitError struct {
	code int
	err  error
}

// Error returns the wrapped message verbatim.
func (e *exitError) Error() string { return e.err.Error() }

// Unwrap exposes the cause for errors.Is/As.
func (e *exitError) Unwrap() error { return e.err }

// ExitCode reports the process exit code for this error.
func (e *exitError) ExitCode() int { return e.code }

// withExit wraps err with an exit code, unless err is nil (then it returns nil) or
// already carries an exit code (then it is returned unchanged, so the FIRST/most
// specific classification wins as the error propagates up).
func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	var ec *exitError
	if errors.As(err, &ec) {
		return err
	}
	return &exitError{code: code, err: err}
}

// exitCodeFor extracts the exit code an error asks for, defaulting to exitGeneric
// for an unclassified (but non-nil) error and exitOK for nil.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ec *exitError
	if errors.As(err, &ec) {
		return ec.code
	}
	return exitGeneric
}
