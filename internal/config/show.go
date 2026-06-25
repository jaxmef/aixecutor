package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Render returns the effective configuration as YAML, annotating every value
// that did not come from the hardcoded defaults with a trailing comment naming
// its origin (global / local / flag) and, for file layers, the file it came
// from. Values from the defaults are left unannotated to keep the output quiet.
//
// The provenance slice is the one returned by Load; an empty/nil slice yields
// plain YAML with no annotations.
func Render(cfg Config, sources []Source) (string, error) {
	prov := indexSources(sources)

	var root yaml.Node
	if err := root.Encode(cfg); err != nil {
		return "", fmt.Errorf("encoding config to yaml node: %w", err)
	}
	// root is a document/mapping node depending on version; Encode yields a
	// mapping node here. Annotate it in place.
	annotate(&root, "", prov)

	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return "", fmt.Errorf("encoding annotated config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("closing yaml encoder: %w", err)
	}
	return b.String(), nil
}

// indexSources turns the Source slice into a lookup keyed by dotted path. Only
// non-default origins are kept, since default values are rendered without a
// comment.
func indexSources(sources []Source) provenance {
	p := provenance{}
	for _, s := range sources {
		if s.Origin == OriginDefault {
			continue
		}
		p[s.Path] = s
	}
	return p
}

// annotate walks a mapping node, recursing into child mappings, and attaches a
// line comment to each leaf (scalar or sequence) value whose dotted path has a
// non-default origin. The path is built from mapping keys.
func annotate(node *yaml.Node, prefix string, prov provenance) {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			annotate(c, prefix, prov)
		}
	case yaml.MappingNode:
		// Mapping content is [key, value, key, value, ...].
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			val := node.Content[i+1]
			path := key.Value
			if prefix != "" {
				path = prefix + "." + key.Value
			}
			if val.Kind == yaml.MappingNode {
				annotate(val, path, prov)
				continue
			}
			// Leaf (scalar or sequence): annotate if non-default.
			s, ok := prov.lookup(path)
			if !ok {
				continue
			}
			// A line comment on a multi-line block sequence is emitted against
			// the line that follows the sequence (the next key), not the list.
			// Anchor list annotations on the key node ("args:") instead; scalars
			// annotate cleanly on the value node.
			if val.Kind == yaml.SequenceNode {
				key.LineComment = comment(s)
			} else {
				val.LineComment = comment(s)
			}
		}
	}
}

// lookup returns the source for an exact path, or for the nearest recorded
// ancestor (so a whole sequence attributed at "harnesses.claude.args" annotates
// that key's value node).
func (p provenance) lookup(path string) (Source, bool) {
	if s, ok := p[path]; ok {
		return s, true
	}
	for {
		idx := strings.LastIndex(path, ".")
		if idx < 0 {
			return Source{}, false
		}
		path = path[:idx]
		if s, ok := p[path]; ok {
			return s, true
		}
	}
}

// comment renders the trailing annotation for a non-default value.
func comment(s Source) string {
	if s.File != "" {
		return fmt.Sprintf("from %s (%s)", s.Origin, s.File)
	}
	return fmt.Sprintf("from %s", s.Origin)
}
