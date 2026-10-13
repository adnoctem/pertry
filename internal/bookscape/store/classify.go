package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ClassifySource records what produced a classification.
type ClassifySource string

const (
	// ClassifySourceRule is the deterministic first pass.
	ClassifySourceRule ClassifySource = "rule"

	// ClassifySourceModel is the model pass over the remainder.
	ClassifySourceModel ClassifySource = "model"

	// ClassifySourceManual is a human decision made during review. It always wins and is
	// never overwritten by a later pass.
	ClassifySourceManual ClassifySource = "manual"
)

// Classification is one resolved classification for a bookmark.
type Classification struct {
	Collection  string
	Description string

	// Confidence is the model's own confidence, or the rule's (always 1: a rule either
	// matched or it did not).
	Confidence float64

	Source ClassifySource

	// Provenance for a model result. The prompt will be iterated on, and without these
	// there is no way to tell which results came from which version
	// (docs/ARCHITECTURE.md#classification).
	Model      string
	RunID      int64
	PromptHash string
}

// ClassifyTarget is one bookmark to classify, with the inputs both passes use: the URL, the
// title, and every original folder path.
type ClassifyTarget struct {
	ID    int64
	URL   string
	Host  string
	Title string

	// SourcePaths is every folder the link was filed under, across the whole duplicate
	// group. Often a better classification signal than the domain.
	SourcePaths []string

	// ExistingSource is what already classified this row, empty if nothing has.
	ExistingSource ClassifySource
}

// ClassifyScope selects which bookmarks a classification pass covers.
type ClassifyScope string

const (
	// ClassifyScopeUnclassified covers rows with no classification at all. This is what the
	// model pass uses: "the remainder" after the rules pass.
	ClassifyScopeUnclassified ClassifyScope = "unclassified"

	// ClassifyScopeRuleOrNone covers rows with no classification plus rows a rule
	// classified. This is what the rules pass uses, so that editing the rules in
	// bookscape.yaml and re-running picks up the change - without discarding a model
	// result, which cost tokens, or a manual one, which a human made deliberately.
	ClassifyScopeRuleOrNone ClassifyScope = "rule-or-none"
)

// ClassifyTargets returns the bookmarks a pass with the given scope should classify.
//
// Only survivors are returned: a row deduplication collapsed will never be exported.
func (db *DB) ClassifyTargets(ctx context.Context, scope ClassifyScope, limit int) ([]ClassifyTarget, error) {
	var where string

	switch scope {
	case ClassifyScopeUnclassified:
		where = `c.bookmark_id IS NULL`
	case ClassifyScopeRuleOrNone:
		where = `(c.bookmark_id IS NULL OR c.source = 'rule')`
	default:
		return nil, fmt.Errorf("unknown classify scope %q", scope)
	}

	query := `
		SELECT b.id, b.url, b.url_norm, coalesce(b.title, ''), coalesce(c.source, '')
		FROM bookmarks b
		LEFT JOIN classifications c ON c.bookmark_id = b.id
		WHERE b.canonical_id IS NULL AND ` + where + `
		ORDER BY b.id`

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read classify targets (%s): %w", scope, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		targets []ClassifyTarget
		ids     []int64
	)

	for rows.Next() {
		var (
			t       ClassifyTarget
			urlNorm string
			source  string
		)

		if err := rows.Scan(&t.ID, &t.URL, &urlNorm, &t.Title, &source); err != nil {
			return nil, fmt.Errorf("scan classify target: %w", err)
		}

		// The host comes from url_norm rather than url: it is already lowercased and
		// www-stripped, which is exactly what domain rules want to match against.
		t.Host = hostOf(urlNorm)
		t.ExistingSource = ClassifySource(source)

		targets = append(targets, t)
		ids = append(ids, t.ID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classify targets: %w", err)
	}

	paths, err := db.sourcePathsFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	for i := range targets {
		targets[i].SourcePaths = paths[targets[i].ID]
	}

	return targets, nil
}

// sourcePathsFor loads the folder paths for a set of bookmarks in one query.
func (db *DB) sourcePathsFor(ctx context.Context, ids []int64) (map[int64][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// A temporary table rather than an IN clause with thousands of placeholders, which
	// SQLite caps at 999 parameters by default.
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin source path lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE wanted (id INTEGER PRIMARY KEY)`); err != nil {
		return nil, fmt.Errorf("create lookup table: %w", err)
	}

	insert, err := tx.PrepareContext(ctx, `INSERT INTO wanted (id) VALUES (?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare lookup insert: %w", err)
	}
	defer func() { _ = insert.Close() }()

	for _, id := range ids {
		if _, err := insert.ExecContext(ctx, id); err != nil {
			return nil, fmt.Errorf("populate lookup table: %w", err)
		}
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT sp.bookmark_id, sp.path
		FROM source_paths sp
		JOIN wanted w ON w.id = sp.bookmark_id
		ORDER BY sp.path`)
	if err != nil {
		return nil, fmt.Errorf("read source paths: %w", err)
	}

	out := make(map[int64][]string)

	for rows.Next() {
		var (
			id   int64
			path string
		)

		if err := rows.Scan(&id, &path); err != nil {
			_ = rows.Close()

			return nil, fmt.Errorf("scan source path: %w", err)
		}

		out[id] = append(out[id], path)
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()

		return nil, fmt.Errorf("iterate source paths: %w", err)
	}

	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close source path rows: %w", err)
	}

	// The temp table is dropped with the transaction; the rollback in the defer is the
	// cleanup, since nothing here needs committing.
	return out, nil
}

// ClassificationWrite is one classification plus the tags that go with it.
type ClassificationWrite struct {
	BookmarkID int64
	Classification
	Tags []string
}

// RecordClassifications writes a batch of classifications and their tags in one transaction.
//
// A manual classification is never overwritten: a human made that decision deliberately, and a
// later pass has no business undoing it.
func (db *DB) RecordClassifications(ctx context.Context, writes []ClassificationWrite) (int, error) {
	if len(writes) == 0 {
		return 0, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin classification transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()
	tags := newTagInterner()
	written := 0

	for _, w := range writes {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO classifications
			    (bookmark_id, collection, description, confidence, source, model, run_id, prompt_hash, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(bookmark_id) DO UPDATE SET
			    collection  = excluded.collection,
			    description = excluded.description,
			    confidence  = excluded.confidence,
			    source      = excluded.source,
			    model       = excluded.model,
			    run_id      = excluded.run_id,
			    prompt_hash = excluded.prompt_hash,
			    created_at  = excluded.created_at
			WHERE classifications.source <> 'manual'`,
			w.BookmarkID, w.Collection, nullString(w.Description), w.Confidence, string(w.Source),
			nullString(w.Model), nullInt64(w.RunID), nullString(w.PromptHash), now)
		if err != nil {
			return written, fmt.Errorf("record classification for bookmark %d: %w", w.BookmarkID, err)
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			written++
		}

		if err := tags.attach(ctx, tx, w.BookmarkID, w.Tags, TagSource(w.Source)); err != nil {
			return written, fmt.Errorf("attach tags to bookmark %d: %w", w.BookmarkID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit classifications: %w", err)
	}

	return written, nil
}

// NextRunID returns a run identifier one higher than any already recorded, so that every model
// pass is distinguishable in the classifications table.
func (db *DB) NextRunID(ctx context.Context) (int64, error) {
	var next int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT coalesce(max(run_id), 0) + 1 FROM classifications`).Scan(&next); err != nil {
		return 0, fmt.Errorf("read next run id: %w", err)
	}

	return next, nil
}

// ClassificationCounts returns the number of surviving bookmarks per classification source,
// with rows that have none under the key "unclassified".
func (db *DB) ClassificationCounts(ctx context.Context) (map[string]int, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT coalesce(c.source, 'unclassified'), count(*)
		FROM bookmarks b
		LEFT JOIN classifications c ON c.bookmark_id = b.id
		WHERE b.canonical_id IS NULL
		GROUP BY 1
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read classification counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)

	for rows.Next() {
		var (
			source string
			n      int
		)

		if err := rows.Scan(&source, &n); err != nil {
			return nil, fmt.Errorf("scan classification count: %w", err)
		}

		counts[source] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classification counts: %w", err)
	}

	return counts, nil
}

// nullInt64 maps zero to SQL NULL. A rule result has no run id.
func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}

	return n
}

// hostOf extracts the hostname from a normalised URL, for domain rules to match against.
//
// url.Hostname rather than url.Host, so that a port is not part of the comparison, and rather
// than hand-slicing the string, so that userinfo ("https://user@go.dev/x") does not end up in
// the host. A URL that will not parse yields the empty string, which simply matches no domain
// rule - it was already stored, so refusing to classify it here would be worse than
// classifying it by folder path alone.
func hostOf(urlNorm string) string {
	u, err := url.Parse(urlNorm)
	if err != nil {
		return ""
	}

	return strings.ToLower(u.Hostname())
}
