//go:build e2e

package linkwarden

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestLinkwardenImportsBookscapeExport is the whole point of this package: a document emitted by
// `bookscape export` is posted to a real Linkwarden and read back, and every channel bookscape
// uses is checked to have survived.
//
// docs/ARCHITECTURE.md#process-model calls hand-rolled Netscape emission "the single most likely
// cause of a silent import failure downstream". Nothing in the unit tests can catch that, because
// the other half of the round trip is a different program's parser.
func TestLinkwardenImportsBookscapeExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	emitted, want := buildExport(ctx, t)

	t.Logf("emitted %d bytes for %d links", len(emitted), len(want))

	stack := Start(t)
	client := NewClient(ctx, t, stack)

	client.ImportHTML(ctx, t, emitted)

	backup := client.Backup(ctx, t)
	got, collections := backup.LinksByURL(t)

	// 1. Nothing dropped, nothing duplicated. Catches a silent partial import, a swallowed
	//    IMPORT_LIMIT rejection, and a double import. LinksByURL reports duplicates itself.
	if len(got) != len(want) {
		t.Errorf("Linkwarden holds %d links, want %d", len(got), len(want))
	}

	for _, w := range want {
		t.Run(label(w.URL), func(t *testing.T) {
			link, ok := got[w.URL]
			if !ok {
				// 7. URL byte-equality. A link missing here usually means Linkwarden
				//    rewrote the URL rather than lost it, so say which URLs exist.
				t.Fatalf("no link at %q.\nLinkwarden rewriting the URL would look like this; "+
					"the closest URLs it does hold are:\n%s", w.URL, nearest(got, w.URL))
			}

			// 2, 4, 5, 6, 8 and 9 - the date, the title and its truncation, the tags
			//    and the collection - are everything both formats preserve, so they are
			//    asserted in one shared place. `false`: through this format an undated
			//    link must arrive with a null importDate.
			checkLink(t, w, link, collections[w.URL], false)

			// 3. The <DD> description does NOT survive, and this pins that.
			//
			//    Linkwarden v2.16.3 reads a link's description from a <DD> parsed as a
			//    child node of the item, and in practice that never matches a real
			//    Netscape document: every <DD> placement was probed against a live
			//    instance - sibling on the next line (what bookscape and every browser
			//    emit), same line, explicitly closed, inside the anchor, and a
			//    DESCRIPTION attribute - and all of them arrive empty. The
			//    Chrome-shaped fixture *input* behaves identically, so this is a
			//    Linkwarden limitation rather than a defect in what bookscape emits.
			//
			//    Asserting the real behaviour rather than the hoped-for one is
			//    deliberate: the assertion fails the day Linkwarden starts honouring
			//    <DD>, which is exactly when someone should re-read docs/ARCHITECTURE.md#export-and-linkwarden and
			//    re-enable the channel. See docs/ARCHITECTURE.md#export-and-linkwarden.
			if link.Description != "" {
				t.Errorf("description = %q, want empty.\n"+
					"Linkwarden has started importing <DD> descriptions - this is good news. "+
					"bookscape emitted %q for this link. Re-read the note in "+
					"docs/ARCHITECTURE.md#export-and-linkwarden, change this assertion to compare "+
					"against the emitted value, and the generated descriptions from "+
					"`classify` will start reaching the archive.",
					link.Description, w.Description)
			}
		})
	}

	t.Run("collections", func(t *testing.T) {
		// 5. Every collection bookscape emitted exists. An *extra* collection is logged
		//    rather than failed: whether Linkwarden also makes one from the document's <H1>
		//    is its choice, and a finding for the design doc rather than a bug here.
		var wanted []string
		for _, w := range want {
			if !slices.Contains(wanted, w.Collection) {
				wanted = append(wanted, w.Collection)
			}
		}

		slices.Sort(wanted)

		present := backup.CollectionNames()
		slices.Sort(present)

		for _, name := range wanted {
			if !slices.Contains(present, name) {
				t.Errorf("collection %q is missing; Linkwarden holds %v", name, present)
			}
		}

		for _, name := range present {
			if !slices.Contains(wanted, name) {
				t.Logf("NOTE: Linkwarden created the extra collection %q, which bookscape did "+
					"not emit as a folder. Worth recording in docs/ARCHITECTURE.md "+
					"docs/ARCHITECTURE.md#export-and-linkwarden if it is the document <H1>.", name)
			}
		}
	})

	t.Run("importDate versus createdAt", func(t *testing.T) {
		// Logged, never asserted. If Linkwarden sets createdAt to the import time and its UI
		// sorts by createdAt, then the chronology round-trips in the database and is still
		// invisible in the product - which is a finding about this project's premise, not a
		// defect in this test. The owner should see the real answer and decide what correct
		// behaviour even is, so this prints rather than passing or failing.
		for _, w := range want {
			link, ok := got[w.URL]
			if !ok || w.AddDate == nil {
				continue
			}

			created := "null"
			if link.CreatedAt != nil {
				created = link.CreatedAt.UTC().Format(time.RFC3339)
			}

			imported := "null"
			if link.ImportDate != nil {
				imported = link.ImportDate.UTC().Format(time.RFC3339)
			}

			t.Logf("createdAt=%s importDate=%s for %s", created, imported, label(w.URL))

			break
		}
	})
}

// TestLinkwardenImportsBookscapeJSONExport is the evidence behind the decision in
// docs/ARCHITECTURE.md#export-and-linkwarden: the generated description reaches the archive
// through Linkwarden's own backup format, where it does not through Netscape HTML.
//
// It is a separate test rather than a subtest because it needs its own Linkwarden account.
// importFromLinkwarden creates every collection unconditionally and never reuses one, so a second
// import into the same account would double every collection.
//
// The reason this exists at all rather than a reading of importFromLinkwarden.ts: the HTML
// description loss was also "verified from source" first, and the source said the <DD> was read.
// It was read from a position no real document uses. A second source reading is not evidence.
func TestLinkwardenImportsBookscapeJSONExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	emitted, want := buildExportJSON(ctx, t)

	t.Logf("emitted %d bytes of JSON for %d links", len(emitted), len(want))

	stack := Start(t)
	client := NewClientFor(ctx, t, stack, "json")

	client.ImportBackup(ctx, t, emitted)

	backup := client.Backup(ctx, t)
	got, collections := backup.LinksByURL(t)

	// A count of zero is the specific failure this format invites: three of the importer's
	// dereferences have no nil guard, the transaction's rejection is logged and discarded,
	// and the endpoint answers 200 "Success." either way. So the count is checked before
	// anything else and says what it means.
	if len(got) == 0 {
		t.Fatalf("Linkwarden holds no links at all after a 200 from the import.\n"+
			"That is what a document importFromLinkwarden choked on looks like: it runs the "+
			"import in a transaction whose rejection is swallowed by a .catch that logs, and "+
			"the endpoint reports success regardless. The usual causes are a null where it "+
			"expects an array - pinnedLinks, collections[].links or links[].tags - or a link "+
			"with neither importDate nor createdAt, which yields an Invalid Date that Prisma "+
			"rejects. Check the container log for the Prisma error.\n%s",
			stack.diagnostics(ctx))
	}

	if len(got) != len(want) {
		t.Errorf("Linkwarden holds %d links, want %d", len(got), len(want))
	}

	var withDescription int

	for _, w := range want {
		t.Run(label(w.URL), func(t *testing.T) {
			link, ok := got[w.URL]
			if !ok {
				t.Fatalf("no link at %q.\nthe closest URLs Linkwarden does hold are:\n%s",
					w.URL, nearest(got, w.URL))
			}

			// THE HEADLINE, and the whole reason this format is the default. The
			// generated one-line description is what docs/ARCHITECTURE.md#classification calls where the model
			// pass earns its cost, and through Netscape HTML it arrives empty.
			if link.Description != w.Description {
				t.Errorf("description = %q (%d runes), want %q (%d runes).\n"+
					"This format is the default *because* it carries descriptions; if this "+
					"fails, re-read docs/ARCHITECTURE.md#export-and-linkwarden before changing the "+
					"assertion.",
					link.Description, len([]rune(link.Description)),
					w.Description, len([]rune(w.Description)))
			}

			if w.Description != "" && link.Description != "" {
				withDescription++
			}

			checkLink(t, w, link, collections[w.URL], true)
		})
	}

	t.Run("descriptions actually arrived", func(t *testing.T) {
		// Guards against the assertion above passing vacuously: if the fixture stopped
		// producing descriptions, every comparison would be "" == "" and the test would
		// be green while proving nothing.
		if withDescription == 0 {
			t.Error("not one link arrived with a description, so the comparisons above " +
				"compared empty to empty and this test proved nothing")
		}

		t.Logf("%d of %d links arrived carrying a description", withDescription, len(want))
	})

	t.Run("collections", func(t *testing.T) {
		var wanted []string
		for _, w := range want {
			if !slices.Contains(wanted, w.Collection) {
				wanted = append(wanted, w.Collection)
			}
		}

		slices.Sort(wanted)

		present := backup.CollectionNames()
		slices.Sort(present)

		for _, name := range wanted {
			if !slices.Contains(present, name) {
				t.Errorf("collection %q is missing; Linkwarden holds %v", name, present)
			}
		}

		// Unlike the HTML importer there is no document <H1> to make a collection from,
		// so an extra one here would be a real surprise rather than a known quirk.
		for _, name := range present {
			if !slices.Contains(wanted, name) {
				t.Errorf("Linkwarden created the extra collection %q, which this document "+
					"does not contain", name)
			}
		}
	})
}

// checkLink asserts everything both formats are expected to preserve, so that the two tests
// differ only where the formats genuinely do.
//
// substitutedDates is the one such place. The Netscape path leaves an undated link's importDate
// null; Linkwarden's backup format has no way to say "date unknown", so `export` substitutes its
// own timestamp and reports how many - see export.BackupLink.ImportDate for why omitting the
// field instead would discard the entire import.
func checkLink(t *testing.T, w expected, link Link, collection string, substitutedDates bool) {
	t.Helper()

	// ADD_DATE must survive, and it lands in importDate rather than createdAt. Without this
	// the whole archive is dated today and the chronology the project exists to preserve is
	// gone.
	switch {
	case w.AddDate == nil && !substitutedDates:
		// An entry that never had a date must not acquire one. A 1970 date here is the
		// rescaling failure from docs/ARCHITECTURE.md#parsing reappearing on the far side of the wire.
		if link.ImportDate != nil && !link.ImportDate.IsZero() {
			t.Errorf("importDate = %s, want null for a link that carried no ADD_DATE",
				link.ImportDate.Format(time.RFC3339))
		}
	case w.AddDate == nil:
		// Substituted, so a date is expected - but never 1970, which would be the
		// rescaling failure rather than a substitution.
		if link.ImportDate == nil {
			t.Error("importDate = null, want this export's own timestamp for a link that " +
				"carried no ADD_DATE")
		} else if link.ImportDate.Year() == 1970 {
			t.Errorf("importDate = %s; a 1970 date is the epoch-rescaling failure, not a "+
				"substitution", link.ImportDate.Format(time.RFC3339))
		}
	case link.ImportDate == nil:
		t.Errorf("importDate = null, want %s (unix %d)",
			time.Unix(*w.AddDate, 0).UTC().Format(time.RFC3339), *w.AddDate)
	default:
		if got, want := link.ImportDate.Unix(), *w.AddDate; got != want {
			t.Errorf("importDate = %d (%s), want %d (%s)",
				got, link.ImportDate.UTC().Format(time.RFC3339),
				want, time.Unix(want, 0).UTC().Format(time.RFC3339))
		}
	}

	// Title, including the unicode case and the 254-rune truncation.
	if link.Name != w.Name {
		t.Errorf("name = %q (%d runes), want %q (%d runes)",
			link.Name, len([]rune(link.Name)), w.Name, len([]rune(w.Name)))
	}

	// Tags, order-insensitive: the parser sorts them on decode, and Linkwarden has no reason
	// to preserve any particular order.
	gotTags := link.TagNames()
	wantTags := slices.Clone(w.Tags)
	slices.Sort(gotTags)
	slices.Sort(wantTags)

	if diff := cmp.Diff(wantTags, gotTags, cmpEmptyAsEqual()); diff != "" {
		t.Errorf("tags mismatch (-want +got):\n%s", diff)
	}

	if collection != w.Collection {
		t.Errorf("collection = %q, want %q", collection, w.Collection)
	}
}

// cmpEmptyAsEqual treats a nil slice and an empty one as the same, since "no tags" arrives either
// way depending on whether Linkwarden omitted the field or returned an empty array.
func cmpEmptyAsEqual() cmp.Option {
	return cmp.FilterValues(
		func(x, y []string) bool { return len(x) == 0 && len(y) == 0 },
		cmp.Comparer(func([]string, []string) bool { return true }),
	)
}

// label shortens a fixture URL into a readable subtest name, so a failure says which case broke.
func label(rawURL string) string {
	trimmed := strings.TrimSuffix(rawURL, "/")
	if i := strings.Index(trimmed, "/e2e/"); i >= 0 {
		trimmed = trimmed[i+len("/e2e/"):]
	}

	return strings.NewReplacer("/", "_", "?", "_", "#", "_", "=", "-", "&", "_", " ", "_").
		Replace(trimmed)
}

// nearest lists a few URLs Linkwarden actually holds, to make a URL-rewriting failure readable.
func nearest(got map[string]Link, missing string) string {
	var b strings.Builder

	stem := missing
	if i := strings.LastIndex(stem, "/"); i >= 0 {
		stem = stem[:i]
	}

	shown := 0

	for url := range got {
		if strings.HasPrefix(url, stem) && shown < 5 {
			b.WriteString("  " + url + "\n")
			shown++
		}
	}

	if shown == 0 {
		b.WriteString("  (none share a path prefix)\n")
	}

	return b.String()
}
