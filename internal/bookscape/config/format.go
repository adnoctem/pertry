package config

import "fmt"

// ExportFormat is the document an export produces.
//
// Two formats, because Linkwarden reads two and they are not equivalent: its JSON importer keeps
// the generated description and its HTML importer discards it. See the Format constants and
// docs/ARCHITECTURE.md#export-and-linkwarden.
type ExportFormat string

const (
	// ExportJSON is Linkwarden's own backup document, and the default.
	//
	// Chosen as the default because it is the only one of the two that preserves everything
	// this pipeline produces. Its importer reads `description` into the link's own field
	// (importFromLinkwarden.ts in v2.16.3), so the one-line description the model pass
	// generates - which docs/ARCHITECTURE.md#classification calls where that pass earns its cost - survives the
	// round trip instead of staying behind in SQLite.
	ExportJSON ExportFormat = "json"

	// ExportHTML is the Netscape bookmark document.
	//
	// The format itself carries a description, in the <DD> element, and bookscape writes one.
	// Linkwarden's HTML importer is what drops it: its lookup expects the <DD> as a child
	// node of the <DT>, which no real document produces. Everything else - URLs, titles,
	// ADD_DATE, tags and collections - lands correctly, so this stays available for importing
	// somewhere that is not Linkwarden, and for a browser, which reads this format and not
	// the other.
	ExportHTML ExportFormat = "html"
)

// Extension is the file extension a format conventionally uses.
func (f ExportFormat) Extension() string {
	if f == ExportHTML {
		return ".html"
	}

	return ".json"
}

// ParseExportFormat resolves a --type value or an export.format config value.
//
// "netscape" and "linkwarden" are accepted as aliases because those are what the two formats are
// called when the question is "which importer reads this", which is how somebody arrives at the
// flag in the first place.
func ParseExportFormat(s string) (ExportFormat, error) {
	switch s {
	case "json", "linkwarden":
		return ExportJSON, nil
	case "html", "netscape":
		return ExportHTML, nil
	default:
		return "", fmt.Errorf("unknown export type %q\nhint: json (Linkwarden's backup format, the default) or html (Netscape bookmarks)", s)
	}
}
