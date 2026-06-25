package pi

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test: it is a fake "pi" binary that the
// exec-path tests re-exec via the standard Go pattern. When invoked with
// GO_WANT_HELPER_PROCESS=1 it takes over, emits canned text to stdout (and
// optionally records its argv to HELPER_ARGS_FILE so arg propagation can be
// asserted), then exits — so the preset genuinely spawns a subprocess without
// ever invoking the real pi CLI.
//
// HELPER_MODE selects the behavior:
//   - pi-success: emit the canned text result (with a trailing newline, so the
//     wrapper's trailing-whitespace trim is exercised) and exit 0.
//   - pi-fail:    write to stderr and exit non-zero (adapter-level failure).
//
// The canned text mirrors the recorded testdata sample (same result text),
// keeping the fixture and the integration fake consistent.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Record the rendered argv (everything after the "--" separator) so tests can
	// assert that --model and the positional {{.Prompt}} were templated through.
	if f := os.Getenv("HELPER_ARGS_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(helperArgs(), "\n")), 0o644)
	}

	switch os.Getenv("HELPER_MODE") {
	case "pi-success":
		// Trailing newline on purpose: the preset must trim it from Result.Text
		// while leaving Result.Raw intact.
		fmt.Fprint(os.Stdout, successOutput+"\n")
	case "pi-fail":
		fmt.Fprint(os.Stderr, "boom: pi failed to start")
		os.Exit(7)
	default:
		fmt.Fprintf(os.Stderr, "unknown HELPER_MODE %q", os.Getenv("HELPER_MODE"))
		os.Exit(2)
	}
	os.Exit(0)
}

// helperArgs returns the args passed to the helper after the "--" separator (the
// preset's rendered argv); go test passes its own flags first, so we split on the
// conventional separator.
func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

// successOutput mirrors testdata/harness/pi/success.txt (same result text, minus
// the trailing newline the fixture carries), inlined so the fake subprocess needs
// no file access.
const successOutput = "Implemented the feature and all tests pass."
