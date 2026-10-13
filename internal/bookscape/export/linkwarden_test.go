package export_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/export"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// TestBackupNeverEncodesAnArrayAsNull is the most consequential test in this file.
//
// importFromLinkwarden.ts (v2.16.3) dereferences three arrays with no nil guard -
// `data.pinnedLinks.forEach`, `collection.links.length` and `link.tags.map` - inside a
// transaction whose rejection is swallowed by `.catch(err => console.log(err))`. The endpoint
// then answers 200 "Success." having imported nothing at all. Go marshals a nil slice as null,
// so an empty archive, a collection with no links or a link with no tags is exactly how that
// happens by accident.
func TestBackupNeverEncodesAnArrayAsNull(t *testing.T) {
	t.Parallel()

	tests := map[string][]store.ExportRecord{
		"no records at all": {},
		"a link with no tags": {
			{URL: "https://example.test/a", Title: "A", AddDate: ptr(1385000100), Collection: "Development"},
		},
	}

	for name, records := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			backup, _ := export.BuildBackup(records, config.Default().Export, []string{"Development"}, time.Now())

			encoded, err := export.EmitBackup(backup)
			if err != nil {
				t.Fatalf("EmitBackup() error = %v", err)
			}

			raw := string(encoded)

			for _, forbidden := range []string{
				`"collections": null`, `"pinnedLinks": null`,
				`"links": null`, `"tags": null`,
			} {
				if strings.Contains(raw, forbidden) {
					t.Errorf("output contains %s:\n%s", forbidden, raw)
				}
			}

			// And the same from the decoder's side, which is what the importer sees.
			var doc struct {
				Collections []struct {
					Links []struct {
						Tags []any `json:"tags"`
					} `json:"links"`
				} `json:"collections"`
				PinnedLinks []any `json:"pinnedLinks"`
			}

			if err := json.Unmarshal(encoded, &doc); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}

			if doc.PinnedLinks == nil {
				t.Error("pinnedLinks is nil")
			}

			for _, c := range doc.Collections {
				for _, l := range c.Links {
					if l.Tags == nil {
						t.Error("a link's tags is nil")
					}
				}
			}
		})
	}
}

// TestBackupSubstitutesAnUndatedLink covers the one place this format loses information that the
// Netscape one keeps.
//
// The importer reads `new Date(link.importDate || link.createdAt)` with no third fallback, so
// omitting both yields an Invalid Date, Prisma rejects it, and - per the test above - the whole
// import is discarded while the endpoint reports success. One undated link would therefore cost
// the entire archive, which is why a date is substituted and counted rather than left out.
func TestBackupSubstitutesAnUndatedLink(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)

	records := []store.ExportRecord{
		{URL: "https://example.test/dated", Title: "Dated", AddDate: ptr(1385000100), Collection: "Development"},
		{URL: "https://example.test/undated", Title: "Undated", Collection: "Development"},
	}

	backup, stats := export.BuildBackup(records, config.Default().Export, []string{"Development"}, now)

	if stats.DatesSubstituted != 1 {
		t.Errorf("DatesSubstituted = %d, want 1", stats.DatesSubstituted)
	}

	byURL := map[string]export.BackupLink{}
	for _, c := range backup.Collections {
		for _, l := range c.Links {
			byURL[l.URL] = l
		}
	}

	if got, want := byURL["https://example.test/dated"].ImportDate,
		time.Unix(1385000100, 0).UTC().Format(time.RFC3339); got != want {
		t.Errorf("the dated link's importDate = %q, want %q", got, want)
	}

	if got, want := byURL["https://example.test/undated"].ImportDate,
		now.Format(time.RFC3339); got != want {
		t.Errorf("the undated link's importDate = %q, want %q", got, want)
	}

	// Every link must parse as a date, whatever its origin. This is the invariant that keeps
	// the import from being discarded.
	for url, link := range byURL {
		for field, value := range map[string]string{"importDate": link.ImportDate, "createdAt": link.CreatedAt} {
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				t.Errorf("%s: %s = %q does not parse: %v", url, field, value, err)
			}
		}
	}
}

// TestBackupCollectionOrder mirrors the Netscape projection: configured collections in their
// configured order, then anything left over sorted, so the two formats describe the same archive
// rather than two differently shaped ones.
func TestBackupCollectionOrder(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{URL: "https://example.test/1", AddDate: ptr(1), Collection: "Reference"},
		{URL: "https://example.test/2", AddDate: ptr(2), Collection: "Development"},
		// A collection removed from the config after it was assigned. Emitting it where
		// the user has to notice beats dropping it silently.
		{URL: "https://example.test/3", AddDate: ptr(3), Collection: "Zebra"},
		{URL: "https://example.test/4", AddDate: ptr(4), Collection: "Aardvark"},
	}

	backup, _ := export.BuildBackup(records, config.Default().Export, []string{"Development", "Reference"}, time.Now())

	var got []string
	for _, c := range backup.Collections {
		got = append(got, c.Name)
	}

	want := []string{"Development", "Reference", "Aardvark", "Zebra"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("collection order = %v, want %v", got, want)
	}
}

// TestBackupOmitsAnEmptyCollection: a configured collection with nothing in it must not become an
// empty collection in Linkwarden, which the importer would create and never clean up.
func TestBackupOmitsAnEmptyCollection(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{URL: "https://example.test/1", AddDate: ptr(1), Collection: "Development"},
	}

	backup, _ := export.BuildBackup(records, config.Default().Export,
		[]string{"Development", "Reference", "Media"}, time.Now())

	if len(backup.Collections) != 1 {
		t.Fatalf("got %d collections, want 1: %+v", len(backup.Collections), backup.Collections)
	}

	if backup.Collections[0].Name != "Development" {
		t.Errorf("collection = %q, want Development", backup.Collections[0].Name)
	}
}

// TestBackupTruncatesTheDescription keeps bookscape's own limit below Linkwarden's 254, so the
// text that lands is the text that was reviewed rather than whatever survived someone else's cut.
func TestBackupTruncatesTheDescription(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Export
	cfg.MaxDescriptionLength = 10

	records := []store.ExportRecord{
		{URL: "https://example.test/1", AddDate: ptr(1), Collection: "Development",
			Description: "0123456789 and then some more"},
	}

	backup, _ := export.BuildBackup(records, cfg, []string{"Development"}, time.Now())

	if got := backup.Collections[0].Links[0].Description; got != "0123456789" {
		t.Errorf("description = %q, want %q", got, "0123456789")
	}
}

// TestEmitBackupDoesNotEscapeHTML: Go's encoder rewrites &, < and > by default, which would turn
// every query string in the archive into \u0026 soup. The document is meant to be readable before
// it is handed to an importer.
func TestEmitBackupDoesNotEscapeHTML(t *testing.T) {
	t.Parallel()

	records := []store.ExportRecord{
		{URL: "https://example.test/search?q=go&page=2", AddDate: ptr(1), Collection: "Development"},
	}

	backup, _ := export.BuildBackup(records, config.Default().Export, []string{"Development"}, time.Now())

	encoded, err := export.EmitBackup(backup)
	if err != nil {
		t.Fatalf("EmitBackup() error = %v", err)
	}

	raw := string(encoded)

	if strings.Contains(raw, `\u0026`) {
		t.Errorf("the URL was HTML-escaped:\n%s", raw)
	}

	if !strings.Contains(raw, "https://example.test/search?q=go&page=2") {
		t.Errorf("the URL is not verbatim in the output:\n%s", raw)
	}
}
