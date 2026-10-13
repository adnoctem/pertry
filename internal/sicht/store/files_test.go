package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/store"
)

func scanned(hash, path string, size int64) store.Scanned {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	return store.Scanned{
		File: store.File{
			Hash: hash, Size: size, Ext: "jpg",
			Width: 4032, Height: 3024,
			DHash: 0x0f0f0f0f0f0f0f0f, HasDHash: true,
			FirstSeen: now,
		},
		Location: store.Location{Path: path, Hash: hash, MTime: now, SeenAt: now},
		Metadata: store.Metadata{
			Hash: hash, ExifJSON: `{"IFD0:Make":"Apple"}`, ExtractedWith: "exiftool 13.55",
		},
	}
}

func TestSaveScanned(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	if err := db.SaveScanned(ctx, []store.Scanned{scanned("aaa", "photos/a.jpg", 1024)}); err != nil {
		t.Fatalf("SaveScanned() error = %v, want nil", err)
	}

	file, ok, err := db.FileByHash(ctx, "aaa")
	if err != nil || !ok {
		t.Fatalf("FileByHash() = (%v, %v, %v)", file, ok, err)
	}

	if file.Size != 1024 || file.Width != 4032 || file.Height != 3024 {
		t.Errorf("file = %+v, want the saved values", file)
	}

	if !file.HasDHash || file.DHash != 0x0f0f0f0f0f0f0f0f {
		t.Errorf("dhash = (%x, %v), want the saved value", file.DHash, file.HasDHash)
	}

	md, ok, err := db.MetadataForHash(ctx, "aaa")
	if err != nil || !ok {
		t.Fatalf("MetadataForHash() = (%v, %v, %v)", md, ok, err)
	}

	if md.ExifJSON != `{"IFD0:Make":"Apple"}` {
		t.Errorf("ExifJSON = %q, want it stored verbatim", md.ExifJSON)
	}

	// Absent rather than empty-string, so a caller can tell "no sidecar" from "empty sidecar".
	if md.SidecarJSON != "" {
		t.Errorf("SidecarJSON = %q, want empty", md.SidecarJSON)
	}
}

func TestSaveScannedIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	batch := []store.Scanned{scanned("aaa", "photos/a.jpg", 1024)}

	for i := range 3 {
		if err := db.SaveScanned(ctx, batch); err != nil {
			t.Fatalf("SaveScanned() #%d error = %v", i+1, err)
		}
	}

	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}

	if counts.Files != 1 || counts.Locations != 1 {
		t.Errorf("Count() = %+v, want one file and one location", counts)
	}
}

// TestFirstSeenSurvivesARescan protects the only chronological fact sicht itself contributes.
// Moving it forward on every scan would make it a "last seen" with a misleading name.
func TestFirstSeenSurvivesARescan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	first := scanned("aaa", "photos/a.jpg", 1024)
	original := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	first.File.FirstSeen = original

	if err := db.SaveScanned(ctx, []store.Scanned{first}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	later := scanned("aaa", "photos/a.jpg", 1024)
	later.File.FirstSeen = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

	if err := db.SaveScanned(ctx, []store.Scanned{later}); err != nil {
		t.Fatalf("SaveScanned() #2 error = %v", err)
	}

	file, _, err := db.FileByHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("FileByHash() error = %v", err)
	}

	if !file.FirstSeen.Equal(original) {
		t.Errorf("FirstSeen = %v, want it unchanged at %v", file.FirstSeen, original)
	}
}

// TestRescanDoesNotForgetDimensions is why the upsert coalesces: a later scan that could not
// measure the file must not blank out what an earlier one knew.
func TestRescanDoesNotForgetDimensions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	if err := db.SaveScanned(ctx, []store.Scanned{scanned("aaa", "photos/a.jpg", 1024)}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	blind := scanned("aaa", "photos/a.jpg", 1024)
	blind.File.Width, blind.File.Height = 0, 0
	blind.File.HasDHash = false

	if err := db.SaveScanned(ctx, []store.Scanned{blind}); err != nil {
		t.Fatalf("SaveScanned() #2 error = %v", err)
	}

	file, _, err := db.FileByHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("FileByHash() error = %v", err)
	}

	if file.Width != 4032 || file.Height != 3024 {
		t.Errorf("dimensions = %dx%d, want the earlier scan's values retained", file.Width, file.Height)
	}

	if !file.HasDHash {
		t.Error("dhash was cleared by a scan that could not compute one")
	}
}

func TestSaveScannedRejectsAHashlessFile(t *testing.T) {
	t.Parallel()

	db := open(t)

	err := db.SaveScanned(context.Background(), []store.Scanned{{
		File: store.File{Size: 10, Ext: "jpg"},
	}})
	if err == nil {
		t.Error("SaveScanned() error = nil, want a refusal to save a file with no hash")
	}
}

// TestSaveScannedIsAtomic is what makes a failed batch a clean retry rather than a partial state.
func TestSaveScannedIsAtomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	batch := []store.Scanned{
		scanned("aaa", "photos/a.jpg", 1024),
		{File: store.File{Ext: "jpg"}}, // no hash: fails mid-transaction
	}

	if err := db.SaveScanned(ctx, batch); err == nil {
		t.Fatal("SaveScanned() error = nil, want the bad entry to fail the batch")
	}

	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}

	if counts.Files != 0 {
		t.Errorf("Count().Files = %d, want 0: the whole batch must roll back", counts.Files)
	}
}

func TestSaveScannedEmpty(t *testing.T) {
	t.Parallel()

	if err := open(t).SaveScanned(context.Background(), nil); err != nil {
		t.Errorf("SaveScanned(nil) error = %v, want nil", err)
	}
}

// TestStamps is the incremental check's input: path, mtime and size, per docs/ARCHITECTURE.md#scan step 1.
func TestStamps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	mtime := time.Date(2019, 8, 25, 2, 45, 28, 0, time.UTC)

	entry := scanned("aaa", "Fotos von 2019/IMG_1234.JPG", 2048)
	entry.Location.MTime = mtime

	if err := db.SaveScanned(ctx, []store.Scanned{entry}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	stamps, err := db.Stamps(ctx)
	if err != nil {
		t.Fatalf("Stamps() error = %v", err)
	}

	got, ok := stamps["Fotos von 2019/IMG_1234.JPG"]
	if !ok {
		t.Fatalf("Stamps() = %v, want the saved path", stamps)
	}

	if !got.MTime.Equal(mtime) {
		t.Errorf("MTime = %v, want %v", got.MTime, mtime)
	}

	if got.Size != 2048 {
		t.Errorf("Size = %d, want 2048", got.Size)
	}

	if got.Hash != "aaa" {
		t.Errorf("Hash = %q, want %q", got.Hash, "aaa")
	}
}

// TestStampsPreservesSubSecondTime matters because the incremental check compares mtimes for
// equality. A timestamp that loses precision on the way to the database would make every file
// look changed on every scan, silently turning an incremental scan into a full one.
func TestStampsPreservesSubSecondTime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	mtime := time.Date(2019, 8, 25, 2, 45, 28, 123456789, time.UTC)

	entry := scanned("aaa", "a.jpg", 1)
	entry.Location.MTime = mtime

	if err := db.SaveScanned(ctx, []store.Scanned{entry}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	stamps, err := db.Stamps(ctx)
	if err != nil {
		t.Fatalf("Stamps() error = %v", err)
	}

	if got := stamps["a.jpg"].MTime; !got.Equal(mtime) {
		t.Errorf("MTime round-tripped as %v, want %v - every file would look changed", got, mtime)
	}
}

// TestOneHashSeveralPaths is the premise of docs/ARCHITECTURE.md#apply-manifests-and-undo: the same content filed twice is one file in two
// places.
func TestOneHashSeveralPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	if err := db.SaveScanned(ctx, []store.Scanned{
		scanned("aaa", "photos/a.jpg", 1024),
		scanned("aaa", "backup/a copy.jpg", 1024),
	}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	locs, err := db.LocationsForHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("LocationsForHash() error = %v", err)
	}

	if len(locs) != 2 {
		t.Fatalf("LocationsForHash() returned %d, want 2", len(locs))
	}

	// Sorted, because docs/ARCHITECTURE.md#apply-manifests-and-undo makes apply move "the first location" and that has to mean the
	// same thing on every run and every platform.
	if locs[0].Path != "backup/a copy.jpg" {
		t.Errorf("locations are not sorted by path: %v", locs)
	}
}

// TestAPathThatChangesContent covers a file being edited in place: the path keeps one row and
// points at the new hash.
func TestAPathThatChangesContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	if err := db.SaveScanned(ctx, []store.Scanned{scanned("aaa", "photos/a.jpg", 1024)}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	if err := db.SaveScanned(ctx, []store.Scanned{scanned("bbb", "photos/a.jpg", 2048)}); err != nil {
		t.Fatalf("SaveScanned() #2 error = %v", err)
	}

	stamps, err := db.Stamps(ctx)
	if err != nil {
		t.Fatalf("Stamps() error = %v", err)
	}

	if len(stamps) != 1 {
		t.Errorf("Stamps() = %v, want one entry for one path", stamps)
	}

	if stamps["photos/a.jpg"].Hash != "bbb" {
		t.Errorf("hash = %q, want the new content", stamps["photos/a.jpg"].Hash)
	}
}

func TestDeleteLocations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	if err := db.SaveScanned(ctx, []store.Scanned{
		scanned("aaa", "photos/a.jpg", 1024),
		scanned("aaa", "backup/a.jpg", 1024),
		scanned("bbb", "photos/b.jpg", 2048),
	}); err != nil {
		t.Fatalf("SaveScanned() error = %v", err)
	}

	removed, err := db.DeleteLocations(ctx, []string{"photos/b.jpg"})
	if err != nil {
		t.Fatalf("DeleteLocations() error = %v", err)
	}

	if removed != 1 {
		t.Errorf("DeleteLocations() = %d, want 1", removed)
	}

	// bbb had only that one location, so the file goes too - otherwise a vanished file keeps
	// its decisions alive forever.
	if _, ok, _ := db.FileByHash(ctx, "bbb"); ok {
		t.Error("the file with no remaining location was kept")
	}

	// aaa still has one location, so it stays.
	if _, ok, _ := db.FileByHash(ctx, "aaa"); !ok {
		t.Error("a file that still has a location was deleted")
	}
}

func TestDeleteLocationsEmpty(t *testing.T) {
	t.Parallel()

	got, err := open(t).DeleteLocations(context.Background(), nil)
	if err != nil || got != 0 {
		t.Errorf("DeleteLocations(nil) = (%d, %v), want (0, nil)", got, err)
	}
}

func TestFileByHashMissing(t *testing.T) {
	t.Parallel()

	_, ok, err := open(t).FileByHash(context.Background(), "nope")
	if err != nil {
		t.Errorf("FileByHash() error = %v, want nil for a missing hash", err)
	}

	if ok {
		t.Error("FileByHash() ok = true for a missing hash")
	}
}

func TestMetadataForHashMissing(t *testing.T) {
	t.Parallel()

	_, ok, err := open(t).MetadataForHash(context.Background(), "nope")
	if err != nil {
		t.Errorf("MetadataForHash() error = %v, want nil", err)
	}

	if ok {
		t.Error("MetadataForHash() ok = true for a missing hash")
	}
}

// TestEachFileForRulesAllowsQueryingInTheCallback is a deadlock regression test.
//
// The connection pool is limited to one connection so that SQLite never sees concurrent writers.
// A query issued while rows are still open therefore waits forever for the connection the open
// query is holding. Every caller of EachFileForRules queries inside the callback - apply reads
// each file's decisions, export reads its sources, rules writes its results - so a single
// streaming cursor deadlocked all three.
//
// The row count matters: it must exceed one page, so that the second page is fetched *after* the
// callback has already issued queries. With fewer rows than a page the bug is invisible, which is
// exactly how it survived a run over 1337 real files.
func TestEachFileForRulesAllowsQueryingInTheCallback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	// Comfortably more than one page.
	const files = 1200

	batch := make([]store.Scanned, 0, files)

	for i := range files {
		hash := fmt.Sprintf("%064x", i)
		batch = append(batch, scanned(hash, fmt.Sprintf("photos/%04d.jpg", i), int64(i+1)))
	}

	if err := db.SaveScanned(ctx, batch); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	results := make([]store.Result, 0, files)

	for i := range files {
		results = append(results, store.Result{
			Hash: fmt.Sprintf("%064x", i), Facet: "kind", Value: "photo",
			Source: store.RuleSource(0), CreatedAt: time.Now(),
		})
	}

	if err := db.SaveResults(ctx, results); err != nil {
		t.Fatalf("SaveResults(): %v", err)
	}

	// A context with a deadline, so a regression fails the test rather than hanging it.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	seen := 0

	err := db.EachFileForRules(ctx, func(in store.RuleInput) error {
		seen++

		// A read inside the callback.
		if _, err := db.DecisionMap(ctx, in.Hash); err != nil {
			return err
		}

		if _, err := db.LocationsForHash(ctx, in.Hash); err != nil {
			return err
		}

		// And a write, which is what rules does when it flushes mid-iteration.
		return db.SaveResults(ctx, []store.Result{{
			Hash: in.Hash, Facet: "checked", Value: "yes",
			Source: store.RuleSource(1), CreatedAt: time.Now(),
		}})
	})
	if err != nil {
		t.Fatalf("EachFileForRules() error = %v, want nil", err)
	}

	if seen != files {
		t.Errorf("visited %d files, want %d", seen, files)
	}
}

// TestEachFileForRulesVisitsEveryFileOnce guards the paging itself: a boundary that skipped or
// repeated a row would be a quiet correctness bug, since every stage after scan iterates this way.
func TestEachFileForRulesVisitsEveryFileOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	// Deliberately not a multiple of the page size, so the last page is partial.
	const files = 1103

	batch := make([]store.Scanned, 0, files)
	for i := range files {
		hash := fmt.Sprintf("%064x", i)
		batch = append(batch, scanned(hash, fmt.Sprintf("p/%04d.jpg", i), 1))
	}

	if err := db.SaveScanned(ctx, batch); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	seen := map[string]int{}

	var order []string

	if err := db.EachFileForRules(ctx, func(in store.RuleInput) error {
		seen[in.Hash]++

		order = append(order, in.Hash)

		return nil
	}); err != nil {
		t.Fatalf("EachFileForRules(): %v", err)
	}

	if len(seen) != files {
		t.Errorf("saw %d distinct files, want %d", len(seen), files)
	}

	for hash, n := range seen {
		if n != 1 {
			t.Errorf("file %s visited %d times, want once", hash, n)
		}
	}

	// Hash order, which is what makes --limit take a stable prefix.
	if !slices.IsSorted(order) {
		t.Error("files were not visited in hash order")
	}
}

// TestEachFileForRulesStopsOnCallbackError keeps a cancellation or a failure from being swallowed.
func TestEachFileForRulesStopsOnCallbackError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	batch := make([]store.Scanned, 0, 10)
	for i := range 10 {
		batch = append(batch, scanned(fmt.Sprintf("%064x", i), fmt.Sprintf("p/%d.jpg", i), 1))
	}

	if err := db.SaveScanned(ctx, batch); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	sentinel := errors.New("stop here")

	seen := 0

	err := db.EachFileForRules(ctx, func(store.RuleInput) error {
		seen++

		if seen == 3 {
			return sentinel
		}

		return nil
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the callback's own error", err)
	}

	if seen != 3 {
		t.Errorf("visited %d files, want iteration to stop at 3", seen)
	}
}
