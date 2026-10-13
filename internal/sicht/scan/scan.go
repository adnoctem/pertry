package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/meta"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// DefaultBatchSize is how many files are metadata-extracted and written per transaction.
//
// Matched to meta.DefaultBatchSize so that one exiftool invocation feeds one database
// transaction, which keeps the two costs in step and bounds how much work a cancellation loses.
const DefaultBatchSize = 200

// Options configures a scan.
type Options struct {
	// Workspace is the root to walk. Required.
	Workspace *core.Workspace

	// DB is where results are written. Required.
	DB *store.DB

	// Reader extracts metadata. Required; an interface so a test needs no exiftool.
	Reader meta.Reader

	// Extensions are the extensions to index, lowercase and without a dot.
	Extensions []string

	// Root limits the scan to a subdirectory of the workspace. Empty means the whole
	// workspace. A relative path, forward slashes.
	Root string

	// Workers bounds concurrent hashing and decoding. Zero means runtime.NumCPU().
	Workers int

	// BatchSize overrides DefaultBatchSize.
	BatchSize int

	// Full forces every file to be re-read even if its stamp is unchanged.
	Full bool

	// Prune removes locations for files that are no longer on disk. Off by default, because a
	// scan of a subdirectory must not delete what it did not look at, and because an
	// unplugged drive should not erase a year of decisions.
	Prune bool

	// ExtractedWith is recorded against the metadata, e.g. "exiftool 13.55".
	ExtractedWith string

	Log *slog.Logger

	// Progress is called as files are completed. Optional; never called concurrently.
	Progress func(Progress)
}

// Progress reports how far a scan has got.
type Progress struct {
	// Done is how many candidate files have been processed, including skipped ones.
	Done int

	// Total is how many candidates the walk found.
	Total int

	// Path is the file just processed, workspace-relative.
	Path string
}

// Result summarises a scan.
type Result struct {
	// Candidates is how many files had an indexed extension.
	Candidates int

	// Scanned is how many were hashed and written.
	Scanned int

	// Skipped is how many were unchanged since the last scan.
	Skipped int

	// Failed is how many could not be read.
	Failed int

	// Sidecars is how many files matched a Takeout sidecar.
	Sidecars int

	// OrphanSidecars is how many .json files were found that no indexed file claimed. Worth
	// reporting rather than ignoring: a non-zero count on a Takeout export means either a
	// video companion (expected) or a naming variant the rule in docs/ARCHITECTURE.md#scan does not cover.
	OrphanSidecars int

	// NoDimensions is how many files yielded no pixel dimensions from either source.
	NoDimensions int

	// NoPerceptualHash is how many could not be decoded for a dHash. Expected to equal the
	// HEIC count on an iPhone archive.
	NoPerceptualHash int

	// Pruned is how many locations were removed.
	Pruned int

	// Errors are the per-file failures, already logged.
	Errors []error
}

// Run scans the workspace.
//
// The pipeline is: walk to find candidates, then for each batch hash and decode in parallel,
// extract metadata in one exiftool call, pair sidecars, and write the batch in one transaction.
// Batching is what keeps a scan of ten thousand files from being ten thousand transactions and
// ten thousand process launches.
func Run(ctx context.Context, opts Options) (Result, error) {
	var result Result

	if opts.Workspace == nil || opts.DB == nil || opts.Reader == nil {
		return result, errors.New("scan: workspace, database and metadata reader are required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	extensions := normaliseExtensions(opts.Extensions)
	if len(extensions) == 0 {
		return result, errors.New("scan: no extensions configured, so nothing would be indexed")
	}

	walked, err := walk(opts.Workspace, opts.Root, extensions)
	if err != nil {
		return result, err
	}

	result.Candidates = len(walked.candidates)

	stamps, err := opts.DB.Stamps(ctx)
	if err != nil {
		return result, err
	}

	log.Info("scan starting",
		"candidates", result.Candidates,
		// The number of sidecar files, not of directories holding them. The first
		// version of this logged len(walked.sidecars), which is the directory count, and
		// duly reported 14 for an export with 1557 sidecars.
		"sidecars_found", countSidecars(walked),
		"known", len(stamps),
		"workers", workers)

	started := time.Now()

	var done int

	for chunk := range slices.Chunk(walked.candidates, batchSize) {
		if err := ctx.Err(); err != nil {
			// Everything already committed stays committed, which is what makes a
			// cancelled scan resumable rather than wasted.
			return result, err
		}

		batch, stats := processBatch(ctx, chunk, batchParams{
			workspace:     opts.Workspace,
			reader:        opts.Reader,
			stamps:        stamps,
			full:          opts.Full,
			workers:       workers,
			extractedWith: opts.ExtractedWith,
			log:           log,
		})

		result.merge(stats)

		if err := opts.DB.SaveScanned(ctx, batch); err != nil {
			return result, err
		}

		done += len(chunk)

		if opts.Progress != nil {
			last := ""
			if len(chunk) > 0 {
				last = chunk[len(chunk)-1].rel
			}

			opts.Progress(Progress{Done: done, Total: result.Candidates, Path: last})
		}
	}

	// Pairing is a property of the archive, not of this run, so both counts come from the
	// walk. Deriving them from the processed batches would make an incremental scan report
	// zero sidecars and then call every one of them an orphan.
	result.Sidecars = len(walked.claimed)

	// Sidecars nobody claimed. On a Takeout export the expected cause is a video whose
	// extension is not indexed, so this is reported rather than warned about.
	result.OrphanSidecars = countOrphans(walked)

	if opts.Prune {
		pruned, err := prune(ctx, opts.DB, stamps, walked)
		if err != nil {
			return result, err
		}

		result.Pruned = pruned
	}

	log.Info("scan finished",
		"scanned", result.Scanned,
		"skipped", result.Skipped,
		"failed", result.Failed,
		"sidecars", result.Sidecars,
		"pruned", result.Pruned,
		"elapsed", time.Since(started).Round(time.Millisecond))

	return result, nil
}

func (r *Result) merge(other Result) {
	r.Scanned += other.Scanned
	r.Skipped += other.Skipped
	r.Failed += other.Failed
	r.NoDimensions += other.NoDimensions
	r.NoPerceptualHash += other.NoPerceptualHash
	r.Errors = append(r.Errors, other.Errors...)
}

// candidate is one file the walk decided to index.
type candidate struct {
	// abs is the absolute path, for reading and for exiftool.
	abs string

	// rel is the workspace-relative path with forward slashes, which is what is stored.
	rel string

	name  string
	ext   string
	size  int64
	mtime time.Time

	// sidecar is the matched Takeout sidecar's filename, "" when there is none. Resolved
	// during the walk rather than when the file is read, so pairing is complete even for files
	// an incremental scan skips - otherwise every skipped file's sidecar would be counted as
	// an orphan.
	sidecar string
}

// walked is what one walk found.
type walked struct {
	candidates []candidate

	// sidecars maps a directory's absolute path to the set of .json filenames in it, which is
	// what sidecar pairing looks up. Built during the walk so that pairing costs no extra
	// directory reads - the walk has already paid for them, and on a translated filesystem
	// that is the expensive part.
	sidecars map[string]map[string]bool

	// claimed records which sidecars were paired, keyed by absolute path.
	claimed map[string]bool

	// seen holds every candidate's relative path, for pruning.
	seen map[string]bool

	// root is the relative directory the scan was limited to, "" for the whole workspace.
	root string
}

func walk(ws *core.Workspace, root string, extensions map[string]bool) (*walked, error) {
	start := ws.Root()
	if root != "" {
		start = ws.Abs(root)
	}

	info, err := os.Stat(start)
	if err != nil {
		return nil, fmt.Errorf("scan root %s: %w", start, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("scan root %s is not a directory", start)
	}

	out := &walked{
		sidecars: map[string]map[string]bool{},
		claimed:  map[string]bool{},
		seen:     map[string]bool{},
		root:     root,
	}

	err = filepath.WalkDir(start, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is reported and stepped over. Failing the
			// whole scan because one folder is unreadable would be worse.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		rel, relErr := ws.Rel(path)
		if relErr != nil {
			//nolint:nilerr // A path outside the workspace is not part of the archive.
			// Skipped rather than aborting a walk over ten thousand files.
			return nil
		}

		if entry.IsDir() {
			// Never walk into the workspace's own directory: hashing sicht's database
			// and thumbnails as if they were part of the archive is both wasteful and
			// wrong, and the thumbnail cache would then contain itself.
			if core.IsInternal(rel) {
				return fs.SkipDir
			}

			return nil
		}

		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))

		if ext == "json" {
			dir := filepath.Dir(path)

			if out.sidecars[dir] == nil {
				out.sidecars[dir] = map[string]bool{}
			}

			out.sidecars[dir][entry.Name()] = true

			return nil
		}

		if !extensions[ext] {
			return nil
		}

		stat, statErr := entry.Info()
		if statErr != nil {
			//nolint:nilerr // A file that vanished between the walk and the stat is
			// skipped. One unreadable entry must not fail the whole scan.
			return nil
		}

		out.candidates = append(out.candidates, candidate{
			abs:   path,
			rel:   rel,
			name:  entry.Name(),
			ext:   ext,
			size:  stat.Size(),
			mtime: stat.ModTime(),
		})

		out.seen[rel] = true

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", start, err)
	}

	// Sorted so that a scan processes files in a stable order, which makes a partial scan
	// resumable in a predictable place and makes --limit mean something.
	slices.SortFunc(out.candidates, func(a, b candidate) int {
		return strings.Compare(a.rel, b.rel)
	})

	out.pairSidecars()

	return out, nil
}

// pairSidecars resolves each candidate's sidecar and records which sidecars were claimed.
//
// Pure map lookups over what the walk already read, so it costs no directory access - which
// matters when the archive is on a filesystem where every access is a round trip. It runs over the
// sorted candidate list, so that if two files could claim the same sidecar the winner is
// deterministic rather than dependent on walk order.
func (w *walked) pairSidecars() {
	for i := range w.candidates {
		c := &w.candidates[i]

		dir := filepath.Dir(c.abs)

		present := w.sidecars[dir]
		if len(present) == 0 {
			continue
		}

		for _, name := range meta.SidecarNames(c.name) {
			if !present[name] {
				continue
			}

			abs := filepath.Join(dir, name)

			// A sidecar belongs to one file. Nothing in a real export has produced a
			// collision, but a second claimant would mean the constructed name is
			// ambiguous, and silently double-counting it would hide that.
			if w.claimed[abs] {
				continue
			}

			w.claimed[abs] = true
			c.sidecar = name

			break
		}
	}
}

type batchParams struct {
	workspace     *core.Workspace
	reader        meta.Reader
	stamps        map[string]store.Stamp
	full          bool
	workers       int
	extractedWith string
	log           *slog.Logger
}

// hashed is the result of the parallel phase for one file.
type hashed struct {
	cand candidate

	content content

	width, height int

	dhash    uint64
	hasDHash bool

	err error
}

func processBatch(ctx context.Context, chunk []candidate, p batchParams) ([]store.Scanned, Result) {
	var stats Result

	// Phase one: decide which files need reading at all, in series, since it is pure
	// comparison against the stamp map.
	todo := make([]candidate, 0, len(chunk))

	for _, c := range chunk {
		if !p.full {
			if stamp, known := p.stamps[c.rel]; known &&
				stamp.Size == c.size && stamp.MTime.Equal(c.mtime) {
				stats.Skipped++

				continue
			}
		}

		todo = append(todo, c)
	}

	if len(todo) == 0 {
		return nil, stats
	}

	// Phase two: hash and decode in parallel. This is the CPU and I/O heavy part.
	results := make([]hashed, len(todo))

	var wg sync.WaitGroup

	sem := make(chan struct{}, p.workers)

	for i, c := range todo {
		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = hashed{cand: c, err: ctx.Err()}

				return
			}

			results[i] = hashOne(c)
		}()
	}

	wg.Wait()

	// Phase three: one exiftool invocation for the whole batch.
	paths := make([]string, 0, len(todo))

	for _, r := range results {
		if r.err == nil {
			paths = append(paths, r.cand.abs)
		}
	}

	tags, err := p.reader.Read(ctx, paths)
	if err != nil {
		// Metadata extraction failing is not fatal for the batch: hashes and locations are
		// still worth recording, and a later scan can fill the metadata in. Rules that
		// need EXIF simply will not fire yet.
		p.log.Warn("metadata extraction failed for a batch; hashes are still recorded",
			"files", len(paths), "error", err)

		tags = map[string]meta.Tags{}
	}

	// Phase four: assemble.
	now := time.Now().UTC()

	out := make([]store.Scanned, 0, len(todo))

	for _, r := range results {
		if r.err != nil {
			stats.Failed++
			stats.Errors = append(stats.Errors, r.err)

			p.log.Warn("skipping a file that could not be read",
				"path", r.cand.rel, "error", r.err)

			continue
		}

		tag := tags[r.cand.abs]

		width, height := r.width, r.height

		// docs/ARCHITECTURE.md#scan step 4 prefers exiftool, which is also the only source that answers for
		// HEIC; the decoded dimensions are the fallback.
		if w, h, ok := tag.Dimensions(); ok {
			width, height = w, h
		}

		if width == 0 || height == 0 {
			stats.NoDimensions++
		}

		if !r.hasDHash {
			stats.NoPerceptualHash++
		}

		exifJSON := ""

		if len(tag) > 0 {
			encoded, err := json.Marshal(tag)
			if err != nil {
				p.log.Warn("could not encode metadata", "path", r.cand.rel, "error", err)
			} else {
				exifJSON = string(encoded)
			}
		}

		sidecarJSON, _ := readSidecar(r.cand, p)

		out = append(out, store.Scanned{
			File: store.File{
				Hash: r.content.Hash, Size: r.content.Size, Ext: r.cand.ext,
				Width: width, Height: height,
				DHash: int64(r.dhash), HasDHash: r.hasDHash,
				FirstSeen: now,
			},
			Location: store.Location{
				Path: r.cand.rel, Hash: r.content.Hash,
				MTime: r.cand.mtime.UTC(), SeenAt: now,
			},
			Metadata: store.Metadata{
				Hash: r.content.Hash, ExifJSON: exifJSON, SidecarJSON: sidecarJSON,
				ExtractedWith: p.extractedWith,
			},
		})

		stats.Scanned++
	}

	return out, stats
}

func hashOne(c candidate) hashed {
	out := hashed{cand: c}

	content, err := readContent(c.abs, c.size)
	if err != nil {
		out.err = err

		return out
	}

	out.content = content

	if content.Bytes == nil {
		// Too large to have been retained, so there is nothing to decode from. Its
		// dimensions come from exiftool and it gets no perceptual hash.
		return out
	}

	if w, h, err := dimensions(content.Bytes); err == nil {
		out.width, out.height = w, h
	}

	if hash, err := dhash(content.Bytes); err == nil {
		out.dhash, out.hasDHash = hash, true
	}

	return out
}

// readSidecar reads the sidecar the walk already paired with this candidate.
func readSidecar(c candidate, p batchParams) (string, bool) {
	if c.sidecar == "" {
		return "", false
	}

	path := filepath.Join(filepath.Dir(c.abs), c.sidecar)

	raw, err := os.ReadFile(path)
	if err != nil {
		p.log.Warn("could not read a sidecar that exists", "path", path, "error", err)

		return "", false
	}

	parsed, err := meta.ParseSidecar(raw)
	if err != nil {
		// docs/ARCHITECTURE.md#scan step 3: log and carry on. A malformed sidecar costs that file its Takeout
		// metadata, not the scan.
		p.log.Warn("ignoring an unparsable sidecar", "path", path, "error", err)

		return "", false
	}

	// A diagnostic, not a gate. The title can legitimately be longer than the file beside it,
	// which TitleMatches already allows for; anything else means the constructed name found a
	// sidecar belonging to a different file.
	if !parsed.TitleMatches(c.name) {
		p.log.Warn("sidecar title does not match the file it was found for",
			"file", c.name, "sidecar", c.sidecar, "title", parsed.Title)
	}

	return string(raw), true
}

func normaliseExtensions(in []string) map[string]bool {
	out := make(map[string]bool, len(in))

	for _, e := range in {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			out[e] = true
		}
	}

	return out
}

// countSidecars counts the .json files the walk found.
func countSidecars(w *walked) int {
	total := 0

	for _, names := range w.sidecars {
		total += len(names)
	}

	return total
}

// countOrphans counts .json files no candidate claimed.
func countOrphans(w *walked) int {
	return countSidecars(w) - len(w.claimed)
}

// prune removes locations under the scanned root that are no longer on disk.
//
// Scoped to the walked subtree on purpose: a scan of one subdirectory must not delete everything
// outside it, and an archive on an unplugged drive must not lose a year of decisions because the
// mount point was empty.
func prune(ctx context.Context, db *store.DB, stamps map[string]store.Stamp, w *walked) (int, error) {
	var gone []string

	prefix := ""
	if w.root != "" {
		prefix = strings.TrimSuffix(w.root, "/") + "/"
	}

	for path := range stamps {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}

		if !w.seen[path] {
			gone = append(gone, path)
		}
	}

	if len(gone) == 0 {
		return 0, nil
	}

	slices.Sort(gone)

	removed, err := db.DeleteLocations(ctx, gone)
	if err != nil {
		return 0, err
	}

	return int(removed), nil
}
