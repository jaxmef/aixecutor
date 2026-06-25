package config

// deepMerge overlays src onto dst and returns the merged result, following the
// schema's documented merge semantics (CLAUDE.md §4.1):
//
//   - maps merge key-by-key, recursively;
//   - scalars replace;
//   - lists (slices) replace WHOLESALE — the override wins entirely, no element
//     concatenation or index-wise merge.
//
// We merge generic map[string]any values (decoded from each YAML layer) rather
// than typed structs so that an "absent" key is distinguishable from a present
// "zero value": a key missing from src leaves dst untouched, while a key set to
// e.g. 0/""/false in src replaces dst. The result is decoded into the typed
// Config only after all layers are merged.
//
// dst is mutated in place and also returned for convenience.
func deepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		dv, ok := dst[k]
		if !ok {
			dst[k] = sv
			continue
		}

		dm, dIsMap := asStringMap(dv)
		sm, sIsMap := asStringMap(sv)
		if dIsMap && sIsMap {
			// Both sides are maps: recurse so sibling keys are preserved.
			dst[k] = deepMerge(dm, sm)
			continue
		}

		// Scalars and lists: the override replaces wholesale. (A list on either
		// side, or a type change between layers, falls through to here.)
		dst[k] = sv
	}
	return dst
}

// asStringMap normalizes a decoded YAML value into a map[string]any if it is one.
// yaml.v3 decodes mappings into map[string]any when the destination is `any`,
// but we also accept map[any]any defensively (e.g. from other decoders) and
// stringify its keys so the recursion stays uniform.
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = val
		}
		return out, true
	default:
		return nil, false
	}
}
