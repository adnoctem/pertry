// Package netscape wraps github.com/virtualtam/netscape-go/v2 with the guards the library does
// not provide.
//
// Parse and emit go through the same library on purpose: hand-rolled Netscape HTML emission is
// the single most likely cause of a silent import failure downstream
// (docs/ARCHITECTURE.md#process-model).
//
// The library is sound on the format but silent on two failure modes, both found by the spike
// recorded in docs/ARCHITECTURE.md#parsing and pinned by library_test.go:
//
//   - A bookmark whose element or attribute name does not match case-sensitively is dropped,
//     and Unmarshal returns no error. A successful parse is therefore not evidence that
//     anything was parsed. Parse counts anchor tags independently of the library and reports
//     the difference.
//   - An implausible timestamp is silently rescaled rather than rejected, so a WebKit-epoch
//     value meaning 2020 lands in 1970 without a word. Parse range-checks every decoded date.
//
// Neither is treated as a hard error here. Parse returns what it found along with a Report, and
// leaves the decision to the caller - `bookscape import` refuses to proceed quietly on a
// mismatch, but a human looking at a weird export may well want to see it anyway.
package netscape

import (
	"fmt"
	"os"
	"strings"
	"time"

	netscape "github.com/virtualtam/netscape-go/v2"
)

// Entry is one bookmark, flattened out of the folder tree with the path it was filed under.
// This is the shape the importer stores.
type Entry struct {
	// URL is the href exactly as the export carried it. Normalisation happens later and
	// separately; the original is never overwritten.
	URL string

	// Title is the anchor text, trimmed. The library does not trim, and an unclosed <A> -
	// which old exports do contain - absorbs the following whitespace into the title.
	Title string

	// Description is the <DD> text, if any. Chrome does not write these; Firefox and
	// Linkwarden do.
	Description string

	// Path is the slash-joined folder path, excluding the root folder's own name. Empty for
	// a bookmark filed directly at the root.
	Path string

	// AddDate is the ADD_DATE value in unix seconds, or nil when the export carried none.
	// A nil AddDate is meaningfully different from a zero one and must stay distinguishable
	// all the way to export, where a missing date makes the encoder omit the attribute.
	AddDate *int64

	// LastModified is the LAST_MODIFIED value in unix seconds, or nil when absent. Note that
	// the library back-fills this from ADD_DATE, so in practice it is non-nil whenever
	// AddDate is (docs/ARCHITECTURE.md#parsing).
	LastModified *int64

	// Private is the PRIVATE attribute.
	Private bool

	// Tags are the TAGS attribute, split and sorted by the library. Chrome never writes
	// these; a re-import of a bookscape export does.
	Tags []string

	// Attributes carries everything the format does not model: ICON, ICON_URI,
	// PERSONAL_TOOLBAR_FOLDER and whatever else the exporting browser emitted. ADD_DATE,
	// LAST_MODIFIED, PRIVATE and TAGS are never in here - the library decodes them into the
	// typed fields above, and writing them back through this map would emit each attribute
	// twice.
	Attributes map[string]string
}

// SuspectDate is a decoded timestamp that fell outside the plausible range. The library
// rescales an out-of-range value through milliseconds, microseconds and nanoseconds until one
// lands within 30 years of now, so a nonsense input becomes a plausible-looking but wrong date
// rather than an error.
type SuspectDate struct {
	// URL identifies the entry, since at this point there is no database id yet.
	URL string

	// Field is "ADD_DATE" or "LAST_MODIFIED".
	Field string

	// Decoded is what the library produced.
	Decoded time.Time
}

// Report is what Parse observed about the document, beyond the entries themselves. A caller
// that ignores it gets correct data or silently loses some; `import` treats a non-empty
// Report as a reason to stop.
type Report struct {
	// Title is the document's <TITLE>.
	Title string

	// RootName is the root folder's <H1> text.
	RootName string

	// Folders counts folders below the root.
	Folders int

	// AnchorTags is how many anchor elements carrying an href the raw bytes contain, counted
	// without the library. This is the independent check: the parser is exactly what is
	// under suspicion, so it cannot also be the witness.
	AnchorTags int

	// SuspectDates lists timestamps outside the plausible range.
	SuspectDates []SuspectDate

	// UndatedEntries counts entries with no ADD_DATE. Not a defect - plenty of real
	// bookmarks have none - but worth surfacing, because a *large* count means the dates
	// were lost upstream of bookscape.
	UndatedEntries int
}

// Dropped reports how many anchor tags the raw document contains that the parser did not
// return. A positive value means the library silently discarded entries.
func (r Report) Dropped(parsed int) int {
	if r.AnchorTags <= parsed {
		return 0
	}

	return r.AnchorTags - parsed
}

// Plausible bounds for a bookmark timestamp.
//
// The lower bound is deliberately generous: the Netscape bookmark format predates most of the
// web, and a genuinely ancient bookmark should not be flagged. It only needs to be late enough
// to catch the library's rescaling failure, which lands a WebKit-epoch microsecond value in
// mid-1970.
var (
	minPlausibleDate = time.Date(1990, time.January, 1, 0, 0, 0, 0, time.UTC)
	maxPlausibleSkew = 24 * time.Hour
)

// implausible reports whether t is outside the range a real bookmark timestamp can occupy.
// A zero time means "no date at all" and is not implausible.
func implausible(t, now time.Time) bool {
	if t.IsZero() {
		return false
	}

	return t.Before(minPlausibleDate) || t.After(now.Add(maxPlausibleSkew))
}

// CountAnchors counts anchor elements carrying an href attribute in raw, without using the
// library.
//
// Case-insensitivity is the entire point: the library matches `A` and `HREF` case-sensitively
// and drops anything else without a word, so a count that shared the library's case
// sensitivity would agree with it for exactly the wrong reason. A lowercase `<a href=…>` is
// counted here and dropped there, which is what makes the mismatch visible.
//
// This is a tag scanner rather than a regular expression because the count is the only safety
// net against the parser's silent drops, and its errors are not symmetric. Under-counting
// hides a dropped entry, which is the failure being guarded against; over-counting makes
// `import` refuse a perfectly good file. A regexp for `<a[^>]*href=` over-counts on an `<a
// href=…>` that appears inside a quoted attribute value, so the scanner tracks quoting and
// walks attribute names properly.
func CountAnchors(raw []byte) int {
	count := 0

	for i := 0; i < len(raw); i++ {
		if raw[i] != '<' {
			continue
		}

		end := tagEnd(raw, i+1)
		if isAnchorWithHref(raw[i+1 : end]) {
			count++
		}

		i = end
	}

	return count
}

// tagEnd returns the index of the '>' closing the tag whose body starts at from, or len(raw)
// if the tag is unterminated. A '>' inside a quoted attribute value does not close the tag.
func tagEnd(raw []byte, from int) int {
	var quote byte

	for i := from; i < len(raw); i++ {
		switch c := raw[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}

	return len(raw)
}

// isAnchorWithHref reports whether tag - the text between '<' and '>' - is an anchor open tag
// with an href attribute.
func isAnchorWithHref(tag []byte) bool {
	i := 0
	for i < len(tag) && !isTagSpace(tag[i]) {
		i++
	}

	// Exactly "a" or "A". Checking the length rules out "abbr", "address" and friends.
	if name := tag[:i]; len(name) != 1 || (name[0] != 'a' && name[0] != 'A') {
		return false
	}

	for i < len(tag) {
		for i < len(tag) && isTagSpace(tag[i]) {
			i++
		}

		start := i
		for i < len(tag) && !isTagSpace(tag[i]) && tag[i] != '=' {
			i++
		}

		if strings.EqualFold(string(tag[start:i]), "href") {
			return true
		}

		i = skipAttributeValue(tag, i)
	}

	return false
}

// skipAttributeValue advances past an optional `= value`, where the value may be quoted with
// either quote character or be a bare token.
func skipAttributeValue(tag []byte, i int) int {
	for i < len(tag) && isTagSpace(tag[i]) {
		i++
	}

	if i >= len(tag) || tag[i] != '=' {
		return i
	}

	i++

	for i < len(tag) && isTagSpace(tag[i]) {
		i++
	}

	if i < len(tag) && (tag[i] == '"' || tag[i] == '\'') {
		quote := tag[i]

		for i++; i < len(tag) && tag[i] != quote; i++ {
		}

		if i < len(tag) {
			i++
		}

		return i
	}

	for i < len(tag) && !isTagSpace(tag[i]) {
		i++
	}

	return i
}

func isTagSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// ParseFile reads and parses a Netscape bookmark file.
func ParseFile(path string) ([]Entry, Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Report{}, fmt.Errorf("read bookmark file %q: %w", path, err)
	}

	return Parse(raw)
}

// Parse parses a Netscape bookmark document and reports what it found.
func Parse(raw []byte) ([]Entry, Report, error) {
	doc, err := netscape.Unmarshal(raw)
	if err != nil {
		return nil, Report{}, fmt.Errorf("parse Netscape bookmark document: %w\n"+
			"hint: the file must start with <!DOCTYPE NETSCAPE-Bookmark-file-1>, which is what every browser's "+
			"\"export bookmarks to HTML\" writes", err)
	}

	report := Report{
		Title:      doc.Title,
		RootName:   doc.Root.Name,
		AnchorTags: CountAnchors(raw),
	}

	now := time.Now().UTC()

	var entries []Entry
	walk(&doc.Root, nil, now, &entries, &report)

	return entries, report, nil
}

// walk descends the folder tree depth-first, accumulating the folder path. The root folder's
// own name is not part of any path: it is the document's <H1> ("Bookmarks"), not a folder the
// user created.
func walk(f *netscape.Folder, path []string, now time.Time, entries *[]Entry, report *Report) {
	for i := range f.Bookmarks {
		*entries = append(*entries, entry(&f.Bookmarks[i], path, now, report))
	}

	for i := range f.Subfolders {
		sub := &f.Subfolders[i]
		report.Folders++

		if implausible(sub.CreatedAt, now) {
			report.SuspectDates = append(report.SuspectDates, SuspectDate{
				URL:     "folder " + strings.Join(append(path, sub.Name), "/"),
				Field:   "ADD_DATE",
				Decoded: sub.CreatedAt,
			})
		}

		walk(sub, append(path, sub.Name), now, entries, report)
	}
}

func entry(b *netscape.Bookmark, path []string, now time.Time, report *Report) Entry {
	e := Entry{
		URL:          b.URL,
		Title:        strings.TrimSpace(b.Title),
		Description:  strings.TrimSpace(b.Description),
		Path:         strings.Join(path, "/"),
		Private:      b.Private,
		Tags:         b.Tags,
		Attributes:   b.Attributes,
		AddDate:      unixOrNil(b.CreatedAt),
		LastModified: unixOrNil(b.UpdatedAt),
	}

	if e.AddDate == nil {
		report.UndatedEntries++
	}

	// A slice rather than a map, so a document with both fields out of range reports them in
	// a fixed order.
	candidates := []SuspectDate{
		{URL: b.URL, Field: "ADD_DATE", Decoded: b.CreatedAt},
		{URL: b.URL, Field: "LAST_MODIFIED", Decoded: b.UpdatedAt},
	}
	for _, c := range candidates {
		if implausible(c.Decoded, now) {
			report.SuspectDates = append(report.SuspectDates, c)
		}
	}

	return e
}

// Emit renders a document as Netscape bookmark HTML.
//
// Parse and emit go through the same library on purpose: hand-rolled Netscape HTML emission is
// the single most likely cause of a silent import failure downstream.
func Emit(doc *Document) ([]byte, error) {
	out, err := netscape.Marshal(doc.toLibrary())
	if err != nil {
		return nil, fmt.Errorf("encode Netscape bookmark document: %w", err)
	}

	return out, nil
}

// Document is a document to emit: a title and a flat set of named folders, which is the shape
// a projection from SQLite produces. Collections become folders, and Linkwarden creates
// sub-collections from nested ones.
type Document struct {
	Title   string
	Folders []Folder
}

// Folder is one collection's worth of bookmarks.
type Folder struct {
	Name      string
	Bookmarks []Bookmark
}

// Bookmark is one bookmark to emit.
//
// There is deliberately no Attributes field here, unlike Entry on the parse side. The encoder
// writes a typed field *and* every Attributes entry and de-duplicates neither, so carrying an
// arbitrary attribute map into the emitter is how ADD_DATE ends up on an anchor twice - see the
// caution in docs/ARCHITECTURE.md#parsing. Everything the downstream importer reads
// has a typed field below; nothing else is emitted at all.
type Bookmark struct {
	URL         string
	Title       string
	Description string

	// AddDate and LastModified are unix seconds. A nil value makes the encoder omit the
	// attribute, which is the right behaviour for an entry whose date was never known.
	AddDate      *int64
	LastModified *int64

	Private bool
	Tags    []string
}

// toLibrary converts to the library's own types.
func (d *Document) toLibrary() *netscape.Document {
	root := netscape.Folder{Name: d.Title}

	for _, f := range d.Folders {
		folder := netscape.Folder{
			Name:      f.Name,
			Bookmarks: make([]netscape.Bookmark, 0, len(f.Bookmarks)),
		}

		for _, b := range f.Bookmarks {
			folder.Bookmarks = append(folder.Bookmarks, netscape.Bookmark{
				URL:         b.URL,
				Title:       b.Title,
				Description: b.Description,
				CreatedAt:   timeOrZero(b.AddDate),
				UpdatedAt:   timeOrZero(b.LastModified),
				Private:     b.Private,
				Tags:        b.Tags,
			})
		}

		root.Subfolders = append(root.Subfolders, folder)
	}

	return &netscape.Document{Title: d.Title, Root: root}
}

// timeOrZero converts unix seconds to a time, mapping nil to the zero time - which is how the
// encoder is told to omit the attribute entirely.
func timeOrZero(secs *int64) time.Time {
	if secs == nil {
		return time.Time{}
	}

	return time.Unix(*secs, 0).UTC()
}

// unixOrNil converts a decoded timestamp to unix seconds, mapping the zero time - which is how
// the library represents an absent attribute - to nil rather than to a date in year 1.
func unixOrNil(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}

	secs := t.Unix()

	return &secs
}
