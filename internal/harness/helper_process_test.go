package harness

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: it is a fake "agent" binary that the
// exec-path tests re-exec via the standard Go pattern. When the test binary is
// run with GO_WANT_HELPER_PROCESS=1, this function takes over, performs the
// behavior requested through HELPER_MODE / env, and exits — so the cliHarness
// genuinely spawns a subprocess (exercising real stdin/file/arg delivery, env
// and workdir propagation, exit codes, and process-group kill) without ever
// invoking a real AI agent.
//
// The args after the "--" separator are the rendered command args the harness
// built, letting tests assert on argument delivery.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// From here we behave as the fake agent, then exit; never return to the
	// test runner.
	args := helperArgs()

	switch os.Getenv("HELPER_MODE") {
	case "echo-args":
		// Print the args we received (the harness's rendered argv) as JSON-free
		// text so arg-delivery can be asserted.
		fmt.Fprint(os.Stdout, strings.Join(args, "\n"))

	case "echo-stdin":
		// Copy stdin to stdout so stdin delivery can be asserted.
		data, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(os.Stdout, "STDIN:%s", string(data))

	case "cat-file":
		// args[0] is a file path (delivered via {{.PromptFile}}); print its
		// contents and the path itself so the test can assert the file exists
		// now, lives under the OS temp dir, and holds the prompt.
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cat-file: no path arg")
			os.Exit(3)
		}
		path := args[0]
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cat-file: %v", err)
			os.Exit(4)
		}
		fmt.Fprintf(os.Stdout, "FILE:%s\nPATH:%s", string(data), path)

	case "emit-json":
		// Emit a JSON document so json/resultPath parsing can be asserted.
		fmt.Fprint(os.Stdout, `{"outer":{"result":"nested-ok"},"result":"top-ok"}`)

	case "print-env":
		// Print selected env vars (named in args) as KEY=VALUE lines so env
		// propagation/override can be asserted.
		for _, k := range args {
			fmt.Fprintf(os.Stdout, "%s=%s\n", k, os.Getenv(k))
		}

	case "print-cwd":
		// Print the working directory so workdir propagation can be asserted.
		wd, _ := os.Getwd()
		fmt.Fprint(os.Stdout, wd)

	case "fail":
		// Write to stderr and exit non-zero so error/exit-code handling can be
		// asserted. The exit code comes from HELPER_EXIT (default 7).
		fmt.Fprint(os.Stderr, "boom: something went wrong")
		code := 7
		if c := os.Getenv("HELPER_EXIT"); c != "" {
			if n, err := strconv.Atoi(c); err == nil {
				code = n
			}
		}
		os.Exit(code)

	case "sleep":
		// Sleep far longer than the test's timeout so the timeout path can kill
		// us. The deadline-driven group kill is what ends this process.
		d := 30 * time.Second
		if v := os.Getenv("HELPER_SLEEP"); v != "" {
			if parsed, err := time.ParseDuration(v); err == nil {
				d = parsed
			}
		}
		time.Sleep(d)
		fmt.Fprint(os.Stdout, "slept-and-returned")

	default:
		fmt.Fprintf(os.Stderr, "unknown HELPER_MODE %q", os.Getenv("HELPER_MODE"))
		os.Exit(2)
	}
	os.Exit(0)
}

// helperArgs returns the args passed to the helper after the "--" separator
// (the harness's rendered argv). go test passes its own flags first, so we split
// on the conventional separator.
func helperArgs() []string {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// scanLines splits s into trimmed, non-empty lines for convenient assertions.
func scanLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		out = append(out, line)
	}
	return out
}
