package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/formr/analysis"
	"github.com/adnoctem/pertry/internal/formr/report"
	"github.com/adnoctem/pertry/internal/formr/schema"
	"github.com/adnoctem/pertry/pkg/stamp"
)

// NewSchemaCommand builds `formr schema`.
func NewSchemaCommand(a *App) *cobra.Command {
	var src sourceFlags
	var output string

	cmd := &cobra.Command{
		Use:   "schema [dir]",
		Short: "Generate SCHEMA.md for a module",
		Long: "Writes a truthful report even when the module has gaps or formr could not analyse\n" +
			"everything: gaps are listed, not hidden. Writing a report certifies nothing; `check`\n" +
			"is what evaluates it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSchema(cmd.Context(), singleDir(args), src, output)
		},
	}

	src.register(cmd)
	src.registerRender(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write the report here instead of standard output")

	return cmd
}

func (a *App) runSchema(ctx context.Context, dir string, src sourceFlags, output string) error {
	acq, err := a.acquire(ctx, dir, src)
	if err != nil {
		return err
	}
	text, _, err := a.build(dir, acq, src.maxWidth)
	if err != nil {
		return err
	}

	if output == "" || output == "-" {
		fmt.Fprint(a.Out, text)

		return nil
	}

	return os.WriteFile(output, []byte(text), 0o644)
}

// maxListed bounds how many changed rows check prints; a provider upgrade can change hundreds.
const maxListed = 60

// policyDefault is the only policy: a complete inventory with nothing unresolved.
const policyDefault = "default"

// NewCheckCommand builds `formr check`.
func NewCheckCommand(a *App) *cobra.Command {
	var src sourceFlags
	var reportPath, policy string

	cmd := &cobra.Command{
		Use:   "check [dir]",
		Short: "Fail when SCHEMA.md drifts or a coverage policy is not met",
		Long: "Regenerates the report in memory and compares it with the committed one, then\n" +
			"evaluates the policy against the regenerated analysis - never against the file, so\n" +
			"rewriting the report cannot make a coverage failure pass. Writes nothing.\n\n" +
			"Policies:\n" +
			"  default  the inventory is complete and nothing is unresolved\n\n" +
			"An omitted field is not a policy failure. Adding or removing an assignment changes the\n" +
			"report, so it fails as drift and is reviewed as a change to SCHEMA.md.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runCheck(cmd.Context(), singleDir(args), src, reportPath, policy)
		},
	}

	src.register(cmd)
	src.registerRender(cmd)
	cmd.Flags().StringVar(&reportPath, "report", "SCHEMA.md", "The committed report to compare against")
	cmd.Flags().StringVar(&policy, "policy", policyDefault, "Coverage policy (only default exists)")

	return cmd
}

func (a *App) runCheck(ctx context.Context, dir string, src sourceFlags, reportPath, policy string) error {
	switch policy {
	case policyDefault:
	default:
		return fmt.Errorf("unknown policy %q", policy)
	}

	committed, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("read the committed report: %w", err)
	}

	acq, err := a.acquire(ctx, dir, src)
	if err != nil {
		return err
	}
	text, res, err := a.build(dir, acq, src.maxWidth)
	if err != nil {
		return err
	}

	var problems []string
	if string(committed) != text {
		changes := report.Changes(string(committed), text)
		problems = append(problems, fmt.Sprintf("%s is out of date (%d rows differ); regenerate it with `formr schema`", reportPath, len(changes)))
		for i, c := range changes {
			if i == maxListed {
				fmt.Fprintf(a.Err, "  ... %d more\n", len(changes)-i)
				break
			}
			fmt.Fprintf(a.Err, "  %-7s %s: %s\n", c.Kind, c.Section, c.Row)
			for _, col := range c.Columns {
				fmt.Fprintf(a.Err, "            %s\n", col)
			}
		}
		if len(changes) == 0 {
			// Only whitespace or ordering moved; say so rather than reporting nothing.
			fmt.Fprintf(a.Err, "  the rows are identical but the bytes are not; was the file reformatted, or --max-width changed?\n")
		}
	}
	problems = append(problems, evaluateDefault(res)...)

	if len(problems) > 0 {
		return &failure{msg: strings.Join(problems, "\n")}
	}
	fmt.Fprintf(a.Out, "%s is current and the %s policy is met.\n", reportPath, policy)

	return nil
}

// evaluateDefault is the default policy: a complete inventory with nothing unresolved.
func evaluateDefault(res *analysis.Result) []string {
	var problems []string
	if res.Diagnostics.HasErrors() {
		problems = append(problems, "the inventory is incomplete; see the errors above")
	}

	unresolved := 0
	for _, d := range res.Declarations {
		for _, f := range d.Fields {
			if f.Input == analysis.Unresolved {
				unresolved++
			}
		}
	}
	for _, p := range res.Providers {
		for _, f := range p.Fields {
			if f.Input == analysis.Unresolved {
				unresolved++
			}
		}
	}
	if unresolved > 0 {
		problems = append(problems, fmt.Sprintf("%d field(s) have unresolved mappings", unresolved))
	}

	return problems
}

// NewSnapshotCommand builds `formr snapshot`.
func NewSnapshotCommand(a *App) *cobra.Command {
	var src sourceFlags
	var output string

	cmd := &cobra.Command{
		Use:   "snapshot [dir]",
		Short: "Save the module's provider schemas with their provenance",
		Long: "Writes the schemas the engine reports for the module, with the engine and the provider\n" +
			"releases the lock file selects, so that `--schema-file` can analyse without an engine.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSnapshot(cmd.Context(), singleDir(args), src, output)
		},
	}

	src.register(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "", "Where to write the snapshot")

	return cmd
}

func (a *App) runSnapshot(ctx context.Context, dir string, src sourceFlags, output string) error {
	if src.schemaFile != "" {
		return errors.New("snapshot reads from the engine; --schema-file makes no sense here")
	}
	if output == "" {
		return errors.New("--output is required")
	}

	acq, err := a.acquire(ctx, dir, src)
	if err != nil {
		return err
	}
	data, err := schema.WriteSnapshot(acq.raw, acq.prov)
	if err != nil {
		return err
	}

	return os.WriteFile(output, data, 0o644)
}

// NewVersionCommand builds `formr version`.
func NewVersionCommand(a *App) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
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
