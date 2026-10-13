package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Count is one row of a counted breakdown.
type Count struct {
	Label string
	N     int
}

// Overview is the corpus inventory `stats` prints.
type Overview struct {
	Batches   int
	Bookmarks int
	Survivors int
	Collapsed int

	// Undated is how many survivors carry no add_date. A large number means the dates were
	// lost upstream of bookscape, not here.
	Undated int

	Liveness        []Count
	Classifications []Count
	Collections     []Count
	Domains         []Count
	Tags            []Count

	// Years is a date histogram over add_date, which is the thing that makes a decade-old
	// archive legible.
	Years []Count
}

// Stats collects the corpus inventory (cmd/bookscape/README.md).
func (db *DB) Stats(ctx context.Context, topN int) (Overview, error) {
	var o Overview

	scalars := []struct {
		dst   *int
		query string
	}{
		{&o.Batches, `SELECT count(*) FROM import_batches`},
		{&o.Bookmarks, `SELECT count(*) FROM bookmarks`},
		{&o.Survivors, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NULL`},
		{&o.Collapsed, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NOT NULL`},
		{&o.Undated, `SELECT count(*) FROM bookmarks WHERE canonical_id IS NULL AND add_date IS NULL`},
	}

	for _, s := range scalars {
		if err := db.sql.QueryRowContext(ctx, s.query).Scan(s.dst); err != nil {
			return Overview{}, fmt.Errorf("read corpus counts: %w", err)
		}
	}

	breakdowns := []struct {
		dst   *[]Count
		what  string
		query string
	}{
		{&o.Liveness, "liveness breakdown", `
			SELECT coalesce(l.status, 'unchecked'), count(*)
			FROM bookmarks b LEFT JOIN liveness l ON l.bookmark_id = b.id
			WHERE b.canonical_id IS NULL
			GROUP BY 1 ORDER BY 2 DESC, 1`},
		{&o.Classifications, "classification breakdown", `
			SELECT coalesce(c.source, 'unclassified'), count(*)
			FROM bookmarks b LEFT JOIN classifications c ON c.bookmark_id = b.id
			WHERE b.canonical_id IS NULL
			GROUP BY 1 ORDER BY 2 DESC, 1`},
		{&o.Collections, "collection breakdown", `
			SELECT c.collection, count(*)
			FROM bookmarks b JOIN classifications c ON c.bookmark_id = b.id
			WHERE b.canonical_id IS NULL
			GROUP BY 1 ORDER BY 2 DESC, 1`},
		{&o.Years, "date histogram", `
			SELECT strftime('%Y', add_date, 'unixepoch'), count(*)
			FROM bookmarks
			WHERE canonical_id IS NULL AND add_date IS NOT NULL
			GROUP BY 1 ORDER BY 1`},
	}

	for _, b := range breakdowns {
		counts, err := db.counts(ctx, b.query, b.what)
		if err != nil {
			return Overview{}, err
		}

		*b.dst = counts
	}

	// Domains are derived in Go rather than SQL: SQLite has no URL functions, and the host
	// is already embedded in url_norm in a known shape.
	domains, err := db.topDomains(ctx, topN)
	if err != nil {
		return Overview{}, err
	}

	o.Domains = domains

	tags, err := db.counts(ctx, fmt.Sprintf(`
		SELECT t.name, count(*)
		FROM bookmark_tags bt
		JOIN tags t ON t.id = bt.tag_id
		JOIN bookmarks b ON b.id = bt.bookmark_id
		WHERE b.canonical_id IS NULL
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT %d`, topN), "tag breakdown")
	if err != nil {
		return Overview{}, err
	}

	o.Tags = tags

	return o, nil
}

func (db *DB) counts(ctx context.Context, query, what string) ([]Count, error) {
	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Count

	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Label, &c.N); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}

		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}

	return out, nil
}

// topDomains counts survivors per host.
func (db *DB) topDomains(ctx context.Context, topN int) ([]Count, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT url_norm FROM bookmarks WHERE canonical_id IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("read domains: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tally := make(map[string]int)

	for rows.Next() {
		var urlNorm string
		if err := rows.Scan(&urlNorm); err != nil {
			return nil, fmt.Errorf("scan domain: %w", err)
		}

		host := hostOf(urlNorm)
		if host == "" {
			host = "(no host)"
		}

		tally[host]++
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate domains: %w", err)
	}

	return topCounts(tally, topN), nil
}

// topCounts turns a tally into the topN entries, highest first, with ties broken by label so
// the output is stable across runs.
func topCounts(tally map[string]int, topN int) []Count {
	out := make([]Count, 0, len(tally))
	for label, n := range tally {
		out = append(out, Count{Label: label, N: n})
	}

	slices.SortFunc(out, func(a, b Count) int {
		if a.N != b.N {
			return b.N - a.N
		}

		return strings.Compare(a.Label, b.Label)
	})

	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}

	return out
}

// RedirectTarget is one destination that several links resolve to.
type RedirectTarget struct {
	FinalURL string
	N        int
}

// RedirectTargets returns destinations that more than one link redirects to.
//
// A large number of links collapsing to the same destination is itself a useful signal: a
// parked-domain page, a generic 404 served with a 200, a site that moved everything to its
// front page (docs/ARCHITECTURE.md#liveness).
func (db *DB) RedirectTargets(ctx context.Context, topN int) ([]RedirectTarget, error) {
	query := `
		SELECT l.final_url, count(*)
		FROM liveness l
		JOIN bookmarks b ON b.id = l.bookmark_id
		WHERE b.canonical_id IS NULL AND l.final_url IS NOT NULL AND l.final_url <> ''
		GROUP BY 1
		HAVING count(*) > 1
		ORDER BY 2 DESC, 1`

	if topN > 0 {
		query += fmt.Sprintf(" LIMIT %d", topN)
	}

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read redirect targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RedirectTarget

	for rows.Next() {
		var t RedirectTarget
		if err := rows.Scan(&t.FinalURL, &t.N); err != nil {
			return nil, fmt.Errorf("scan redirect target: %w", err)
		}

		out = append(out, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate redirect targets: %w", err)
	}

	return out, nil
}
