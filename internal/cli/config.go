package cli

import (
	"fmt"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/spf13/cobra"
)

// newConfigCmd builds the `config` command group: show (effective merged config
// with provenance), path (resolved file locations), and init (scaffold a local
// config file). With no subcommand it prints help.
func newConfigCmd(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and scaffold configuration",
		Long: "Inspect the effective (merged) configuration and the files it is\n" +
			"loaded from, or scaffold a local config file. Configuration is layered:\n" +
			"built-in defaults -> ~/.aixecutor/config.yaml -> <repo>/.aixecutor/config.yaml -> flags.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(
		newConfigShowCmd(opts),
		newConfigPathCmd(opts),
		newConfigInitCmd(opts),
	)
	return cmd
}

// loadOptionsFromGlobals maps the shared CLI flags onto config.LoadOptions.
// --docs-path is wired to paths.runsDir inside the loader (see config.Load).
func loadOptionsFromGlobals(opts *GlobalOptions) config.LoadOptions {
	return config.LoadOptions{
		GlobalConfigPath: opts.GlobalConfigPath,
		LocalConfigPath:  opts.ConfigPath,
		DocsPathOverride: opts.DocsPath,
	}
}

// loadConfig resolves the layered configuration and classifies any failure as a
// configuration error (exitConfig), so an invalid/unparseable config file or a
// failed Validate() exits with a stable, non-generic code and the loader's
// already-actionable message. It is the single config-load entrypoint the
// pipeline/inspection commands use.
func loadConfig(opts *GlobalOptions) (config.Config, []config.Source, error) {
	cfg, sources, err := config.Load(loadOptionsFromGlobals(opts))
	if err != nil {
		return cfg, sources, withExit(exitConfig, err)
	}
	return cfg, sources, nil
}

func newConfigShowCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective merged config (annotated with each value's origin)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, sources, err := loadConfig(opts)
			if err != nil {
				return err
			}
			out, err := config.Render(cfg, sources)
			if err != nil {
				return err
			}
			c.Print(out)
			return nil
		},
	}
}

func newConfigPathCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "List the config files that would be loaded and whether each exists",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			locs, err := loadOptionsFromGlobals(opts).Locations()
			if err != nil {
				return err
			}
			for _, l := range locs {
				path := l.Path
				if path == "" {
					path = "(not found)"
				}
				status := "missing"
				if l.Exists {
					status = "exists"
				}
				c.Printf("%-7s %-7s %s\n", l.Origin.String(), status, path)
			}
			return nil
		},
	}
}

func newConfigInitCmd(opts *GlobalOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented local .aixecutor/config.yaml in the current directory",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			res, err := config.WriteScaffold(".", force)
			if err != nil {
				return err
			}
			if !res.Created {
				return fmt.Errorf("%s already exists; use --force to overwrite", res.Path)
			}
			c.Printf("wrote %s\n", res.Path)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing config file")
	return cmd
}
