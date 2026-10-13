package store

import (
	"context"
	"fmt"
	"time"
)

// Status is a liveness outcome. The set matches the CHECK constraint on liveness.status.
type Status string

const (
	// StatusAlive means the URL resolved to a 2xx, directly or through redirects.
	StatusAlive Status = "alive"

	// StatusDead means the URL is gone: 404, 410, or a host that does not resolve.
	StatusDead Status = "dead"

	// StatusUncertain means the probe was inconclusive - bot protection, a rate limit, a
	// server error, a timeout. Not evidence that the link is gone.
	StatusUncertain Status = "uncertain"

	// StatusSkipped means the URL cannot be probed over HTTP at all, so there is nothing to
	// be uncertain about. javascript: and place: bookmarks land here.
	StatusSkipped Status = "skipped"
)

// Liveness is one recorded probe result.
type Liveness struct {
	Status     Status
	HTTPStatus int
	FinalURL   string
	Method     string
	Error      string
}

// Scope selects which bookmarks a liveness pass covers.
type Scope string

const (
	// ScopeUnchecked covers bookmarks with no recorded result. This is the default, which
	// is what makes the pass resumable: an interrupted run picks up where it stopped
	// instead of re-probing thousands of links that already answered.
	ScopeUnchecked Scope = "unchecked"

	// ScopeUncertain re-probes the inconclusive bucket. The useful one to re-run later: a
	// rate limit or a transient server error may well have cleared.
	ScopeUncertain Scope = "uncertain"

	// ScopeDead re-probes links previously concluded dead.
	ScopeDead Scope = "dead"

	// ScopeAlive re-probes links previously concluded alive.
	ScopeAlive Scope = "alive"

	// ScopeAll re-probes everything.
	ScopeAll Scope = "all"
)

// Scopes is every valid Scope, for flag validation and help text.
var Scopes = []Scope{ScopeUnchecked, ScopeUncertain, ScopeDead, ScopeAlive, ScopeAll}

// ParseScope validates a --only value.
func ParseScope(s string) (Scope, error) {
	for _, valid := range Scopes {
		if Scope(s) == valid {
			return valid, nil
		}
	}

	return "", fmt.Errorf("unknown scope %q", s)
}

// LivenessTarget is one bookmark to probe.
type LivenessTarget struct {
	ID  int64
	URL string
}

// LivenessTargets returns the bookmarks a pass with the given scope should probe.
//
// Only survivors are probed. A row that deduplication collapsed will never be exported, so
// spending a request on it is pure waste - and before `dedupe` has run every row is a
// survivor, so the restriction is correct either way.
func (db *DB) LivenessTargets(ctx context.Context, scope Scope) ([]LivenessTarget, error) {
	var where string

	switch scope {
	case ScopeUnchecked:
		where = `l.bookmark_id IS NULL`
	case ScopeAll:
		where = `1 = 1`
	default:
		where = `l.status = ?`
	}

	query := `
		SELECT b.id, b.url
		FROM bookmarks b
		LEFT JOIN liveness l ON l.bookmark_id = b.id
		WHERE b.canonical_id IS NULL AND ` + where + `
		ORDER BY b.id`

	var args []any
	if scope != ScopeUnchecked && scope != ScopeAll {
		args = append(args, string(scope))
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read liveness targets (%s): %w", scope, err)
	}
	defer func() { _ = rows.Close() }()

	var targets []LivenessTarget

	for rows.Next() {
		var t LivenessTarget
		if err := rows.Scan(&t.ID, &t.URL); err != nil {
			return nil, fmt.Errorf("scan liveness target: %w", err)
		}

		targets = append(targets, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate liveness targets: %w", err)
	}

	return targets, nil
}

// RecordLiveness stores one result, replacing any previous one for the same bookmark.
//
// Called once per probe rather than once per pass: a run over several thousand links takes long
// enough that a crash or a Ctrl-C part way through must not discard completed work.
func (db *DB) RecordLiveness(ctx context.Context, bookmarkID int64, result Liveness) error {
	if _, err := db.sql.ExecContext(ctx, `
		INSERT INTO liveness (bookmark_id, status, http_status, final_url, method, error, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bookmark_id) DO UPDATE SET
		    status      = excluded.status,
		    http_status = excluded.http_status,
		    final_url   = excluded.final_url,
		    method      = excluded.method,
		    error       = excluded.error,
		    checked_at  = excluded.checked_at`,
		bookmarkID, string(result.Status), nullInt(result.HTTPStatus),
		nullString(result.FinalURL), nullString(result.Method), nullString(result.Error),
		time.Now().UTC().Unix()); err != nil {
		return fmt.Errorf("record liveness for bookmark %d: %w", bookmarkID, err)
	}

	return nil
}

// LivenessCounts returns the number of surviving bookmarks in each state, plus how many have no
// result yet under the key "unchecked".
func (db *DB) LivenessCounts(ctx context.Context) (map[string]int, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT coalesce(l.status, 'unchecked'), count(*)
		FROM bookmarks b
		LEFT JOIN liveness l ON l.bookmark_id = b.id
		WHERE b.canonical_id IS NULL
		GROUP BY 1
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read liveness counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)

	for rows.Next() {
		var (
			status string
			n      int
		)

		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan liveness count: %w", err)
		}

		counts[status] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate liveness counts: %w", err)
	}

	return counts, nil
}

// nullInt maps zero to SQL NULL. A result with no HTTP status never reached the server at all,
// which is different from a status of 0.
func nullInt(n int) any {
	if n == 0 {
		return nil
	}

	return n
}
