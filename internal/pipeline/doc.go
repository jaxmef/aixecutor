// Package pipeline is the orchestrator: the state machine, DAG scheduler, and
// phase + loop logic that drive plan → execute → review. It depends on the
// lower packages (config, harness, git, run, prompt, log) and is never imported
// by them. See CLAUDE.md §3.3.
package pipeline
