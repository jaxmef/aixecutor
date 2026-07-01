# CLAUDE.md

Guidance for AI agents (and humans) working **in this repository** — i.e. building
`aixecutor` itself. For what the product *does*, read [README.md](./README.md) first.
This file is the canonical source for architecture, naming, the configuration schema, and
the hard invariants every change must respect.

---

## 1. What we are building

`aixecutor` is a Go CLI that orchestrates AI coding agents through a
**plan → execute → review** pipeline. See [README.md](./README.md#the-pipeline) for the
diagram.

---

## 2. Non-negotiable invariants

These hold across every change. A change that violates one is wrong even if it "works".

1. **No write git commands, ever.** The application must not execute `git commit`, `push`,
   `add`, `rm`, `reset`, `checkout <ref>`, `stash`, `merge`, `rebase`, `tag`, `apply`, or
   any other mutating git operation. Only **read** commands are allowed
   (`status`, `diff`, `log`, `show`, `rev-parse`, `ls-files`, `cat-file`). The **only**
   mutating exception is `git worktree add` / `git worktree remove`, permitted **solely**
   when the user opts into worktree isolation (`git.policy: allow-worktree`). All git
   access goes through the read-only **git gateway** (`internal/git`) — never shell out to
   git directly from other packages. This invariant also applies to what we instruct
   sub-agent harnesses to do: prompts must forbid them from committing/pushing.
2. **No required configuration.** A complete, valid default config is compiled into the
   binary. The tool must run end-to-end with zero config files present.
3. **Configuration is layered and deep-merged.** defaults → `~/.aixecutor/config.yaml` →
   `<repo>/.aixecutor/config.yaml`. Later layers override earlier ones **granularly**
   (per key), never by wholesale replacement of a section.
4. **Everything is configurable.** Every role (planner/executor/reviewers) can use a
   different harness, model, prompt, and timeout. Loop bounds, parallelism, and paths are
   all config-driven. Avoid hardcoding anything a user might reasonably want to change.
5. **Harness-agnostic core.** The pipeline depends on the `Harness` interface, not on any
   specific agent. Adding a harness should not require touching pipeline code.
6. **Runs are resumable.** Persist run state after every state transition. `resume` must
   continue from the last completed step without redoing finished work.
7. **Go, single binary, minimal dependencies.** Keep the dependency tree small (see
   [§6](#6-dependencies)). Use `internal/` so packages are not importable externally.

---

## 3. Architecture

### 3.1 Package layout

```
aixecutor/
├── main.go                  # thin entrypoint → internal/cli.Execute()
├── go.mod                   # module github.com/jaxmef/aixecutor (go 1.26)
├── Makefile                 # build / test / lint / fmt
├── prompts/                 # default prompt templates (*.tmpl), embedded via go:embed
├── internal/
│   ├── cli/                 # cobra commands: run, plan, resume, review, amend, status, list, backlog, config, version
│   ├── config/             # schema structs, hardcoded defaults, layered load + deep-merge, validate
│   ├── harness/            # Harness interface, generic CLI adapter, registry, mock harness
│   ├── git/                # read-only gateway, baseline snapshots, diff engine, opt-in worktree
│   ├── run/                # Run model, run IDs, artifact layout, state persistence (run.yaml)
│   ├── pipeline/           # orchestrator (state machine), DAG scheduler, phase + loop logic
│   ├── backlog/            # backlog runner: ticket discovery, dependency DAG, gating, resumable state
│   ├── workspace/          # multi-root workspace: repo discovery, unified enumeration, baseline/diff/restore
│   ├── prompt/             # template loading (embedded + overrides), render context
│   └── log/                # structured logging + human progress output
└── testdata/               # fixtures (sample configs, fake repos, recorded harness outputs)
```

> The `prompts/` entry above is illustrative: the embedded default templates actually live under `internal/prompt/prompts/*.tmpl` (go:embed cannot reach `..`); user overrides live under `~/.aixecutor/prompts/` and `<repo>/.aixecutor/prompts/`.

`main.go` stays trivial. All logic lives under `internal/`. Packages depend "downward":
`pipeline` → {`config`, `harness`, `git`, `run`, `prompt`, `log`}; lower packages must not
import `pipeline` or `cli`.

### 3.2 Core domain types (target shapes — refine during implementation)

```go
// internal/harness — how we drive an AI coding agent.
type Harness interface {
    Name() string
    // Run executes a single prompt in workDir and returns the agent's result.
    // Implementations MUST NOT perform write git operations.
    Run(ctx context.Context, req Request) (Result, error)
}

type Request struct {
    Prompt         string
    Model          string
    WorkDir        string
    PermissionMode string            // harness-specific (e.g. claude: plan|acceptEdits)
    Env            map[string]string
    Timeout        time.Duration
}

type Result struct {
    Text     string        // final assistant text / summary
    Raw      []byte        // raw stdout for logging
    ExitCode int
    Duration time.Duration
}
```

```go
// internal/run — a single pipeline execution, persisted to run.yaml.
type Run struct {
    ID            string
    Task          string
    Status        Status          // created|planning|planned|executing|seniorReview|completed|failed|aborted
    Baseline      Baseline        // working-tree state captured at run start (for diffing)
    Subtasks      []Subtask
    SeniorReview  SeniorReview
    Dir           string          // .aixecutor/runs/<id>
}

type Subtask struct {
    ID       string
    Title    string
    Deps     []string            // subtask IDs that must finish first
    Files    []string            // declared file ownership (globs) — drives non-overlapping parallelism
    Status   SubtaskStatus       // pending|implementing|reviewing|blocked|done|failed
    Loops    int                 // executor↔reviewer cycles spent
}
```

### 3.3 Pipeline state machine

`planning → planned → executing → seniorReview → completed` (with `failed`/`aborted` as
terminal off-ramps, and `paused` as a resumable off-ramp from `executing`). The
orchestrator persists `run.yaml` on every transition.

- **Planning** — invoke the `planner` role once. It writes `docs/plan.md`,
  `docs/context.md`, `docs/manual-testing.md`, and a machine-readable `docs/subtasks.yaml`
  (the subtask DAG, with `deps` and `files` per subtask). Print the docs path. If
  `pipeline.autostartExecution` is true, proceed to execution immediately (the user reads
  the docs in parallel); otherwise stop after planning.
- **Execution** — schedule subtasks over the DAG. A subtask is *ready* when all its `deps`
  are `done`. Among ready subtasks, run up to `execution.maxParallel` concurrently subject
  to the isolation policy ([§4.3](#43-parallelism--isolation)). Each subtask runs the
  **executor**, captures its diff, then enters the subtask review loop.
- **Subtask review loop** — run the `subtaskReviewer` on just that subtask's diff. If it
  reports actionable findings, feed them back to the `executor` and repeat, up to
  `subtaskReview.maxLoops` (default 3; `-1` = unlimited). Then mark the subtask `done`.
- **Senior review** — after all subtasks are `done`, run the `seniorReviewer` on the full
  diff (current tree vs. the run-start baseline). Findings spawn a remediation loop
  (executor → reviewer, reusing the loop machinery) up to `seniorReview.maxLoops`, until
  the diff is clean or the bound is hit.
- **Review checkpoint** — `aixecutor review <id>` writes a pause request to the
  run's `.control/` channel; the scheduler honors it at the next **subtask boundary** (never
  mid-subtask-write), persists `paused`, and stops. From `paused`: `resume` continues
  (clarify, no rework), or `amend --confirm` **reverts** the working tree to the run-start
  baseline (`git.RestoreTree` — raw file I/O, **no mutating git**, excludes `runsDir`/docs so
  amended docs and pre-existing uncommitted changes survive), re-reads `docs/subtasks.yaml`,
  resets subtask state, and restarts execution from the amended plan.
- **Done** — write a summary. **Never** commit. The working tree is left for the user.

---

## 4. Key mechanisms

### 4.1 Configuration loading (`internal/config`)

1. Start from hardcoded `Default()` (a fully-populated struct).
2. If `~/.aixecutor/config.yaml` exists, deep-merge it on top.
3. If `<repo>/.aixecutor/config.yaml` exists, deep-merge it on top.
4. Apply CLI flag overrides (highest precedence).
5. `Validate()` the result; fail with a clear, actionable error.

**Merge semantics:** maps merge key-by-key (recursively); scalars replace; **lists
replace wholesale** (predictable, documented). Implement deep-merge explicitly rather than
relying on a library's surprising defaults — unmarshal each layer into a generic
`map[string]any`, deep-merge the maps, then decode into the typed struct so that "absent"
is distinguishable from "zero value".

### 4.2 Harness invocation (`internal/harness`)

Harnesses are CLI subprocesses described declaratively, so most new agents are config-only:

- `command`, `args` (Go `text/template`, with `{{.Prompt}}`, `{{.Model}}`,
  `{{.PermissionMode}}`, `{{.WorkDir}}` …).
- `promptDelivery`: `arg` | `stdin` | `file` (write prompt to a temp file, pass its path).
- `output`: `text` | `json` (+ `resultPath` to extract the final text from JSON).
- `timeout`, `env`.

Ship a **mock harness** (records requests, returns canned/file-backed results) and a
global `--dry-run` so the whole pipeline is testable without spending tokens or hitting
real agents. Concrete presets exist for `claude` (Claude Code) and `pi`.

### 4.3 Parallelism & isolation (`internal/git`, `internal/pipeline`)

The planner declares each subtask's `files` (ownership globs). Default isolation is
**`non-overlapping`**: two ready subtasks run in parallel only if their declared file sets
are disjoint; otherwise they serialize. This needs zero git writes.

Opt-in **`worktree`** isolation (requires `git.policy: allow-worktree`) gives each parallel
subtask its own `git worktree`, allowing overlapping edits; changes are reconciled back
into the main tree by copying changed paths (still no commits). `none` runs everything in
the main tree with no overlap checks (advanced/unsafe).

### 4.4 Diffing (`internal/git`)

- At run start, capture a **baseline** of the working tree (so diffs are relative to the
  user's starting point, not `HEAD`, and pre-existing uncommitted changes are excluded).
- **Per-subtask diff:** snapshot the subtask's touched paths before/after and compute the
  diff (e.g. `git diff --no-index` between snapshots — read-only).
- **Full diff** (senior review): current tree vs. baseline.
- All of this is read-only; persist `*.patch` files under the run dir.

### 4.5 Prompts (`internal/prompt`, `prompts/`)

Default templates are `*.tmpl` embedded with `go:embed`. Each role has one. Users override
by name in config (`roles.<role>.promptTemplate`) or by dropping a file in
`~/.aixecutor/prompts/` or `<repo>/.aixecutor/prompts/`. Document the render context
(available fields) for each template. Every executor/worker prompt must include the
"do not run git write commands / do not commit" instruction.

---

## 5. Configuration schema

This is the canonical schema **and** the hardcoded default. Keep `internal/config`'s
`Default()` in sync with this block; if you change one, change both and update README.

```yaml
version: 1

paths:
  runsDir: .aixecutor/runs        # base path for run artifacts (per-project). CONFIGURABLE.
  docsSubdir: docs                # docs live under <runsDir>/<run-id>/<docsSubdir>

harnesses:
  claude:                         # Claude Code (headless `claude -p`)
    type: cli
    command: claude
    promptDelivery: arg           # arg | stdin | file
    args:
      - "-p"
      - "{{.Prompt}}"
      - "--output-format"
      - "json"
      - "--model"
      - "{{.Model}}"
      - "--permission-mode"
      - "{{.PermissionMode}}"
    output: json
    resultPath: "result"          # JSON field holding the final text
    timeout: 30m
    retry:                        # bounded retry of TRANSIENT failures only
      maxAttempts: 2              # total attempts incl. the first; 1 = no retry
      backoff: 2s                 # base delay between attempts
    env: {}
  pi:                             # pi coding agent — headless contract verified
    type: cli                      # `-p`/--print runs one prompt and exits; prompt is a
    command: pi                    # POSITIONAL arg; --model selects the model; --mode text
    promptDelivery: arg            # is the default. (--mode json exists but its resultPath
    args:                          # for the final text is unverified — TODO before switching.)
      - "--print"
      - "--model"
      - "{{.Model}}"
      - "{{.Prompt}}"
    output: text
    timeout: 30m
    retry:                        # bounded retry of TRANSIENT failures only
      maxAttempts: 2              # total attempts incl. the first; 1 = no retry
      backoff: 2s                 # base delay between attempts
    env: {}

roles:
  planner:
    harness: claude
    model: opus                   # claude model alias; full IDs also accepted
    permissionMode: plan
    promptTemplate: planner
    timeout: 30m
  executor:
    harness: claude
    model: sonnet
    permissionMode: acceptEdits
    promptTemplate: executor
    timeout: 30m
  subtaskReviewer:
    harness: claude
    model: sonnet
    permissionMode: plan
    promptTemplate: subtask-reviewer
    timeout: 20m
  seniorReviewer:
    harness: claude
    model: opus
    permissionMode: plan
    promptTemplate: senior-reviewer
    timeout: 30m

pipeline:
  autostartExecution: true        # begin executing while the user reviews the docs
  execution:
    parallel: true
    maxParallel: 4
    isolation: non-overlapping    # non-overlapping | worktree | none
  subtaskReview:
    enabled: true
    maxLoops: 3                   # executor↔reviewer cycles per subtask; -1 = unlimited
  seniorReview:
    enabled: true
    maxLoops: 3                   # -1 = unlimited

git:
  policy: read-only               # read-only | allow-worktree
  # mutating git (commit/push/add/reset/stash/merge/rebase/...) is NEVER permitted.

ignore:                           # dir/file NAMES dropped from diffs & reviews at ANY depth.
  - .idea                         #   Applies to in-repo AND workspace diffs, and to the
  - .vscode                       #   non-git discovery walk. Matches a path SEGMENT anywhere
  - .DS_Store                     #   (not a leading prefix); runsDir stays excluded on top.
  - node_modules                  #   Replaces WHOLESALE on merge (list semantics, §4.1) — an
  - vendor                        #   override supplies the full list, it does not append.
  - dist
  - build
  - .next
  - target

backlog:                          # backlog runner / driver mode
  dir: ""                         # default backlog directory; `backlog run [dir]` overrides
  gate: manual                    # manual | stop-on-finding | auto

workspace:                        # multi-root operation
  root: ""                        # workspace root; "" = single repo / cwd. --workspace overrides
  maxDepth: 4                     # how deep beneath root to discover git repos (>= 1)
```

**Model aliases:** for the `claude` harness, `opus`/`sonnet`/`haiku` are accepted and
preferred over pinned IDs (more robust across releases). Current full IDs at time of
writing: Opus 4.8 = `claude-opus-4-8`, Sonnet 4.6 = `claude-sonnet-4-6`,
Haiku 4.5 = `claude-haiku-4-5-20251001`. Verify against the installed `claude` build.

**Harness retry** (`harnesses.<name>.retry`): each harness retries only
**transient** invocation failures — a process spawn failure ("couldn't run the agent"),
a timeout, or empty/unparseable output ("agent ran but produced no usable result") — up
to `maxAttempts` total attempts with a `backoff` base delay. A **hard** failure is never
retried: a successful run that simply yielded a result (even a reviewer's "not approved"
verdict is a *valid result*, not a failure), and an unambiguous non-retryable error (e.g.
a non-zero process exit). The retry wrapper composes over the real CLI harness; the
`--dry-run` placeholder is never retried. Defaults are conservative: `maxAttempts: 2`
(one retry), `backoff: 2s`.

**Validation rules** (non-exhaustive): `isolation: worktree` requires
`git.policy: allow-worktree`; every `roles.*.harness` must exist in `harnesses`;
`maxLoops >= -1`; `maxParallel >= 1`; `retry.maxAttempts >= 1`; `retry.backoff >= 0`;
`backlog.gate` must be `manual|stop-on-finding|auto`; `workspace.maxDepth >= 1`; unknown
top-level keys are an error.

---

## 6. Dependencies

Keep the tree minimal and justified:

- `github.com/spf13/cobra` — CLI commands/flags. (Stdlib `flag` is acceptable if a
  contributor prefers zero-dep; cobra is the default choice for room to grow.)
- `gopkg.in/yaml.v3` — config + `subtasks.yaml` + `run.yaml`.
- Standard library for everything else (`os/exec`, `text/template`, `context`,
  `path/filepath`, `log/slog`).

Prefer reading git via `os/exec` against the user's `git` binary (through the gateway)
over a CGo/library git. Add a dependency only when it removes real complexity; note the
justification in the change.

---

## 7. Conventions

- **Go style:** standard `gofmt`/`go vet`; small packages with clear seams; constructor
  funcs returning interfaces where it aids testing; wrap errors with `%w` and context.
- **Concurrency — share memory by communicating.** Follow Go's model
  ([share by communicating, not communicate by sharing](https://go.dev/blog/codelab-share)):
  coordinate goroutines through channels, not locks.
  - One goroutine **owns** each piece of mutable state. If state is shared, pick the owner
    and route every other goroutine through it.
  - External reads/writes go through **query channels**: send `{args, reply chan T}` on the
    owner's channel; its `select` loop answers on `reply`. Both the send and the receive
    also select against a `done` channel so shutdown never deadlocks callers.
  - `sync.Mutex`/`RWMutex` are acceptable **only at true leaves** — self-contained data
    structures with no goroutine of their own (e.g. an in-memory index reached from one
    goroutine's call graph, a small test helper). Never lock state that already has a
    dedicated goroutine.
  - **Reaching for a mutex to fix a race is a signal the ownership model is wrong.** Move
    the state behind its owner's channel, or split responsibilities so the race can't
    happen — don't bolt on a lock.
- **Dependency direction — components don't depend on the runtime that owns them.** A
  runtime composes components (readers, writers, publishers, stores); those components must
  not take the runtime back as a dependency.
  - No "pass the runtime into its component" shortcuts. If a component needs live state only
    the runtime knows, combine them at the call site **inside** the runtime, not inside the
    component.
  - Component interfaces describe the **narrow capability** they need, not "give me the
    runtime." A reader that needs best bid/ask per contract accepts a small interface for
    exactly that — not the whole engine.
  - Cyclic ownership (runtime → component → runtime) is a circular dependency in disguise.
    Break the cycle before shipping.
- **Comments — let the code speak.** Prefer self-explanatory names and structure over
  prose. Write a comment only where it earns its keep: the *why* behind a non-obvious
  choice, a constraint or invariant, a workaround, a subtle edge case. No restating what the
  code already says, no decorative banners, no narrating the obvious. A stale comment is
  worse than none — if it can drift out of sync with the code, it probably shouldn't exist.
- **Errors:** user-facing CLI errors are actionable (what failed, where, how to fix).
  Never panic in library code; return errors.
- **Testing:** table-driven unit tests; the **mock harness** + a temp git repo make the
  pipeline testable hermetically. No test may invoke a real AI harness or hit the network.
  No test may run a mutating git command.
- **Logging:** `log/slog`; structured logs to the run's `logs/`, concise human progress to
  stdout. Respect `-v/--verbose`.
- **Determinism in tests:** inject clock/IDs so run-IDs and timestamps are reproducible.
- **Commits/PRs:** contributors follow normal git practice. (This is about *people*
  working on the repo — distinct from invariant #1, which constrains the *application*.)
  Do not commit/push on a user's behalf unless explicitly asked.

---

## 8. Build & run

```bash
go build -o bin/aixecutor .     # build
go test ./...                   # test
go vet ./... && gofmt -l .      # lint
bin/aixecutor run "<task>"      # run
bin/aixecutor --dry-run run "<task>"   # exercise the pipeline without real agents
```

---

## 9. Working guidelines

Behavioral guidelines to reduce common LLM coding mistakes. They bias toward caution over
speed — for trivial tasks, use judgment.

### 10.1 Think before coding

**Don't assume. Don't hide confusion. Surface tradeoffs.** Before implementing:

- State your assumptions explicitly; if uncertain, ask.
- If multiple interpretations exist, present them — don't pick one silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop, name what's confusing, and ask.

### 10.2 Simplicity first

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or configurability that wasn't requested.
- No error handling for impossible scenarios.
- If you wrote 200 lines and it could be 50, rewrite it.

Litmus test: would a senior engineer call this overcomplicated? If yes, simplify.

### 10.3 Goal-driven execution

**Define success criteria, then loop until verified.** Turn tasks into verifiable goals:

- "Add validation" → write tests for invalid inputs, then make them pass.
- "Fix the bug" → write a test that reproduces it, then make it pass.
- "Refactor X" → ensure tests pass before and after.

For multi-step work, state a brief plan with a check per step:

```
1. [step] → verify: [check]
2. [step] → verify: [check]
```

Strong success criteria let you loop independently; weak ones ("make it work") force
constant clarification. These guidelines are working if diffs carry fewer unnecessary
changes, fewer rewrites from overcomplication, and questions land before implementation
rather than after mistakes.
