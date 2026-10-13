package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/store"
)

var _ CLIOpt = NewTaxonomyCommand // assure type compatibility

// NewTaxonomyCommand creates the 'taxonomy' subcommand.
func NewTaxonomyCommand(a *App) *cobra.Command {
	var (
		asJSON   bool
		asYAML   bool
		minCount int
	)

	cmd := &cobra.Command{
		Use:   "taxonomy <facet>",
		Short: "Aggregate a facet's free-text values so they can be curated",
		Long: "List every value recorded for a facet, with counts, and group the ones that differ only\n" +
			"by case, spacing or punctuation.\n\n" +
			"This is step two of the taxonomy workflow. Run an open-mode stage, aggregate here,\n" +
			"curate into sicht.yaml, then switch the stage to closed and re-run with --rerun.\n\n" +
			"Grouping is lowercase-and-trim plus punctuation, nothing cleverer. Embeddings are out\n" +
			"of scope, and a near-miss the tool cannot see is one you will see in the list.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTaxonomy(cmd.Context(), args[0], asJSON, asYAML, minCount)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&asYAML, "yaml", false, "Emit a pasteable taxonomies block")
	cmd.Flags().IntVar(&minCount, "min-count", 1,
		"Only include values seen at least this many times")

	return cmd
}

// normaliseLabel is the grouping key docs/ARCHITECTURE.md#model-stages specifies: lowercase, trimmed, and with punctuation and
// runs of whitespace reduced, so that "Twitter / X", "twitter/x" and "twitter x" group together.
func normaliseLabel(s string) string {
	var b strings.Builder

	lastSpace := false

	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r > 0x7f:
			b.WriteRune(r)

			lastSpace = false
		default:
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')

				lastSpace = true
			}
		}
	}

	return strings.TrimSpace(b.String())
}

// labelGroup is a set of raw values that normalise to the same key.
type labelGroup struct {
	Key string `json:"key"`

	// Variants are the raw values, most common first.
	Variants []store.FacetValueCount `json:"variants"`

	Count int `json:"count"`
}

func (a *App) runTaxonomy(ctx context.Context, facet string, asJSON, asYAML bool, minCount int) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	return a.withStore(ctx, func(db *store.DB) error {
		values, err := db.FacetValues(ctx, facet)
		if err != nil {
			return err
		}

		if len(values) == 0 {
			a.printf("Nothing has been recorded for %q yet.\n", facet)
			a.printf("\nRun the stage that decides it:\n  sicht run %s\n", facet)

			return nil
		}

		byKey := map[string]*labelGroup{}

		var order []string

		for _, v := range values {
			key := normaliseLabel(v.Value)

			group := byKey[key]
			if group == nil {
				group = &labelGroup{Key: key}
				byKey[key] = group

				order = append(order, key)
			}

			group.Variants = append(group.Variants, v)
			group.Count += v.Count
		}

		groups := make([]labelGroup, 0, len(order))

		for _, key := range order {
			if byKey[key].Count < minCount {
				continue
			}

			groups = append(groups, *byKey[key])
		}

		sort.SliceStable(groups, func(i, j int) bool {
			if groups[i].Count != groups[j].Count {
				return groups[i].Count > groups[j].Count
			}

			return groups[i].Key < groups[j].Key
		})

		switch {
		case asJSON:
			encoder := json.NewEncoder(a.Out)
			encoder.SetIndent("", "  ")

			return encoder.Encode(groups)

		case asYAML:
			return a.printTaxonomyYAML(facet, groups, cfg.Taxonomies[facet])
		}

		return a.printTaxonomyTable(facet, groups, cfg.Taxonomies[facet])
	})
}

func (a *App) printTaxonomyTable(facet string, groups []labelGroup, curated []string) error {
	total := 0
	for _, g := range groups {
		total += g.Count
	}

	a.printf("%d distinct label(s) for %q, over %d result(s)\n\n", len(groups), facet, total)

	known := map[string]bool{}
	for _, c := range curated {
		known[normaliseLabel(c)] = true
	}

	for _, g := range groups {
		marker := "  "
		if len(curated) > 0 && !known[g.Key] {
			// Already-curated labels are the uninteresting ones; the new arrivals are
			// what a curation pass is for.
			marker = a.paint("+ ", "32")
		}

		share := 0.0
		if total > 0 {
			share = float64(g.Count) * 100 / float64(total)
		}

		a.printf("%s%-30s %6d  %5.1f%%", marker, g.Key, g.Count, share)

		// Variants that normalise together are shown, because the merge is the decision
		// the user is being asked to make.
		if len(g.Variants) > 1 {
			raw := make([]string, 0, len(g.Variants))
			for _, v := range g.Variants {
				raw = append(raw, fmt.Sprintf("%q (%d)", v.Value, v.Count))
			}

			a.printf("   <- %s", strings.Join(raw, ", "))
		}

		a.printf("\n")
	}

	if len(curated) > 0 {
		a.printf("\n%s already curated; %s marks a label that is not in sicht.yaml yet.\n",
			fmt.Sprintf("%d label(s)", len(curated)), a.paint("+", "32"))
	}

	if len(groups) > maxEnumValues {
		a.printf("\n%s\n", a.paint(fmt.Sprintf(
			"There are %d labels, and docs/ARCHITECTURE.md#model-stages caps a facet at %d. Split the facet into separate "+
				"stages rather than widening one enum.", len(groups), maxEnumValues), "33"))
	}

	a.printf("\nTo curate:\n  sicht taxonomy %s --yaml >> notes.yaml\n", facet)
	a.printf("then merge, rename and drop by hand, put the result in sicht.yaml under\n")
	a.printf("`taxonomies.%s`, switch the stage to `mode: closed` and run:\n", facet)
	a.printf("  sicht run %s --rerun\n", facet)

	return nil
}

// maxEnumValues mirrors docs/ARCHITECTURE.md#model-stages's cap, which config validation also enforces.
const maxEnumValues = 50

func (a *App) printTaxonomyYAML(facet string, groups []labelGroup, curated []string) error {
	a.printf("# Generated by `sicht taxonomy %s --yaml`.\n", facet)
	a.printf("#\n")
	a.printf("# Curate by hand before using this: merge the near-duplicates, rename the awkward\n")
	a.printf("# ones, and delete the labels you do not want to file by. The list is what the model\n")
	a.printf("# will be constrained to, so a label left in is a label it will use.\n")
	a.printf("#\n")
	a.printf("# docs/ARCHITECTURE.md#model-stages caps a facet at %d values. Beyond that, split it into separate stages.\n", maxEnumValues)
	a.printf("taxonomies:\n")
	a.printf("  %s:\n", facet)

	for i, g := range groups {
		if i >= maxEnumValues {
			a.printf("  # %d more label(s) omitted: the cap is %d.\n", len(groups)-i, maxEnumValues)

			break
		}

		note := fmt.Sprintf("# %d", g.Count)
		if len(g.Variants) > 1 {
			note += fmt.Sprintf(", merged from %d variants", len(g.Variants))
		}

		a.printf("    - %-28s %s\n", fmt.Sprintf("%q", g.Key), note)
	}

	if len(curated) > 0 {
		a.printf("#\n")
		a.printf("# The current sicht.yaml has %d label(s) for this facet. Replacing the list\n", len(curated))
		a.printf("# wholesale will discard any you curated by hand that the model has not produced\n")
		a.printf("# again, so merge rather than overwrite.\n")
	}

	return nil
}
