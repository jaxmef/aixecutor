package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// scaffold is the commented starter config written by `aixecutor config init`.
// It mirrors the canonical schema (CLAUDE.md §5) but ships every key commented
// out: an empty-but-present local config is a valid no-op layer, and users
// uncomment only what they want to override. The header explains the two merge
// rules most likely to surprise: lists replace wholesale, maps merge per key.
const scaffold = `# aixecutor local configuration (<repo>/.aixecutor/config.yaml)
#
# This file is OPTIONAL. aixecutor ships a complete, valid default config
# compiled into the binary, so it runs end-to-end with no config at all. Use
# this file to override only the keys you care about.
#
# Layering (lowest to highest precedence):
#   built-in defaults  ->  ~/.aixecutor/config.yaml  ->  THIS FILE  ->  CLI flags
#
# Merge semantics (IMPORTANT):
#   * Maps merge key-by-key. Setting roles.executor.harness alone leaves the
#     other roles.executor.* keys (model, permissionMode, ...) at their defaults.
#   * Lists REPLACE WHOLESALE. If you set harnesses.claude.args below, you must
#     list the COMPLETE argument vector — your list replaces the default entirely,
#     it is not appended to it.
#   * Scalars replace.
#
# Run "aixecutor config show" to see the effective merged config (annotated with
# where each non-default value came from), and "aixecutor config path" to see
# which config files were found.
#
# Everything below is commented out; uncomment and edit what you need.

# version: 1

# paths:
#   runsDir: .aixecutor/runs     # base path for run artifacts (per-project)
#   docsSubdir: docs             # docs live under <runsDir>/<run-id>/<docsSubdir>

# harnesses:
#   claude:
#     type: cli
#     command: claude
#     promptDelivery: arg        # arg | stdin | file
#     args:                      # NOTE: replaces the default list wholesale
#       - "-p"
#       - "{{.Prompt}}"
#       - "--output-format"
#       - "json"
#       - "--model"
#       - "{{.Model}}"
#       - "--permission-mode"
#       - "{{.PermissionMode}}"
#     output: json
#     resultPath: result
#     timeout: 30m
#     env: {}

# roles:
#   planner:
#     harness: claude
#     model: opus
#     permissionMode: plan
#     promptTemplate: planner
#     timeout: 30m
#   executor:
#     harness: claude
#     model: sonnet
#     permissionMode: acceptEdits
#     promptTemplate: executor
#     timeout: 30m
#   subtaskReviewer:
#     harness: claude
#     model: sonnet
#     permissionMode: plan
#     promptTemplate: subtask-reviewer
#     timeout: 20m
#   seniorReviewer:
#     harness: claude
#     model: opus
#     permissionMode: plan
#     promptTemplate: senior-reviewer
#     timeout: 30m

# pipeline:
#   autostartExecution: true
#   execution:
#     parallel: true
#     maxParallel: 4
#     isolation: non-overlapping  # non-overlapping | worktree | none
#   subtaskReview:
#     enabled: true
#     maxLoops: 3                 # -1 = unlimited
#   seniorReview:
#     enabled: true
#     maxLoops: 3                 # -1 = unlimited

# git:
#   policy: read-only             # read-only | allow-worktree
`

// LocalConfigRelPath is the conventional location of the local config relative
// to a repository root.
var LocalConfigRelPath = filepath.Join(configDirName, configFileName)

// ScaffoldBytes returns the commented starter config content.
func ScaffoldBytes() []byte { return []byte(scaffold) }

// InitResult reports the outcome of WriteScaffold.
type InitResult struct {
	// Path is the file that was (or would have been) written.
	Path string
	// Created is true when a new file was written; false when an existing file
	// was left untouched.
	Created bool
}

// WriteScaffold writes the commented starter config to <dir>/.aixecutor/config.yaml,
// creating the .aixecutor directory if needed. Writing a local config file is
// not a git operation, so it is allowed (invariant #1 forbids mutating *git*).
//
// If the target already exists and force is false, the file is left untouched
// and InitResult.Created is false (the caller should tell the user). With force
// true, an existing file is overwritten.
func WriteScaffold(dir string, force bool) (InitResult, error) {
	target := filepath.Join(dir, configDirName, configFileName)
	if !force && fileExists(target) {
		return InitResult{Path: target, Created: false}, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return InitResult{Path: target}, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(scaffold), 0o644); err != nil {
		return InitResult{Path: target}, fmt.Errorf("writing %s: %w", target, err)
	}
	return InitResult{Path: target, Created: true}, nil
}
