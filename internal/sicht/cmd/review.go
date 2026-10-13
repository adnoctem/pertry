package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/confidence"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

var _ CLIOpt = NewReviewCommand // assure type compatibility

// NewReviewCommand creates the 'review' subcommand and its topics.
func NewReviewCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review <facet>",
		Short: "Look at what the model was unsure about, and correct it",
		Long: "The CLI fallback for review. The web UI (M3) is the primary surface, because judging a\n" +
			"hundred images is a visual job; this exists so that everything available there is also\n" +
			"available headless.\n\n" +
			"A decision you record here is final: it is stored as a human decision and always wins\n" +
			"over a rule or a model, for that facet, without anything being deleted. What the model\n" +
			"said is kept, so `sicht stats facets --all` can still show where you disagreed.",
		Args:             cobra.MaximumNArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			// Bare `review <facet>` lists, since that is what someone means by it.
			return a.runReviewList(cmd.Context(), args[0], reviewFlags{})
		},
	}

	cmd.AddCommand(newReviewListCommand(a), newReviewSetCommand(a))

	return cmd
}

type reviewFlags struct {
	below  float64
	limit  int
	asJSON bool
}

func newReviewListCommand(a *App) *cobra.Command {
	var flags reviewFlags

	cmd := &cobra.Command{
		Use:              "list <facet>",
		Short:            "The decisions that warrant a look, least confident first",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runReviewList(cmd.Context(), args[0], flags)
		},
	}

	cmd.Flags().Float64Var(&flags.below, "below", 0,
		"Confidence threshold (default: the stage's review_below)")
	cmd.Flags().IntVar(&flags.limit, "limit", 30, "Show at most this many")
	cmd.Flags().BoolVar(&flags.asJSON, "json", false, "Machine-readable output")

	return cmd
}

func newReviewSetCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <path-or-hash> <facet>=<value>",
		Short: "Record your own decision for one file",
		Long: "Record a human decision, which always wins for that facet.\n\n" +
			"The file may be named by its workspace-relative path or by any unique prefix of its\n" +
			"content hash. A path is resolved to the content it holds right now, because decisions\n" +
			"are keyed by content rather than by location - which is what lets them survive the\n" +
			"file being moved.",
		Args:             cobra.ExactArgs(2),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runReviewSet(cmd.Context(), args[0], args[1])
		},
	}

	return cmd
}

func (a *App) runReviewList(ctx context.Context, facet string, flags reviewFlags) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	below := flags.below

	if below <= 0 {
		// The stage that decides this facet sets the threshold; docs/ARCHITECTURE.md#model-stages puts review_below on
		// the stage because what counts as uncertain differs between facets.
		for _, stage := range cfg.Stages {
			for _, field := range stage.DecisionFields() {
				if field == facet && stage.ReviewBelow > 0 {
					below = stage.ReviewBelow
				}
			}
		}
	}

	if below <= 0 {
		below = cfg.Model.ReviewBelow
	}

	if below <= 0 {
		below = 0.75
	}

	limit := flags.limit
	if limit == 0 {
		limit = 30
	}

	return a.withStore(ctx, func(db *store.DB) error {
		items, err := db.ReviewQueue(ctx, facet, below, limit)
		if err != nil {
			return err
		}

		if flags.asJSON {
			encoder := json.NewEncoder(a.Out)
			encoder.SetIndent("", "  ")

			return encoder.Encode(items)
		}

		if len(items) == 0 {
			a.printf("Nothing to review for %q below a confidence of %.2f.\n", facet, below)

			return nil
		}

		a.printf("%d item(s) for %q below %.2f, least confident first.\n\n", len(items), facet, below)

		for _, item := range items {
			score := "  none"
			if item.HasConfidence {
				score = fmt.Sprintf("%6.1f%%", item.Confidence*100)
			}

			a.printf("%s  %-14s %s\n", score, item.Value, item.Path)

			// The alternatives are what make a correction a keystroke rather than a
			// decision from scratch.
			if alts := formatAlternatives(item.AlternativesJSON); alts != "" {
				a.printf("%8s  %-14s or: %s\n", "", "", alts)
			}

			// What the model said it saw is often the most useful thing a reviewer can
			// read, because it shows why it answered as it did.
			if observed := formatObservations(item.PayloadJSON); observed != "" {
				a.printf("%8s  %-14s %s\n", "", "", observed)
			}
		}

		a.printf("\nTo correct one:\n")
		a.printf("  sicht review set %s %s=<value>\n", items[0].Path, facet)

		return nil
	})
}

// formatAlternatives renders the stored alternatives for one line of output.
func formatAlternatives(raw string) string {
	if raw == "" {
		return ""
	}

	var alternatives []confidence.Alternative

	if err := json.Unmarshal([]byte(raw), &alternatives); err != nil {
		return ""
	}

	parts := make([]string, 0, len(alternatives))

	for i, alt := range alternatives {
		if i >= 3 {
			break
		}

		parts = append(parts, fmt.Sprintf("%s %.0f%%", alt.Value, alt.Probability*100))
	}

	return strings.Join(parts, ", ")
}

// formatObservations renders the stage's non-decision fields compactly.
func formatObservations(raw string) string {
	if raw == "" {
		return ""
	}

	var payload map[string]any

	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}

	parts := make([]string, 0, len(payload))

	for _, key := range sortedKeys(payload) {
		text, ok := payload[key].(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}

		const maxLen = 70
		if len(text) > maxLen {
			text = text[:maxLen-1] + "…"
		}

		parts = append(parts, fmt.Sprintf("%s: %s", key, text))
	}

	return strings.Join(parts, " | ")
}

func (a *App) runReviewSet(ctx context.Context, target, assignment string) error {
	facet, value, ok := strings.Cut(assignment, "=")
	if !ok {
		return usagef("expected <facet>=<value>, got %q", assignment)
	}

	facet = strings.TrimSpace(facet)
	value = strings.TrimSpace(value)

	if facet == "" || value == "" {
		return usagef("both a facet and a value are required, got %q", assignment)
	}

	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	db, err := a.openStore(ctx, ws)
	if err != nil {
		return err
	}

	defer func() {
		if err := db.Close(); err != nil {
			a.Log.Warn("closing the database failed", "error", err)
		}
	}()

	hash, err := a.resolveTarget(ctx, db, ws, target)
	if err != nil {
		return err
	}

	// What it was before, so the output can say what changed - and so that recording the
	// same value twice is visibly a no-op rather than silently one.
	previous, err := db.DecisionsForHash(ctx, hash)
	if err != nil {
		return err
	}

	var was *store.Decision

	for i := range previous {
		if previous[i].Facet == facet {
			was = &previous[i]
		}
	}

	err = db.SaveResults(ctx, []store.Result{{
		Hash:  hash,
		Facet: facet,
		Value: value,
		// A human decision carries no confidence: it is not a probability, and storing
		// 1.0 would make it indistinguishable from a model that was very sure.
		Source:    store.SourceHuman,
		CreatedAt: time.Now().UTC(),
	}})
	if err != nil {
		return err
	}

	a.printf("%s = %s\n", facet, value)

	switch {
	case was == nil:
		a.printf("  was undecided\n")
	case was.Source == store.SourceHuman && was.Value == value:
		a.printf("  unchanged\n")
	case was.Value == value:
		a.printf("  confirms the %s result\n", store.SourceKind(was.Source))
	default:
		a.printf("  was %q from %s", was.Value, store.SourceKind(was.Source))

		if was.HasConfidence {
			a.printf(" at %.0f%% confidence", was.Confidence*100)
		}

		a.printf("\n")
	}

	a.printf("\nYour decision wins for this facet. Nothing was deleted: `sicht stats facets --all`\n")
	a.printf("still shows what the model said.\n")

	return nil
}

// resolveTarget turns a path or a hash prefix into a content hash.
func (a *App) resolveTarget(ctx context.Context, db *store.DB, ws interface{ Rel(string) (string, error) }, target string) (string, error) {
	// A path first, since that is what someone copies out of a review listing.
	if hash, ok, err := db.HashForPath(ctx, target); err != nil {
		return "", err
	} else if ok {
		return hash, nil
	}

	// An absolute path, or one relative to the current directory, resolved into the
	// workspace.
	if rel, err := ws.Rel(target); err == nil {
		if hash, ok, err := db.HashForPath(ctx, rel); err != nil {
			return "", err
		} else if ok {
			return hash, nil
		}
	}

	matches, err := db.HashesByPrefix(ctx, strings.ToLower(target))
	if err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", usagef("no file at %q, and no content hash starting with it\n"+
			"hint: paths are relative to the workspace root; `sicht export results` lists them",
			target)
	case 1:
		return matches[0], nil
	default:
		return "", usagef("%q matches %d files; use a longer prefix", target, len(matches))
	}
}
