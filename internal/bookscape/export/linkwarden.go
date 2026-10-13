package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// Backup is the subset of Linkwarden's own backup document that its importer reads.
//
// Linkwarden's `Backup` type is a whole serialised user - subscriptions, settings, RSS feeds and
// all - but importFromLinkwarden.ts (v2.16.3) touches exactly two of its fields, so those two are
// all this emits. A document carrying the rest would be no more acceptable to it and would invite
// the reader to believe bookscape models a Linkwarden user, which it does not.
//
// Every slice here is emitted as an array and never as null, which is not a style preference: see
// the comment on PinnedLinks and on BackupLink.Tags.
type Backup struct {
	Collections []BackupCollection `json:"collections"`

	// PinnedLinks is always present and normally empty. The importer runs
	// `data?.pinnedLinks.forEach(...)` - the optional chain guards the document and not this
	// field - so a missing or null value throws inside the import transaction. The throw is
	// swallowed by a `.catch` that logs and returns, and the endpoint then answers 200
	// "Success." having imported nothing at all. Go marshals a nil slice as null, so this
	// being initialised rather than nil is the difference between an import and a silent
	// no-op that reports success.
	PinnedLinks []BackupLink `json:"pinnedLinks"`
}

// BackupCollection becomes one Linkwarden collection, which the importer creates unconditionally.
//
// It does not look for an existing collection of the same name, so importing the same document
// twice produces two of every collection. That is a property of the importer rather than of this
// document, and the reason `export` says to try a small import first.
type BackupCollection struct {
	// Name is required and must be a string: the importer calls `e.name.trim()` on it
	// without a nil guard before anything else. It is truncated to 254 characters on import.
	Name string `json:"name"`

	// Description is the collection's own, which bookscape has no source for, so it is
	// always empty. Present because the field is cheap and its absence reads like an
	// oversight.
	Description string `json:"description"`

	Links []BackupLink `json:"links"`
}

// BackupLink becomes one Linkwarden link.
type BackupLink struct {
	// URL is validated with `new URL()` on import and the link is skipped - silently, with a
	// `continue` - if it does not parse. Truncated to 2047 characters.
	URL string `json:"url"`

	// Name is the link's title, truncated to 254 characters on import. Linkwarden's own
	// field name for it is `name`, not `title`.
	Name string `json:"name"`

	// Description is the whole reason this format is the default. Truncated to 254 on
	// import, which bookscape's own max_description_length default of 200 stays under.
	Description string `json:"description"`

	// ImportDate and CreatedAt both carry the original ADD_DATE, because the importer reads
	// `new Date(link.importDate || link.createdAt)` and there is no third fallback. Emitting
	// neither yields `new Date(undefined)`, an Invalid Date, which Prisma rejects - and that
	// rejection rolls back the whole transaction while the endpoint still answers 200
	// "Success". One undated link would therefore discard the entire import, silently, which
	// is why BuildBackup substitutes a date rather than omitting the field.
	//
	// Both are set to the same value so the document says the same thing whichever one a
	// future version reads.
	ImportDate string `json:"importDate"`
	CreatedAt  string `json:"createdAt"`

	// Tags is always present and never null, for the same reason as Backup.PinnedLinks: the
	// importer calls `link.tags.map(...)` with no guard. Each name is truncated to 49
	// characters on import and reused across links rather than duplicated.
	Tags []BackupTag `json:"tags"`
}

// BackupTag is a tag name. Linkwarden's tags are objects in this document, not strings, which is the
// one place the JSON format differs in shape rather than in content from the TAGS attribute.
type BackupTag struct {
	Name string `json:"name"`
}

// BackupStats accounts for anything BuildBackup had to change to produce an importable document.
type BackupStats struct {
	// DatesSubstituted counts links that carried no ADD_DATE and were given the export's own
	// timestamp, because the format has no way to say "date unknown" - see BackupLink.ImportDate.
	// The HTML path has no equivalent: there an undated link simply arrives with a null
	// importDate.
	DatesSubstituted int
}

// BuildBackup projects records into Linkwarden's backup format.
//
// Collections are emitted in configured order and empty ones omitted, exactly as the Netscape
// projection does, so the two formats describe the same archive. `now` is the timestamp to
// substitute for an undated link; it is a parameter so a test can assert on the result.
func BuildBackup(
	records []store.ExportRecord,
	cfg config.Export,
	collections []string,
	now time.Time,
) (*Backup, BackupStats) {
	var stats BackupStats

	grouped := make(map[string][]BackupLink)

	for _, r := range records {
		link, substituted := backupLink(r, cfg, now)
		if substituted {
			stats.DatesSubstituted++
		}

		grouped[r.Collection] = append(grouped[r.Collection], link)
	}

	backup := &Backup{
		Collections: []BackupCollection{},
		PinnedLinks: []BackupLink{},
	}

	appendCollection := func(name string, links []BackupLink) {
		backup.Collections = append(backup.Collections, BackupCollection{
			Name:  name,
			Links: links,
		})
	}

	for _, name := range collections {
		if links, ok := grouped[name]; ok {
			appendCollection(name, links)
			delete(grouped, name)
		}
	}

	// Then anything left over, sorted - a collection removed from the config after it was
	// assigned. Same reasoning as the Netscape projection: emitting them in a collection the
	// user has to notice beats dropping them silently.
	leftover := make([]string, 0, len(grouped))
	for name := range grouped {
		leftover = append(leftover, name)
	}

	slices.Sort(leftover)

	for _, name := range leftover {
		appendCollection(name, grouped[name])
	}

	return backup, stats
}

// backupLink projects one record, reporting whether its date had to be substituted.
func backupLink(r store.ExportRecord, cfg config.Export, now time.Time) (BackupLink, bool) {
	stamp := now.UTC().Format(time.RFC3339)
	substituted := true

	if r.AddDate != nil {
		stamp = time.Unix(*r.AddDate, 0).UTC().Format(time.RFC3339)
		substituted = false
	}

	tags := make([]BackupTag, 0, len(r.Tags))
	for _, t := range r.Tags {
		tags = append(tags, BackupTag{Name: t})
	}

	return BackupLink{
		URL:         r.URL,
		Name:        r.Title,
		Description: truncate(r.Description, cfg.MaxDescriptionLength),
		ImportDate:  stamp,
		CreatedAt:   stamp,
		Tags:        tags,
	}, substituted
}

// EmitBackup encodes a backup document.
//
// Indented, because the file is something a person inspects before handing it to an importer and
// a single 64 KB line is not inspectable. json.Encoder rather than json.MarshalIndent because
// only the encoder can turn HTML escaping off, and Go escapes the three HTML-significant
// characters by default - correct for embedding JSON in a page, merely disfiguring for a document
// full of URLs with query strings.
func EmitBackup(backup *Backup) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(backup); err != nil {
		return nil, fmt.Errorf("encode the backup document: %w", err)
	}

	return buf.Bytes(), nil
}
