package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewStatsCommand // assure type compatibility

// NewStatsCommand creates the 'stats' subcommand.
func NewStatsCommand(a *App) *cobra.Command {
	var (
		topN            int
		redirectTargets bool
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Corpus inventory",
		Long: "Counts by domain, collection, tag and liveness state, plus a date histogram over the\n" +
			"original save dates - which is the view that makes a decade-old archive legible.\n\n" +
			"--redirect-targets instead lists destinations that several links resolve to. A large\n" +
			"number of links collapsing onto one page usually means a parked domain, a generic 404\n" +
			"served with a 200, or a site that moved everything to its front page.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if redirectTargets {
				return a.runRedirectTargets(cmd.Context(), topN)
			}

			return a.runStats(cmd.Context(), topN)
		},
	}

	cmd.Flags().IntVar(&topN, "top", 15,
		"Show at most this many entries per breakdown (0 means all)")
	cmd.Flags().BoolVar(&redirectTargets, "redirect-targets", false,
		"List destinations that more than one link redirects to")

	return cmd
}

func (a *App) runStats(ctx context.Context, topN int) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	o, err := db.Stats(ctx, topN)
	if err != nil {
		return err
	}

	if o.Bookmarks == 0 {
		a.printf("The database has no bookmarks yet. Run `bookscape import <file.html>` first.\n")

		return nil
	}

	a.printf("Corpus\n")
	a.printf("  imports        %d\n", o.Batches)
	a.printf("  bookmarks      %d\n", o.Bookmarks)
	a.printf("  survivors      %d\n", o.Survivors)
	a.printf("  collapsed      %d\n", o.Collapsed)

	if o.Undated > 0 {
		a.printf("  undated        %d  (no ADD_DATE in the export)\n", o.Undated)
	}

	a.printBreakdown("Liveness", o.Liveness, topN)
	a.printBreakdown("Classification", o.Classifications, topN)
	a.printBreakdown("Collections", o.Collections, topN)
	a.printBreakdown("Top domains", o.Domains, topN)
	a.printBreakdown("Top tags", o.Tags, topN)
	a.printBreakdown("Saved per year", o.Years, 0)

	return nil
}

// printBreakdown renders one counted breakdown, with a bar so the shape is visible at a glance.
func (a *App) printBreakdown(title string, counts []store.Count, topN int) {
	if len(counts) == 0 {
		return
	}

	shown := counts
	if topN > 0 && len(shown) > topN {
		shown = shown[:topN]
	}

	width, maxN := 0, 0

	for _, c := range shown {
		if len(c.Label) > width {
			width = len(c.Label)
		}

		if c.N > maxN {
			maxN = c.N
		}
	}

	a.printf("\n%s\n", title)

	for _, c := range shown {
		a.printf("  %-*s  %6d  %s\n", width, c.Label, c.N, bar(c.N, maxN))
	}

	if len(counts) > len(shown) {
		a.printf("  ... and %s more\n", plural(len(counts)-len(shown), "entry", "entries"))
	}
}

// bar renders a proportional bar, at most 40 columns wide so that the output stays readable in
// a standard terminal.
func bar(n, maxN int) string {
	const width = 40

	if maxN <= 0 {
		return ""
	}

	filled := n * width / maxN
	if filled == 0 && n > 0 {
		// Never render a non-zero count as nothing at all.
		filled = 1
	}

	out := make([]byte, filled)
	for i := range out {
		out[i] = '#'
	}

	return string(out)
}

func (a *App) runRedirectTargets(ctx context.Context, topN int) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	targets, err := db.RedirectTargets(ctx, topN)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		a.printf("No destination has more than one link redirecting to it.\n")
		a.printf("\nIf you have not run `bookscape check` yet, there is nothing to report on.\n")

		return nil
	}

	a.printf("Destinations that several links resolve to:\n\n")

	for _, t := range targets {
		a.printf("  %4d  %s\n", t.N, t.FinalURL)
	}

	a.printf("\nA large count usually means a parked domain, a generic 404 served with a 200,\n")
	a.printf("or a site that moved everything to its front page.\n")

	return nil
}
