package config

import (
	"sort"
	"strings"
)

// Origin identifies which layer supplied a configuration value. Layers are
// applied in increasing precedence: defaults < global < local < flag.
type Origin int

const (
	// OriginDefault is the hardcoded Default() baseline.
	OriginDefault Origin = iota
	// OriginGlobal is ~/.aixecutor/config.yaml.
	OriginGlobal
	// OriginLocal is <repo>/.aixecutor/config.yaml.
	OriginLocal
	// OriginFlag is a CLI flag override (highest precedence).
	OriginFlag
)

// String returns the lowercase label used in `config show` annotations.
func (o Origin) String() string {
	switch o {
	case OriginGlobal:
		return "global"
	case OriginLocal:
		return "local"
	case OriginFlag:
		return "flag"
	default:
		return "default"
	}
}

// Source records that the effective value at a dotted key Path originated from
// Origin (and, for file layers, the file Path it came from). One Source is
// emitted per leaf key the loader touches; `config show` uses these to annotate
// where each non-default value came from.
type Source struct {
	// Path is the dotted key path, e.g. "roles.executor.harness".
	Path string
	// Origin is the layer that supplied the effective value.
	Origin Origin
	// File is the file that supplied it, for global/local origins ("" otherwise).
	File string
}

// provenance accumulates the effective Origin for every leaf key as layers are
// applied. Later (higher-precedence) layers overwrite earlier entries for the
// same path, so the final map reflects the winning layer per key.
type provenance map[string]Source

// record walks a decoded layer map and stamps every leaf key it contains with
// the given origin/file, overwriting any earlier entry for the same path. Maps
// recurse; scalars and lists are leaves (lists replace wholesale, so the whole
// list is attributed to this layer).
func (p provenance) record(prefix string, m map[string]any, origin Origin, file string) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := asStringMap(v); ok {
			p.record(path, child, origin, file)
			continue
		}
		p[path] = Source{Path: path, Origin: origin, File: file}
	}
}

// set records a single leaf path (used for flag overrides, which do not arrive
// as a layer map).
func (p provenance) set(path string, origin Origin, file string) {
	p[path] = Source{Path: path, Origin: origin, File: file}
}

// sources returns the provenance as a slice sorted by key path, suitable for
// deterministic output and tests.
func (p provenance) sources() []Source {
	out := make([]Source, 0, len(p))
	for _, s := range p {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// originFor returns the recorded origin for an exact dotted path, falling back
// to the nearest recorded ancestor (so a list/scalar attributed at "a.b" also
// answers for a query about "a.b" itself). Returns OriginDefault if nothing was
// recorded along the path.
func (p provenance) originFor(path string) Origin {
	if s, ok := p[path]; ok {
		return s.Origin
	}
	// Walk up ancestors so that a value attributed at a parent path (e.g. a
	// whole list at "harnesses.claude.args") covers descendant queries.
	for {
		idx := strings.LastIndex(path, ".")
		if idx < 0 {
			return OriginDefault
		}
		path = path[:idx]
		if s, ok := p[path]; ok {
			return s.Origin
		}
	}
}
