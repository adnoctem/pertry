package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/rules"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

var _ CLIOpt = NewStatsCommand // assure type compatibility

// NewStatsCommand creates the 'stats' subcommand and its topics.
func NewStatsCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Report on what has been indexed and decided",
		Long: "Summarise the workspace. With no topic, prints an overview.\n\n" +
			"`stats dims` is the one to reach for first: it lists the pixel sizes present in your\n" +
			"own files, which is how `screen_resolutions` gets populated. sicht ships that list\n" +
			"empty on purpose, because a rule must not match an invented device resolution.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatsOverview(cmd.Context())
		},
	}

	cmd.AddCommand(newStatsFacetsCommand(a), newStatsDimsCommand(a), newStatsDupesCommand(a))

	return cmd
}

func newStatsFacetsCommand(a *App) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "facets",
		Short: "What has been decided, by facet and value",
		Long: "Report the decisions that hold, grouped by facet and value, with the source that\n" +
			"produced each.\n\n" +
			"By default this reads the decisions that won - human over rule over model. With --all it\n" +
			"reads every opinion ever recorded, including the ones precedence discards, which is how\n" +
			"you see where a human overrode a model.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatsFacets(cmd.Context(), all)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false,
		"Include every recorded opinion, not just the ones that won")

	return cmd
}

func newStatsDimsCommand(a *App) *cobra.Command {
	var (
		top       int
		where     string
		yaml      bool
		facet     string
		pure      string
		minPurity float64
		minFiles  int
	)

	cmd := &cobra.Command{
		Use:   "dims",
		Short: "Pixel sizes present, most common first",
		Long: "List the pixel dimensions of your files, so that `screen_resolutions` can be filled in\n" +
			"from real data rather than from a list of device specifications off the internet.\n\n" +
			"Narrow it with --where, using the same expression language and the same variables a rule\n" +
			"sees. The useful filter is the one that isolates files a screenshot rule could not\n" +
			"already decide:\n\n" +
			"  sicht stats dims --where '!has_tag(exif, \"Make\")'\n\n" +
			"With --yaml the output is a pasteable screen_resolutions block.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatsDims(cmd.Context(), dimsFlags{
				top: top, where: where, asYAML: yaml,
				facet: facet, value: pure, minPurity: minPurity, minFiles: minFiles,
			})
		},
	}

	cmd.Flags().IntVar(&top, "top", 25, "Show this many sizes")
	cmd.Flags().StringVar(&where, "where", "", "Only count files matching this expression")
	cmd.Flags().BoolVar(&yaml, "yaml", false, "Emit a pasteable screen_resolutions block")
	cmd.Flags().StringVar(&facet, "facet", "kind",
		"Break each size down by this facet's decided values, to show how pure it is")
	cmd.Flags().StringVar(&pure, "value", "screenshot",
		"The facet value a size should mean, for the purity column and --yaml")
	cmd.Flags().Float64Var(&minPurity, "min-evidence", 0.95,
		"--yaml includes only sizes with at least this share of known files (0 to 1)")
	cmd.Flags().IntVar(&minFiles, "min-files", 5,
		"--yaml includes only sizes backed by at least this many known files")

	return cmd
}

func newStatsDupesCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dupes",
		Short: "The same content, or the same photo, more than once",
		Long: "Report files that are byte-identical, and separately report the same photo exported in\n" +
			"several formats.\n\n" +
			"Neither is acted on. Byte-identical files are one file in two places and `apply` moves\n" +
			"the first; format twins are genuinely separate files with separate decisions, because\n" +
			"their bytes differ and so do their hashes.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatsDupes(cmd.Context())
		},
	}

	return cmd
}

// withStore runs fn against an open database, for the read-only reports.
func (a *App) withStore(ctx context.Context, fn func(*store.DB) error) error {
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

	return fn(db)
}

func (a *App) runStatsOverview(ctx context.Context) error {
	return a.withStore(ctx, func(db *store.DB) error {
		counts, err := db.Count(ctx)
		if err != nil {
			return err
		}

		bytes, err := db.TotalBytes(ctx)
		if err != nil {
			return err
		}

		a.printf("%-22s %d\n", "distinct files", counts.Files)
		a.printf("%-22s %d\n", "locations", counts.Locations)
		a.printf("%-22s %s\n", "total size", humanBytes(bytes))
		a.printf("%-22s %d\n", "recorded opinions", counts.Results)
		a.printf("%-22s %d\n", "decisions holding", counts.Decided)

		if counts.Files == 0 {
			a.printf("\nNothing scanned yet. Run `sicht scan`.\n")

			return nil
		}

		extensions, err := db.Extensions(ctx)
		if err != nil {
			return err
		}

		a.printf("\nBy extension:\n")

		for _, e := range extensions {
			a.printf("  %-8s %6d  %10s\n", e.Ext, e.Count, humanBytes(e.Bytes))
		}

		facets, err := db.FacetSummary(ctx)
		if err != nil {
			return err
		}

		if len(facets) == 0 {
			a.printf("\nNothing decided yet. Run `sicht rules`.\n")

			return nil
		}

		a.printf("\nDecided, by facet:\n")

		perFacet := map[string]int{}
		for _, f := range facets {
			perFacet[f.Facet] += f.Count
		}

		for _, facet := range sortedKeys(perFacet) {
			decided := perFacet[facet]

			a.printf("  %-12s %6d of %d  (%.1f%%)\n", facet, decided, counts.Files,
				float64(decided)*100/float64(counts.Files))
		}

		return nil
	})
}

func (a *App) runStatsFacets(ctx context.Context, all bool) error {
	return a.withStore(ctx, func(db *store.DB) error {
		var (
			rows []store.FacetCount
			err  error
		)

		if all {
			rows, err = db.ResultSummary(ctx)
		} else {
			rows, err = db.FacetSummary(ctx)
		}

		if err != nil {
			return err
		}

		if len(rows) == 0 {
			a.printf("Nothing decided yet. Run `sicht rules`.\n")

			return nil
		}

		if all {
			a.printf("Every recorded opinion, including those precedence discards.\n\n")
		}

		// Group by facet, then by value, keeping sources visible: which source decided
		// something is as interesting as what was decided.
		byFacet := map[string]map[string]map[string]int{}

		for _, r := range rows {
			if byFacet[r.Facet] == nil {
				byFacet[r.Facet] = map[string]map[string]int{}
			}

			if byFacet[r.Facet][r.Value] == nil {
				byFacet[r.Facet][r.Value] = map[string]int{}
			}

			byFacet[r.Facet][r.Value][r.Source] += r.Count
		}

		for _, facet := range sortedKeys(byFacet) {
			total := 0
			for _, sources := range byFacet[facet] {
				for _, n := range sources {
					total += n
				}
			}

			a.printf("%s  (%d)\n", facet, total)

			values := byFacet[facet]

			ordered := sortedKeys(values)
			sort.SliceStable(ordered, func(i, j int) bool {
				return sumOf(values[ordered[i]]) > sumOf(values[ordered[j]])
			})

			for _, value := range ordered {
				n := sumOf(values[value])

				a.printf("  %-26s %6d  %5.1f%%   %s\n", value, n,
					float64(n)*100/float64(total), describeSources(values[value]))
			}

			a.printf("\n")
		}

		disagreements, err := db.Disagreements(ctx)
		if err != nil {
			return err
		}

		if len(disagreements) > 0 {
			a.printf("A human decided differently from another source %d time(s):\n",
				len(disagreements))

			byPair := map[string]int{}
			for _, d := range disagreements {
				byPair[fmt.Sprintf("%s: %s -> %s (was %s)",
					d.Facet, d.OtherValue, d.HumanValue, store.SourceKind(d.OtherSource))]++
			}

			for _, k := range sortedKeys(byPair) {
				a.printf("  %-56s %d\n", k, byPair[k])
			}
		}

		return nil
	})
}

type dimsFlags struct {
	top       int
	where     string
	asYAML    bool
	facet     string
	value     string
	minPurity float64
	minFiles  int
}

// dimRow is one pixel size with the evidence needed to judge whether a rule may match on it.
type dimRow struct {
	w, h int

	// Files is how many files have this size, after any --where filter.
	Files int

	// Matching is how many of them are already decided as the target value, and Other how
	// many are decided as something else. Undecided is the remainder.
	Matching  int
	Other     int
	Undecided int
}

// Evidence is the share of *all* files at this size for which there is already positive evidence
// of the target value.
//
// Computed over every file at the size, not only the decided ones. That distinction is the whole
// value of this column, and getting it wrong is easy: computing it over decided files alone made
// 750x1334 report 100% in the reference archive, because 14 files there were decided as
// screenshots and 20 were undecided. Those 20 are precisely what a size rule would newly decide,
// and there is no evidence at all about them - an independent check found they are Picasa-processed
// JPEGs that may well be photographs. Measured over all 34 files the figure is 41%, which is the
// honest answer and the one that stops a bad rule being written.
func (r dimRow) Evidence() float64 {
	if r.Files == 0 {
		return 0
	}

	return float64(r.Matching) / float64(r.Files)
}

// Disqualified reports a hard counterexample: a file at this size already decided as something
// else. One is enough, because a size rule cannot be right about a file that is known to be
// something different.
func (r dimRow) Disqualified() bool { return r.Other > 0 }

func (a *App) runStatsDims(ctx context.Context, flags dimsFlags) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	filter, err := rules.CompileFilter(flags.where)
	if err != nil {
		return &usageError{err: err}
	}

	return a.withStore(ctx, func(db *store.DB) error {
		// What is already decided for the facet, so each size can be reported with the
		// evidence for and against it. This is the difference between a frequency list and
		// a list you can safely turn into rules.
		decided := map[string]string{}

		if flags.facet != "" {
			decisions, err := db.DecisionsForFacet(ctx, flags.facet)
			if err != nil {
				return err
			}

			for _, d := range decisions {
				decided[d.Hash] = d.Value
			}
		}

		type size struct{ w, h int }

		rows := map[size]*dimRow{}

		total, matched := 0, 0

		err := db.EachFileForRules(ctx, func(in store.RuleInput) error {
			total++

			if filter != nil {
				ok, err := filter.Match(rules.EnvFor(in, cfg, a.Log))
				if err != nil {
					return err
				}

				if !ok {
					return nil
				}
			}

			matched++

			// A file with unknown dimensions is excluded rather than reported as 0x0,
			// which would otherwise look like a resolution somebody could match on.
			if in.Width <= 0 || in.Height <= 0 {
				return nil
			}

			key := size{in.Width, in.Height}

			row := rows[key]
			if row == nil {
				row = &dimRow{w: in.Width, h: in.Height}
				rows[key] = row
			}

			row.Files++

			switch value, ok := decided[in.Hash]; {
			case !ok:
				row.Undecided++
			case value == flags.value:
				row.Matching++
			default:
				row.Other++
			}

			return nil
		})
		if err != nil {
			return err
		}

		ordered := make([]dimRow, 0, len(rows))
		for _, r := range rows {
			ordered = append(ordered, *r)
		}

		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Files != ordered[j].Files {
				return ordered[i].Files > ordered[j].Files
			}

			if ordered[i].w != ordered[j].w {
				return ordered[i].w > ordered[j].w
			}

			return ordered[i].h > ordered[j].h
		})

		if flags.asYAML {
			return a.printDimsYAML(ordered, flags)
		}

		return a.printDimsTable(ordered, flags, filter, matched, total)
	})
}

// printDimsYAML emits only the sizes that clear the purity bar.
//
// Filtering by purity rather than by frequency is the whole point. A frequent size is not a safe
// one: in the reference archive 750x1334 is the second most common size among files no camera
// touched, and only 41% of the decided files at that size are screenshots - a rule matching it
// would mislabel twenty files silently.
func (a *App) printDimsYAML(rows []dimRow, flags dimsFlags) error {
	var kept, thin []dimRow

	for _, r := range rows {
		if r.Matching == 0 || r.Disqualified() || r.Evidence() < flags.minPurity {
			continue
		}

		// A size backed by one or two files is a coincidence, not a device resolution.
		// Without this the reference archive suggested 750x1217, 750x1190, 750x1150,
		// 750x1147 and 640x635 - each one file, each 100% evidence, and every one of them
		// a *cropped* screenshot. Adding a crop size as a device resolution is how a
		// future photograph cropped to that size gets silently mislabelled.
		if r.Matching < flags.minFiles {
			thin = append(thin, r)

			continue
		}

		kept = append(kept, r)
	}

	if flags.top > 0 && len(kept) > flags.top {
		kept = kept[:flags.top]
	}

	a.printf("# Generated by `sicht stats dims --yaml`. Paste into sicht.yaml.\n")
	a.printf("#\n")
	a.printf("# Listed: sizes where at least %.0f%% of *all* files that size are already known to\n",
		flags.minPurity*100)
	a.printf("# be %q, and none is known to be anything else.\n", flags.value)
	a.printf("#\n")
	a.printf("# Measured over every file at the size, not only the decided ones. A size can look\n")
	a.printf("# perfect among the files already decided and still be mostly something else - in\n")
	a.printf("# the archive this was developed against, 750x1334 had 14 known screenshots and 20\n")
	a.printf("# files with no evidence either way, which is 41%% and not 100%%.\n")
	a.printf("#\n")
	a.printf("# Sizes backed by fewer than %d known files are left out as coincidences rather\n", flags.minFiles)
	a.printf("# than device resolutions; a cropped screenshot is a size that occurs once.\n")
	a.printf("#\n")
	a.printf("# Check each one against a device you actually own before keeping it.\n")

	if len(kept) == 0 {
		a.printf("screen_resolutions: []\n")
		a.printf("#\n")
		a.printf("# Nothing cleared the bar. Either no %s has been decided yet - run `sicht rules`\n",
			flags.value)
		a.printf("# first - or no single size is reliable in this archive.\n")

		return nil
	}

	a.printf("screen_resolutions:\n")

	for _, r := range kept {
		a.printf("  - \"%dx%d\"   # %d of %d files are known %s (%.0f%%); the rule would newly decide %d\n",
			r.w, r.h, r.Matching, r.Files, flags.value, r.Evidence()*100, r.Undecided)
	}

	if len(thin) > 0 {
		a.printf("#\n")
		a.printf("# Left out, backed by fewer than %d files each (likely crops, not device sizes):\n",
			flags.minFiles)

		for _, r := range thin {
			a.printf("#   %dx%d  (%d file)\n", r.w, r.h, r.Matching)
		}
	}

	return nil
}

func (a *App) printDimsTable(rows []dimRow, flags dimsFlags, filter *rules.Filter, matched, total int) error {
	if filter != nil {
		a.printf("Filter: %s\n", filter.String())
		a.printf("%d of %d files match.\n\n", matched, total)
	}

	if len(rows) == 0 {
		a.printf("No file has known dimensions.\n")

		return nil
	}

	shown := rows
	if flags.top > 0 && len(shown) > flags.top {
		shown = shown[:flags.top]
	}

	a.printf("%-13s %6s  %7s %6s %7s %9s  %s\n",
		"size", "files", "known", "other", "unknown", "evidence", "")

	for _, r := range shown {
		note := ""

		switch {
		case r.Disqualified():
			note = a.paint("a file here is something else", "31")
		case r.Matching > 0 && r.Evidence() >= flags.minPurity:
			if r.Undecided == 0 {
				note = "already fully decided - a rule would add nothing"
			} else {
				note = a.paint("safe for a rule", "32")
			}
		case r.Matching > 0:
			note = a.paint(fmt.Sprintf("only %.0f%% evidence - do not use", r.Evidence()*100), "31")
		case r.w == r.h:
			// A square image is almost never a screenshot, and square sizes dominate
			// the download-heavy part of a real archive.
			note = "square"
		}

		evidence := "      -  "
		if r.Matching > 0 {
			evidence = fmt.Sprintf("%7.1f%%", r.Evidence()*100)
		}

		a.printf("%-13s %6d  %7d %6d %7d %s  %s\n",
			fmt.Sprintf("%dx%d", r.w, r.h), r.Files,
			r.Matching, r.Other, r.Undecided, evidence, note)
	}

	a.printf("\n  known    already decided as %q by some other means\n", flags.value)
	a.printf("  other    decided as something else - one of these rules the size out\n")
	a.printf("  unknown  no evidence either way; this is what a size rule would newly decide\n")
	a.printf("\nEvidence is known/files, over *every* file that size. A size can be unanimous\n")
	a.printf("among the files already decided and still be mostly unknown, which is the case a\n")
	a.printf("frequency list hides: a common size is not a safe one.\n")
	a.printf("\nTo emit only the safe ones:\n")
	a.printf("  sicht stats dims --where '!has_tag(exif, \"Make\")' --yaml\n")

	return nil
}

func (a *App) runStatsDupes(ctx context.Context) error {
	return a.withStore(ctx, func(db *store.DB) error {
		exact, err := db.ExactDuplicates(ctx)
		if err != nil {
			return err
		}

		a.printf("Byte-identical files (one file, several places)\n")

		if len(exact) == 0 {
			a.printf("  none\n")
		}

		wasted := int64(0)

		for _, g := range exact {
			a.printf("  %s  %s\n", humanBytes(g.Size), g.Paths[0])

			for _, p := range g.Paths[1:] {
				a.printf("  %9s  %s\n", "", p)
			}

			wasted += g.Size * int64(len(g.Paths)-1)
		}

		if wasted > 0 {
			a.printf("\n  %d group(s), %s held more than once. `apply` moves the first path of\n",
				len(exact), humanBytes(wasted))
			a.printf("  each and records the others; removal is not something sicht does.\n")
		}

		twins, err := db.FormatTwins(ctx)
		if err != nil {
			return err
		}

		a.printf("\nSame stem, several formats  [advisory]\n")

		if len(twins) == 0 {
			a.printf("  none\n")

			return nil
		}

		for _, g := range twins {
			where := g.Dir
			if where == "" {
				where = "."
			}

			a.printf("  %s/%s   %s\n", where, g.Stem, strings.Join(prefixed(g.Exts), " "))
		}

		a.printf("\n  %d group(s). Not acted on: the bytes differ, so each is its own file with\n", len(twins))
		a.printf("  its own decision. Google exported the same photo more than once.\n")

		return nil
	})
}

func prefixed(exts []string) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		out = append(out, "."+e)
	}

	return out
}

func describeSources(sources map[string]int) string {
	kinds := map[string]int{}
	for s, n := range sources {
		kinds[store.SourceKind(s)] += n
	}

	parts := make([]string, 0, len(kinds))
	for _, k := range []string{"human", "rule", "model", "other"} {
		if n, ok := kinds[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
	}

	return strings.Join(parts, ", ")
}

func sumOf(m map[string]int) int {
	total := 0
	for _, n := range m {
		total += n
	}

	return total
}

// sortedKeys returns a map's keys in sorted order, so that every report is stable between runs.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// humanBytes renders a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)

	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit

		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}

	return fmt.Sprintf("%.1f PiB", value)
}
