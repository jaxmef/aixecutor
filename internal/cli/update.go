package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaxmef/aixecutor/internal/update"
	"github.com/spf13/cobra"
)

// releaseFetcher is the narrow capability the update check needs; it mirrors the
// update package's own interface so tests can inject a fake without the network.
type releaseFetcher interface {
	LatestVersion(ctx context.Context) (string, error)
}

// Package-level seams kept injectable so hermetic tests can substitute a fast or
// blocking fake fetcher, a fixed clock, and a temp cache path — never the network.
var (
	currentVersion = resolveCurrentVersion
	newFetcher     = func(version string) releaseFetcher {
		return &update.GitHubReleaseFetcher{
			Repo:      "jaxmef/aixecutor",
			UserAgent: "aixecutor/" + version,
		}
	}
	nowFn           = time.Now
	updateCachePath = defaultUpdateCachePath
	// afterUpdateCheck fires when the check goroutine finishes; a no-op in
	// production, overridden by tests to synchronize on the goroutine.
	afterUpdateCheck = func() {}
)

// resolveCurrentVersion returns the version to compare against the latest
// release: the ldflag-injected version, else the module version the Go toolchain
// embeds in the binary (see resolvedBuildInfo). An unusable value yields "" — the
// caller treats that as "skip", so dev/source builds never nag.
func resolveCurrentVersion() string {
	return resolvedVersion()
}

func defaultUpdateCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aixecutor", ".update-check")
}

// installUpdateCheck wires a PersistentPreRunE that kicks off the update check
// and returns the channel the resulting notice (if any) is delivered on. The
// channel is buffered (cap 1) so the writer goroutine never blocks even if the
// command exits before the fetch completes.
func installUpdateCheck(root *cobra.Command, opts *GlobalOptions) <-chan string {
	notice := make(chan string, 1)

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if updateCheckSuppressed(opts) {
			return nil
		}

		cur := currentVersion()
		if cur == "" {
			return nil
		}

		if cfg, _, err := loadConfig(opts); err == nil && !cfg.Update.Check {
			return nil
		} else if err == nil {
			go runUpdateCheck(cmd.Context(), opts, cur, cfg.Update.Interval.Std(), notice)
			return nil
		}

		// Config load failed; the check is best-effort, so proceed with defaults
		// rather than failing the command.
		go runUpdateCheck(cmd.Context(), opts, cur, 0, notice)
		return nil
	}

	return notice
}

func updateCheckSuppressed(opts *GlobalOptions) bool {
	if isTruthyEnv(os.Getenv("AIXECUTOR_NO_UPDATE_CHECK")) {
		return true
	}
	return opts.NoUpdateCheck || opts.DryRun
}

func runUpdateCheck(ctx context.Context, opts *GlobalOptions, current string, interval time.Duration, notice chan<- string) {
	defer afterUpdateCheck()
	checker := &update.Checker{
		Current:   current,
		Fetcher:   newFetcher(current),
		CachePath: updateCachePath(),
		Interval:  interval,
		Now:       nowFn,
	}
	latest, newer := checker.Check(ctx)
	if newer && !opts.Quiet {
		select {
		case notice <- update.Notice(current, latest):
		default:
		}
	}
}

// printUpdateNotice prints a ready notice to stderr without ever blocking: if no
// notice is waiting (e.g. the fetch is still in flight or was suppressed), it
// returns immediately and the notice is simply dropped.
func printUpdateNotice(ch <-chan string) {
	select {
	case msg := <-ch:
		if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	default:
	}
}

func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
