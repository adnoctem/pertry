package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/audit"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
)

var _ CLIOpt = NewAuditCommand // assure type compatibility

// NewAuditCommand creates the 'audit' subcommand, which inspects an export without importing it.
func NewAuditCommand(a *App) *cobra.Command {
	opts := audit.Options{Top: 20, Suggest: 12, MinSuggest: 3}

	cmd := &cobra.Command{
		Use:   "audit <file.html>",
		Short: "Inspect an export and report what the rules would and would not resolve",
		Long: "Read a Netscape bookmark export and report what is in it, what the configured rules\n" +
			"would resolve, and which rules would fix the rest.\n\n" +
			"Nothing is imported, nothing is written, and no model is called. `audit` is a pure\n" +
			"function of the file and the configuration, so you can re-run it freely while editing\n" +
			"classify.rules and watch the coverage number move.\n\n" +
			"That is the point. The default rule set is written for nobody's corpus in particular - on\n" +
			"a real decade-old export it resolved under a quarter of the links, because the folders\n" +
			"were named things like \"Products\" and \"DSGN\" rather than \"Dev\". Every link a rule\n" +
			"resolves is a link not spent on tokens, and this is the cheap way to find the rules worth\n" +
			"writing instead of running the model pass and reading the bill.\n\n" +
			"It also reports the two conditions that make `import` refuse a file - a parsed count that\n" +
			"disagrees with the anchors in the file, and timestamps outside a plausible range - so you\n" +
			"can check an export is usable before committing it to a database.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runAudit(cmd.Context(), args[0], opts)
		},
	}

	cmd.Flags().IntVar(&opts.Top, "top", opts.Top,
		"Show at most this many entries per breakdown (0 means all)")
	cmd.Flags().IntVar(&opts.Suggest, "suggest", opts.Suggest,
		"Propose at most this many rules for the uncovered remainder (0 disables)")
	cmd.Flags().IntVar(&opts.MinSuggest, "min-links", opts.MinSuggest,
		"Only propose a rule for a domain or folder holding at least this many links")

	return cmd
}

func (a *App) runAudit(_ context.Context, path string, opts audit.Options) error {
	entries, parsed, err := netscape.ParseFile(path)
	if err != nil {
		return err
	}

	a.Log.Debug("parsed bookmark file for audit",
		"path", path, "entries", len(entries), "folders", parsed.Folders)

	report := audit.Analyze(entries, parsed, a.Config, opts)

	return audit.Render(a.Out, report, opts)
}
