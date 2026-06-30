package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// fakeFetcher is a hermetic stand-in for the GitHub fetcher: it never touches
// the network. block (if non-nil) is awaited before returning, modelling a
// hanging GitHub.
type fakeFetcher struct {
	latest string
	err    error
	block  <-chan struct{}
}

func (f fakeFetcher) LatestVersion(ctx context.Context) (string, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.latest, f.err
}

// updateTestEnv installs the package seams for a hermetic update check and
// restores them on cleanup. cur is the reported current version; fetch is the
// fetcher used by the check goroutine.
func updateTestEnv(t *testing.T, cur string, fetch releaseFetcher) {
	t.Helper()

	origCur, origFetch, origNow, origCache, origAfter := currentVersion, newFetcher, nowFn, updateCachePath, afterUpdateCheck
	t.Cleanup(func() {
		currentVersion, newFetcher, nowFn, updateCachePath, afterUpdateCheck = origCur, origFetch, origNow, origCache, origAfter
	})

	cache := filepath.Join(t.TempDir(), ".update-check")
	currentVersion = func() string { return cur }
	newFetcher = func(string) releaseFetcher { return fetch }
	nowFn = func() time.Time { return time.Unix(0, 0) }
	updateCachePath = func() string { return cache }
}

// runUpdateCmd runs the `version` subcommand through the full Execute-style flow
// (installUpdateCheck → root.Execute → printUpdateNotice) in a hermetic temp dir,
// capturing whatever printUpdateNotice writes to os.Stderr. waitForCheck, when
// true, blocks until the check goroutine completes before reading the notice.
func runUpdateCmd(t *testing.T, opts *GlobalOptions, args []string, waitForCheck bool) string {
	t.Helper()
	t.Chdir(t.TempDir()) // non-repo dir → config falls back to compiled defaults.

	var wg sync.WaitGroup
	if waitForCheck {
		wg.Add(1)
		orig := afterUpdateCheck
		afterUpdateCheck = func() { orig(); wg.Done() }
	}

	root := newRootCmd(opts)
	notice := installUpdateCheck(root, opts)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	root.SetArgs(append([]string{"--config", missing, "--global-config", missing}, append(args, "version")...))

	stderr := captureStderr(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("version command returned error: %v", err)
		}
		if waitForCheck {
			wg.Wait()
		}
		printUpdateNotice(notice)
	})
	return stderr
}

// captureStderr swaps os.Stderr for a pipe around fn and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return buf.String()
}

func TestUpdateNoticePrintedForStaleVersion(t *testing.T) {
	updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})

	got := runUpdateCmd(t, &GlobalOptions{}, nil, true)

	if c := strings.Count(got, "newer aixecutor is available"); c != 1 {
		t.Fatalf("expected exactly one update notice, got %d in %q", c, got)
	}
	if !strings.Contains(got, "v2.0.0") || !strings.Contains(got, "v1.0.0") {
		t.Errorf("notice missing versions: %q", got)
	}
}

func TestUpdateNoticeSuppressedForEqualOrOlder(t *testing.T) {
	updateTestEnv(t, "v2.0.0", fakeFetcher{latest: "v2.0.0"})

	if got := runUpdateCmd(t, &GlobalOptions{}, nil, true); got != "" {
		t.Errorf("equal version should print no notice, got %q", got)
	}
}

func TestUpdateCheckOptOuts(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})
		t.Setenv("AIXECUTOR_NO_UPDATE_CHECK", "1")
		// Suppressed before the goroutine launches, so don't wait on the check.
		if got := runUpdateCmd(t, &GlobalOptions{}, nil, false); got != "" {
			t.Errorf("env opt-out should suppress notice, got %q", got)
		}
	})

	t.Run("flag", func(t *testing.T) {
		updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})
		if got := runUpdateCmd(t, &GlobalOptions{}, []string{"--no-update-check"}, false); got != "" {
			t.Errorf("--no-update-check should suppress notice, got %q", got)
		}
	})

	t.Run("dry-run", func(t *testing.T) {
		updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})
		if got := runUpdateCmd(t, &GlobalOptions{}, []string{"--dry-run"}, false); got != "" {
			t.Errorf("--dry-run should suppress notice, got %q", got)
		}
	})

	t.Run("quiet", func(t *testing.T) {
		updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})
		// The goroutine runs but must not emit while quiet, so wait for it.
		if got := runUpdateCmd(t, &GlobalOptions{}, []string{"--quiet"}, true); got != "" {
			t.Errorf("--quiet should suppress notice, got %q", got)
		}
	})

	t.Run("dev-version", func(t *testing.T) {
		updateTestEnv(t, "", fakeFetcher{latest: "v2.0.0"})
		if got := runUpdateCmd(t, &GlobalOptions{}, nil, false); got != "" {
			t.Errorf("dev version should suppress notice, got %q", got)
		}
	})

	t.Run("config-check-false", func(t *testing.T) {
		updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0"})
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(cfgPath, []byte("version: 1\nupdate:\n  check: false\n"), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		opts := &GlobalOptions{}
		t.Chdir(t.TempDir())
		root := newRootCmd(opts)
		notice := installUpdateCheck(root, opts)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		missing := filepath.Join(t.TempDir(), "nope.yaml")
		root.SetArgs([]string{"--config", cfgPath, "--global-config", missing, "version"})
		got := captureStderr(t, func() {
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			printUpdateNotice(notice)
		})
		if got != "" {
			t.Errorf("update.check:false should suppress notice, got %q", got)
		}
	})
}

func TestUpdateCheckBlockingFetcherDoesNotHang(t *testing.T) {
	t.Chdir(t.TempDir())
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the leaked check goroutine.
	updateTestEnv(t, "v1.0.0", fakeFetcher{latest: "v2.0.0", block: block})

	opts := &GlobalOptions{}
	root := newRootCmd(opts)
	notice := installUpdateCheck(root, opts)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	root.SetArgs([]string{"--config", missing, "--global-config", missing, "version"})

	// The check goroutine blocks on the fetcher, but Execute must not: run it
	// with a watchdog and assert it returns promptly with the notice dropped.
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command hung waiting on a blocking fetcher")
	}

	if got := captureStderr(t, func() { printUpdateNotice(notice) }); got != "" {
		t.Errorf("blocking fetcher should drop the notice, got %q", got)
	}
}

func TestUpdateCheckFetchErrorIsSilent(t *testing.T) {
	updateTestEnv(t, "v1.0.0", fakeFetcher{err: context.DeadlineExceeded})

	if got := runUpdateCmd(t, &GlobalOptions{}, nil, true); got != "" {
		t.Errorf("fetch error should surface nothing, got %q", got)
	}
}

// Ensure PersistentPreRunE is installed by Execute's wiring, not newRootCmd.
func TestInstallUpdateCheckSetsPreRun(t *testing.T) {
	root := &cobra.Command{Use: "x"}
	if root.PersistentPreRunE != nil {
		t.Fatal("precondition: PersistentPreRunE already set")
	}
	installUpdateCheck(root, &GlobalOptions{})
	if root.PersistentPreRunE == nil {
		t.Error("installUpdateCheck did not set PersistentPreRunE")
	}
}
