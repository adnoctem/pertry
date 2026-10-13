package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/doctor"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

var _ CLIOpt = NewDoctorCommand // assure type compatibility

// NewDoctorCommand creates the 'doctor' subcommand.
func NewDoctorCommand(a *App) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the environment sicht needs is present",
		Long: "Check everything sicht depends on and say what to do about whatever is missing.\n\n" +
			"Each check reports ok, warn or fail. A warning means sicht will work but something will\n" +
			"be slower, less accurate or less safe than it should be; a failure means it cannot do its\n" +
			"job until you fix it. Only a failure sets the exit code, which is 3 - distinct from a\n" +
			"usage error so that a wrapper can tell \"fix your machine\" from \"fix your command\".\n\n" +
			"A check whose precondition failed is reported as skipped rather than passed or failed,\n" +
			"so one unreachable server does not produce five failures that all mean the same thing.\n\n" +
			"Run it after `sicht init`, and again whenever something behaves oddly.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor(cmd.Context(), asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the report as JSON")

	return cmd
}

func (a *App) runDoctor(ctx context.Context, asJSON bool) error {
	opts := doctor.Options{
		Workspace:    a.Workspace,
		Config:       a.Config,
		OpenDatabase: a.databaseOpener(),
		MinDiskFree:  doctor.DefaultMinDiskFree,
	}

	// Constructing the backend resolves OLLAMA_HOST but contacts nothing, so a failure here
	// is a configuration problem rather than an unreachable server, and the two are reported
	// differently.
	backend, err := ollamabackend.New()
	if err != nil {
		opts.BackendErr = err
	} else {
		opts.Backend = backend
	}

	report := doctor.Run(ctx, opts)

	if asJSON {
		if err := report.WriteJSON(a.Out); err != nil {
			return err
		}
	} else if err := report.WriteText(a.Out, a.color()); err != nil {
		return err
	}

	if report.Failed() {
		// The report has already said what is wrong and how to fix it, so this exists only
		// to set the exit code without printing a second, vaguer version of the same thing.
		return notReadyf("the environment is not ready; see the report above")
	}

	return nil
}
