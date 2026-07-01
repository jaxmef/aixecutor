// Package cli wires up the cobra command tree for aixecutor. The root command
// owns the shared global flags; each subcommand lives in its own file.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// GlobalOptions holds the persistent flags shared across every subcommand. It
// is populated by cobra during flag parsing and handed to subcommands so they
// can read global configuration without touching package-level state.
type GlobalOptions struct {
	// ConfigPath overrides the repo-level config file location
	// (<repo>/.aixecutor/config.yaml).
	ConfigPath string
	// GlobalConfigPath overrides the user-level config file location
	// (~/.aixecutor/config.yaml).
	GlobalConfigPath string
	// DocsPath overrides where planning docs are written.
	DocsPath string
	// Workspace overrides the workspace root (AIX-0020): the dir to operate over,
	// beneath which git repos are discovered. Empty = single repo / cwd default.
	Workspace string
	// DryRun exercises the pipeline without invoking real agents.
	DryRun bool
	// Verbose enables verbose (debug-level) logging.
	Verbose bool
	// Quiet suppresses info-level logging, leaving warnings/errors only. Mutually
	// exclusive with Verbose in spirit; if both are set, Verbose wins (more detail
	// is the safer surprise when a user explicitly asked to see more).
	Quiet bool
	// NoUpdateCheck disables the startup check for newer releases (AIX-0022).
	NoUpdateCheck bool
	// NoColor disables coloured human output regardless of TTY detection.
	NoColor bool
}

// newRootCmd builds the root command, attaches the persistent global flags to
// the supplied *GlobalOptions, and registers every subcommand. It is the single
// source of the command tree, shared by Execute and by tests.
func newRootCmd(opts *GlobalOptions) *cobra.Command {
	root := &cobra.Command{
		Use:   "aixecutor",
		Short: "Orchestrate AI coding agents through a plan → execute → review pipeline",
		Long: "aixecutor orchestrates AI coding agents through a plan → execute → review\n" +
			"pipeline. It plans a task into a subtask DAG, executes the subtasks with\n" +
			"per-subtask review loops, then runs a senior review over the full diff.\n" +
			"It never commits or pushes — the working tree is left for you.",
		// We print our own errors and manage exit codes via Execute.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	flags := root.PersistentFlags()
	flags.StringVar(&opts.ConfigPath, "config", "",
		"path to the repo config file (default <repo>/.aixecutor/config.yaml)")
	flags.StringVar(&opts.GlobalConfigPath, "global-config", "",
		"path to the user config file (default ~/.aixecutor/config.yaml)")
	flags.StringVar(&opts.DocsPath, "docs-path", "",
		"override the directory where planning docs are written")
	flags.StringVar(&opts.Workspace, "workspace", "",
		"run over a workspace root (discovers git repos beneath it; enables multi-repo / non-git mode)")
	flags.BoolVar(&opts.DryRun, "dry-run", false,
		"exercise the pipeline without invoking real AI agents")
	flags.BoolVarP(&opts.Verbose, "verbose", "v", false,
		"enable verbose (debug) logging")
	flags.BoolVarP(&opts.Quiet, "quiet", "q", false,
		"suppress info logging (warnings and errors only)")
	flags.BoolVar(&opts.NoUpdateCheck, "no-update-check", false,
		"skip the startup check for a newer aixecutor release")
	flags.BoolVar(&opts.NoColor, "no-color", false,
		"disable coloured output")

	root.AddCommand(
		newRunCmd(opts),
		newPlanCmd(opts),
		newResumeCmd(opts),
		newReviewCmd(opts),
		newStopCmd(opts),
		newAmendCmd(opts),
		newStatusCmd(opts),
		newListCmd(opts),
		newBacklogCmd(opts),
		newConfigCmd(opts),
		newVersionCmd(opts),
	)

	return root
}

// Execute runs the root command and returns a process exit code. main.go passes
// the result straight to os.Exit, so all error handling and exit-code policy lives
// here. The exit code is derived from the error (see errors.go): common failures
// (invalid config, missing harness binary, unknown run id) map to stable,
// documented non-zero codes; anything else is the generic code 1. The message is
// always printed to stderr with the program prefix so it is actionable and
// distinct from command stdout.
func Execute() int {
	opts := &GlobalOptions{}
	root := newRootCmd(opts)
	notice := installUpdateCheck(root, opts)
	err := root.Execute()
	printUpdateNotice(notice)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aixecutor: "+err.Error())
		return exitCodeFor(err)
	}
	return exitOK
}
