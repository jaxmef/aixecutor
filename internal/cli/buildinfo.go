package cli

import "runtime/debug"

// resolvedBuildInfo returns the version, commit, and build date to display.
//
// Precedence: ldflag-injected values (set by `make build`, see version.go) win.
// For a plain `go install`/`go build` — where the ldflags are absent and the
// vars keep their dev defaults — it falls back to the metadata the Go toolchain
// embeds in the binary via runtime/debug.ReadBuildInfo:
//
//   - `go install …@v1.2.3` (or `@latest` resolving to a tag) → Main.Version is
//     the exact tag, e.g. "v1.2.3".
//   - `go install …@latest` on an untagged commit → Main.Version is Go's
//     pseudo-version, e.g. "v1.2.3-0.20260630025458-c32bae7f1234" (base tag +
//     timestamp + short commit), which already encodes "latest tag + hash".
//   - `go build` / `go install .` from a source checkout → Main.Version is
//     "(devel)" (ignored), but the vcs.* settings give the real commit + dirty
//     flag + build time.
func resolvedBuildInfo() (version, commit, date string) {
	version, commit, date = ldflagVersion, ldflagCommit, ldflagDate

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}

	if !versionInjected(version) {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}

	var revision, vcsTime string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			vcsTime = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if !commitInjected(commit) && revision != "" {
		short := revision
		if len(short) > 7 {
			short = short[:7]
		}
		if dirty {
			short += "-dirty"
		}
		commit = short
	}
	if !dateInjected(date) && vcsTime != "" {
		date = vcsTime
	}

	return version, commit, date
}

// resolvedVersion is the version string alone, or "" when nothing usable is
// available (the update check treats "" as "skip", and the version command
// falls back to the literal "dev").
func resolvedVersion() string {
	v, _, _ := resolvedBuildInfo()
	if !versionInjected(v) {
		return ""
	}
	return v
}

func versionInjected(v string) bool {
	switch v {
	case "", "dev", "none", "unknown":
		return false
	default:
		return true
	}
}

func commitInjected(c string) bool { return c != "" && c != "none" }
func dateInjected(d string) bool   { return d != "" && d != "unknown" }
