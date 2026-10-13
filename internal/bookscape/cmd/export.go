package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/export"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewExportCommand // assure type compatibility

// NewExportCommand creates the 'export' subcommand.
func NewExportCommand(a *App) *cobra.Command {
	var (
		output   string
		typeName string
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Project the database into a document an importer reads",
		Long: "Project the database back into a document for import into Linkwarden.\n\n" +
			"Two formats, and they are not equivalent:\n\n" +
			"  json  Linkwarden's own backup format, and the default. The only one of the two\n" +
			"        that keeps the generated description, which is what the model pass is for.\n" +
			"  html  Netscape bookmarks. Everything lands except the description: the format\n" +
			"        carries one and bookscape writes it, but Linkwarden's HTML importer looks\n" +
			"        for the <DD> in a position no real document puts it. Use this to import\n" +
			"        somewhere that is not Linkwarden, or back into a browser.\n\n" +
			"Either way collections become collections - one flat level, since the collection set is\n" +
			"deliberately small and shallow - tags become tags, and the original ADD_DATE is\n" +
			"preserved, without which the whole archive would land dated today.\n\n" +
			"Held back by default: bookmarks deduplication collapsed, bookmarks with no collection,\n" +
			"dead links, and model classifications still below min_confidence. The last of those is\n" +
			"what `bookscape review` clears. Everything excluded is reported with its reason, so a\n" +
			"count smaller than you expected is accounted for rather than mysterious.\n\n" +
			"Import 50 links into Linkwarden first and check what landed. Import creates collections\n" +
			"as a side effect and never reuses one, so unwinding a bad full run by hand is genuinely\n" +
			"tedious.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runExport(cmd.Context(), output, typeName, force)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "",
		"Write to this file instead of the default for the format")
	cmd.Flags().StringVarP(&typeName, "type", "t", "",
		"Document to produce: json (default) or html. Overrides export.format")
	cmd.Flags().BoolVar(&force, "force", false,
		"Overwrite the output file if it already exists")

	return cmd
}

func (a *App) runExport(ctx context.Context, output, typeName string, force bool) error {
	// The flag wins over the config, and the config over the default, as everywhere else.
	spec := typeName
	if spec == "" {
		spec = a.Config.Export.Format
	}

	format, err := config.ParseExportFormat(spec)
	if err != nil {
		return &usageError{err: err}
	}

	if output == "" {
		output = a.Config.Export.Output
	}

	if output == "" {
		output = "./enriched-bookmarks" + format.Extension()
	}

	// A mismatched extension is the user's own choice and not an error - but writing JSON
	// into a file called .html is worth one line of warning, because the mistake surfaces
	// much later, as an importer rejecting a file that looks right by name.
	if ext := filepath.Ext(output); ext != "" && ext != format.Extension() {
		a.printf("Note: writing %s to %s, whose extension says %s.\n", format, output, ext)
	}

	// Refuse to clobber an existing export by default. A previous export may already have
	// been imported somewhere, and silently replacing it loses the record of what was sent.
	if _, err := os.Stat(output); err == nil && !force {
		return usagef("%s already exists\nhint: pass --force to overwrite it, or -o to write elsewhere", output)
	}

	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	records, stats, err := db.ExportRecords(ctx, store.ExportFilter{
		IncludeDead:      a.Config.Export.IncludeDead,
		IncludeUncertain: a.Config.Export.IncludeUncertain,
		MinConfidence:    a.Config.Classify.MinConfidence,
	})
	if err != nil {
		return err
	}

	if len(records) == 0 {
		a.printf("Nothing to export.\n")
		a.printExclusions(stats)

		return nil
	}

	encoded, counts, err := a.encodeExport(format, records)
	if err != nil {
		return err
	}

	if err := writeFileAtomic(output, encoded); err != nil {
		return err
	}

	a.printf("Exported %s in %s to %s.\n",
		plural(stats.Included, "bookmark", "bookmarks"),
		plural(len(counts), "collection", "collections"), output)

	for _, c := range counts {
		a.printf("  %-16s %d\n", c.name, c.links)
	}

	a.printExclusions(stats)

	a.printf("\nNext: import this file into Linkwarden - Settings, then Import. Try 50 links\n")
	a.printf("first and check what landed.\n")

	if format == config.ExportHTML {
		a.printf("\nThis format does not carry the generated descriptions through Linkwarden's\n")
		a.printf("HTML importer. Export as json if you want them.\n")
	}

	return nil
}

// collectionCount is one line of the per-collection summary, so that both formats can be
// reported the same way without the command knowing either document's shape.
type collectionCount struct {
	name  string
	links int
}

// encodeExport builds and encodes the document for one format.
func (a *App) encodeExport(
	format config.ExportFormat,
	records []store.ExportRecord,
) ([]byte, []collectionCount, error) {
	if format == config.ExportHTML {
		doc := export.BuildNetscape(records, a.Config.Export, a.Config.Classify.Collections)

		encoded, err := netscape.Emit(doc)
		if err != nil {
			return nil, nil, err
		}

		counts := make([]collectionCount, 0, len(doc.Folders))
		for _, f := range doc.Folders {
			counts = append(counts, collectionCount{name: f.Name, links: len(f.Bookmarks)})
		}

		return encoded, counts, nil
	}

	backup, backupStats := export.BuildBackup(
		records, a.Config.Export, a.Config.Classify.Collections, time.Now())

	encoded, err := export.EmitBackup(backup)
	if err != nil {
		return nil, nil, err
	}

	// Not a warning in passing: this format has no way to say "date unknown", so an undated
	// link is given the export's own timestamp. Saying nothing would mean inventing a date
	// silently in a pipeline whose point is preserving the real ones.
	if backupStats.DatesSubstituted > 0 {
		a.printf("Note: %s carried no date, so this export's timestamp was used - "+
			"Linkwarden's backup format cannot express an unknown one.\n",
			plural(backupStats.DatesSubstituted, "bookmark", "bookmarks"))
	}

	counts := make([]collectionCount, 0, len(backup.Collections))
	for _, c := range backup.Collections {
		counts = append(counts, collectionCount{name: c.Name, links: len(c.Links)})
	}

	return encoded, counts, nil
}

// printExclusions accounts for every surviving bookmark the filter held back.
func (a *App) printExclusions(stats store.ExportStats) {
	if stats.Excluded() == 0 && stats.Collapsed == 0 {
		return
	}

	a.printf("\nHeld back\n")

	reasons := []struct {
		n    int
		what string
		hint string
	}{
		{stats.Collapsed, "duplicates collapsed by dedupe", ""},
		{stats.Unclassified, "with no collection", "bookscape classify"},
		{stats.Dead, "dead links", "set export.include_dead to ship them"},
		{stats.Uncertain, "links with an inconclusive liveness check", "set export.include_uncertain to ship them"},
		{stats.PendingReview, "below min_confidence and not yet reviewed", "bookscape review"},
	}

	for _, r := range reasons {
		if r.n == 0 {
			continue
		}

		if r.hint == "" {
			a.printf("  %6d  %s\n", r.n, r.what)

			continue
		}

		a.printf("  %6d  %-45s %s\n", r.n, r.what, r.hint)
	}
}

// writeFileAtomic writes through a temporary file in the same directory and renames it into
// place, so that an interrupted export leaves either the previous file or the new one - never a
// half-written document that looks importable.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temporary file next to %s: %w", path, err)
	}

	tmpName := tmp.Name()

	defer func() {
		// Harmless once the rename has succeeded; the cleanup that matters is on the
		// failure paths below.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write %s: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("flush %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("move %s into place at %s: %w", tmpName, path, err)
	}

	return nil
}
