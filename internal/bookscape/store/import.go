package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// txer is the subset of *sql.Tx the helpers in this package need. Naming it lets the dedupe
// and import paths share the tag interner without either one owning the transaction type.
type txer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// TagSource records where a tag came from. Provenance is kept for every derived fact so that a
// re-run of a later stage can be told apart from the input (docs/ARCHITECTURE.md#bookscape-data-model).
type TagSource string

const (
	// TagSourceImport is a TAGS attribute the input already carried.
	TagSourceImport TagSource = "import"

	// TagSourceRule is a tag assigned by a deterministic rule.
	TagSourceRule TagSource = "rule"

	// TagSourceModel is a tag assigned by the model pass.
	TagSourceModel TagSource = "model"

	// TagSourceManual is a tag assigned by a human during review.
	TagSourceManual TagSource = "manual"
)

// Batch is one recorded import of one file.
type Batch struct {
	ID         int64
	Filename   string
	SHA256     string
	EntryCount int
	ImportedAt time.Time
}

// NewBookmark is one bookmark to insert. It is deliberately a narrow struct of its own rather
// than the parser's Entry type: store knows nothing about the Netscape format, and the caller
// is responsible for having derived URLNorm with the configured policy.
type NewBookmark struct {
	// URL is stored verbatim from the export and is never rewritten.
	URL string

	// URLNorm is the derived comparison key deduplication groups on.
	URLNorm string

	// Title may be empty; a decade-old collection is full of entries that have none.
	Title string

	// Description is the <DD> the export carried, if any.
	Description string

	// AddDate and LastModified are unix seconds, or nil when the export carried none. A nil
	// value must stay distinguishable from zero all the way to export, where it makes the
	// encoder omit the attribute rather than write a 1970 date.
	AddDate      *int64
	LastModified *int64

	Private bool

	// SourcePath is the single folder path this anchor was filed under. One anchor produces
	// one row with one path; the union across a duplicate group is assembled by `dedupe`.
	SourcePath string

	// Tags are TAGS values the input carried, stored with TagSourceImport.
	Tags []string
}

// FindBatch returns the batch previously imported from a file with the given SHA-256, or nil if
// there is none. This is what makes `import` idempotent: re-importing the same file is
// detectable rather than producing a second copy of every row.
func (db *DB) FindBatch(ctx context.Context, sha256 string) (*Batch, error) {
	var (
		b        Batch
		imported int64
	)

	err := db.sql.QueryRowContext(ctx,
		`SELECT id, filename, sha256, entry_count, imported_at FROM import_batches WHERE sha256 = ?`,
		sha256,
	).Scan(&b.ID, &b.Filename, &b.SHA256, &b.EntryCount, &imported)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("look up import batch %s: %w", sha256, err)
	}

	b.ImportedAt = time.Unix(imported, 0).UTC()

	return &b, nil
}

// Import records a batch and inserts every bookmark in it.
//
// The whole batch is one transaction, for two reasons. Correctness: a partial import would
// leave an import_batches row claiming an entry_count the bookmarks table does not have, and
// the SHA-256 check would then skip the file on the next run. Speed: modernc.org/sqlite has
// higher per-statement overhead than the cgo binding, and inserting several thousand rows one
// autocommit at a time is noticeably sluggish (docs/ARCHITECTURE.md#storage).
func (db *DB) Import(ctx context.Context, filename, sha256 string, bookmarks []NewBookmark) (*Batch, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin import transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO import_batches (filename, sha256, entry_count, imported_at) VALUES (?, ?, ?, ?)`,
		filename, sha256, len(bookmarks), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("record import batch for %q: %w", filename, err)
	}

	batchID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read import batch id: %w", err)
	}

	insertBookmark, err := tx.PrepareContext(ctx, `
		INSERT INTO bookmarks (batch_id, url, url_norm, title, description, add_date, last_modified, private)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare bookmark insert: %w", err)
	}
	defer func() { _ = insertBookmark.Close() }()

	insertPath, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO source_paths (bookmark_id, path) VALUES (?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare source path insert: %w", err)
	}
	defer func() { _ = insertPath.Close() }()

	tags := newTagInterner()

	for i := range bookmarks {
		b := &bookmarks[i]

		res, err := insertBookmark.ExecContext(ctx,
			batchID, b.URL, b.URLNorm, nullString(b.Title), nullString(b.Description),
			b.AddDate, b.LastModified, b.Private)
		if err != nil {
			return nil, fmt.Errorf("insert bookmark %q: %w", b.URL, err)
		}

		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read id of inserted bookmark %q: %w", b.URL, err)
		}

		if b.SourcePath != "" {
			if _, err := insertPath.ExecContext(ctx, id, b.SourcePath); err != nil {
				return nil, fmt.Errorf("insert source path %q for %q: %w", b.SourcePath, b.URL, err)
			}
		}

		if err := tags.attach(ctx, tx, id, b.Tags, TagSourceImport); err != nil {
			return nil, fmt.Errorf("attach tags to %q: %w", b.URL, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import of %q: %w", filename, err)
	}

	return &Batch{
		ID:         batchID,
		Filename:   filename,
		SHA256:     sha256,
		EntryCount: len(bookmarks),
		ImportedAt: now,
	}, nil
}

// tagInterner maps tag names to tags.id, caching within one transaction so that a corpus with
// a few distinct tags across thousands of bookmarks does not re-query for every row.
type tagInterner struct {
	ids map[string]int64
}

func newTagInterner() *tagInterner {
	return &tagInterner{ids: make(map[string]int64)}
}

// attach resolves each name to a tag id and links it to the bookmark.
func (t *tagInterner) attach(ctx context.Context, tx txer, bookmarkID int64, names []string, source TagSource) error {
	_, err := t.attachWithSource(ctx, tx, bookmarkID, names, source)

	return err
}

// attachCounting is attach for tags arriving from a merge, returning how many links were new.
// The provenance stays TagSourceImport because a merged tag is still a tag the input carried -
// on a different row of the same duplicate group.
func (t *tagInterner) attachCounting(ctx context.Context, tx txer, bookmarkID int64, names []string) (int, error) {
	return t.attachWithSource(ctx, tx, bookmarkID, names, TagSourceImport)
}

func (t *tagInterner) attachWithSource(ctx context.Context, tx txer, bookmarkID int64, names []string, source TagSource) (int, error) {
	added := 0

	for _, name := range names {
		if name == "" {
			continue
		}

		id, err := t.resolve(ctx, tx, name)
		if err != nil {
			return added, err
		}

		// OR IGNORE because the primary key is (bookmark_id, tag_id): the same tag listed
		// twice on one anchor is the input's problem, not an error.
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id, source) VALUES (?, ?, ?)`,
			bookmarkID, id, string(source))
		if err != nil {
			return added, fmt.Errorf("link tag %q: %w", name, err)
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			added++
		}
	}

	return added, nil
}

func (t *tagInterner) resolve(ctx context.Context, tx txer, name string) (int64, error) {
	if id, ok := t.ids[name]; ok {
		return id, nil
	}

	// RETURNING makes this one round trip whether the tag is new or not: ON CONFLICT DO
	// UPDATE (rather than DO NOTHING) means the row is always returned.
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO UPDATE SET name = excluded.name RETURNING id`,
		name).Scan(&id); err != nil {
		return 0, fmt.Errorf("resolve tag %q: %w", name, err)
	}

	t.ids[name] = id

	return id, nil
}

// nullString maps the empty string to SQL NULL, so that "no title" is NULL rather than a row
// that looks like it has a title of zero length.
func nullString(s string) any {
	if s == "" {
		return nil
	}

	return s
}
