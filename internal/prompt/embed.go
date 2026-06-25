// Package prompt loads and renders the prompt templates handed to AI coding
// agents for each pipeline role. It owns three things (CLAUDE.md §4.5):
//
//   - Embedded default templates for every role (planner, executor,
//     subtask-reviewer, senior-reviewer), so the tool works with zero config
//     files present (invariant #2). They live in internal/prompt/prompts/*.tmpl
//     and are embedded below. (CLAUDE.md §3.1's repo-root prompts/ is
//     illustrative; the embedded defaults actually live here, and user overrides
//     live under ~/.aixecutor/prompts/ and <repo>/.aixecutor/prompts/.)
//   - Three-tier override resolution (local dir → global dir → embedded), so
//     users can replace any role's prompt without recompiling. See Renderer.
//   - A shared git-safety preamble (_git-safety.tmpl) parsed into every template
//     set and injected into worker prompts, enforcing the sub-agent half of
//     invariant #1 (never instruct an agent to commit/push or otherwise mutate
//     git).
//
// The typed render contexts in context.go are the contract with the pipeline
// phases (AIX-0009..0012) that build these prompts. This package depends only on
// the standard library; it must not import internal/pipeline or internal/cli.
package prompt

import "embed"

// builtinFS holds the embedded default templates. go:embed cannot reach outside
// the package directory (no ".."), so the defaults live at
// internal/prompt/prompts/*.tmpl and are embedded from here. The "all:" prefix
// also pulls in files whose names begin with "_" (e.g. _git-safety.tmpl), which
// the default embed pattern would otherwise skip.
//
//go:embed all:prompts
var builtinFS embed.FS

// builtinDir is the directory within builtinFS that holds the templates.
const builtinDir = "prompts"

// templateExt is the file extension for every prompt template, embedded or
// override. A bare role name (e.g. "executor") maps to "<name>.tmpl".
const templateExt = ".tmpl"

// partialPattern matches the shared partial templates (their names are prefixed
// with "_"). Every partial is parsed into each template set so role templates —
// embedded or override — can reference partials such as the "git-safety" block.
const partialPattern = "_*" + templateExt

// BuiltinRoles are the role template names shipped as embedded defaults. They are
// the bare names valid for roles.<role>.promptTemplate when no override file is
// present. Phases render by these names (or by a user-configured name/path).
var BuiltinRoles = []string{
	"planner",
	"executor",
	"subtask-reviewer",
	"senior-reviewer",
}
