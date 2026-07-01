package cli

import (
	"path/filepath"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/harness"
	claudeharness "github.com/jaxmef/aixecutor/internal/harness/claude"
	piharness "github.com/jaxmef/aixecutor/internal/harness/pi"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/pipeline"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// promptOverrideSubdir is the directory name (a sibling of the config file inside
// .aixecutor/) where users drop prompt-template overrides, per CLAUDE.md §4.5 and
// the internal/prompt package doc: <repo>/.aixecutor/prompts (local) and
// ~/.aixecutor/prompts (global).
const promptOverrideSubdir = "prompts"

// presetFactories returns the harness factories the CLI wires into the registry:
// the claude and pi presets, merged. A name present here is built by its preset;
// every other harness in config falls back to the generic CLI adapter. This is
// the single, import-cycle-safe construction point for the presets (the registry
// in internal/harness must not import the preset packages, so the wiring is
// supplied here — AIX-0004/0005 left this as the loose end AIX-0009 closes).
func presetFactories() map[string]harness.Factory {
	factories := make(map[string]harness.Factory)
	for name, f := range claudeharness.Factories() {
		factories[name] = f
	}
	for name, f := range piharness.Factories() {
		factories[name] = f
	}
	return factories
}

// newRegistry builds the harness registry from the resolved config, wiring the
// claude + pi presets and honoring the global --dry-run flag (which wraps every
// built harness so no real command runs). logger (may be nil) is handed to the
// registry so the retry wrapper logs each attempt + backoff; it is the same
// *log.Logger the orchestrator later attaches to the run's logs/ dir, so retry
// lines land in the durable log too.
func newRegistry(cfg config.Config, dryRun bool, logger *log.Logger) (*harness.Registry, error) {
	return harness.NewRegistry(cfg, harness.Options{
		Factories: presetFactories(),
		DryRun:    dryRun,
		Logger:    logger,
	})
}

// newRenderer builds a prompt renderer whose override directories are derived from
// the same config-file locations the loader resolves, so a user's
// <repo>/.aixecutor/prompts and ~/.aixecutor/prompts override the embedded
// defaults (local first, then global — the config layering order). Locations
// honors --config / --global-config, so an explicit config path moves the prompts
// dir alongside it. Empty/missing dirs are tolerated by the renderer.
func newRenderer(opts *GlobalOptions) (*prompt.Renderer, error) {
	locs, err := loadOptionsFromGlobals(opts).Locations()
	if err != nil {
		return nil, err
	}
	// Locations is ordered global, then local. The renderer wants local first.
	var globalDir, localDir string
	for _, l := range locs {
		dir := promptDirFor(l.Path)
		switch l.Origin {
		case config.OriginGlobal:
			globalDir = dir
		case config.OriginLocal:
			localDir = dir
		}
	}
	return prompt.NewRenderer(localDir, globalDir), nil
}

// promptDirFor maps a config file path (<...>/.aixecutor/config.yaml) to the
// sibling prompts override directory (<...>/.aixecutor/prompts). It returns "" for
// an empty path so the renderer simply skips that tier.
func promptDirFor(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), promptOverrideSubdir)
}

// configureGitExclusions tells the shared git gateway to skip the tool's own
// output directory (the configured paths.runsDir) when capturing the run-start
// baseline and when computing the senior-review full diff. Without this, a project
// that has NOT gitignored .aixecutor/runs would have the tool snapshot its own
// run artifacts into each run's .baseline (latent recursion + bloat) and surface
// them in the senior-review diff. The exclusion is derived from the existing
// paths.runsDir (no new config knob) and is repo-relative; a runsDir outside the
// repo yields no exclusion (run.RepoRelRunsDir returns "").
//
// It is set on the ONE gateway the command opens, which backs BOTH the baseliner
// (run.GitBaseliner) and the pipeline's FullDiff adapter, so both honor the same
// exclusion and the full diff stays symmetric (neither side captures runsDir).
func configureGitExclusions(cfg config.Config, gw *git.Gateway) {
	if rel := run.RepoRelRunsDir(cfg, gw.RepoRoot()); rel != "" {
		gw.SetExcludePrefixes(rel)
	}
	gw.SetExcludeNames(cfg.Ignore...)
}

// newOrchestrator wires a pipeline.Orchestrator end-to-end from the resolved
// config and an open read-only git gateway, sharing one construction point for
// both `run` and `resume`. It builds the harness registry (claude + pi presets,
// honoring --dry-run), the prompt renderer (with override dirs), a create-capable
// run Store backed by a git baseliner, and the git repo summarizer — then assembles
// the orchestrator, threading the human-facing Progress and the structured Logger
// in (AIX-0014: the orchestrator attaches the logger to the run's logs/ dir and
// wraps the registry so every harness invocation is logged + its raw output
// persisted). The gateway is the caller's (opened once, reused for the baseline,
// summary, diffs, and worktrees); it is the ONLY git access the pipeline uses
// (invariant #1).
func newOrchestrator(opts *GlobalOptions, cfg config.Config, env execEnv, progress *log.Progress, logger *log.Logger) (*pipeline.Orchestrator, error) {
	store, err := newCreateStoreEnv(cfg, env)
	if err != nil {
		return nil, err
	}
	registry, err := newRegistry(cfg, opts.DryRun, logger)
	if err != nil {
		return nil, err
	}
	renderer, err := newRenderer(opts)
	if err != nil {
		return nil, err
	}
	summarizer := pipeline.NewGitRepoSummarizer(env.source, env.root)

	return pipeline.NewOrchestrator(
		store,
		cfg,
		registry,
		env.gateway,
		renderer,
		summarizer,
		pipeline.WithOrchestratorDryRun(opts.DryRun),
		pipeline.WithOrchestratorProgress(progress),
		pipeline.WithOrchestratorLogger(logger),
	)
}
