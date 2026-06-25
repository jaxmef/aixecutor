package cli

import (
	"github.com/spf13/cobra"
)

// Build metadata, injected at build time via -ldflags, e.g.:
//
//	-X github.com/jaxmef/aixecutor/internal/cli.version=v0.1.0
//	-X github.com/jaxmef/aixecutor/internal/cli.commit=abc1234
//	-X github.com/jaxmef/aixecutor/internal/cli.date=2026-06-23
//
// Defaults are used when the binary is built without ldflags (e.g. `go build`).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd(_ *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Printf("aixecutor %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}
