package prompt

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// gitSafetyName is the stable name of the shared git-safety partial, as defined
// by {{define "git-safety"}} in _git-safety.tmpl. ensureGitSafety detects whether
// a template body already includes it and appends the include otherwise.
const gitSafetyName = "git-safety"

// gitSafetyInclude is the action appended to a template body that does not
// already reference the git-safety partial, so the preamble is always emitted.
const gitSafetyInclude = "\n{{template \"" + gitSafetyName + "\" .}}\n"

// Renderer resolves a prompt template by name (or explicit path) and renders it
// with a typed render context (see context.go). Resolution follows the three-tier
// override order from CLAUDE.md §4.5, with an explicit-path shortcut:
//
//  1. If the requested name is an explicit file path (contains a path separator
//     or a ".tmpl" suffix, or is absolute), that exact file is loaded.
//  2. Otherwise it is treated as a bare built-in name and resolved against the
//     override directories in order — LOCAL first, then GLOBAL — looking for
//     "<name>.tmpl" in each.
//  3. If no override file is found, the embedded default "<name>.tmpl" is used.
//
// In config terms, roles.<role>.promptTemplate may be a bare name (resolved by
// steps 2–3) or a file path (step 1). The override directories are typically
// <repo>/.aixecutor/prompts (local) and ~/.aixecutor/prompts (global); the caller
// passes them in local-first order.
//
// In every case the shared partials (_*.tmpl, e.g. _git-safety.tmpl) are parsed
// into the template set first, so a role template — embedded OR override — can
// reference partials such as {{template "git-safety" .}}. Partials always come
// from the embedded defaults so overriding one role's prompt cannot replace them.
//
// The git-safety preamble is, in addition, GUARANTEED in every rendered prompt
// (CLAUDE.md §2 invariant #1): if the resolved body does not already reference
// the git-safety partial, Render appends the include before parsing. So a user
// override or explicit-path template that omits the include still emits the
// preamble, while one that includes it keeps control of its position with no
// duplication. See ensureGitSafety.
type Renderer struct {
	// overrideDirs are searched in order (local first, then global) for a bare
	// name before falling back to the embedded default. Entries that are empty or
	// do not exist are skipped.
	overrideDirs []string
}

// NewRenderer constructs a Renderer that searches the given override directories,
// in the order provided, before falling back to embedded defaults. Pass them
// local-first then global (the same precedence as config layering): the local
// repo's prompts override the user's global prompts override the embedded
// defaults. Empty or missing directories are tolerated and simply skipped, so
// callers can pass paths unconditionally.
func NewRenderer(overrideDirs ...string) *Renderer {
	return &Renderer{overrideDirs: overrideDirs}
}

// funcMap is the small, documented set of helpers available to every template
// (embedded defaults and user overrides alike). Keep it minimal and stable;
// these are part of the override contract.
//
//   - join:  strings.Join with the separator as the LAST arg, e.g.
//     {{join .Subtask.Files ", "}} → "a, b, c".
//   - trim:  strings.TrimSpace, e.g. {{trim .RepoSummary}}.
var funcMap = template.FuncMap{
	"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	"trim": strings.TrimSpace,
}

// Render resolves the template identified by name (a bare built-in name or a file
// path) and executes it against data. It returns the rendered prompt text.
//
// Errors are wrapped with the template name (and, for execution failures, the
// underlying message which text/template annotates with the line) so a parse
// error, an unknown template, or a missing context field is always actionable.
// Because the renderer sets Option("missingkey=error"), referencing a context
// field that does not exist is a render error rather than silently emitting
// "<no value>".
func (r *Renderer) Render(name string, data any) (string, error) {
	if name == "" {
		return "", errors.New("prompt: empty template name")
	}

	mainSrc, label, err := r.resolve(name)
	if err != nil {
		return "", err
	}

	// Guarantee the git-safety preamble in the output (CLAUDE.md §2 invariant #1).
	// The partial is always parsed in below, but it is only *emitted* if the body
	// references it; a user override that omits the include would otherwise render
	// with no git-safety text. ensureGitSafety appends the include when (and only
	// when) the resolved source — embedded, override, or explicit path — does not
	// already reference it, so authors who place it themselves keep control of its
	// position with no duplication, and any source that omits it still gets it.
	mainSrc = ensureGitSafety(mainSrc)

	// Build the template set: a root named for the label, the shared partials,
	// then the resolved main source. The main source is parsed last so a role
	// override may itself (re)define helper blocks if it wants, and so its
	// {{template "git-safety" .}} reference binds to the partial we just added.
	tmpl := template.New(label).Funcs(funcMap).Option("missingkey=error")

	if err := r.parsePartials(tmpl); err != nil {
		return "", err
	}

	if _, err := tmpl.Parse(mainSrc); err != nil {
		return "", fmt.Errorf("prompt: parsing template %q: %w", label, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompt: rendering template %q: %w", label, err)
	}
	return buf.String(), nil
}

// resolve locates the source for name and returns (source, label, error). The
// label is a human-readable identifier used in error messages: the resolved file
// path for overrides/explicit paths, or "<name> (embedded default)" for the
// fallback. See Renderer's doc comment for the resolution order.
func (r *Renderer) resolve(name string) (src string, label string, err error) {
	if isExplicitPath(name) {
		data, rerr := os.ReadFile(name)
		if rerr != nil {
			return "", name, fmt.Errorf("prompt: reading template file %q: %w", name, rerr)
		}
		return string(data), name, nil
	}

	fileName := name + templateExt
	for _, dir := range r.overrideDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, fileName)
		data, rerr := os.ReadFile(candidate)
		if rerr == nil {
			return string(data), candidate, nil
		}
		if !errors.Is(rerr, fs.ErrNotExist) {
			// A present-but-unreadable override is a real error, not a miss: do
			// not silently fall through to the embedded default and mask it.
			return "", candidate, fmt.Errorf("prompt: reading override template %q: %w", candidate, rerr)
		}
	}

	// Fall back to the embedded default.
	data, rerr := builtinFS.ReadFile(builtinDir + "/" + fileName)
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return "", name, fmt.Errorf("prompt: unknown template %q: no override file in %v and no embedded default (known built-ins: %s)",
				name, r.overrideDirs, strings.Join(BuiltinRoles, ", "))
		}
		return "", name, fmt.Errorf("prompt: reading embedded template %q: %w", name, rerr)
	}
	return string(data), name + " (embedded default)", nil
}

// parsePartials parses every shared partial (_*.tmpl) from the embedded defaults
// into tmpl, making their defined blocks (e.g. "git-safety") available to the
// main template. Partials are always sourced from the embedded defaults so that
// overriding a role prompt cannot drop the git-safety preamble.
func (r *Renderer) parsePartials(tmpl *template.Template) error {
	entries, err := fs.Glob(builtinFS, builtinDir+"/"+partialPattern)
	if err != nil {
		return fmt.Errorf("prompt: listing embedded partials: %w", err)
	}
	for _, entry := range entries {
		data, rerr := builtinFS.ReadFile(entry)
		if rerr != nil {
			return fmt.Errorf("prompt: reading embedded partial %q: %w", entry, rerr)
		}
		if _, perr := tmpl.Parse(string(data)); perr != nil {
			return fmt.Errorf("prompt: parsing embedded partial %q: %w", entry, perr)
		}
	}
	return nil
}

// ensureGitSafety returns src unchanged if it already references the git-safety
// partial, and otherwise returns src with the include action appended. This is
// what makes the git-safety preamble non-optional: invariant #1 forbids us from
// instructing a sub-agent to mutate git, so every rendered role prompt must carry
// the preamble even if a user override forgot to include it.
//
// The reference check is a substring scan tolerant of the whitespace
// text/template allows inside an action — `{{template "git-safety"`,
// `{{- template "git-safety"`, and spaces around the keyword all count — so an
// author who places the partial themselves keeps control of its position and
// gets no duplicate. Comments are not stripped, so a body that only *mentions*
// the partial inside `{{/* ... */}}` would suppress the append; that is an
// acceptable, author-intentional edge (a real include is the normal case).
func ensureGitSafety(src string) string {
	if referencesGitSafety(src) {
		return src
	}
	return src + gitSafetyInclude
}

// referencesGitSafety reports whether src contains a template action that invokes
// the git-safety partial, allowing for the optional whitespace/trim markers
// text/template permits between "{{" and the "template" keyword.
func referencesGitSafety(src string) bool {
	const needle = `"` + gitSafetyName + `"`
	for i := 0; ; {
		j := strings.Index(src[i:], needle)
		if j < 0 {
			return false
		}
		j += i
		// Look back from the quoted name for `template` then an opening `{{`
		// (with any spaces / `-` trim marker between the parts).
		head := src[:j]
		head = strings.TrimRight(head, " \t")
		if strings.HasSuffix(head, "template") {
			head = strings.TrimSuffix(head, "template")
			head = strings.TrimRight(head, " \t")
			head = strings.TrimSuffix(head, "-") // optional {{- trim marker
			head = strings.TrimRight(head, " \t")
			if strings.HasSuffix(head, "{{") {
				return true
			}
		}
		i = j + len(needle)
	}
}

// isExplicitPath reports whether name should be treated as a file path rather
// than a bare built-in name. A name counts as a path if it is absolute, contains
// a path separator, or carries the ".tmpl" extension — none of which a bare role
// name (e.g. "executor") ever does.
func isExplicitPath(name string) bool {
	if filepath.IsAbs(name) {
		return true
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return true
	}
	return strings.HasSuffix(name, templateExt)
}
