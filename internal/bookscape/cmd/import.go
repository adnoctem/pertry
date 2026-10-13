package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/dedupe"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewImportCommand // assure type compatibility

// NewImportCommand creates the 'import' subcommand, which parses a Netscape bookmark export
// into the database, preserving folder paths and dates.
func NewImportCommand(a *App) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "import <file.html>",
		Short: "Parse a Netscape bookmark export into the database",
		Long: "Parse a Netscape bookmark export into the database, preserving folder paths and dates.\n\n" +
			"Idempotent via the file's SHA-256: re-importing a file that has already been imported is a\n" +
			"no-op rather than a second copy of every row. Pass --force to import it again anyway.\n\n" +
			"The parser is cross-checked against an independent count of the anchor tags in the file,\n" +
			"and every decoded timestamp is range-checked. Either check failing stops the import,\n" +
			"because both failures are silent in the underlying library.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runImport(cmd.Context(), args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"Import the file even if a file with the same SHA-256 was already imported")

	return cmd
}

func (a *App) runImport(ctx context.Context, path string, force bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read bookmark file %q: %w\n"+
			"hint: export one from your browser's bookmark manager (\"Export bookmarks to HTML\")", path, err)
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	entries, report, err := netscape.Parse(raw)
	if err != nil {
		return err
	}

	a.Log.Info("parsed bookmark file",
		"path", path,
		"title", report.Title,
		"entries", len(entries),
		"folders", report.Folders,
		"sha256", digest[:12])

	if err := a.verifyParse(entries, report); err != nil {
		return err
	}

	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	existing, err := db.FindBatch(ctx, digest)
	if err != nil {
		return err
	}

	if existing != nil && !force {
		a.printf("Already imported: %s\n", existing.Filename)
		a.printf("  batch %d, %d entries, imported %s\n",
			existing.ID, existing.EntryCount, existing.ImportedAt.Format("2006-01-02 15:04:05 MST"))
		a.printf("\nNothing to do. Pass --force to import it again as a new batch.\n")

		return nil
	}

	if existing != nil {
		a.Log.Warn("re-importing a file that was already imported",
			"previous_batch", existing.ID, "previous_entries", existing.EntryCount)
	}

	bookmarks, err := a.toBookmarks(entries)
	if err != nil {
		return err
	}

	batch, err := db.Import(ctx, filepath.Base(path), digest, bookmarks)
	if err != nil {
		// A second import of the same digest violates import_batches.sha256's UNIQUE
		// constraint. That is reachable with --force, and the generic constraint error
		// does not say what to do about it.
		if force && existing != nil {
			return fmt.Errorf("%w\nhint: --force cannot import the same file twice; the SHA-256 is "+
				"recorded as batch %d and must be unique", err, existing.ID)
		}

		return err
	}

	a.printf("Imported %d bookmarks from %s as batch %d.\n", batch.EntryCount, batch.Filename, batch.ID)

	if report.UndatedEntries > 0 {
		a.printf("  %s carried no ADD_DATE and will export without one.\n", plural(report.UndatedEntries, "entry", "entries"))
	}

	a.printf("\nNext: bookscape dedupe --dry-run\n")

	return nil
}

// verifyParse applies the two checks the underlying library does not: that nothing was
// silently dropped, and that no timestamp was silently rescaled. Both failures are invisible
// in the library's own output, so neither can be left to chance
// (docs/ARCHITECTURE.md#parsing).
func (a *App) verifyParse(entries []netscape.Entry, report netscape.Report) error {
	var problems []error

	if dropped := report.Dropped(len(entries)); dropped > 0 {
		problems = append(problems, fmt.Errorf(
			"the file contains %d anchor tags but the parser returned %d bookmarks, so %d were dropped\n"+
				"hint: the parser matches element and attribute names case-sensitively against <A> and HREF, "+
				"and discards anything else without an error. Check the file for lowercase tags",
			report.AnchorTags, len(entries), dropped))
	}

	if n := len(report.SuspectDates); n > 0 {
		for _, s := range report.SuspectDates[:min(n, 5)] {
			a.Log.Error("implausible timestamp",
				"url", s.URL, "field", s.Field, "decoded", s.Decoded.Format("2006-01-02T15:04:05Z"))
		}

		problems = append(problems, fmt.Errorf(
			"%d timestamps decoded to implausible dates\n"+
				"hint: the parser rescales an out-of-range value through milliseconds, microseconds and "+
				"nanoseconds until one lands within 30 years of now, so a WebKit-epoch value meaning 2020 "+
				"becomes a date in 1970. The dates in the file are probably not unix seconds", n))
	}

	if len(problems) > 0 {
		return errors.Join(append(problems,
			errors.New("refusing to import: see https://github.com/adnoctem/pertry "+
				"docs/ARCHITECTURE.md#parsing for why these checks exist"))...)
	}

	return nil
}

// toBookmarks derives the comparison key for every entry and maps it into the store's shape.
func (a *App) toBookmarks(entries []netscape.Entry) ([]store.NewBookmark, error) {
	normalizer := dedupe.NewNormalizer(a.Config.Normalize)

	bookmarks := make([]store.NewBookmark, 0, len(entries))

	for _, e := range entries {
		norm, err := normalizer.Normalize(e.URL)
		if err != nil {
			// A URL that will not parse cannot get a comparison key, and a key that is
			// really an error string would merge every broken URL into one group. Stop
			// rather than import a row that deduplication will mishandle.
			return nil, fmt.Errorf("normalise %q (filed under %q): %w", e.URL, e.Path, err)
		}

		bookmarks = append(bookmarks, store.NewBookmark{
			URL:          e.URL,
			URLNorm:      norm,
			Title:        e.Title,
			Description:  a.truncateDescription(e.Description),
			AddDate:      e.AddDate,
			LastModified: e.LastModified,
			Private:      e.Private,
			SourcePath:   e.Path,
			Tags:         e.Tags,
		})
	}

	return bookmarks, nil
}

// truncateDescription applies export.max_description_length at import time as well as at
// export time. An imported <DD> is stored as-is apart from this: a Firefox export can carry a
// description of any length, and there is no reason to keep bytes that can never be exported.
func (a *App) truncateDescription(s string) string {
	limit := a.Config.Export.MaxDescriptionLength
	if limit <= 0 || len(s) <= limit {
		return s
	}

	// Cut on a rune boundary, so a truncated description is still valid UTF-8.
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
