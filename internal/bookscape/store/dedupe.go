package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Candidate is one bookmark row inside a duplicate group, with its multi-valued fields
// loaded.
type Candidate struct {
	ID    int64
	URL   string
	Title string

	// Description is the imported <DD>, empty when the export carried none.
	Description string

	AddDate      *int64
	LastModified *int64
	Private      bool

	// CanonicalID is nil for a row that is currently a survivor.
	CanonicalID *int64

	SourcePaths []string
	Tags        []string
}

// Group is every row sharing one comparison key. Candidates are ordered the way survivor
// election wants them: rows with a known date first, earliest first, ties broken by id.
type Group struct {
	URLNorm    string
	Candidates []Candidate
}

// dupeCTE selects the comparison keys that more than one row shares. Used by all three
// queries below so they agree on which groups are in scope.
const dupeCTE = `WITH dupes AS (
	SELECT url_norm FROM bookmarks GROUP BY url_norm HAVING count(*) > 1
)`

// DuplicateGroups returns every group of bookmarks sharing a comparison key.
//
// Every row in a group is returned, including rows that already point at a survivor. That
// makes dedupe re-runnable without building canonical_id chains: a later import carrying an
// earlier add_date can displace the previous survivor, and the rows that pointed at it get
// re-pointed at the new one rather than at a row that is itself now a loser.
func (db *DB) DuplicateGroups(ctx context.Context) ([]Group, error) {
	paths, err := db.groupedStrings(ctx, `
		`+dupeCTE+`
		SELECT sp.bookmark_id, sp.path
		FROM source_paths sp
		JOIN bookmarks b ON b.id = sp.bookmark_id
		JOIN dupes d ON d.url_norm = b.url_norm
		ORDER BY sp.path`, "source paths")
	if err != nil {
		return nil, err
	}

	tags, err := db.groupedStrings(ctx, `
		`+dupeCTE+`
		SELECT bt.bookmark_id, t.name
		FROM bookmark_tags bt
		JOIN tags t ON t.id = bt.tag_id
		JOIN bookmarks b ON b.id = bt.bookmark_id
		JOIN dupes d ON d.url_norm = b.url_norm
		ORDER BY t.name`, "tags")
	if err != nil {
		return nil, err
	}

	rows, err := db.sql.QueryContext(ctx, `
		`+dupeCTE+`
		SELECT b.url_norm, b.id, b.url, coalesce(b.title, ''), coalesce(b.description, ''),
		       b.add_date, b.last_modified, b.private, b.canonical_id
		FROM bookmarks b
		JOIN dupes d ON d.url_norm = b.url_norm
		-- Survivor election order: a row with a known date beats one without, then earliest
		-- first, then lowest id so the result is stable across runs.
		ORDER BY b.url_norm, (b.add_date IS NULL), b.add_date, b.id`)
	if err != nil {
		return nil, fmt.Errorf("read duplicate groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []Group

	for rows.Next() {
		var (
			key string
			c   Candidate
		)

		if err := rows.Scan(&key, &c.ID, &c.URL, &c.Title, &c.Description,
			&c.AddDate, &c.LastModified, &c.Private, &c.CanonicalID); err != nil {
			return nil, fmt.Errorf("scan duplicate candidate: %w", err)
		}

		c.SourcePaths = paths[c.ID]
		c.Tags = tags[c.ID]

		if n := len(groups); n > 0 && groups[n-1].URLNorm == key {
			groups[n-1].Candidates = append(groups[n-1].Candidates, c)

			continue
		}

		groups = append(groups, Group{URLNorm: key, Candidates: []Candidate{c}})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate duplicate groups: %w", err)
	}

	return groups, nil
}

// groupedStrings runs a query returning (bookmark_id, value) pairs and collects them by id.
func (db *DB) groupedStrings(ctx context.Context, query, what string) (map[int64][]string, error) {
	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read %s for duplicate groups: %w", what, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][]string)

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}

		out[id] = append(out[id], value)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}

	return out, nil
}

// Merge is one resolved duplicate group: which row survives, with which merged field values,
// and which rows point at it.
type Merge struct {
	URLNorm string

	// SurvivorID is the row that keeps canonical_id NULL.
	SurvivorID int64

	// LoserIDs are the rows whose canonical_id is set to SurvivorID. Never deleted.
	LoserIDs []int64

	// The merged values written onto the survivor.
	AddDate      *int64
	LastModified *int64
	Title        string
	Description  string
	Private      bool
	SourcePaths  []string
	Tags         []string

	// Reason is recorded in merge_log for every loser.
	Reason string
}

// MergeStats is what ApplyMerges actually changed, as opposed to what it was asked to do.
// Re-running dedupe over an already-deduplicated corpus is a no-op, and the difference has to
// be visible in the output rather than reported as thousands of merges every time.
type MergeStats struct {
	// Groups is how many groups had at least one row change.
	Groups int

	// Losers is how many rows gained or changed a canonical_id.
	Losers int

	// Survivors is how many survivor rows had a field value change.
	Survivors int

	// PathsAdded and TagsAdded are new rows on the survivor from the union.
	PathsAdded int
	TagsAdded  int
}

// ApplyMerges writes a set of merges in one transaction.
//
// Nothing is deleted: a loser keeps its row, its original URL and its own dates, and gains a
// canonical_id pointing at its survivor. The export filter is what decides not to ship it
// (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
func (db *DB) ApplyMerges(ctx context.Context, merges []Merge) (MergeStats, error) {
	var stats MergeStats

	if len(merges) == 0 {
		return stats, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin dedupe transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()
	tags := newTagInterner()

	for _, m := range merges {
		changed, err := applyMerge(ctx, tx, m, now, tags, &stats)
		if err != nil {
			return MergeStats{}, err
		}

		if changed {
			stats.Groups++
		}
	}

	if err := tx.Commit(); err != nil {
		return MergeStats{}, fmt.Errorf("commit dedupe: %w", err)
	}

	return stats, nil
}

func applyMerge(ctx context.Context, tx txer, m Merge, now int64, tags *tagInterner, stats *MergeStats) (bool, error) {
	changed := false

	// The survivor is written unconditionally but reports whether anything differed, so that
	// a re-run over settled data reports zero changes.
	res, err := tx.ExecContext(ctx, `
		UPDATE bookmarks
		SET add_date = ?, last_modified = ?, title = ?, description = ?, private = ?, canonical_id = NULL
		WHERE id = ?
		  AND (add_date IS NOT ? OR last_modified IS NOT ? OR coalesce(title, '') <> ?
		       OR coalesce(description, '') <> ? OR private <> ? OR canonical_id IS NOT NULL)`,
		m.AddDate, m.LastModified, nullString(m.Title), nullString(m.Description), m.Private, m.SurvivorID,
		m.AddDate, m.LastModified, m.Title, m.Description, m.Private)
	if err != nil {
		return false, fmt.Errorf("update survivor %d (%s): %w", m.SurvivorID, m.URLNorm, err)
	}

	if n, err := res.RowsAffected(); err == nil && n > 0 {
		stats.Survivors++

		changed = true
	}

	for _, path := range m.SourcePaths {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO source_paths (bookmark_id, path) VALUES (?, ?)`, m.SurvivorID, path)
		if err != nil {
			return false, fmt.Errorf("add source path %q to survivor %d: %w", path, m.SurvivorID, err)
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			stats.PathsAdded++

			changed = true
		}
	}

	added, err := tags.attachCounting(ctx, tx, m.SurvivorID, m.Tags)
	if err != nil {
		return false, fmt.Errorf("add tags to survivor %d: %w", m.SurvivorID, err)
	}

	if added > 0 {
		stats.TagsAdded += added
		changed = true
	}

	for _, loser := range m.LoserIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE bookmarks SET canonical_id = ? WHERE id = ? AND canonical_id IS NOT ?`,
			m.SurvivorID, loser, m.SurvivorID)
		if err != nil {
			return false, fmt.Errorf("point bookmark %d at survivor %d: %w", loser, m.SurvivorID, err)
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			stats.Losers++
			changed = true
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO merge_log (canonical_id, merged_id, reason, merged_at)
			VALUES (?, ?, ?, ?)`, m.SurvivorID, loser, m.Reason, now); err != nil {
			return false, fmt.Errorf("record merge of %d into %d: %w", loser, m.SurvivorID, err)
		}
	}

	return changed, nil
}

// MergedPaths returns the union of source paths across a group, sorted and deduplicated. It is
// here rather than in the dedupe package so that the store's own tests can build expectations
// the same way.
func MergedPaths(candidates []Candidate) []string {
	var all []string
	for _, c := range candidates {
		all = append(all, c.SourcePaths...)
	}

	slices.Sort(all)

	return slices.Compact(all)
}

// MergedTags returns the union of tags across a group, sorted and deduplicated.
func MergedTags(candidates []Candidate) []string {
	var all []string
	for _, c := range candidates {
		all = append(all, c.Tags...)
	}

	slices.Sort(all)

	return slices.Compact(all)
}
