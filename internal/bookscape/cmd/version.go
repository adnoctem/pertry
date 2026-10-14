package cmd

import (
	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/pkg/stamp"
)

// NewVersionCommand builds `bookscape version`.
//
// The rendering lives in pkg/stamp so that every binary in this repository reports its build
// identically - a bug report against one should look like a bug report against the other.
func NewVersionCommand(a *App) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Long: "Prints the version, commit and toolchain this binary was built from.\n\n" +
			"A binary built outside a release reports \"redacted\" and says so, because a\n" +
			"placeholder version in a bug report is worse than no version at all.",
		Args: cobra.NoArgs,
		// Deliberately overrides the root's setup, which cobra skips when a child defines
		// its own. `version` has to answer when there is no workspace and when the
		// configuration is unparsable, which is frequently exactly when it is run.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if a.Out == nil {
				a.Out = cmd.OutOrStdout()
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			stamps := stamp.New(Name)

			if asJSON {
				return stamps.WriteJSON(a.Out)
			}

			return stamps.WriteText(a.Out)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of text")

	return cmd
}
