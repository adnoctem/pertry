package scan_test

import (
	"context"
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/meta"
	"github.com/adnoctem/pertry/internal/sicht/scan"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// fakeReader is a meta.Reader that returns canned tags, so a scan needs no exiftool.
type fakeReader struct {
	tags map[string]meta.Tags

	// byName keys tags by base filename, which is easier to write in a test than by the
	// temp directory's absolute path.
	byName map[string]meta.Tags

	// fail makes Read return an error, to exercise the degraded path.
	fail error

	// calls counts invocations, so batching is observable.
	calls int

	// batchSizes records how many paths each call received.
	batchSizes []int
}

func (f *fakeReader) Read(_ context.Context, paths []string) (map[string]meta.Tags, error) {
	f.calls++
	f.batchSizes = append(f.batchSizes, len(paths))

	if f.fail != nil {
		return nil, f.fail
	}

	out := map[string]meta.Tags{}

	for _, p := range paths {
		if tags, ok := f.tags[p]; ok {
			out[p] = tags

			continue
		}

		if tags, ok := f.byName[filepath.Base(p)]; ok {
			out[p] = tags
		}
	}

	return out, nil
}

// archive builds a workspace with files in it and returns the workspace and an open database.
func archive(t *testing.T, files map[string][]byte) (*core.Workspace, *store.DB) {
	t.Helper()

	root := t.TempDir()

	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}

		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	ws, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close(): %v", err)
		}
	})

	return ws, db
}

func defaultExtensions() []string {
	return []string{"jpg", "jpeg", "png", "heic", "heif", "webp", "gif"}
}

func run(t *testing.T, ws *core.Workspace, db *store.DB, reader meta.Reader, mutate func(*scan.Options)) scan.Result {
	t.Helper()

	opts := scan.Options{
		Workspace:  ws,
		DB:         db,
		Reader:     reader,
		Extensions: defaultExtensions(),
		Workers:    2,
	}

	if mutate != nil {
		mutate(&opts)
	}

	result, err := scan.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("scan.Run() error = %v, want nil", err)
	}

	return result
}

func TestScanIndexesFiles(t *testing.T) {
	t.Parallel()

	png := encodePNG(t, gradient(64, 48))

	ws, db := archive(t, map[string][]byte{
		"Fotos von 2019/IMG_0001.png": png,
		"Fotos von 2019/IMG_0002.png": encodePNG(t, gradient(32, 24)),
		"notes.txt":                   []byte("not an image"),
	})

	result := run(t, ws, db, &fakeReader{}, nil)

	if result.Candidates != 2 {
		t.Errorf("Candidates = %d, want 2 - notes.txt has no indexed extension", result.Candidates)
	}

	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}

	counts, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Files != 2 || counts.Locations != 2 {
		t.Errorf("Count() = %+v, want 2 files and 2 locations", counts)
	}

	stamps, err := db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	// Paths are stored workspace-relative with forward slashes, whatever the platform.
	if _, ok := stamps["Fotos von 2019/IMG_0001.png"]; !ok {
		t.Errorf("Stamps() = %v, want a forward-slashed relative path", stamps)
	}
}

// TestScanSkipsItsOwnWorkspace matters twice over: hashing sicht's own database is wasted work,
// and the thumbnail cache would otherwise end up containing itself.
func TestScanSkipsItsOwnWorkspace(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"photo.png": encodePNG(t, gradient(32, 24)),
	})

	// A plausible-looking image inside the workspace directory.
	thumb := filepath.Join(ws.ThumbsPath(), "ab", "cd.png")
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(thumb, encodePNG(t, gradient(16, 16)), 0o644); err != nil {
		t.Fatalf("write thumb: %v", err)
	}

	result := run(t, ws, db, &fakeReader{}, nil)

	if result.Candidates != 1 {
		t.Errorf("Candidates = %d, want 1 - the file under .sicht/ must not be indexed", result.Candidates)
	}
}

// TestIncrementalScanSkipsUnchangedFiles is docs/ARCHITECTURE.md#scan step 1. Without it a re-scan re-reads and
// re-hashes the whole archive, which on a translated filesystem is minutes rather than seconds.
func TestIncrementalScanSkipsUnchangedFiles(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"a.png": encodePNG(t, gradient(32, 24)),
		"b.png": encodePNG(t, gradient(48, 32)),
	})

	if got := run(t, ws, db, &fakeReader{}, nil); got.Scanned != 2 {
		t.Fatalf("first scan Scanned = %d, want 2", got.Scanned)
	}

	second := run(t, ws, db, &fakeReader{}, nil)

	if second.Skipped != 2 || second.Scanned != 0 {
		t.Errorf("second scan = %d scanned, %d skipped; want 0 and 2", second.Scanned, second.Skipped)
	}
}

func TestFullScanRereadsEverything(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(32, 24))})

	run(t, ws, db, &fakeReader{}, nil)

	forced := run(t, ws, db, &fakeReader{}, func(o *scan.Options) { o.Full = true })

	if forced.Scanned != 1 || forced.Skipped != 0 {
		t.Errorf("Full scan = %d scanned, %d skipped; want 1 and 0", forced.Scanned, forced.Skipped)
	}
}

// TestAChangedFileIsRescanned covers the other half of the stamp check.
func TestAChangedFileIsRescanned(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(32, 24))})

	run(t, ws, db, &fakeReader{}, nil)

	path := filepath.Join(ws.Root(), "a.png")

	// Different content and a different size, with an mtime far enough from the original
	// that no filesystem timestamp granularity can confuse the two.
	if err := os.WriteFile(path, encodePNG(t, gradient(64, 48)), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	second := run(t, ws, db, &fakeReader{}, nil)

	if second.Scanned != 1 {
		t.Errorf("Scanned = %d, want the changed file to be re-read", second.Scanned)
	}

	counts, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	// New content is a new file row; the path still has exactly one location.
	if counts.Files != 2 || counts.Locations != 1 {
		t.Errorf("Count() = %+v, want 2 files and 1 location", counts)
	}
}

// TestScanPairsSidecars uses the real truncated naming from docs/ARCHITECTURE.md#scan, not a tidy invented one.
func TestScanPairsSidecars(t *testing.T) {
	t.Parallel()

	sidecar := func(title string) []byte {
		raw, err := json.Marshal(map[string]any{
			"title":          title,
			"photoTakenTime": map[string]string{"timestamp": "1539208044"},
		})
		if err != nil {
			t.Fatalf("marshal sidecar: %v", err)
		}

		return raw
	}

	ws, db := archive(t, map[string][]byte{
		// The ordinary case.
		"IMG_1854.JPG": encodeJPEG(t, gradient(64, 48), 80),
		"IMG_1854.JPG.supplemental-metadata.json": sidecar("IMG_1854.JPG"),

		// A duplicate counter, which moves to the end on the sidecar.
		"IMG_1663(1).JPG": encodeJPEG(t, gradient(48, 32), 80),
		"IMG_1663.JPG.supplemental-metadata(1).json": sidecar("IMG_1663.JPG"),

		// Truncated at the cap, so the suffix is cut.
		"Corporate_Identity_PZ.jpg":                           encodeJPEG(t, gradient(32, 24), 80),
		"Corporate_Identity_PZ.jpg.supplemental-metadat.json": sidecar("Corporate_Identity_PZ.jpg"),

		// A Live Photo motion file with no sidecar of its own, and not an indexed
		// extension either, so it is neither a candidate nor an orphan's cause.
		"IMG_2114.MP4": []byte("not really a video"),
	})

	result := run(t, ws, db, &fakeReader{}, nil)

	if result.Candidates != 3 {
		t.Fatalf("Candidates = %d, want 3", result.Candidates)
	}

	if result.Sidecars != 3 {
		t.Errorf("Sidecars = %d, want all 3 paired", result.Sidecars)
	}

	if result.OrphanSidecars != 0 {
		t.Errorf("OrphanSidecars = %d, want 0", result.OrphanSidecars)
	}

	// The sidecar is stored verbatim against the file's hash.
	stamps, err := db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	md, ok, err := db.MetadataForHash(context.Background(), stamps["IMG_1854.JPG"].Hash)
	if err != nil || !ok {
		t.Fatalf("MetadataForHash() = (%v, %v)", ok, err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(md.SidecarJSON), &parsed); err != nil {
		t.Fatalf("stored sidecar is not valid JSON: %v", err)
	}

	if parsed["title"] != "IMG_1854.JPG" {
		t.Errorf("stored sidecar title = %v, want the original", parsed["title"])
	}
}

// TestSidecarCountSurvivesAnIncrementalScan is the bug this was written to prevent: deriving the
// counts from the processed batches made a second scan report zero sidecars and then call every
// one of them an orphan.
func TestSidecarCountSurvivesAnIncrementalScan(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"IMG_1854.JPG": encodeJPEG(t, gradient(64, 48), 80),
		"IMG_1854.JPG.supplemental-metadata.json": []byte(`{"title":"IMG_1854.JPG"}`),
	})

	first := run(t, ws, db, &fakeReader{}, nil)
	second := run(t, ws, db, &fakeReader{}, nil)

	if second.Skipped != 1 {
		t.Fatalf("second scan Skipped = %d, want 1", second.Skipped)
	}

	if second.Sidecars != first.Sidecars {
		t.Errorf("Sidecars = %d on the second scan, %d on the first; pairing is a property of "+
			"the archive, not of the run", second.Sidecars, first.Sidecars)
	}

	if second.OrphanSidecars != 0 {
		t.Errorf("OrphanSidecars = %d on an incremental scan, want 0", second.OrphanSidecars)
	}
}

func TestOrphanSidecarsAreCounted(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"IMG_1854.JPG": encodeJPEG(t, gradient(64, 48), 80),
		// Belongs to a video, whose extension is not indexed.
		"IMG_2114.MP4.supplemental-metadata.json": []byte(`{"title":"IMG_2114.MP4"}`),
	})

	result := run(t, ws, db, &fakeReader{}, nil)

	if result.OrphanSidecars != 1 {
		t.Errorf("OrphanSidecars = %d, want 1", result.OrphanSidecars)
	}
}

// TestExiftoolDimensionsWin is docs/ARCHITECTURE.md#scan step 4: exiftool is preferred, and it is the only source that
// answers for HEIC, which no Go decoder can measure.
func TestExiftoolDimensionsWin(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(64, 48))})

	reader := &fakeReader{byName: map[string]meta.Tags{
		"a.png": {"File:ImageWidth": float64(1242), "File:ImageHeight": float64(2688)},
	}}

	run(t, ws, db, reader, nil)

	stamps, err := db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	file, ok, err := db.FileByHash(context.Background(), stamps["a.png"].Hash)
	if err != nil || !ok {
		t.Fatalf("FileByHash() = (%v, %v)", ok, err)
	}

	if file.Width != 1242 || file.Height != 2688 {
		t.Errorf("dimensions = %dx%d, want exiftool's 1242x2688 rather than the decoded 64x48",
			file.Width, file.Height)
	}
}

// TestDecodedDimensionsAreTheFallback covers a file exiftool said nothing about.
func TestDecodedDimensionsAreTheFallback(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(64, 48))})

	run(t, ws, db, &fakeReader{}, nil)

	stamps, _ := db.Stamps(context.Background())

	file, ok, err := db.FileByHash(context.Background(), stamps["a.png"].Hash)
	if err != nil || !ok {
		t.Fatalf("FileByHash() = (%v, %v)", ok, err)
	}

	if file.Width != 64 || file.Height != 48 {
		t.Errorf("dimensions = %dx%d, want the decoded 64x48", file.Width, file.Height)
	}
}

// TestAnUndecodableFileIsStillIndexed is the HEIC case, and the reason docs/ARCHITECTURE.md#apply-manifests-and-undo resolves the
// way it does: the file gets a hash, a location, metadata and no perceptual hash.
func TestAnUndecodableFileIsStillIndexed(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		// Not a real HEIC; the point is that no registered decoder handles it.
		"IMG_0007.heic": []byte("ftypheic not actually decodable"),
	})

	reader := &fakeReader{byName: map[string]meta.Tags{
		"IMG_0007.heic": {
			"File:ImageWidth": float64(4032), "File:ImageHeight": float64(3024),
			"IFD0:Make": "Apple", "ExifIFD:FNumber": 1.8,
		},
	}}

	result := run(t, ws, db, reader, nil)

	if result.Scanned != 1 {
		t.Fatalf("Scanned = %d, want the undecodable file indexed anyway", result.Scanned)
	}

	if result.NoPerceptualHash != 1 {
		t.Errorf("NoPerceptualHash = %d, want 1", result.NoPerceptualHash)
	}

	if result.NoDimensions != 0 {
		t.Errorf("NoDimensions = %d, want 0 - exiftool answered for it", result.NoDimensions)
	}

	stamps, _ := db.Stamps(context.Background())

	file, _, err := db.FileByHash(context.Background(), stamps["IMG_0007.heic"].Hash)
	if err != nil {
		t.Fatalf("FileByHash(): %v", err)
	}

	if file.HasDHash {
		t.Error("an undecodable file was given a perceptual hash")
	}

	if file.Width != 4032 {
		t.Errorf("width = %d, want exiftool's answer", file.Width)
	}
}

// TestADecodableFileGetsAPerceptualHash is the positive case, and checks the hash is actually
// stored rather than silently dropped by the nullable column.
func TestADecodableFileGetsAPerceptualHash(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(64, 48))})

	run(t, ws, db, &fakeReader{}, nil)

	stamps, _ := db.Stamps(context.Background())

	file, _, err := db.FileByHash(context.Background(), stamps["a.png"].Hash)
	if err != nil {
		t.Fatalf("FileByHash(): %v", err)
	}

	if !file.HasDHash {
		t.Fatal("a decodable PNG got no perceptual hash")
	}

	// A horizontal gradient is darker on the left of every pair, so no comparison bit is
	// set. Asserting the exact value pins the bit order, which docs/ARCHITECTURE.md#apply-manifests-and-undo will depend on.
	if file.DHash != 0 {
		t.Errorf("dhash = %#x for a left-to-right gradient, want 0", uint64(file.DHash))
	}
}

// TestNearDuplicatesHashClose is the property docs/ARCHITECTURE.md#apply-manifests-and-undo is built on: re-encoding an image changes its
// bytes, and therefore its SHA-256, while leaving the perceptual hash nearly identical.
func TestNearDuplicatesHashClose(t *testing.T) {
	t.Parallel()

	img := structured(96, 72)

	ws, db := archive(t, map[string][]byte{
		"high.jpg": encodeJPEG(t, img, 95),
		"low.jpg":  encodeJPEG(t, img, 40),
	})

	run(t, ws, db, &fakeReader{}, nil)

	stamps, _ := db.Stamps(context.Background())

	if stamps["high.jpg"].Hash == stamps["low.jpg"].Hash {
		t.Fatal("the two encodings have the same SHA-256, so this test proves nothing")
	}

	var hashes []uint64

	for _, name := range []string{"high.jpg", "low.jpg"} {
		file, ok, err := db.FileByHash(context.Background(), stamps[name].Hash)
		if err != nil || !ok {
			t.Fatalf("FileByHash(%s) = (%v, %v)", name, ok, err)
		}

		if !file.HasDHash {
			t.Fatalf("%s got no perceptual hash", name)
		}

		hashes = append(hashes, uint64(file.DHash))
	}

	if d := scan.Distance(hashes[0], hashes[1]); d > 6 {
		t.Errorf("Distance = %d between two encodings of one image, want it small; "+
			"near-duplicate grouping depends on this", d)
	}
}

// structured builds an image with enough local variation that a difference hash is meaningful,
// unlike a flat gradient.
func structured(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			v := uint8((x*7 + y*13) % 256)
			if (x/8+y/8)%2 == 0 {
				v = 255 - v
			}

			img.Set(x, y, colorGray(v))
		}
	}

	return img
}

func TestBatching(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{}
	for i := range 25 {
		files[string(rune('a'+i))+".png"] = encodePNG(t, gradient(16+i, 16))
	}

	ws, db := archive(t, files)

	reader := &fakeReader{}

	run(t, ws, db, reader, func(o *scan.Options) { o.BatchSize = 10 })

	if reader.calls != 3 {
		t.Errorf("metadata reader called %d times for 25 files at a batch size of 10, want 3; "+
			"sizes %v", reader.calls, reader.batchSizes)
	}
}

// TestMetadataFailureStillRecordsHashes is the degraded path: EXIF is valuable but a hash and a
// location are the irreplaceable parts, and a later scan can fill the metadata in.
func TestMetadataFailureStillRecordsHashes(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{"a.png": encodePNG(t, gradient(32, 24))})

	result := run(t, ws, db, &fakeReader{fail: os.ErrNotExist}, nil)

	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want the file recorded despite metadata failing", result.Scanned)
	}

	counts, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Files != 1 {
		t.Errorf("Count().Files = %d, want 1", counts.Files)
	}
}

func TestPruneRemovesVanishedFiles(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"a.png": encodePNG(t, gradient(32, 24)),
		"b.png": encodePNG(t, gradient(48, 32)),
	})

	run(t, ws, db, &fakeReader{}, nil)

	if err := os.Remove(filepath.Join(ws.Root(), "b.png")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	result := run(t, ws, db, &fakeReader{}, func(o *scan.Options) { o.Prune = true })

	if result.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", result.Pruned)
	}

	stamps, err := db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	if _, ok := stamps["b.png"]; ok {
		t.Error("the vanished file's location was kept")
	}
}

// TestPruneIsOffByDefault protects a year of decisions from an unplugged drive.
func TestPruneIsOffByDefault(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"a.png": encodePNG(t, gradient(32, 24)),
		"b.png": encodePNG(t, gradient(48, 32)),
	})

	run(t, ws, db, &fakeReader{}, nil)

	if err := os.Remove(filepath.Join(ws.Root(), "b.png")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	result := run(t, ws, db, &fakeReader{}, nil)

	if result.Pruned != 0 {
		t.Errorf("Pruned = %d without --prune, want 0", result.Pruned)
	}

	stamps, _ := db.Stamps(context.Background())
	if _, ok := stamps["b.png"]; !ok {
		t.Error("a location was removed without pruning being asked for")
	}
}

// TestPruneIsScopedToTheScannedRoot: scanning one subdirectory must not delete everything else.
func TestPruneIsScopedToTheScannedRoot(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"2019/a.png": encodePNG(t, gradient(32, 24)),
		"2020/b.png": encodePNG(t, gradient(48, 32)),
	})

	run(t, ws, db, &fakeReader{}, nil)

	result := run(t, ws, db, &fakeReader{}, func(o *scan.Options) {
		o.Root = "2019"
		o.Prune = true
	})

	if result.Pruned != 0 {
		t.Errorf("Pruned = %d, want 0 - 2020 was not looked at", result.Pruned)
	}

	stamps, _ := db.Stamps(context.Background())
	if _, ok := stamps["2020/b.png"]; !ok {
		t.Error("pruning a subdirectory scan deleted a location outside it")
	}
}

func TestScanRoot(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, map[string][]byte{
		"2019/a.png": encodePNG(t, gradient(32, 24)),
		"2020/b.png": encodePNG(t, gradient(48, 32)),
	})

	result := run(t, ws, db, &fakeReader{}, func(o *scan.Options) { o.Root = "2019" })

	if result.Candidates != 1 {
		t.Errorf("Candidates = %d, want only the files under 2019", result.Candidates)
	}
}

func TestScanProgress(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{}
	for i := range 5 {
		files[string(rune('a'+i))+".png"] = encodePNG(t, gradient(16+i, 16))
	}

	ws, db := archive(t, files)

	var seen []scan.Progress

	run(t, ws, db, &fakeReader{}, func(o *scan.Options) {
		o.BatchSize = 2
		o.Progress = func(p scan.Progress) { seen = append(seen, p) }
	})

	if len(seen) == 0 {
		t.Fatal("Progress was never called")
	}

	last := seen[len(seen)-1]
	if last.Done != 5 || last.Total != 5 {
		t.Errorf("final progress = %+v, want Done and Total both 5", last)
	}
}

func TestRunRequiresItsDependencies(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, nil)

	tests := map[string]scan.Options{
		"no workspace":  {DB: db, Reader: &fakeReader{}, Extensions: defaultExtensions()},
		"no database":   {Workspace: ws, Reader: &fakeReader{}, Extensions: defaultExtensions()},
		"no reader":     {Workspace: ws, DB: db, Extensions: defaultExtensions()},
		"no extensions": {Workspace: ws, DB: db, Reader: &fakeReader{}},
	}

	for name, opts := range tests {
		if _, err := scan.Run(context.Background(), opts); err == nil {
			t.Errorf("%s: Run() error = nil, want an error", name)
		}
	}
}

func TestRunRejectsAMissingRoot(t *testing.T) {
	t.Parallel()

	ws, db := archive(t, nil)

	_, err := scan.Run(context.Background(), scan.Options{
		Workspace: ws, DB: db, Reader: &fakeReader{},
		Extensions: defaultExtensions(), Root: "nope",
	})
	if err == nil {
		t.Error("Run() error = nil, want an error for a root that does not exist")
	}
}

// TestCancellationKeepsCommittedWork is what makes a long scan safe to interrupt: everything
// already written stays written, so a re-run resumes rather than starting over.
func TestCancellationKeepsCommittedWork(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{}
	for i := range 20 {
		files[string(rune('a'+i))+".png"] = encodePNG(t, gradient(16+i, 16))
	}

	ws, db := archive(t, files)

	ctx, cancel := context.WithCancel(context.Background())

	reader := &fakeReader{}

	// Cancel once the first batch has been written.
	var committed int

	_, err := scan.Run(ctx, scan.Options{
		Workspace: ws, DB: db, Reader: reader,
		Extensions: defaultExtensions(), BatchSize: 5, Workers: 2,
		Progress: func(p scan.Progress) {
			committed = p.Done

			cancel()
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil after cancellation, want ctx.Err()")
	}

	counts, countErr := db.Count(context.Background())
	if countErr != nil {
		t.Fatalf("Count(): %v", countErr)
	}

	if counts.Files != committed {
		t.Errorf("Count().Files = %d, want the %d already committed to survive cancellation",
			counts.Files, committed)
	}

	if counts.Files == 0 {
		t.Error("cancellation discarded every committed batch")
	}
}
