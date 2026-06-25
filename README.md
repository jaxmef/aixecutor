# aixecutor

> **AI + executor** — a configurable orchestrator that drives multiple AI coding agents
> and harnesses through a structured plan → execute → review pipeline.

`aixecutor` takes a single task description and runs it through a multi-agent pipeline:
one agent analyzes the work and writes planning documents, then worker agents implement
the plan subtask-by-subtask, reviewer agents check each subtask, and a senior reviewer
audits the full diff at the end. Every step is configurable — which AI harness, which
model, which prompt, how many review loops — and **nothing is required to get started**
because a complete default configuration is hardcoded into the binary.

> [!IMPORTANT]
> **aixecutor never runs write/mutating git commands.** No commits, pushes, `add`,
> `reset`, `stash`, `merge`, or `rebase` — ever. It only *reads* git state (`status`,
> `diff`, `log`, `show`) to compute diffs for reviewers. The single exception is
> `git worktree add/remove`, and only when you explicitly opt into worktree isolation.
> You stay in control of your history.

## The pipeline

```
            ┌───────────────────────────────────────────────────────────────────┐
  task ───► │ 1. PLANNING                                                       │
            │    planner agent analyzes the repo + task and writes markdown:    │
            │    plan.md · context.md · manual-testing.md · subtasks.yaml (DAG) │
            └───────────────────────────────────────────────────────────────────┘
                          │  prints where the docs were saved (base path configurable)
                          │  ── you review the docs while execution begins ──
                          ▼
            ┌───────────────────────────────────────────────────────────────────┐
  2. EXECUTION (per subtask, scheduled over a dependency DAG)                   │
            │                                                                   │
            │   ┌────────────┐   diff    ┌──────────────────┐                   │
            │   │  executor  │ ────────► │ subtask reviewer │                   │
            │   │  (worker)  │ ◄──────── │  (small, scoped) │   loop ≤ maxLoops │
            │   └────────────┘  findings └──────────────────┘   (default 3)     │
            │                                                                   │
            │   independent subtasks may run in parallel (non-overlapping files │
            │   by default; git worktrees if you opt in)                        │
            └───────────────────────────────────────────────────────────────────┘
                          ▼
            ┌───────────────────────────────────────────────────────────────────┐
  3. SENIOR REVIEW                                                              │
            │    senior reviewer audits the FULL diff. Findings spawn another   │
            │    worker → reviewer remediation loop (≤ maxLoops), until clean.  │
            └───────────────────────────────────────────────────────────────────┘
                          ▼
                   summary report — no git writes, your tree is staged for you to commit
```

Each labeled role (**planner**, **executor**, **subtask reviewer**, **senior reviewer**)
is an independently configurable agent: it can use a different harness, model, prompt,
and timeout from every other role.

---

## Why

Single-shot AI coding is unreliable on non-trivial tasks: no plan, no isolation between
steps, no independent review, and no record of *why* the change looks the way it does.
`aixecutor` makes the loop explicit and configurable — planning is written to disk for a
human to read, work is decomposed and reviewed in small pieces, and a senior pass guards
the whole diff — while staying strictly hands-off on your git history.

---

## Installation

> Requires Go 1.26+. At least one supported harness CLI must be installed and on `PATH`
> (e.g. [Claude Code](https://claude.com/claude-code)'s `claude`, or `pi`).

**Install the binary** (lands in `$GOBIN`, or `$(go env GOPATH)/bin` — make sure it's on your `PATH`):

```bash
go install github.com/jaxmef/aixecutor@latest
aixecutor run "Add OAuth2 login with Google as a provider"
```

`@latest` resolves to the newest release tag (or pin one, e.g. `@v0.1.0`; `@main` tracks the branch).

**Update** — re-run the same command; it overwrites the installed binary with the newest version:

```bash
go install github.com/jaxmef/aixecutor@latest
aixecutor version   # confirm the installed version
```

> `go install …@latest` is cached. If it doesn't pick up a fresh tag, run
> `GOPROXY=direct go install github.com/jaxmef/aixecutor@latest` (or `go clean -cache`).

**Build from source** — see [Quickstart](#quickstart) below.

---

## Quickstart

> Requires Go 1.26+. At least one supported harness CLI must be installed and on `PATH`
> (e.g. [Claude Code](https://claude.com/claude-code)'s `claude`, or `pi`).

```bash
# build
go build -o bin/aixecutor .

# run a task — no config needed, sensible defaults are built in
bin/aixecutor run "Add OAuth2 login with Google as a provider"

# planning only (write the docs, don't execute)
bin/aixecutor plan "Add OAuth2 login with Google as a provider"

# read the task from a file instead of typing it inline
bin/aixecutor run --task-file spec.md
bin/aixecutor run @spec.md          # @<path> shorthand (use @@ for a literal leading '@')

# or pipe it on stdin / compose it interactively (no task + no --task-file)
bin/aixecutor run < spec.md         # piped/redirected stdin (multi-line)
bin/aixecutor run                   # on a terminal: opens $VISUAL/$EDITOR (empty buffer aborts;
                                    # lines starting with '#', incl. Markdown headings, are dropped)

# inspect / resume
bin/aixecutor list
bin/aixecutor status <run-id>
bin/aixecutor resume <run-id>

# review checkpoint: pause a running run, then continue or amend the plan
bin/aixecutor review <run-id>          # pause at the next subtask boundary
bin/aixecutor amend  <run-id> --confirm # revert execution & restart from the edited docs

# see the effective merged configuration and where it came from
bin/aixecutor config show
bin/aixecutor config path
```

When planning finishes, `aixecutor` prints the path to the generated markdown so you can
review it while the executor starts working.

### Review checkpoint (pause, amend, restart)

Execution starts immediately by default, so you read the docs while it works. If review
reveals a needed change, `aixecutor review <run-id>` pauses execution at the next **safe
subtask boundary** (run state stays consistent — never mid-subtask). From the paused run:

- **clarify only** — `aixecutor resume <run-id>` continues from where it paused (no rework);
- **amend** — edit `docs/subtasks.yaml` / `context.md` / `plan.md`, then
  `aixecutor amend <run-id> --confirm`: the working tree is reverted to the **exact**
  pre-execution state (including any uncommitted changes you had before the run, restored
  byte-for-byte — your doc edits are kept) and execution restarts from the amended plan.

The revert uses **no git write commands** — it restores from the run-start baseline snapshot
via plain file I/O. Nothing is ever committed.

### Driving a backlog

Point `aixecutor` at a directory of ticket files and it drives them through the pipeline
in dependency order, one ready ticket at a time:

```bash
bin/aixecutor backlog run ./tickets
```

Each ticket is a Markdown file with a small YAML frontmatter block; the body below it is
the task fed to the pipeline:

```markdown
---
id: T-002
dependsOn: [T-001]
status: pending      # pending | done | blocked
---
Add a `/healthz` endpoint that returns 200 and the build SHA.
```

The runner builds the dependency DAG (rejecting cycles), runs the next ready ticket
(all its deps `done`) end-to-end, then **gates** advancement:

- `manual` (default) — run one ticket, then pause; inspect the working tree and re-run to
  continue. The safe default.
- `stop-on-finding` — advance through cleanly-reviewed tickets, but stop the moment a
  ticket completes with unresolved senior-review findings.
- `auto` — run the whole backlog unattended; only a non-completing ticket stops it.

Set the mode with `--gate` or `backlog.gate` in config; set a default directory with
`backlog.dir`. The multi-ticket run is **resumable** — already-`done` tickets are never
re-run. As always, nothing is committed: each ticket leaves its changes in the working tree.

### Workspaces: non-git dirs and multiple repos

aixecutor runs in a single git repo by default, but a task can span more — point it at a
**workspace** root and it discovers the git repos beneath it (and treats the rest as plain
dirs):

```bash
bin/aixecutor --workspace ~/work/org run "rename the User.email field everywhere"
```

The baseline, per-subtask and senior diffs, and the **revert** (the review-checkpoint
amend) all span the whole workspace — every repo and plain dir — using each repo's
`.gitignore` where it exists and a configurable ignore set elsewhere. Subtask file globs are
**workspace-relative** (`repoA/internal/**`, `dirB/...`). It also runs in a **plain non-git
directory** (no git at all). The single-repo path is unchanged. As everywhere, **no git
write commands** run in any repo — restoration is plain file I/O against the run-start
snapshot, including each repo's pre-existing uncommitted changes. Set a default root and the
off-repo ignore set under `workspace.*` in config.

---

## Configuration

No configuration is required. Settings are resolved by **deep-merging three layers**, each
overriding the previous one *granularly* (per-key, not whole-file replacement):

| Layer | Location | Purpose |
|------:|----------|---------|
| 1 | **Hardcoded defaults** (in the binary) | Always-valid baseline. The app runs with zero config files. |
| 2 | `~/.aixecutor/config.yaml` | Your global preferences (default harness, models, loop counts). |
| 3 | `<repo>/.aixecutor/config.yaml` | Per-project overrides for the current repository. |

Configuration is **YAML**. A minimal local override only needs the keys you want to change:

```yaml
# <repo>/.aixecutor/config.yaml — overrides only what it names
roles:
  executor:
    harness: pi          # use the `pi` harness for implementation in this repo
pipeline:
  subtaskReview:
    maxLoops: 5          # allow more remediation cycles here
  execution:
    maxParallel: 2
```

See [CLAUDE.md → Configuration schema](./CLAUDE.md#configuration-schema) for the full schema
and the complete default configuration.

### Harnesses and roles

A **harness** describes how to invoke a coding-agent CLI (command, how the prompt is
delivered, how to parse the result). A **role** binds a pipeline step to a harness + model
+ prompt. Built-in harnesses target **Claude Code** (`claude`) and **pi**, and the generic
CLI adapter means adding another harness is usually configuration-only.

```yaml
roles:
  planner:         { harness: claude, model: opus,   promptTemplate: planner }
  executor:        { harness: claude, model: sonnet, promptTemplate: executor }
  subtaskReviewer: { harness: claude, model: sonnet, promptTemplate: subtask-reviewer }
  seniorReviewer:  { harness: claude, model: opus,   promptTemplate: senior-reviewer }
```

Each harness also has a **retry** policy for *transient* failures (a process that
couldn't start, a timeout, or empty/unparseable output) — bounded, logged, and never
applied to a real result (a reviewer's "not approved" is a valid result, not a failure).
It defaults to one retry with a short backoff:

```yaml
harnesses:
  claude:
    retry:
      maxAttempts: 2   # total attempts incl. the first; 1 = no retry
      backoff: 2s      # base delay between attempts
```

### Observability

A live run prints concise, incremental progress to stdout (phase banners, per-subtask
start/finish with loop counts, review verdicts, senior-review rounds) and writes
structured logs — one record per harness invocation, with a pointer to that
invocation's full output — under each run's `logs/` directory. `-v/--verbose` adds debug
detail; `-q/--quiet` keeps only warnings and errors; output degrades to plain,
line-oriented text on a non-TTY (pipes/CI). Secrets in the environment are never logged.
`aixecutor status <run-id>` shows the phase, per-subtask state and loop counts, the
senior-review status (clean vs. unresolved findings), elapsed time, and the docs path.

---

## Run artifacts

Each run writes a self-contained directory (base path configurable via `paths.runsDir`):

```
.aixecutor/runs/<run-id>/
├── run.yaml                 # run state — makes runs resumable after a crash/interrupt
├── task.md                  # the original task
├── config.snapshot.yaml     # exact merged config used for this run
├── docs/                    # ← the markdown you review
│   ├── plan.md
│   ├── context.md
│   ├── manual-testing.md
│   └── subtasks.yaml        # machine-readable subtask DAG (deps + file ownership)
├── subtasks/<id>/
│   ├── diff.patch
│   └── reviews/round-N.md
├── senior-review/round-N.md
└── logs/
```

Runs are **resumable**: state is persisted after every transition, so `aixecutor resume
<run-id>` continues from the last completed step.

---

## Design principles

- **No required config** — hardcoded defaults make the tool work out of the box.
- **Everything configurable** — every role, harness, model, prompt, and loop bound.
- **Read-only on git** — the app never mutates your repository history. (See banner above.)
- **Plans are for humans** — planning output is markdown on disk, reviewable in your editor.
- **Small, reviewed steps** — decompose, review each subtask, then audit the whole diff.
- **Harness-agnostic** — a generic CLI adapter so new agents are mostly config.
- **Resumable** — durable run state; pick up where an interrupted run left off.
- **Go, single binary** — no runtime dependencies beyond the harness CLIs you choose.

---

## Roadmap

v1 is intentionally light (CLI-first):

- **Foundation** — scaffolding, layered YAML configuration.
- **Harness layer** — generic CLI adapter, Claude Code adapter, pi adapter.
- **Git + diff** — read-only gateway, snapshot/diff engine, opt-in worktree isolation.
- **Run + state** — run model, artifact layout, resumable state persistence.
- **Pipeline** — prompt templates, planning, scheduler/executor, subtask review loop,
  senior review loop, end-to-end orchestrator.
- **UX** — logging, progress, status reporting.

**Deferred (post-v1):** a lightweight TUI for live monitoring, and a web interface.

---

## License

[MIT](./LICENSE) © 2026 Ihor Tytarenko
