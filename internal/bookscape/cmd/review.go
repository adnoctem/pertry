package cmd

import (
	"context"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/review"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewReviewCommand // assure type compatibility

// NewReviewCommand creates the 'review' subcommand and its children.
func NewReviewCommand(a *App) *cobra.Command {
	var (
		only   = string(store.ReviewFilterPending)
		sample int
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Inspect classifications before export",
		Long: "Print classified bookmarks for inspection, lowest confidence first.\n\n" +
			"This is the gate in front of the irreversible step. Linkwarden creates collections as a\n" +
			"side effect of importing, so unwinding a bad full run by hand is genuinely tedious - and a\n" +
			"model classification below classify.min_confidence does not reach the export until a human\n" +
			"has confirmed it here.\n\n" +
			"Confirm with `bookscape review accept`, or correct with `bookscape review set`. Either way\n" +
			"the result becomes a manual decision, which no later pass will overwrite.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			filter, err := store.ParseReviewFilter(only)
			if err != nil {
				return usagef("%w\nhint: --only must be one of %s", err, reviewFilterList())
			}

			return a.runReview(cmd.Context(), filter, sample)
		},
	}

	cmd.Flags().StringVar(&only, "only", only,
		"Which bookmarks to show: "+reviewFilterList())
	cmd.Flags().IntVar(&sample, "sample", 25,
		"Show at most this many bookmarks (0 means all)")

	cmd.AddCommand(newReviewSetCommand(a), newReviewAcceptCommand(a))

	return cmd
}

func reviewFilterList() string {
	names := make([]string, 0, len(store.ReviewFilters))
	for _, f := range store.ReviewFilters {
		names = append(names, string(f))
	}

	return strings.Join(names, ", ")
}

func (a *App) runReview(ctx context.Context, filter store.ReviewFilter, sample int) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	items, err := db.ReviewItems(ctx, filter, a.Config.Classify.MinConfidence, sample)
	if err != nil {
		return err
	}

	if len(items) == 0 {
		a.printf("Nothing to review in scope %q.\n", filter)

		return a.printReviewGate(ctx, db)
	}

	if err := review.Table(a.Out, items); err != nil {
		return err
	}

	a.printf("%s\n", review.Legend)
	a.printf("\n")

	return a.printReviewGate(ctx, db)
}

// printReviewGate says whether the export is blocked and how to unblock it.
func (a *App) printReviewGate(ctx context.Context, db *store.DB) error {
	pending, err := db.PendingReviewCount(ctx, a.Config.Classify.MinConfidence)
	if err != nil {
		return err
	}

	if pending == 0 {
		a.printf("Nothing is blocking the export.\n\nNext: bookscape export\n")

		return nil
	}

	a.printf("%s below min_confidence (%.2f) and will not be exported until confirmed.\n",
		plural(pending, "model classification is", "model classifications are"), a.Config.Classify.MinConfidence)
	a.printf("\nConfirm them with `bookscape review accept --all`, or correct one with\n")
	a.printf("`bookscape review set <id> collection=<name>`.\n")

	return nil
}

func newReviewSetCommand(a *App) *cobra.Command {
	var replaceTags bool

	cmd := &cobra.Command{
		Use:   "set <bookmark-id> <key>=<value>...",
		Short: "Record a human decision for one bookmark",
		Long: "Record a human decision, which becomes the authoritative classification: the source is set\n" +
			"to 'manual' and no later pass will overwrite it.\n\n" +
			"Keys: collection, description, tags (comma-separated).\n\n" +
			"Only the keys you pass are changed. Correcting the collection leaves a generated\n" +
			"description that was fine exactly as it was.\n\n" +
			"Examples:\n" +
			"  bookscape review set 412 collection=Infrastructure\n" +
			"  bookscape review set 412 collection=Reference tags=dns,networking\n" +
			"  bookscape review set 412 description=\"The DNS RFC index\"",
		Args:             cobra.MinimumNArgs(2),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return usagef("%q is not a bookmark id\nhint: ids are the first column of `bookscape review`", args[0])
			}

			decision := store.ManualDecision{BookmarkID: id, ReplaceTags: replaceTags}

			for _, assignment := range args[1:] {
				key, value, ok := strings.Cut(assignment, "=")
				if !ok {
					return usagef("%q is not a key=value assignment\nhint: keys are collection, description, tags", assignment)
				}

				switch strings.ToLower(strings.TrimSpace(key)) {
				case "collection":
					decision.Collection = strings.TrimSpace(value)
				case "description":
					decision.Description = strings.TrimSpace(value)
				case "tags":
					for _, tag := range strings.Split(value, ",") {
						if tag = strings.TrimSpace(tag); tag != "" {
							decision.Tags = append(decision.Tags, tag)
						}
					}
				default:
					return usagef("unknown key %q\nhint: keys are collection, description, tags", key)
				}
			}

			return a.runReviewSet(cmd.Context(), decision)
		},
	}

	cmd.Flags().BoolVar(&replaceTags, "replace-tags", false,
		"Remove the bookmark's existing tags instead of adding to them")

	return cmd
}

func (a *App) runReviewSet(ctx context.Context, decision store.ManualDecision) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// A collection outside the configured set is the one mistake worth refusing: it would
	// create a collection in Linkwarden as a side effect of the import, which is exactly
	// what the closed set exists to prevent.
	if decision.Collection != "" && !a.Config.Classify.HasCollection(decision.Collection) {
		return usagef("collection %q is not in classify.collections %v\n"+
			"hint: add it to bookscape.yaml first, or pick one of the configured names",
			decision.Collection, a.Config.Classify.Collections)
	}

	if err := db.RecordManual(ctx, decision); err != nil {
		return err
	}

	a.printf("Recorded a manual decision for bookmark %d.\n", decision.BookmarkID)

	return a.printReviewGate(ctx, db)
}

func newReviewAcceptCommand(a *App) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "accept [bookmark-id...]",
		Short: "Confirm model classifications as they stand",
		Long: "Confirm model classifications without changing them. The source becomes 'manual', which is\n" +
			"what clears a low-confidence result for export.\n\n" +
			"Pass --all to confirm everything currently below min_confidence. Read the table first:\n" +
			"accepting a batch unseen defeats the point of the gate.",
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return usagef("pass either --all or a list of ids, not both")
			}

			if !all && len(args) == 0 {
				return usagef("nothing to accept\nhint: pass bookmark ids, or --all to accept everything below min_confidence")
			}

			ids := make([]int64, 0, len(args))

			for _, arg := range args {
				id, err := strconv.ParseInt(arg, 10, 64)
				if err != nil {
					return usagef("%q is not a bookmark id\nhint: ids are the first column of `bookscape review`", arg)
				}

				ids = append(ids, id)
			}

			return a.runReviewAccept(cmd.Context(), ids, all)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false,
		"Accept every model classification currently below min_confidence")

	return cmd
}

func (a *App) runReviewAccept(ctx context.Context, ids []int64, all bool) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if all {
		pending, err := db.PendingReviewIDs(ctx, a.Config.Classify.MinConfidence)
		if err != nil {
			return err
		}

		if len(pending) == 0 {
			a.printf("Nothing is below min_confidence (%.2f).\n", a.Config.Classify.MinConfidence)

			return a.printReviewGate(ctx, db)
		}

		ids = pending
	}

	accepted, err := db.Accept(ctx, ids)
	if err != nil {
		return err
	}

	a.printf("Accepted %s.\n", plural(accepted, "classification", "classifications"))

	if skipped := len(ids) - accepted; skipped > 0 {
		// Already manual, or no classification to accept. Not an error, but silence here
		// would look like the command had done more than it did.
		a.printf("  %s were already manual or had nothing to accept.\n",
			plural(skipped, "bookmark", "bookmarks"))
	}

	return a.printReviewGate(ctx, db)
}
