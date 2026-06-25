package harness

import "errors"

// FailureKind classifies why a harness invocation failed, so the retry wrapper
// (retry.go, AIX-0014) can decide whether a failure is worth retrying without
// re-parsing error strings. The classification is the ONLY thing that drives
// retry; the human-facing message lives on the wrapped error.
//
// The two coarse buckets the ticket cares about are "couldn't run the agent" vs
// "agent ran but produced bad output" — both transient — versus a hard failure
// that must not be retried. Distinguishing spawn from bad-output also lets logs
// and error messages say which happened.
type FailureKind int

const (
	// FailureHard is a non-retryable failure: the invocation produced an
	// unambiguous error that retrying cannot fix (e.g. a non-zero exit that is a
	// real semantic failure, or an unexpected I/O error). It is the zero value so
	// an unclassified error is treated as hard (fail safe: never retry something we
	// don't understand).
	FailureHard FailureKind = iota
	// FailureSpawn means the process could not be started at all ("couldn't run the
	// agent" — e.g. the binary is missing or not executable). Transient: a flaky
	// exec or a just-installed binary may succeed on retry.
	FailureSpawn
	// FailureTimeout means the invocation exceeded its deadline and was killed.
	// Transient: a slow agent may finish within the limit on a retry.
	FailureTimeout
	// FailureBadOutput means the agent RAN (process exited) but produced no usable
	// result — empty output, or output that could not be parsed per the configured
	// format. Transient: agents are nondeterministic, so a re-run may produce a
	// well-formed result.
	FailureBadOutput
)

// String renders the kind for logs and messages.
func (k FailureKind) String() string {
	switch k {
	case FailureSpawn:
		return "spawn"
	case FailureTimeout:
		return "timeout"
	case FailureBadOutput:
		return "bad-output"
	default:
		return "hard"
	}
}

// Transient reports whether a failure of this kind is worth retrying. Spawn,
// timeout, and bad-output are transient; everything else (FailureHard) is not.
func (k FailureKind) Transient() bool {
	switch k {
	case FailureSpawn, FailureTimeout, FailureBadOutput:
		return true
	default:
		return false
	}
}

// RunError wraps a harness invocation error with its FailureKind so callers can
// classify it with errors.As. The underlying error carries the actionable,
// human-facing message (command, stderr tail, …); RunError only adds the
// machine-readable kind. It implements Unwrap so errors.Is/As still see the
// cause chain.
type RunError struct {
	// Kind is the failure classification driving retry decisions.
	Kind FailureKind
	// Err is the wrapped cause with the descriptive message.
	Err error
}

// Error returns the wrapped error's message verbatim, so wrapping in a RunError
// does not change what the user reads — only what the retry wrapper can detect.
func (e *RunError) Error() string {
	if e.Err == nil {
		return "harness: " + e.Kind.String() + " failure"
	}
	return e.Err.Error()
}

// Unwrap exposes the cause so errors.Is/As traverse it.
func (e *RunError) Unwrap() error { return e.Err }

// classifyKind returns the FailureKind of err by looking for a RunError in its
// chain. An error with no RunError (e.g. a context cancellation, or a foreign
// error) classifies as FailureHard, so unknown failures are never retried.
func classifyKind(err error) FailureKind {
	var re *RunError
	if errors.As(err, &re) {
		return re.Kind
	}
	return FailureHard
}

// Classify is the exported view of classifyKind, so observability code (the
// invocation logger in internal/log) can record WHY an invocation failed without
// re-parsing the message. A nil error classifies as FailureHard, but callers
// should only call this when err != nil.
func Classify(err error) FailureKind { return classifyKind(err) }

// spawnError, timeoutError, badOutputError, and hardError wrap cause with the
// matching kind. They keep the cliHarness call sites terse and self-documenting.
func spawnError(cause error) error     { return &RunError{Kind: FailureSpawn, Err: cause} }
func timeoutError(cause error) error   { return &RunError{Kind: FailureTimeout, Err: cause} }
func badOutputError(cause error) error { return &RunError{Kind: FailureBadOutput, Err: cause} }
func hardError(cause error) error      { return &RunError{Kind: FailureHard, Err: cause} }

// BadOutputError and HardError are the exported constructors presets use to
// classify their OWN result-handling failures so the retry wrapper treats them
// correctly. A preset (e.g. claude) that parses the agent's output itself — past
// the point the generic adapter returns success — wraps an "agent ran but output
// is unusable" failure with BadOutputError (transient, retried) and a deliberate
// agent error result with HardError (not retried). They mirror the unexported
// constructors above so the classification surface is the same for presets and
// the generic adapter.
func BadOutputError(cause error) error { return badOutputError(cause) }
func HardError(cause error) error      { return hardError(cause) }
