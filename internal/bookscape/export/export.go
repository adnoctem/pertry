// Package export projects the database back into a Netscape bookmark document.
//
// The exporter is a pure projection. The HTML file is an interchange format, not a working
// store: all mutation happened in SQLite, and nothing here writes back
// (docs/ARCHITECTURE.md#process-model).
package export

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// BuildNetscape projects records into a Netscape document ready to emit.
//
// Collections become folders, tags become the TAGS attribute, and descriptions
// become <DD>. Folders are emitted in the order the collections are configured, so that the
// shape of the exported file matches the taxonomy the user wrote rather than whatever order
// the database happened to return - and a collection with no bookmarks is omitted rather than
// creating an empty collection in Linkwarden on import.
func BuildNetscape(records []store.ExportRecord, cfg config.Export, collections []string) *netscape.Document {
	grouped := make(map[string][]netscape.Bookmark)

	for _, r := range records {
		grouped[r.Collection] = append(grouped[r.Collection], bookmark(r, cfg))
	}

	doc := &netscape.Document{Title: cfg.Title}

	// Configured collections first, in their configured order.
	for _, name := range collections {
		if marks, ok := grouped[name]; ok {
			doc.Folders = append(doc.Folders, netscape.Folder{Name: name, Bookmarks: marks})
			delete(grouped, name)
		}
	}

	// Then anything left over, sorted. Reaching this means a collection was removed from
	// the config after it was assigned; dropping those bookmarks silently would be worse
	// than emitting them in a folder the user has to notice.
	leftover := make([]string, 0, len(grouped))
	for name := range grouped {
		leftover = append(leftover, name)
	}

	slices.Sort(leftover)

	for _, name := range leftover {
		doc.Folders = append(doc.Folders, netscape.Folder{Name: name, Bookmarks: grouped[name]})
	}

	return doc
}

// bookmark projects one record.
//
// No anchor attributes are emitted. Everything Linkwarden reads - HREF, ADD_DATE, TAGS and the
// <DD> - is a typed field, and everything else the input carried (ICON and ICON_URI base64
// payloads above all) is discarded at import rather than filtered here: see the note in
// docs/ARCHITECTURE.md#export-and-linkwarden. A real export is mostly favicon, so this is the
// difference between a 64 KB file and a 1.2 MB one.
func bookmark(r store.ExportRecord, cfg config.Export) netscape.Bookmark {
	return netscape.Bookmark{
		URL:          r.URL,
		Title:        r.Title,
		Description:  truncate(r.Description, cfg.MaxDescriptionLength),
		AddDate:      r.AddDate,
		LastModified: r.LastModified,
		Private:      r.Private,
		Tags:         r.Tags,
	}
}

// truncate cuts a description to at most limit bytes, on a rune boundary so the result is still
// valid UTF-8. A limit of zero means no limit.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return strings.TrimRight(s[:cut], " ")
}
