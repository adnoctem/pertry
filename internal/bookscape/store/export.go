package store

import (
	"context"
	"fmt"
)

// ExportRecord is one bookmark as the exporter needs it.
type ExportRecord struct {
	ID    int64
	URL   string
	Title string

	// Description is the generated one if there is one, falling back to the <DD> the import
	// carried. The generated description is what the classify pass earns its cost with, so
	// it wins; the imported one keeps a Firefox export's own text from being lost.
	Description string

	// AddDate and LastModified are unix seconds, nil when the export carried none. A nil
	// value makes the encoder omit the attribute rather than write a 1970 date.
	AddDate      *int64
	LastModified *int64

	Private bool

	// Collection becomes the folder the bookmark is filed under in the emitted document.
	Collection string

	Tags []string
}

// ExportFilter is the config-driven filter applied to the projection
// (docs/ARCHITECTURE.md#export-and-linkwarden).
type ExportFilter struct {
	// IncludeDead ships links the liveness pass concluded are gone.
	IncludeDead bool

	// IncludeUncertain ships links whose liveness check was inconclusive. On by default:
	// uncertain usually means bot protection, not a dead link.
	IncludeUncertain bool

	// MinConfidence is the threshold below which a model classification is held back
	// pending review. A manual or rule classification is never held back.
	MinConfidence float64
}

// ExportStats explains what the filter excluded, so that a count smaller than expected is
// accounted for rather than mysterious.
type ExportStats struct {
	Included int

	Collapsed     int
	Unclassified  int
	Dead          int
	Uncertain     int
	PendingReview int
}

// Excluded is the total number of surviving bookmarks the filter held back.
func (s ExportStats) Excluded() int {
	return s.Unclassified + s.Dead + s.Uncertain + s.PendingReview
}

// ExportRecords returns the bookmarks to emit, plus a breakdown of what was excluded and why.
//
// Exclusions are counted in a fixed order of precedence - unclassified, then dead, then
// uncertain, then pending review - so that a bookmark excluded for several reasons is counted
// exactly once, against the first reason that applies.
func (db *DB) ExportRecords(ctx context.Context, filter ExportFilter) ([]ExportRecord, ExportStats, error) {
	var stats ExportStats

	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM bookmarks WHERE canonical_id IS NOT NULL`).Scan(&stats.Collapsed); err != nil {
		return nil, stats, fmt.Errorf("count collapsed bookmarks: %w", err)
	}

	rows, err := db.sql.QueryContext(ctx, `
		SELECT b.id, b.url, coalesce(b.title, ''),
		       coalesce(nullif(c.description, ''), b.description, ''),
		       b.add_date, b.last_modified, b.private,
		       coalesce(c.collection, ''), coalesce(c.source, ''), coalesce(c.confidence, 0),
		       coalesce(l.status, '')
		FROM bookmarks b
		LEFT JOIN classifications c ON c.bookmark_id = b.id
		LEFT JOIN liveness l ON l.bookmark_id = b.id
		WHERE b.canonical_id IS NULL
		ORDER BY c.collection, b.add_date, b.id`)
	if err != nil {
		return nil, stats, fmt.Errorf("read export records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		records []ExportRecord
		ids     []int64
	)

	for rows.Next() {
		var (
			r          ExportRecord
			source     string
			confidence float64
			liveness   string
		)

		if err := rows.Scan(&r.ID, &r.URL, &r.Title, &r.Description,
			&r.AddDate, &r.LastModified, &r.Private,
			&r.Collection, &source, &confidence, &liveness); err != nil {
			return nil, stats, fmt.Errorf("scan export record: %w", err)
		}

		switch {
		case r.Collection == "":
			// Nothing to file it under. The collection is the single axis the emitted
			// folder level projects onto.
			stats.Unclassified++
		case liveness == string(StatusDead) && !filter.IncludeDead:
			stats.Dead++
		case liveness == string(StatusUncertain) && !filter.IncludeUncertain:
			stats.Uncertain++
		case ClassifySource(source) == ClassifySourceModel && confidence < filter.MinConfidence:
			// Low-confidence model results route to review, not to export. A human
			// confirming one turns it into a manual row, which this case no longer
			// matches - that is how `review` gates `export`.
			stats.PendingReview++
		default:
			records = append(records, r)
			ids = append(ids, r.ID)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, stats, fmt.Errorf("iterate export records: %w", err)
	}

	stats.Included = len(records)

	tags, err := db.tagsFor(ctx, ids)
	if err != nil {
		return nil, stats, err
	}

	for i := range records {
		records[i].Tags = tags[records[i].ID]
	}

	return records, stats, nil
}
