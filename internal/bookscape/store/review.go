package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ReviewItem is one bookmark as the review pass sees it: everything a human needs to judge the
// classification without opening the link.
type ReviewItem struct {
	ID    int64
	URL   string
	Title string

	Collection  string
	Description string
	Confidence  float64
	Source      ClassifySource

	// Liveness is the probe result, empty when the link has not been checked.
	Liveness Status

	// SourcePaths are the folders the link was originally filed under - the signal a human
	// is most likely to want when the model's collection looks wrong.
	SourcePaths []string

	Tags []string
}

// ReviewFilter selects which bookmarks to review.
type ReviewFilter string

const (
	// ReviewFilterPending covers model classifications below the confidence threshold:
	// exactly the rows that are blocking the export.
	ReviewFilterPending ReviewFilter = "pending"

	// ReviewFilterModel covers every model classification, confident or not. The useful
	// one for spot-checking a prompt change.
	ReviewFilterModel ReviewFilter = "model"

	// ReviewFilterAll covers every classified survivor.
	ReviewFilterAll ReviewFilter = "all"

	// ReviewFilterUnclassified covers survivors with no classification at all.
	ReviewFilterUnclassified ReviewFilter = "unclassified"
)

// ReviewFilters is every valid filter, for flag validation and help text.
var ReviewFilters = []ReviewFilter{
	ReviewFilterPending, ReviewFilterModel, ReviewFilterAll, ReviewFilterUnclassified,
}

// ParseReviewFilter validates an --only value.
func ParseReviewFilter(s string) (ReviewFilter, error) {
	for _, valid := range ReviewFilters {
		if ReviewFilter(s) == valid {
			return valid, nil
		}
	}

	return "", fmt.Errorf("unknown review filter %q", s)
}

// ReviewItems returns bookmarks to review, lowest confidence first so that the worst
// classifications are the ones a sampled pass actually looks at.
func (db *DB) ReviewItems(ctx context.Context, filter ReviewFilter, minConfidence float64, limit int) ([]ReviewItem, error) {
	var (
		where string
		args  []any
	)

	switch filter {
	case ReviewFilterPending:
		where = `c.source = 'model' AND coalesce(c.confidence, 0) < ?`
		args = append(args, minConfidence)
	case ReviewFilterModel:
		where = `c.source = 'model'`
	case ReviewFilterAll:
		where = `c.bookmark_id IS NOT NULL`
	case ReviewFilterUnclassified:
		where = `c.bookmark_id IS NULL`
	default:
		return nil, fmt.Errorf("unknown review filter %q", filter)
	}

	query := `
		SELECT b.id, b.url, coalesce(b.title, ''),
		       coalesce(c.collection, ''), coalesce(c.description, ''),
		       coalesce(c.confidence, 0), coalesce(c.source, ''),
		       coalesce(l.status, '')
		FROM bookmarks b
		LEFT JOIN classifications c ON c.bookmark_id = b.id
		LEFT JOIN liveness l ON l.bookmark_id = b.id
		WHERE b.canonical_id IS NULL AND ` + where + `
		ORDER BY coalesce(c.confidence, 0), b.id`

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read review items (%s): %w", filter, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		items []ReviewItem
		ids   []int64
	)

	for rows.Next() {
		var (
			item     ReviewItem
			source   string
			liveness string
		)

		if err := rows.Scan(&item.ID, &item.URL, &item.Title,
			&item.Collection, &item.Description, &item.Confidence, &source, &liveness); err != nil {
			return nil, fmt.Errorf("scan review item: %w", err)
		}

		item.Source = ClassifySource(source)
		item.Liveness = Status(liveness)

		items = append(items, item)
		ids = append(ids, item.ID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review items: %w", err)
	}

	paths, err := db.sourcePathsFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	tags, err := db.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	for i := range items {
		items[i].SourcePaths = paths[items[i].ID]
		items[i].Tags = tags[items[i].ID]
	}

	return items, nil
}

// tagsFor loads tag names for a set of bookmarks.
func (db *DB) tagsFor(ctx context.Context, ids []int64) (map[int64][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	out := make(map[int64][]string)

	// Chunked to stay well inside SQLite's default 999-parameter limit. Review works on a
	// sample rather than the whole corpus, so a handful of round trips is fine and avoids
	// the temp table the classify path needs.
	const chunk = 500

	for start := 0; start < len(ids); start += chunk {
		batch := ids[start:min(start+chunk, len(ids))]

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch))

		for _, id := range batch {
			args = append(args, id)
		}

		rows, err := db.sql.QueryContext(ctx, `
			SELECT bt.bookmark_id, t.name
			FROM bookmark_tags bt
			JOIN tags t ON t.id = bt.tag_id
			WHERE bt.bookmark_id IN (`+placeholders+`)
			ORDER BY t.name`, args...)
		if err != nil {
			return nil, fmt.Errorf("read tags: %w", err)
		}

		for rows.Next() {
			var (
				id   int64
				name string
			)

			if err := rows.Scan(&id, &name); err != nil {
				_ = rows.Close()

				return nil, fmt.Errorf("scan tag: %w", err)
			}

			out[id] = append(out[id], name)
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()

			return nil, fmt.Errorf("iterate tags: %w", err)
		}

		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close tag rows: %w", err)
		}
	}

	return out, nil
}

// ManualDecision is a human's override for one bookmark. An empty Collection or Description
// leaves the existing value alone, so that correcting only the collection does not wipe a
// generated description that was fine.
type ManualDecision struct {
	BookmarkID  int64
	Collection  string
	Description string

	// Tags replace the existing manual tags when ReplaceTags is set; otherwise they are
	// added.
	Tags        []string
	ReplaceTags bool
}

// RecordManual stores a human decision. It always wins: source becomes 'manual', which every
// later pass refuses to overwrite.
func (db *DB) RecordManual(ctx context.Context, d ManualDecision) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin manual decision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()

	// coalesce(nullif(?, ''), …) is what makes a blank field mean "leave it" rather than
	// "clear it": a reviewer correcting the collection should not lose the description.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO classifications (bookmark_id, collection, description, confidence, source, created_at)
		VALUES (?, ?, ?, 1, 'manual', ?)
		ON CONFLICT(bookmark_id) DO UPDATE SET
		    collection  = coalesce(nullif(excluded.collection, ''), classifications.collection),
		    description = coalesce(nullif(excluded.description, ''), classifications.description),
		    confidence  = 1,
		    source      = 'manual',
		    created_at  = excluded.created_at`,
		d.BookmarkID, d.Collection, d.Description, now)
	if err != nil {
		return fmt.Errorf("record manual decision for bookmark %d: %w", d.BookmarkID, err)
	}

	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("bookmark %d has no classification to amend and no collection was given\n"+
			"hint: pass collection=<name> to classify it outright", d.BookmarkID)
	}

	if d.ReplaceTags {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM bookmark_tags WHERE bookmark_id = ?`, d.BookmarkID); err != nil {
			return fmt.Errorf("clear tags for bookmark %d: %w", d.BookmarkID, err)
		}
	}

	if err := newTagInterner().attach(ctx, tx, d.BookmarkID, d.Tags, TagSourceManual); err != nil {
		return fmt.Errorf("attach manual tags to bookmark %d: %w", d.BookmarkID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit manual decision: %w", err)
	}

	return nil
}

// Accept promotes model classifications to 'manual' without changing their values. This is a
// reviewer saying "yes, these are right", and it is what clears the export gate for a
// low-confidence result.
func (db *DB) Accept(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin accept: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()
	accepted := 0

	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `
			UPDATE classifications
			SET source = 'manual', confidence = 1, created_at = ?
			WHERE bookmark_id = ? AND source <> 'manual'`, now, id)
		if err != nil {
			return accepted, fmt.Errorf("accept classification for bookmark %d: %w", id, err)
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			accepted++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit accept: %w", err)
	}

	return accepted, nil
}

// PendingReviewCount is how many model classifications fall below the confidence threshold.
// These do not reach the export until a human confirms them, which is how `review` gates
// `export` (docs/ARCHITECTURE.md sections 5.5 and 2).
func (db *DB) PendingReviewCount(ctx context.Context, minConfidence float64) (int, error) {
	var n int

	if err := db.sql.QueryRowContext(ctx, `
		SELECT count(*)
		FROM bookmarks b
		JOIN classifications c ON c.bookmark_id = b.id
		WHERE b.canonical_id IS NULL AND c.source = 'model' AND coalesce(c.confidence, 0) < ?`,
		minConfidence).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending reviews: %w", err)
	}

	return n, nil
}

// PendingReviewIDs returns the bookmark ids blocking the export, for `review accept --all`.
func (db *DB) PendingReviewIDs(ctx context.Context, minConfidence float64) ([]int64, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT b.id
		FROM bookmarks b
		JOIN classifications c ON c.bookmark_id = b.id
		WHERE b.canonical_id IS NULL AND c.source = 'model' AND coalesce(c.confidence, 0) < ?
		ORDER BY b.id`, minConfidence)
	if err != nil {
		return nil, fmt.Errorf("read pending review ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending review id: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending review ids: %w", err)
	}

	return ids, nil
}
