package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SourceHuman is the source string for a decision a person made. It is the only value the
// decisions view ranks above a rule (docs/ARCHITECTURE.md#review).
const SourceHuman = "human"

// Result is one source's verdict about one facet of one file.
type Result struct {
	Hash  string
	Facet string
	Value string

	Confidence    float64
	HasConfidence bool

	// Source is "human", "rule:<index>" or "model:<name>". Part of the primary key, so a
	// human decision and a model's opinion about the same facet coexist and the decisions
	// view picks between them (docs/ARCHITECTURE.md#review).
	Source string

	Stage string

	Model       string
	ModelDigest string
	PromptHash  string
	SchemaHash  string

	PayloadJSON      string
	AlternativesJSON string

	CreatedAt time.Time
}

// SaveResults writes results in one transaction, replacing any existing row for the same
// (hash, facet, source).
//
// Replacing rather than appending is forced by the primary key, and is right: a correction from
// the same source supersedes rather than accumulates. Precedence between *different* sources is
// the decisions view's job and is never resolved by deleting anything.
func (db *DB) SaveResults(ctx context.Context, results []Result) error {
	if len(results) == 0 {
		return nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin results transaction: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	const stmt = `
		INSERT INTO results (
			hash, facet, value, confidence, source, stage,
			model, model_digest, prompt_hash, schema_hash,
			payload_json, alternatives_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hash, facet, source) DO UPDATE SET
			value             = excluded.value,
			confidence        = excluded.confidence,
			stage             = excluded.stage,
			model             = excluded.model,
			model_digest      = excluded.model_digest,
			prompt_hash       = excluded.prompt_hash,
			schema_hash       = excluded.schema_hash,
			payload_json      = excluded.payload_json,
			alternatives_json = excluded.alternatives_json,
			created_at        = excluded.created_at`

	prepared, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("prepare result upsert: %w", err)
	}

	defer func() { _ = prepared.Close() }()

	for _, r := range results {
		created := r.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}

		var confidence any
		if r.HasConfidence {
			confidence = r.Confidence
		}

		if _, err := prepared.ExecContext(ctx,
			r.Hash, r.Facet, r.Value, confidence, r.Source, nullString(r.Stage),
			nullString(r.Model), nullString(r.ModelDigest),
			nullString(r.PromptHash), nullString(r.SchemaHash),
			nullString(r.PayloadJSON), nullString(r.AlternativesJSON),
			formatTime(created),
		); err != nil {
			return fmt.Errorf("save result %s/%s from %s: %w", r.Hash, r.Facet, r.Source, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit results transaction: %w", err)
	}

	return nil
}

// DeleteResultsBySource removes every result a given source produced for a facet.
//
// What `rules` uses to retract decisions before re-evaluating: an edited rule set must not leave
// behind results from a rule that no longer matches, and a rule index that no longer exists would
// otherwise keep deciding files forever.
func (db *DB) DeleteResultsBySource(ctx context.Context, sourcePrefix string) (int64, error) {
	result, err := db.sql.ExecContext(ctx,
		`DELETE FROM results WHERE source LIKE ? || '%'`, sourcePrefix)
	if err != nil {
		return 0, fmt.Errorf("delete results from %q: %w", sourcePrefix, err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete results from %q: %w", sourcePrefix, err)
	}

	return n, nil
}

// Decision is a winning row of the decisions view: the value that holds for a facet after
// human-beats-rule-beats-model precedence is applied.
type Decision struct {
	Hash  string
	Facet string
	Value string

	Confidence    float64
	HasConfidence bool

	Source string
	Stage  string

	Model       string
	ModelDigest string

	PayloadJSON      string
	AlternativesJSON string

	CreatedAt time.Time
}

const decisionColumns = `hash, facet, value, confidence, source, stage, model, model_digest,
	payload_json, alternatives_json, created_at`

// DecisionsForHash returns every decided facet for one file.
func (db *DB) DecisionsForHash(ctx context.Context, hash string) ([]Decision, error) {
	return db.queryDecisions(ctx,
		`SELECT `+decisionColumns+` FROM decisions WHERE hash = ? ORDER BY facet`, hash)
}

// DecisionMap returns the decided facets for one file as facet to value, which is the shape
// `apply` needs to render a target template.
func (db *DB) DecisionMap(ctx context.Context, hash string) (map[string]string, error) {
	decisions, err := db.DecisionsForHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(decisions))
	for _, d := range decisions {
		out[d.Facet] = d.Value
	}

	return out, nil
}

// DecisionsForFacet returns every file's decision for one facet.
func (db *DB) DecisionsForFacet(ctx context.Context, facet string) ([]Decision, error) {
	return db.queryDecisions(ctx,
		`SELECT `+decisionColumns+` FROM decisions WHERE facet = ? ORDER BY hash`, facet)
}

func (db *DB) queryDecisions(ctx context.Context, query string, args ...any) ([]Decision, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read decisions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Decision

	for rows.Next() {
		var (
			d                                           Decision
			confidence                                  sql.NullFloat64
			stage, model, digest, payload, alternatives sql.NullString
			created                                     string
		)

		if err := rows.Scan(&d.Hash, &d.Facet, &d.Value, &confidence, &d.Source,
			&stage, &model, &digest, &payload, &alternatives, &created); err != nil {
			return nil, fmt.Errorf("scan decision row: %w", err)
		}

		d.Confidence, d.HasConfidence = confidence.Float64, confidence.Valid
		d.Stage, d.Model, d.ModelDigest = stage.String, model.String, digest.String
		d.PayloadJSON, d.AlternativesJSON = payload.String, alternatives.String

		if d.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}

		out = append(out, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read decisions: %w", err)
	}

	return out, nil
}

// UndecidedHashes returns the hashes with no decision for a facet, sorted.
//
// This is what `undecided("kind")` in a stage's input expression resolves to, and what `rules`
// reports as its remainder. Sorted so that --limit and --sample take a stable prefix.
func (db *DB) UndecidedHashes(ctx context.Context, facet string) ([]string, error) {
	const query = `
		SELECT f.hash FROM files f
		WHERE NOT EXISTS (
			SELECT 1 FROM decisions d WHERE d.hash = f.hash AND d.facet = ?
		)
		ORDER BY f.hash`

	return db.queryHashes(ctx, query, facet)
}

// DecidedHashes returns the hashes that have a decision for a facet, sorted.
func (db *DB) DecidedHashes(ctx context.Context, facet string) ([]string, error) {
	return db.queryHashes(ctx,
		`SELECT hash FROM decisions WHERE facet = ? ORDER BY hash`, facet)
}

func (db *DB) queryHashes(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read hashes: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var hash string

		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan hash row: %w", err)
		}

		out = append(out, hash)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hashes: %w", err)
	}

	return out, nil
}

// RuleSource renders a rule index as the source string stored in results.
func RuleSource(index int) string { return fmt.Sprintf("rule:%d", index) }

// ModelSource renders a model name as the source string stored in results.
func ModelSource(name string) string { return "model:" + name }

// SourceKind classifies a source string into the three classes the decisions view ranks.
// Used for reporting; the ordering itself lives in SQL, where it is enforced for every reader.
func SourceKind(source string) string {
	switch {
	case source == SourceHuman:
		return SourceHuman
	case strings.HasPrefix(source, "rule:"):
		return "rule"
	case strings.HasPrefix(source, "model:"):
		return "model"
	default:
		return "other"
	}
}

// FacetCount is one (facet, value, source) group and how many files it covers.
type FacetCount struct {
	Facet  string
	Value  string
	Source string
	Count  int
}

// FacetSummary returns the winning decisions grouped by facet, value and source.
//
// Read from the decisions view, so it counts what actually holds rather than every opinion ever
// recorded - the difference being the point of docs/ARCHITECTURE.md#review's precedence.
func (db *DB) FacetSummary(ctx context.Context) ([]FacetCount, error) {
	const query = `
		SELECT facet, value, source, count(*)
		FROM decisions
		GROUP BY facet, value, source
		ORDER BY facet, count(*) DESC, value`

	return db.queryFacetCounts(ctx, query)
}

// ResultSummary returns every recorded opinion grouped the same way, including ones the
// precedence view discards.
//
// The difference between this and FacetSummary is how often a human overrode something, which is
// what docs/ARCHITECTURE.md#confidence's agreement measurement needs.
func (db *DB) ResultSummary(ctx context.Context) ([]FacetCount, error) {
	const query = `
		SELECT facet, value, source, count(*)
		FROM results
		GROUP BY facet, value, source
		ORDER BY facet, count(*) DESC, value`

	return db.queryFacetCounts(ctx, query)
}

func (db *DB) queryFacetCounts(ctx context.Context, query string) ([]FacetCount, error) {
	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("summarise facets: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []FacetCount

	for rows.Next() {
		var fc FacetCount

		if err := rows.Scan(&fc.Facet, &fc.Value, &fc.Source, &fc.Count); err != nil {
			return nil, fmt.Errorf("scan facet row: %w", err)
		}

		out = append(out, fc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summarise facets: %w", err)
	}

	return out, nil
}

// Disagreement is one file where a human decision differs from a model's or a rule's.
type Disagreement struct {
	Hash  string
	Facet string

	HumanValue  string
	OtherValue  string
	OtherSource string
}

// Disagreements returns the files where a human overrode a different value.
//
// Measured against results rather than decisions, because the overridden row is precisely what
// the view hides. This is the raw material for docs/ARCHITECTURE.md#confidence's accuracy numbers.
func (db *DB) Disagreements(ctx context.Context) ([]Disagreement, error) {
	const query = `
		SELECT h.hash, h.facet, h.value, o.value, o.source
		FROM results h
		JOIN results o ON o.hash = h.hash AND o.facet = h.facet AND o.source <> h.source
		WHERE h.source = 'human' AND o.value <> h.value
		ORDER BY h.hash, h.facet, o.source`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read disagreements: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Disagreement

	for rows.Next() {
		var d Disagreement

		if err := rows.Scan(&d.Hash, &d.Facet, &d.HumanValue, &d.OtherValue, &d.OtherSource); err != nil {
			return nil, fmt.Errorf("scan disagreement row: %w", err)
		}

		out = append(out, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read disagreements: %w", err)
	}

	return out, nil
}

// Provenance identifies the exact conditions a model result was produced under (docs/ARCHITECTURE.md#model-stages).
//
// If any of these differ from the current ones, the result is stale: it was produced by different
// weights, a different prompt or a different schema, and conflating it with a fresh one would make
// "I re-ran the stage" mean nothing.
type Provenance struct {
	Value       string
	Model       string
	ModelDigest string
	PromptHash  string
	SchemaHash  string
	Stage       string
}

// Fresh reports whether this result was produced under the given conditions.
//
// An empty recorded field matches anything, which matters for a result written before a particular
// kind of provenance was recorded at all: treating it as stale would force a re-run of the whole
// archive on an upgrade, and treating it as fresh is the lesser wrong - the caller can always pass
// --rerun.
func (p Provenance) Fresh(model, digest, promptHash, schemaHash string) bool {
	for _, pair := range [][2]string{
		{p.Model, model},
		{p.ModelDigest, digest},
		{p.PromptHash, promptHash},
		{p.SchemaHash, schemaHash},
	} {
		if pair[0] != "" && pair[0] != pair[1] {
			return false
		}
	}

	return true
}

// ModelProvenance returns the provenance of every model result for a facet, keyed by hash.
//
// Only model results: a rule's or a human's decision is not something `run` would re-produce, and
// docs/ARCHITECTURE.md#review makes a human decision win regardless.
func (db *DB) ModelProvenance(ctx context.Context, facet string) (map[string]Provenance, error) {
	const query = `
		SELECT hash, value, model, model_digest, prompt_hash, schema_hash, stage
		FROM results
		WHERE facet = ? AND source LIKE 'model:%'`

	rows, err := db.sql.QueryContext(ctx, query, facet)
	if err != nil {
		return nil, fmt.Errorf("read model provenance for %q: %w", facet, err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]Provenance{}

	for rows.Next() {
		var (
			hash                               string
			p                                  Provenance
			model, digest, prompt, schm, stage sql.NullString
		)

		if err := rows.Scan(&hash, &p.Value, &model, &digest, &prompt, &schm, &stage); err != nil {
			return nil, fmt.Errorf("scan provenance row: %w", err)
		}

		p.Model, p.ModelDigest = model.String, digest.String
		p.PromptHash, p.SchemaHash, p.Stage = prompt.String, schm.String, stage.String

		out[hash] = p
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model provenance for %q: %w", facet, err)
	}

	return out, nil
}

// FacetConfidence returns the winning decision's value and confidence per hash for a facet, which
// is what a stage input's `confidence("platform") >= 0.6` is evaluated against.
func (db *DB) FacetConfidence(ctx context.Context, facet string) (map[string]float64, error) {
	const query = `SELECT hash, confidence FROM decisions WHERE facet = ? AND confidence IS NOT NULL`

	rows, err := db.sql.QueryContext(ctx, query, facet)
	if err != nil {
		return nil, fmt.Errorf("read confidence for %q: %w", facet, err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]float64{}

	for rows.Next() {
		var (
			hash  string
			score float64
		)

		if err := rows.Scan(&hash, &score); err != nil {
			return nil, fmt.Errorf("scan confidence row: %w", err)
		}

		out[hash] = score
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read confidence for %q: %w", facet, err)
	}

	return out, nil
}

// AllDecisions returns every facet's winning value per hash, which is what a stage input needs in
// order to resolve a bare facet name.
//
// Loaded in one query rather than per file, for the reason given on Stamps: a stage over ten
// thousand files must not issue ten thousand round trips against a single-connection pool.
func (db *DB) AllDecisions(ctx context.Context) (map[string]map[string]string, error) {
	const query = `SELECT hash, facet, value FROM decisions`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read decisions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]map[string]string{}

	for rows.Next() {
		var hash, facet, value string

		if err := rows.Scan(&hash, &facet, &value); err != nil {
			return nil, fmt.Errorf("scan decision row: %w", err)
		}

		if out[hash] == nil {
			out[hash] = map[string]string{}
		}

		out[hash][facet] = value
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read decisions: %w", err)
	}

	return out, nil
}

// AllConfidence returns every facet's winning confidence per hash.
func (db *DB) AllConfidence(ctx context.Context) (map[string]map[string]float64, error) {
	const query = `SELECT hash, facet, confidence FROM decisions WHERE confidence IS NOT NULL`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]map[string]float64{}

	for rows.Next() {
		var (
			hash, facet string
			score       float64
		)

		if err := rows.Scan(&hash, &facet, &score); err != nil {
			return nil, fmt.Errorf("scan confidence row: %w", err)
		}

		if out[hash] == nil {
			out[hash] = map[string]float64{}
		}

		out[hash][facet] = score
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}

	return out, nil
}

// FacetValueCount is one value of a facet and how many files carry it.
type FacetValueCount struct {
	Value string
	Count int

	// Sources counts how many files got this value from each source class.
	Sources map[string]int
}

// FacetValues returns every value recorded for a facet, most common first.
//
// Read from results rather than from the decisions view, because docs/ARCHITECTURE.md#model-stages's curation wants to see
// every label the model produced - including ones a human has since overridden, since those are
// exactly the labels worth merging or dropping.
func (db *DB) FacetValues(ctx context.Context, facet string) ([]FacetValueCount, error) {
	const query = `
		SELECT value, source, count(*)
		FROM results
		WHERE facet = ?
		GROUP BY value, source
		ORDER BY count(*) DESC, value`

	rows, err := db.sql.QueryContext(ctx, query, facet)
	if err != nil {
		return nil, fmt.Errorf("read values for %q: %w", facet, err)
	}

	defer func() { _ = rows.Close() }()

	byValue := map[string]*FacetValueCount{}

	var order []string

	for rows.Next() {
		var (
			value, source string
			count         int
		)

		if err := rows.Scan(&value, &source, &count); err != nil {
			return nil, fmt.Errorf("scan value row: %w", err)
		}

		entry := byValue[value]
		if entry == nil {
			entry = &FacetValueCount{Value: value, Sources: map[string]int{}}
			byValue[value] = entry

			order = append(order, value)
		}

		entry.Count += count
		entry.Sources[SourceKind(source)] += count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read values for %q: %w", facet, err)
	}

	out := make([]FacetValueCount, 0, len(order))
	for _, value := range order {
		out = append(out, *byValue[value])
	}

	// Re-sorted, because the SQL ordering was by (value, source) group count rather than by
	// the per-value total.
	slices.SortFunc(out, func(a, b FacetValueCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}

		return strings.Compare(a.Value, b.Value)
	})

	return out, nil
}

// ReviewItem is one decision awaiting a human look.
type ReviewItem struct {
	Hash  string
	Path  string
	Facet string
	Value string

	Confidence    float64
	HasConfidence bool

	Source string

	// AlternativesJSON is what else the model weighed, for the review surface to offer.
	AlternativesJSON string

	// PayloadJSON holds the stage's observational fields, which are often the most useful
	// thing a reviewer can read: what the model said it saw.
	PayloadJSON string
}

// ReviewQueue returns the decisions for a facet that warrant a human look, least confident first.
//
// A human decision is never in the queue: docs/ARCHITECTURE.md#review makes it final, and re-presenting it would invite
// someone to confirm their own earlier answer as though it were new evidence. A result with no
// confidence score is included, because "no score" is not "confident" either.
func (db *DB) ReviewQueue(ctx context.Context, facet string, below float64, limit int) ([]ReviewItem, error) {
	const query = `
		SELECT d.hash, d.facet, d.value, d.confidence, d.source,
		       d.alternatives_json, d.payload_json,
		       (SELECT min(path) FROM locations WHERE hash = d.hash)
		FROM decisions d
		WHERE d.facet = ?
		  AND d.source <> 'human'
		  AND (d.confidence IS NULL OR d.confidence < ?)
		ORDER BY d.confidence IS NOT NULL DESC, d.confidence ASC, d.hash
		LIMIT ?`

	if limit <= 0 {
		limit = -1
	}

	rows, err := db.sql.QueryContext(ctx, query, facet, below, limit)
	if err != nil {
		return nil, fmt.Errorf("read the review queue for %q: %w", facet, err)
	}

	defer func() { _ = rows.Close() }()

	var out []ReviewItem

	for rows.Next() {
		var (
			item                        ReviewItem
			score                       sql.NullFloat64
			alternatives, payload, path sql.NullString
		)

		if err := rows.Scan(&item.Hash, &item.Facet, &item.Value, &score, &item.Source,
			&alternatives, &payload, &path); err != nil {
			return nil, fmt.Errorf("scan review row: %w", err)
		}

		item.Confidence, item.HasConfidence = score.Float64, score.Valid
		item.AlternativesJSON, item.PayloadJSON = alternatives.String, payload.String
		item.Path = path.String

		out = append(out, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the review queue for %q: %w", facet, err)
	}

	return out, nil
}

// HashForPath returns the content hash at a workspace-relative path.
//
// What `sicht review set` needs in order to accept a path where the database keys by content:
// docs/ARCHITECTURE.md#workspace-and-identity keys everything by hash so a decision survives the file moving, which means a human
// naming a file has to be resolved to the content it holds right now.
func (db *DB) HashForPath(ctx context.Context, path string) (string, bool, error) {
	var hash string

	err := db.sql.QueryRowContext(ctx,
		`SELECT hash FROM locations WHERE path = ?`, path).Scan(&hash)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("look up %q: %w", path, err)
	}

	return hash, true, nil
}

// HashesByPrefix returns the hashes starting with a prefix, so a human can name one by its first
// few characters rather than all sixty-four.
func (db *DB) HashesByPrefix(ctx context.Context, prefix string) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT hash FROM files WHERE hash LIKE ? || '%' ORDER BY hash LIMIT 10`, prefix)
	if err != nil {
		return nil, fmt.Errorf("look up hashes by prefix: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var hash string

		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan hash row: %w", err)
		}

		out = append(out, hash)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("look up hashes by prefix: %w", err)
	}

	return out, nil
}
