package harness

import (
	"fmt"
	"sort"

	"github.com/jaxmef/aixecutor/internal/config"
)

// Factory builds a Harness from a single config.Harness entry. It is the
// extension point for agent-specific presets (AIX-0004 claude, AIX-0005 pi):
// a preset can register a Factory for its harness name to plug in behavior that
// the generic adapter cannot express, while everything else stays config-only.
type Factory func(name string, cfg config.Harness) (Harness, error)

// Options parameterizes registry construction.
type Options struct {
	// DryRun wraps every built harness in the dry-run wrapper, so no real
	// command is ever executed (honors the global --dry-run flag).
	DryRun bool
	// Factories overrides how a named harness is built. A name present here is
	// constructed by its Factory instead of the generic CLI adapter; absent
	// names fall back to newCLIHarness. This is the preset hook for later
	// tickets. Leave nil to build everything generically.
	Factories map[string]Factory
	// Logger, when set, is used by the dry-run wrapper to log intended
	// invocations. nil is fine — the wrapper degrades to no logging.
	Logger Logger
}

// Logger is the minimal logging surface the harness package needs. The real
// logger lands in AIX-0014 (internal/log); accepting an interface here avoids a
// dependency on it now while letting callers inject one. A nil Logger is safe.
type Logger interface {
	// Infof logs an informational, human-facing line.
	Infof(format string, args ...any)
}

// Registry resolves a harness by name for a role. It is built once from the
// resolved config and then queried during the run.
type Registry struct {
	harnesses map[string]Harness
}

// NewRegistry builds a Harness for every entry in cfg.Harnesses and returns a
// registry keyed by name. Each harness is built by its Options.Factories entry
// if present, otherwise by the generic CLI adapter. A construction error (bad
// template, unsupported type, …) is returned with the harness name for context.
//
// Wrapping order (AIX-0014):
//   - Normal: retry(real harness). The retry wrapper retries only TRANSIENT
//     failures per the harness's retry policy (config.Harness.Retry), logging each
//     attempt + backoff through opts.Logger.
//   - DryRun: dryRun(real harness), and NO retry. A dry run returns a deterministic
//     placeholder with no error, so there is nothing to retry; wrapping it in
//     retry would be pointless, so the dry-run wrapper replaces retry entirely.
func NewRegistry(cfg config.Config, opts Options) (*Registry, error) {
	r := &Registry{harnesses: make(map[string]Harness, len(cfg.Harnesses))}
	// Build in sorted order so any error is deterministic across runs.
	for _, name := range sortedKeys(cfg.Harnesses) {
		hcfg := cfg.Harnesses[name]
		h, err := buildHarness(name, hcfg, opts)
		if err != nil {
			return nil, err
		}
		if opts.DryRun {
			// Dry-run placeholder: deterministic, never fails — do not retry it. The
			// per-invocation logging is added by the pipeline's invocation wrapper
			// (internal/log), so the dry-run wrapper itself logs only when a Logger is
			// explicitly supplied for standalone use; the orchestrator passes none here
			// to avoid a duplicate line.
			h = newDryRun(h, nil)
		} else {
			// Real harness: retry transient failures per the harness's policy, logging
			// each attempt + backoff through the supplied logger.
			h = newRetry(h, hcfg.Retry, opts.Logger)
		}
		r.harnesses[name] = h
	}
	return r, nil
}

// buildHarness constructs a single harness, preferring a registered Factory over
// the generic adapter.
func buildHarness(name string, cfg config.Harness, opts Options) (Harness, error) {
	if f, ok := opts.Factories[name]; ok && f != nil {
		h, err := f(name, cfg)
		if err != nil {
			return nil, fmt.Errorf("building harness %q via factory: %w", name, err)
		}
		return h, nil
	}
	return newCLIHarness(name, cfg)
}

// Get returns the harness registered under name. The boolean is false when no
// such harness exists, mirroring map-lookup semantics so callers can give a
// precise "unknown harness" error.
func (r *Registry) Get(name string) (Harness, bool) {
	h, ok := r.harnesses[name]
	return h, ok
}

// Wrap returns a NEW registry whose every harness is the result of passing the
// original through wrap, leaving this registry unchanged. It is the seam the
// pipeline uses to add per-invocation logging (internal/log.WrapHarness) once the
// run's logs dir is known — the registry is built early (from config), but the
// log destination is per-run, so the orchestrator wraps a registry view at run
// time. A nil wrap (or nil receiver) returns the registry unchanged.
func (r *Registry) Wrap(wrap func(Harness) Harness) *Registry {
	if r == nil || wrap == nil {
		return r
	}
	out := &Registry{harnesses: make(map[string]Harness, len(r.harnesses))}
	for name, h := range r.harnesses {
		out.harnesses[name] = wrap(h)
	}
	return out
}

// Names returns the registered harness names in sorted order (useful for error
// messages and listings).
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.harnesses))
	for k := range r.harnesses {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// sortedKeys returns the keys of a harness map in stable, sorted order.
func sortedKeys(m map[string]config.Harness) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
