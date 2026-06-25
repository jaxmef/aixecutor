package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configFileName is the per-layer file name the loader looks for in both the
// global (~/.aixecutor) and local (<repo>/.aixecutor) directories.
const configFileName = "config.yaml"

// configDirName is the directory holding a layer's config file.
const configDirName = ".aixecutor"

// LoadOptions parameterizes Load. Zero-valued fields fall back to discovery:
// the global config is sought at ~/.aixecutor/config.yaml and the local config
// by walking up from the working directory. Explicit paths (from --config /
// --global-config) bypass discovery for that layer.
type LoadOptions struct {
	// GlobalConfigPath, if set, is the exact global config file to load
	// (from --global-config). Empty means discover ~/.aixecutor/config.yaml.
	GlobalConfigPath string
	// LocalConfigPath, if set, is the exact local config file to load
	// (from --config). Empty means walk up from WorkingDir for .aixecutor/config.yaml.
	LocalConfigPath string
	// WorkingDir is where the upward search for the local config begins.
	// Empty means use the process working directory.
	WorkingDir string
	// HomeDir overrides the home directory used to locate the global config.
	// Empty means use os.UserHomeDir. Tests set this to avoid touching the
	// real ~/.aixecutor.
	HomeDir string

	// DocsPathOverride maps the --docs-path flag onto paths.runsDir (see Load).
	// Empty means no override. It is applied last, as the highest-precedence
	// (flag) layer, so it wins over every file layer.
	DocsPathOverride string
}

// Load resolves the effective configuration by layering, in increasing
// precedence: Default() → global file → local file → CLI flag overrides.
//
// Each present layer is decoded to a generic map and deep-merged (maps key-by-key,
// scalars replace, lists wholesale) so that absent keys preserve lower layers
// while zero values override them. The merged map is then strict-decoded into a
// typed Config (unknown keys are rejected) and validated.
//
// It returns the Config, the per-key provenance (for `config show`), and an
// error. Provenance is returned even on validation failure so callers can show
// what was loaded; it is nil only on a hard load/parse error.
func Load(opts LoadOptions) (Config, []Source, error) {
	base, err := toMap(Default())
	if err != nil {
		return Config{}, nil, fmt.Errorf("encoding default config: %w", err)
	}

	prov := provenance{}
	prov.record("", base, OriginDefault, "")

	// 2a. Global layer (~/.aixecutor/config.yaml or --global-config).
	globalPath, err := opts.globalPath()
	if err != nil {
		return Config{}, nil, err
	}
	if globalPath != "" {
		layer, ok, err := readLayer(globalPath)
		if err != nil {
			return Config{}, nil, err
		}
		if ok {
			base = deepMerge(base, layer)
			prov.record("", layer, OriginGlobal, globalPath)
		}
	}

	// 2b. Local layer (<repo>/.aixecutor/config.yaml or --config).
	localPath, err := opts.localPath()
	if err != nil {
		return Config{}, nil, err
	}
	if localPath != "" {
		layer, ok, err := readLayer(localPath)
		if err != nil {
			return Config{}, nil, err
		}
		if ok {
			base = deepMerge(base, layer)
			prov.record("", layer, OriginLocal, localPath)
		}
	}

	// 3. Flag overrides (highest precedence). --docs-path maps onto
	// paths.runsDir: it is the user-facing knob for "where run artifacts
	// (including docs) are written", and docs live under <runsDir>/<run-id>/.
	if opts.DocsPathOverride != "" {
		paths, _ := asStringMap(base["paths"])
		if paths == nil {
			paths = map[string]any{}
		}
		paths["runsDir"] = opts.DocsPathOverride
		base["paths"] = paths
		prov.set("paths.runsDir", OriginFlag, "")
	}

	// 4. Strict decode into the typed Config, then validate.
	cfg, err := decodeStrict(base)
	if err != nil {
		return Config{}, prov.sources(), err
	}
	if err := Validate(cfg); err != nil {
		return cfg, prov.sources(), err
	}
	return cfg, prov.sources(), nil
}

// FileLocation describes a config file the loader would consult.
type FileLocation struct {
	// Origin is the layer this location feeds (OriginGlobal or OriginLocal).
	Origin Origin
	// Path is the resolved file path, or "" if none could be resolved (e.g. no
	// local config found while walking up).
	Path string
	// Exists reports whether a file is actually present at Path.
	Exists bool
}

// Locations resolves (without reading) the global and local config file paths
// the loader would consult for these options, and whether each exists. It backs
// `config path`. The returned slice is ordered global, then local.
func (o LoadOptions) Locations() ([]FileLocation, error) {
	gp, err := o.globalPath()
	if err != nil {
		return nil, err
	}
	lp, err := o.localPath()
	if err != nil {
		return nil, err
	}
	return []FileLocation{
		{Origin: OriginGlobal, Path: gp, Exists: gp != "" && fileExists(gp)},
		{Origin: OriginLocal, Path: lp, Exists: lp != "" && fileExists(lp)},
	}, nil
}

// globalPath resolves the global config file path, honoring an explicit
// override. It returns "" (no error) when discovery yields no path because the
// home directory is unavailable but no override was given.
func (o LoadOptions) globalPath() (string, error) {
	if o.GlobalConfigPath != "" {
		return o.GlobalConfigPath, nil
	}
	home := o.HomeDir
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			// No home dir and no override: simply skip the global layer.
			return "", nil
		}
		home = h
	}
	return filepath.Join(home, configDirName, configFileName), nil
}

// localPath resolves the local config file path, honoring an explicit override.
// Without an override it walks up from the working directory looking for
// .aixecutor/config.yaml and returns the first hit, or "" if none is found.
func (o LoadOptions) localPath() (string, error) {
	if o.LocalConfigPath != "" {
		return o.LocalConfigPath, nil
	}
	dir := o.WorkingDir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("determining working directory: %w", err)
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving working directory %q: %w", dir, err)
	}

	for {
		candidate := filepath.Join(dir, configDirName, configFileName)
		if fileExists(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil // reached filesystem root
		}
		dir = parent
	}
}

// readLayer reads and parses a single config file into a generic map. The
// boolean is false (with no error) when the file does not exist, so callers can
// merge "only layers that exist". A present-but-malformed file is an error.
func readLayer(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading config %q: %w", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing config %q: %w", path, err)
	}
	if m == nil {
		// An empty file is a valid no-op layer.
		return map[string]any{}, true, nil
	}
	return m, true, nil
}

// toMap round-trips a typed value through YAML into a generic map so it can be
// deep-merged with file layers. Using YAML (not reflection) guarantees the map
// keys match the yaml tags and that Duration values appear as their string form.
func toMap(v any) (map[string]any, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeStrict encodes the merged map back to YAML and decodes it into a typed
// Config with KnownFields(true), so any key that is not part of the schema is
// rejected as a (likely typo) error.
func decodeStrict(m map[string]any) (Config, error) {
	data, err := yaml.Marshal(m)
	if err != nil {
		return Config{}, fmt.Errorf("re-encoding merged config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decoding config: %w", err)
	}
	return cfg, nil
}

// fileExists reports whether path names an existing regular (non-directory) file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
