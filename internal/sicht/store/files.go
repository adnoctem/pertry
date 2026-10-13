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

// timeLayout is how every timestamp is stored.
//
// RFC 3339 in UTC, as text. Chosen because it sorts correctly as a string, which is what lets the
// decisions view order by created_at without a date function, and because it survives a round trip
// through a SQLite column with no driver-specific time handling.
const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(timeLayout, s)
	if err != nil {
		// Tolerate a timestamp written by an older build at second precision.
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return t2, nil
		}

		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}

	return t, nil
}

// File is a row of the files table: one distinct piece of content, identified by its hash.
type File struct {
	// Hash is the SHA-256 of the content, lowercase hex. The primary key for everything:
	// docs/ARCHITECTURE.md#workspace-and-identity keys results by content so they survive the file being moved.
	Hash string

	Size int64

	// Ext is the lowercased extension without a dot.
	Ext string

	// Width and Height are zero when unknown, which happens for a format exiftool cannot
	// measure.
	Width  int
	Height int

	// DHash is a 64-bit perceptual hash, present only for a file that could be decoded. HEIC
	// never has one, because it does not decode in pure Go.
	DHash     int64
	HasDHash  bool
	FirstSeen time.Time
}

// Location is a row of the locations table: one place a hash was found.
type Location struct {
	// Path is workspace-relative with forward slashes on every platform (docs/ARCHITECTURE.md#workspace-and-identity).
	Path   string
	Hash   string
	MTime  time.Time
	SeenAt time.Time
}

// Metadata is a row of the metadata table.
type Metadata struct {
	Hash string

	// ExifJSON is exiftool's output for this file, verbatim, with group-qualified tag names.
	// Stored whole rather than as columns because the useful tags are not knowable in advance
	// and a user writing a rule may reach for any of them.
	ExifJSON string

	// SidecarJSON is the Takeout sidecar, verbatim. Empty when there was none.
	SidecarJSON string

	// ExtractedWith records the tool and version, so a re-extraction under a different
	// exiftool is distinguishable rather than silently conflated.
	ExtractedWith string
}

// Scanned is everything one scanned file contributes, written as a unit.
type Scanned struct {
	File     File
	Location Location
	Metadata Metadata
}

// Stamp is what the incremental check compares: a file is unchanged if its path, modification
// time and size all match the last scan (docs/ARCHITECTURE.md#scan step 1).
type Stamp struct {
	MTime time.Time
	Size  int64
	Hash  string
}

// Stamps returns one stamp per known location, keyed by workspace-relative path.
//
// Loaded in one query rather than asked per file: a scan over several thousand files would
// otherwise issue several thousand round trips against a connection pool deliberately limited to
// one connection. The cost is holding the set in memory, which for the reference archive's 1337
// files is negligible and for a million would be on the order of 150 MB - worth revisiting then,
// and not worth the complexity now.
func (db *DB) Stamps(ctx context.Context) (map[string]Stamp, error) {
	const query = `
		SELECT l.path, l.mtime, f.size, f.hash
		FROM locations l
		JOIN files f ON f.hash = l.hash`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read scan stamps: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]Stamp{}

	for rows.Next() {
		var (
			path, mtime, hash string
			size              int64
		)

		if err := rows.Scan(&path, &mtime, &size, &hash); err != nil {
			return nil, fmt.Errorf("scan stamp row: %w", err)
		}

		parsed, err := parseTime(mtime)
		if err != nil {
			return nil, err
		}

		out[path] = Stamp{MTime: parsed, Size: size, Hash: hash}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scan stamps: %w", err)
	}

	return out, nil
}

// SaveScanned writes a batch of scanned files in one transaction.
//
// One transaction per batch rather than per file: SQLite commits are the dominant cost of a scan,
// and a batch that fails leaves the database exactly as it was, so a re-run is a clean retry
// rather than a partial state to reason about.
func (db *DB) SaveScanned(ctx context.Context, batch []Scanned) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin scan transaction: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	// files: first_seen is preserved on conflict. It records when sicht first saw this
	// content, and a re-scan must not move it forward - it is the only chronological fact
	// sicht itself contributes.
	const fileStmt = `
		INSERT INTO files (hash, size, ext, width, height, dhash, first_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			size   = excluded.size,
			ext    = excluded.ext,
			width  = coalesce(excluded.width, files.width),
			height = coalesce(excluded.height, files.height),
			dhash  = coalesce(excluded.dhash, files.dhash)`

	const locationStmt = `
		INSERT INTO locations (path, hash, mtime, seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET
			hash    = excluded.hash,
			mtime   = excluded.mtime,
			seen_at = excluded.seen_at`

	const metadataStmt = `
		INSERT INTO metadata (hash, exif_json, sidecar_json, extracted_with)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			exif_json      = excluded.exif_json,
			sidecar_json   = coalesce(excluded.sidecar_json, metadata.sidecar_json),
			extracted_with = excluded.extracted_with`

	files, err := tx.PrepareContext(ctx, fileStmt)
	if err != nil {
		return fmt.Errorf("prepare file upsert: %w", err)
	}

	defer func() { _ = files.Close() }()

	locations, err := tx.PrepareContext(ctx, locationStmt)
	if err != nil {
		return fmt.Errorf("prepare location upsert: %w", err)
	}

	defer func() { _ = locations.Close() }()

	metadata, err := tx.PrepareContext(ctx, metadataStmt)
	if err != nil {
		return fmt.Errorf("prepare metadata upsert: %w", err)
	}

	defer func() { _ = metadata.Close() }()

	for _, s := range batch {
		if s.File.Hash == "" {
			return errors.New("refusing to save a file with no hash")
		}

		if _, err := files.ExecContext(ctx,
			s.File.Hash, s.File.Size, s.File.Ext,
			nullInt(s.File.Width, s.File.Width > 0),
			nullInt(s.File.Height, s.File.Height > 0),
			nullInt64(s.File.DHash, s.File.HasDHash),
			formatTime(s.File.FirstSeen),
		); err != nil {
			return fmt.Errorf("save file %s: %w", s.File.Hash, err)
		}

		if s.Location.Path != "" {
			if _, err := locations.ExecContext(ctx,
				s.Location.Path, s.File.Hash,
				formatTime(s.Location.MTime), formatTime(s.Location.SeenAt),
			); err != nil {
				return fmt.Errorf("save location %s: %w", s.Location.Path, err)
			}
		}

		if _, err := metadata.ExecContext(ctx,
			s.File.Hash,
			nullString(s.Metadata.ExifJSON),
			nullString(s.Metadata.SidecarJSON),
			nullString(s.Metadata.ExtractedWith),
		); err != nil {
			return fmt.Errorf("save metadata %s: %w", s.File.Hash, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit scan transaction: %w", err)
	}

	return nil
}

func nullInt(v int, ok bool) any {
	if !ok {
		return nil
	}

	return v
}

func nullInt64(v int64, ok bool) any {
	if !ok {
		return nil
	}

	return v
}

func nullString(s string) any {
	if s == "" {
		return nil
	}

	return s
}

// FileByHash returns one file.
func (db *DB) FileByHash(ctx context.Context, hash string) (File, bool, error) {
	const query = `SELECT hash, size, ext, width, height, dhash, first_seen FROM files WHERE hash = ?`

	file, err := scanFile(db.sql.QueryRowContext(ctx, query, hash))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return File{}, false, nil
	case err != nil:
		return File{}, false, err
	}

	return file, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(row rowScanner) (File, error) {
	var (
		file          File
		width, height sql.NullInt64
		dhash         sql.NullInt64
		firstSeen     string
	)

	if err := row.Scan(&file.Hash, &file.Size, &file.Ext, &width, &height, &dhash, &firstSeen); err != nil {
		return File{}, err
	}

	file.Width = int(width.Int64)
	file.Height = int(height.Int64)
	file.DHash = dhash.Int64
	file.HasDHash = dhash.Valid

	parsed, err := parseTime(firstSeen)
	if err != nil {
		return File{}, err
	}

	file.FirstSeen = parsed

	return file, nil
}

// LocationsForHash returns every path a hash was found at, sorted.
//
// Sorted because docs/ARCHITECTURE.md#apply-manifests-and-undo makes `apply` move the first location and record the rest, and "first" has
// to mean the same thing on every run and every platform.
func (db *DB) LocationsForHash(ctx context.Context, hash string) ([]Location, error) {
	const query = `SELECT path, hash, mtime, seen_at FROM locations WHERE hash = ? ORDER BY path`

	return db.queryLocations(ctx, query, hash)
}

func (db *DB) queryLocations(ctx context.Context, query string, args ...any) ([]Location, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read locations: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Location

	for rows.Next() {
		var (
			loc           Location
			mtime, seenAt string
		)

		if err := rows.Scan(&loc.Path, &loc.Hash, &mtime, &seenAt); err != nil {
			return nil, fmt.Errorf("scan location row: %w", err)
		}

		if loc.MTime, err = parseTime(mtime); err != nil {
			return nil, err
		}

		if loc.SeenAt, err = parseTime(seenAt); err != nil {
			return nil, err
		}

		out = append(out, loc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locations: %w", err)
	}

	return out, nil
}

// MetadataForHash returns the stored metadata.
func (db *DB) MetadataForHash(ctx context.Context, hash string) (Metadata, bool, error) {
	const query = `SELECT hash, exif_json, sidecar_json, extracted_with FROM metadata WHERE hash = ?`

	var (
		out                 Metadata
		exif, sidecar, with sql.NullString
	)

	err := db.sql.QueryRowContext(ctx, query, hash).
		Scan(&out.Hash, &exif, &sidecar, &with)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Metadata{}, false, nil
	case err != nil:
		return Metadata{}, false, fmt.Errorf("read metadata for %s: %w", hash, err)
	}

	out.ExifJSON = exif.String
	out.SidecarJSON = sidecar.String
	out.ExtractedWith = with.String

	return out, true, nil
}

// DeleteLocations removes locations whose paths are no longer present, and then any file left
// with no location at all.
//
// A file vanishing from disk must not silently keep its decisions alive forever, but neither
// should a scan of a subdirectory delete everything outside it - so the caller passes exactly the
// paths it knows are gone.
func (db *DB) DeleteLocations(ctx context.Context, paths []string) (int64, error) {
	if len(paths) == 0 {
		return 0, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin delete transaction: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM locations WHERE path = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare location delete: %w", err)
	}

	defer func() { _ = stmt.Close() }()

	var removed int64

	for _, p := range paths {
		result, err := stmt.ExecContext(ctx, p)
		if err != nil {
			return 0, fmt.Errorf("delete location %s: %w", p, err)
		}

		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("delete location %s: %w", p, err)
		}

		removed += n
	}

	// Files with no remaining location. The cascade on locations does not do this: it runs
	// the other way, from files to locations.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM files WHERE hash NOT IN (SELECT hash FROM locations)`); err != nil {
		return 0, fmt.Errorf("delete orphaned files: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete transaction: %w", err)
	}

	return removed, nil
}

// Counts is a summary of what the database holds.
type Counts struct {
	Files     int
	Locations int
	Results   int
	Decided   int
}

// Count returns the summary.
func (db *DB) Count(ctx context.Context) (Counts, error) {
	var out Counts

	for _, q := range []struct {
		query string
		into  *int
	}{
		{`SELECT count(*) FROM files`, &out.Files},
		{`SELECT count(*) FROM locations`, &out.Locations},
		{`SELECT count(*) FROM results`, &out.Results},
		{`SELECT count(*) FROM decisions`, &out.Decided},
	} {
		if err := db.sql.QueryRowContext(ctx, q.query).Scan(q.into); err != nil {
			return Counts{}, fmt.Errorf("count: %w", err)
		}
	}

	return out, nil
}

// RuleInput is everything rule evaluation needs about one file.
//
// Assembled from a join rather than fetched per file: evaluating rules over ten thousand files
// should be one query, not ten thousand, against a pool deliberately limited to one connection.
type RuleInput struct {
	Hash string
	Ext  string
	Size int64

	Width  int
	Height int

	// ExifJSON and SidecarJSON are the stored documents, empty when absent.
	ExifJSON    string
	SidecarJSON string

	// Path is the lexicographically first location, which is also the one docs/ARCHITECTURE.md#apply-manifests-and-undo makes `apply`
	// move. Empty only if the file has no location at all, which pruning should prevent.
	Path string
}

// pageSize is how many rows EachFileForRules reads per query.
//
// Bounded so that memory does not scale with the archive, and large enough that the per-query
// overhead is negligible: at 500 rows the reference archive is three queries.
const pageSize = 500

// EachFileForRules calls fn for every file, in hash order, with what rule evaluation needs.
//
// Read a page at a time, with each page fully consumed and its rows closed **before** fn is
// called. That is not an optimisation, it is a correctness requirement: the connection pool is
// limited to one connection (so that SQLite never sees concurrent writers), which means a query
// issued while rows are still open waits forever for a connection that the open query is holding.
// Every caller of this does query inside the callback - apply reads each file's decisions, export
// reads its sources, rules writes its results - so streaming a single cursor deadlocked all three.
// It was found by a test; on the owner's archive `rules` had escaped it only because its 411
// results stayed under the 500-row flush threshold.
//
// Paged by `hash > ?` rather than by LIMIT and OFFSET, so that a page boundary cannot skip or
// repeat a row if something is written between pages.
func (db *DB) EachFileForRules(ctx context.Context, fn func(RuleInput) error) error {
	const query = `
		SELECT f.hash, f.ext, f.size, f.width, f.height,
		       m.exif_json, m.sidecar_json,
		       (SELECT min(path) FROM locations WHERE hash = f.hash)
		FROM files f
		LEFT JOIN metadata m ON m.hash = f.hash
		WHERE f.hash > ?
		ORDER BY f.hash
		LIMIT ?`

	after := ""

	for {
		page, err := db.ruleInputPage(ctx, query, after, pageSize)
		if err != nil {
			return err
		}

		if len(page) == 0 {
			return nil
		}

		// The rows are closed by now, so the callback is free to query.
		for _, in := range page {
			if err := fn(in); err != nil {
				return err
			}
		}

		if len(page) < pageSize {
			return nil
		}

		after = page[len(page)-1].Hash
	}
}

// ruleInputPage reads one page and closes its rows before returning.
func (db *DB) ruleInputPage(ctx context.Context, query, after string, limit int) ([]RuleInput, error) {
	rows, err := db.sql.QueryContext(ctx, query, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read files for rule evaluation: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := make([]RuleInput, 0, limit)

	for rows.Next() {
		var (
			in                  RuleInput
			width, height       sql.NullInt64
			exif, sidecar, path sql.NullString
		)

		if err := rows.Scan(&in.Hash, &in.Ext, &in.Size, &width, &height,
			&exif, &sidecar, &path); err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}

		in.Width, in.Height = int(width.Int64), int(height.Int64)
		in.ExifJSON, in.SidecarJSON, in.Path = exif.String, sidecar.String, path.String

		out = append(out, in)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read files for rule evaluation: %w", err)
	}

	return out, nil
}

// DuplicateGroup is one hash found at more than one path.
type DuplicateGroup struct {
	Hash  string
	Size  int64
	Paths []string
}

// ExactDuplicates returns hashes with more than one location, largest first.
//
// These are byte-identical: the same content filed twice. docs/ARCHITECTURE.md#apply-manifests-and-undo makes `apply` move the first
// location and record the rest; removal is a non-goal.
func (db *DB) ExactDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	const query = `
		SELECT f.hash, f.size, l.path
		FROM files f
		JOIN locations l ON l.hash = f.hash
		WHERE f.hash IN (SELECT hash FROM locations GROUP BY hash HAVING count(*) > 1)
		ORDER BY f.size DESC, f.hash, l.path`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read duplicates: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var (
		out     []DuplicateGroup
		current *DuplicateGroup
	)

	for rows.Next() {
		var (
			hash, path string
			size       int64
		)

		if err := rows.Scan(&hash, &size, &path); err != nil {
			return nil, fmt.Errorf("scan duplicate row: %w", err)
		}

		if current == nil || current.Hash != hash {
			out = append(out, DuplicateGroup{Hash: hash, Size: size})
			current = &out[len(out)-1]
		}

		current.Paths = append(current.Paths, path)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read duplicates: %w", err)
	}

	return out, nil
}

// StemGroup is one directory-and-stem that exists in several formats.
type StemGroup struct {
	// Dir is the workspace-relative directory, "" at the root.
	Dir string

	// Stem is the filename without its extension.
	Stem string

	// Paths are the files sharing that stem, sorted.
	Paths []string

	// Exts are the distinct extensions present, sorted.
	Exts []string
}

// FormatTwins returns indexed files that share a directory and a stem but differ in format.
//
// docs/ARCHITECTURE.md#apply-manifests-and-undo's advisory: Google exported some photos more than once, so the same picture exists
// as both a HEIC and a JPEG with different bytes and therefore different hashes. They are separate
// files with separate decisions and sicht does not collapse them - this reports them so they are
// visible without being acted on. Perceptual matching cannot find these, because a HEIC has no
// dHash in a pure-Go build.
func (db *DB) FormatTwins(ctx context.Context) ([]StemGroup, error) {
	const query = `SELECT path, hash FROM locations ORDER BY path`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read locations for format twins: %w", err)
	}

	defer func() { _ = rows.Close() }()

	type key struct{ dir, stem string }

	groups := map[key][]string{}
	hashes := map[key]map[string]struct{}{}

	for rows.Next() {
		var path, hash string

		if err := rows.Scan(&path, &hash); err != nil {
			return nil, fmt.Errorf("scan location row: %w", err)
		}

		dir, file := splitPath(path)
		stem, ext := splitExt(file)

		if ext == "" {
			continue
		}

		k := key{dir, stem}

		groups[k] = append(groups[k], path)

		if hashes[k] == nil {
			hashes[k] = map[string]struct{}{}
		}

		hashes[k][hash] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locations for format twins: %w", err)
	}

	var out []StemGroup

	for k, paths := range groups {
		// One file is not a twin, and neither is the same content filed twice under
		// different names - that is an exact duplicate, which ExactDuplicates reports.
		if len(paths) < 2 || len(hashes[k]) < 2 {
			continue
		}

		exts := map[string]struct{}{}

		for _, p := range paths {
			_, file := splitPath(p)
			if _, ext := splitExt(file); ext != "" {
				exts[strings.ToLower(ext)] = struct{}{}
			}
		}

		// Same stem, same extension, different content cannot happen within one directory,
		// so more than one extension is what makes this a format twin rather than
		// something else.
		if len(exts) < 2 {
			continue
		}

		group := StemGroup{Dir: k.dir, Stem: k.stem, Paths: slices.Clone(paths)}

		for e := range exts {
			group.Exts = append(group.Exts, e)
		}

		slices.Sort(group.Exts)
		slices.Sort(group.Paths)

		out = append(out, group)
	}

	// Stable order so the report does not shuffle between runs.
	slices.SortFunc(out, func(a, b StemGroup) int {
		if c := strings.Compare(a.Dir, b.Dir); c != 0 {
			return c
		}

		return strings.Compare(a.Stem, b.Stem)
	})

	return out, nil
}

// splitPath splits a forward-slashed relative path into its directory and filename.
func splitPath(p string) (dir, file string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}

	return p[:i], p[i+1:]
}

// splitExt splits a filename into its stem and lowercased extension, without the dot.
func splitExt(file string) (stem, ext string) {
	i := strings.LastIndexByte(file, '.')
	if i <= 0 {
		return file, ""
	}

	return file[:i], strings.ToLower(file[i+1:])
}

// DimensionCount is one pixel size and how many files have it.
type DimensionCount struct {
	Width  int
	Height int
	Count  int
}

// Dimensions returns the distinct pixel sizes present, most common first.
//
// This is what populates `screen_resolutions`: docs/ARCHITECTURE.md#rules forbids inventing device dimensions, so the
// list is derived from the user's own files. Files with unknown dimensions are excluded rather
// than reported as 0x0, which would otherwise look like a resolution somebody could match on.
func (db *DB) Dimensions(ctx context.Context) ([]DimensionCount, error) {
	const query = `
		SELECT width, height, count(*)
		FROM files
		WHERE width IS NOT NULL AND height IS NOT NULL AND width > 0 AND height > 0
		GROUP BY width, height
		ORDER BY count(*) DESC, width DESC, height DESC`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read dimensions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []DimensionCount

	for rows.Next() {
		var dc DimensionCount

		if err := rows.Scan(&dc.Width, &dc.Height, &dc.Count); err != nil {
			return nil, fmt.Errorf("scan dimension row: %w", err)
		}

		out = append(out, dc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read dimensions: %w", err)
	}

	return out, nil
}

// ExtensionCount is one extension and how many files have it.
type ExtensionCount struct {
	Ext   string
	Count int
	Bytes int64
}

// Extensions returns the extensions present, most common first.
func (db *DB) Extensions(ctx context.Context) ([]ExtensionCount, error) {
	const query = `
		SELECT ext, count(*), sum(size) FROM files GROUP BY ext ORDER BY count(*) DESC, ext`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read extensions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []ExtensionCount

	for rows.Next() {
		var ec ExtensionCount

		if err := rows.Scan(&ec.Ext, &ec.Count, &ec.Bytes); err != nil {
			return nil, fmt.Errorf("scan extension row: %w", err)
		}

		out = append(out, ec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read extensions: %w", err)
	}

	return out, nil
}

// TotalBytes returns the total size of all distinct content.
func (db *DB) TotalBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64

	if err := db.sql.QueryRowContext(ctx, `SELECT sum(size) FROM files`).Scan(&total); err != nil {
		return 0, fmt.Errorf("sum sizes: %w", err)
	}

	return total.Int64, nil
}

// MoveLocation updates a location's path after the file has been moved on disk.
//
// Called after each successful move rather than in one batch at the end, so that the database and
// the filesystem never disagree by more than the operation in flight. docs/ARCHITECTURE.md#workspace-and-identity keys results by content
// hash precisely so that this is all a move costs: the decisions follow the content, not the path.
func (db *DB) MoveLocation(ctx context.Context, from, to string) error {
	result, err := db.sql.ExecContext(ctx,
		`UPDATE locations SET path = ? WHERE path = ?`, to, from)
	if err != nil {
		return fmt.Errorf("update location %s to %s: %w", from, to, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update location %s: %w", from, err)
	}

	if affected == 0 {
		// Not an error. A companion has no location row, and a re-applied batch may find
		// the row already updated.
		return nil
	}

	return nil
}

// Batch is a row of the batches table: one apply or undo run.
type Batch struct {
	BatchID      string
	Plan         string
	Status       string
	ManifestPath string
	CreatedAt    time.Time
}

// SaveBatch records a batch.
func (db *DB) SaveBatch(ctx context.Context, b Batch) error {
	const stmt = `
		INSERT INTO batches (batch_id, plan, status, manifest_path, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (batch_id) DO UPDATE SET
			status        = excluded.status,
			manifest_path = excluded.manifest_path`

	created := b.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}

	if _, err := db.sql.ExecContext(ctx, stmt,
		b.BatchID, b.Plan, b.Status, nullString(b.ManifestPath), formatTime(created)); err != nil {
		return fmt.Errorf("save batch %s: %w", b.BatchID, err)
	}

	return nil
}

// Batches returns every recorded batch, newest first.
func (db *DB) Batches(ctx context.Context) ([]Batch, error) {
	const query = `
		SELECT batch_id, plan, status, manifest_path, created_at
		FROM batches ORDER BY created_at DESC, batch_id DESC`

	rows, err := db.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read batches: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Batch

	for rows.Next() {
		var (
			b            Batch
			manifestPath sql.NullString
			created      string
		)

		if err := rows.Scan(&b.BatchID, &b.Plan, &b.Status, &manifestPath, &created); err != nil {
			return nil, fmt.Errorf("scan batch row: %w", err)
		}

		b.ManifestPath = manifestPath.String

		if b.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}

		out = append(out, b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read batches: %w", err)
	}

	return out, nil
}

// SetBatchStatus updates one batch's status.
func (db *DB) SetBatchStatus(ctx context.Context, batchID, status string) error {
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE batches SET status = ? WHERE batch_id = ?`, status, batchID); err != nil {
		return fmt.Errorf("update batch %s: %w", batchID, err)
	}

	return nil
}
