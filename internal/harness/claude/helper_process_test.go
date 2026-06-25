package claude

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test: it is a fake "claude" binary that the
// exec-path tests re-exec via the standard Go pattern. When invoked with
// GO_WANT_HELPER_PROCESS=1 it takes over, emits a canned JSON envelope to stdout
// (and optionally records its argv to HELPER_ARGS_FILE so arg propagation can be
// asserted), then exits — so the preset genuinely spawns a subprocess without
// ever invoking the real claude CLI.
//
// HELPER_MODE selects the behavior:
//   - claude-success: emit a success envelope (is_error:false) with a result.
//   - claude-error:   emit an error envelope (is_error:true) on a clean exit.
//   - claude-fail:    write to stderr and exit non-zero (adapter-level failure).
//
// The canned envelopes mirror the recorded testdata samples (same result text),
// keeping the fixtures and the integration fake consistent.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Record the rendered argv (everything after the "--" separator) so tests can
	// assert that --model / --permission-mode were templated through.
	if f := os.Getenv("HELPER_ARGS_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(helperArgs(), "\n")), 0o644)
	}

	switch os.Getenv("HELPER_MODE") {
	case "claude-success":
		fmt.Fprint(os.Stdout, successEnvelope)
	case "claude-error":
		fmt.Fprint(os.Stdout, errorEnvelope)
	case "claude-fail":
		fmt.Fprint(os.Stderr, "boom: claude failed to start")
		os.Exit(11)
	default:
		fmt.Fprintf(os.Stderr, "unknown HELPER_MODE %q", os.Getenv("HELPER_MODE"))
		os.Exit(2)
	}
	os.Exit(0)
}

// helperArgs returns the args passed to the helper after the "--" separator (the
// preset's rendered argv); go test passes its own flags first, so we split on
// the conventional separator.
func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

// successEnvelope mirrors testdata/harness/claude/success.json (same result
// text), inlined so the fake subprocess needs no file access.
const successEnvelope = `{"type":"result","subtype":"success","is_error":false,` +
	`"result":"Implemented the feature and all tests pass.",` +
	`"session_id":"9f1c2b7a-4d3e-4a21-8b6c-1e2f3a4b5c6d",` +
	`"total_cost_usd":0.0421,"duration_ms":12873,"num_turns":4,` +
	`"usage":{"input_tokens":1843,"output_tokens":217}}`

// errorEnvelope mirrors testdata/harness/claude/error.json (is_error:true on a
// clean exit), so the preset must convert it into a Go error.
const errorEnvelope = `{"type":"result","subtype":"error_during_execution","is_error":true,` +
	`"result":"Execution failed: the model could not complete the request.",` +
	`"session_id":"deadbeef-0000-1111-2222-333344445555","duration_ms":5310,"num_turns":2}`
