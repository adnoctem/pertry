package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/dedupe"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewDedupeCommand // assure type compatibility

// NewDedupeCommand creates the 'dedupe' subcommand, which groups bookmarks by comparison key,
// elects a survivor per group and points the rest at it.
func NewDedupeCommand(a *App) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "dedupe",
		Short: "Collapse duplicate bookmarks onto a survivor",
		Long: "Group bookmarks by their normalised comparison key, elect a survivor per group, and point\n" +
			"the rest at it via canonical_id.\n\n" +
			"Nothing is deleted. A collapsed row keeps its own URL and dates and gains a canonical_id;\n" +
			"the export filter is what decides not to ship it. The survivor takes the earliest date in\n" +
			"the group, the union of every original folder path, and the longest title.\n\n" +
			"Re-runnable: running it again after a further import merges the new rows into the existing\n" +
			"survivors, and running it on a settled corpus changes nothing.\n\n" +
			"Use --dry-run first. Stripping \"www.\" and forcing https will merge a small number of hosts\n" +
			"that are genuinely distinct, and that is only visible by reading the proposed merges.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDedupe(cmd.Context(), dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Print the proposed merges without writing anything")

	return cmd
}

func (a *App) runDedupe(ctx context.Context, dryRun bool) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	groups, err := db.DuplicateGroups(ctx)
	if err != nil {
		return err
	}

	plan := dedupe.Build(groups)

	a.Log.Info("built deduplication plan",
		"keys_with_duplicates", plan.Groups, "rows_to_collapse", plan.Collapsed)

	if plan.Empty() {
		a.printf("No duplicates found.\n")

		return nil
	}

	if dryRun {
		return a.printDryRun(plan, groups)
	}

	stats, err := db.ApplyMerges(ctx, plan.Merges)
	if err != nil {
		return err
	}

	if stats.Groups == 0 {
		a.printf("Already deduplicated: nothing changed across %s.\n",
			plural(plan.Groups, "duplicate group", "duplicate groups"))

		return nil
	}

	a.printf("Deduplicated %s.\n", plural(stats.Groups, "group", "groups"))
	a.printf("  %s pointed at a survivor\n", plural(stats.Losers, "row", "rows"))
	a.printf("  %s updated\n", plural(stats.Survivors, "survivor", "survivors"))

	if stats.PathsAdded > 0 {
		a.printf("  %s added to survivors from the union\n", plural(stats.PathsAdded, "folder path", "folder paths"))
	}

	if stats.TagsAdded > 0 {
		a.printf("  %s added to survivors from the union\n", plural(stats.TagsAdded, "tag", "tags"))
	}

	a.printf("\nNext: bookscape check\n")

	return nil
}

func (a *App) printDryRun(plan dedupe.Plan, groups []store.Group) error {
	byKey := make(map[string][]store.Candidate, len(groups))
	for _, g := range groups {
		byKey[g.URLNorm] = g.Candidates
	}

	for _, m := range plan.Merges {
		a.printf("%s", dedupe.Describe(m, byKey[m.URLNorm]))
		a.printf("\n")
	}

	a.printf("%s would collapse onto %s. Nothing was written.\n",
		plural(plan.Collapsed, "row", "rows"), plural(plan.Groups, "survivor", "survivors"))
	a.printf("\nRe-run without --dry-run to apply.\n")

	return nil
}
