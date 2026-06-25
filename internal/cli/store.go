package cli

import (
	"os"

	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/run"
)

// openStore builds a run.Store for the inspection commands (list/status) and for
// resume's read-only pre-load (resolving the id + short-circuiting a finished run
// before any git is required).
//
// These uses are read-only with respect to runs and git: they never create a run,
// so the Store needs no Baseliner. They DO need the runs base directory, which is
// config.Paths.RunsDir resolved relative to the repository root (the default
// ".aixecutor/runs" is repo-relative). The repo root comes from the read-only git
// gateway; outside a git repository we fall back to the process working directory
// so the commands still work (they just resolve runsDir against cwd).
//
// A git-backed Baseliner is intentionally NOT attached here — Create is the only
// method that needs one. The create-capable Store (with a git baseliner) is built
// by newCreateStore (plan.go), which the orchestrator wiring uses for `run`/`resume`.
func openStore(opts *GlobalOptions) (*run.Store, error) {
	cfg, _, err := loadConfig(opts)
	if err != nil {
		return nil, err
	}
	return run.NewStoreFromConfig(cfg, repoRoot())
}

// repoRoot returns the repository root for runsDir resolution, falling back to
// the working directory when the command is not run inside a git repo. It uses
// only the read-only gateway (git rev-parse) for discovery.
func repoRoot() string {
	if gw, err := git.Open(workingDir()); err == nil {
		return gw.RepoRoot()
	}
	return workingDir()
}

// workingDir returns the process working directory, or "." if it cannot be
// determined (a degenerate case; "." keeps path resolution well-defined).
func workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
