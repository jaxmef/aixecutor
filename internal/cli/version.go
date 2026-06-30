package cli

import (
	"github.com/spf13/cobra"
)

// Build metadata, injected at build time via -ldflags, e.g.:
//
//	-X github.com/jaxmef/aixecutor/internal/cli.ldflagVersion=v0.1.0
//	-X github.com/jaxmef/aixecutor/internal/cli.ldflagCommit=abc1234
//	-X github.com/jaxmef/aixecutor/internal/cli.ldflagDate=2026-06-23
//
// When built without ldflags (`go build`, or `go install …@version`), these keep
// their dev defaults and resolvedBuildInfo falls back to runtime/debug build
// metadata so a `go install` still reports the real tag / pseudo-version.
var (
	ldflagVersion = "dev"
	ldflagCommit  = "none"
	ldflagDate    = "unknown"
)

func newVersionCmd(_ *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, commit, date := resolvedBuildInfo()
			cmd.Printf("aixecutor %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}
