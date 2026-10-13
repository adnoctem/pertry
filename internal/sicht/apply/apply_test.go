package apply_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/apply"
	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/manifest"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// fixture is an archive on disk with a matching database.
type fixture struct {
	ws  *core.Workspace
	db  *store.DB
	cfg *config.Config
}

// newFixture builds a workspace containing the given files and indexes the ones whose extension
// sicht handles, so the database matches the disk.
func newFixture(t *testing.T, files map[string]string, decisions map[string]map[string]string) *fixture {
	t.Helper()

	root := t.TempDir()

	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
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
			t.Errorf("Close(): %v", err)
		}
	})

	indexed := map[string]bool{
		"jpg": true, "jpeg": true, "png": true, "heic": true, "webp": true, "gif": true,
	}

	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	var (
		scanned []store.Scanned
		results []store.Result
	)

	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}

	sort.Strings(paths)

	for _, rel := range paths {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(rel), "."))
		if !indexed[ext] {
			continue
		}

		sum := sha256.Sum256([]byte(files[rel]))
		hash := hex.EncodeToString(sum[:])

		scanned = append(scanned, store.Scanned{
			File: store.File{
				Hash: hash, Size: int64(len(files[rel])), Ext: ext,
				Width: 1242, Height: 2688, FirstSeen: now,
			},
			Location: store.Location{Path: rel, Hash: hash, MTime: now, SeenAt: now},
			Metadata: store.Metadata{Hash: hash, ExifJSON: `{}`},
		})

		for facet, value := range decisions[rel] {
			results = append(results, store.Result{
				Hash: hash, Facet: facet, Value: value,
				Source: store.RuleSource(0), CreatedAt: now,
			})
		}
	}

	if err := db.SaveScanned(context.Background(), scanned); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	if err := db.SaveResults(context.Background(), results); err != nil {
		t.Fatalf("SaveResults(): %v", err)
	}

	cfg := config.Default()

	return &fixture{ws: ws, db: db, cfg: &cfg}
}

// plan replaces the fixture's plans with one.
func (f *fixture) plan(p config.Plan) {
	f.cfg.Plans = map[string]config.Plan{"default": p}
}

func (f *fixture) run(t *testing.T, execute bool) apply.Result {
	t.Helper()

	result, err := apply.Run(context.Background(), apply.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg,
		PlanName: "default", Execute: execute, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("apply.Run(execute=%v) error = %v, want nil", execute, err)
	}

	return result
}

// tree returns every file in the archive, workspace-relative, excluding .sicht/.
func (f *fixture) tree(t *testing.T) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(f.ws.Root(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := f.ws.Rel(path)
		if err != nil || core.IsInternal(rel) {
			//nolint:nilerr // A path the workspace cannot place is not part of the
			// archive, so it is skipped rather than failing the walk.
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		out[rel] = string(raw)

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	return out
}

func simplePlan() config.Plan {
	return config.Plan{
		Where:             `decided("kind")`,
		Target:            `sorted/{{ .kind }}`,
		Mode:              "move",
		OnConflict:        "suffix",
		IncludeCompanions: true,
	}
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"2019/IMG_0001.png": "shot"},
		map[string]map[string]string{"2019/IMG_0001.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	before := f.tree(t)

	result := f.run(t, false)

	if result.Executed {
		t.Error("Executed = true for a dry run")
	}

	if result.Planned != 1 {
		t.Errorf("Planned = %d, want 1", result.Planned)
	}

	after := f.tree(t)
	if len(after) != len(before) {
		t.Errorf("the archive changed during a dry run: %v -> %v", before, after)
	}

	// A dry run still writes its manifest, which is the thing you read before committing.
	m, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if m.Status != manifest.StatusPlanned {
		t.Errorf("manifest status = %q, want %q", m.Status, manifest.StatusPlanned)
	}

	if len(m.Operations) != 1 || m.Operations[0].To != "sorted/screenshot/IMG_0001.png" {
		t.Errorf("operations = %+v, want one move to sorted/screenshot/", m.Operations)
	}
}

func TestApplyMovesFiles(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"2019/IMG_0001.png": "shot",
			"2019/IMG_0002.jpg": "photo",
		},
		map[string]map[string]string{
			"2019/IMG_0001.png": {"kind": "screenshot"},
			"2019/IMG_0002.jpg": {"kind": "photo"},
		},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	if result.Done != 2 {
		t.Errorf("Done = %d, want 2", result.Done)
	}

	tree := f.tree(t)

	for path, want := range map[string]string{
		"sorted/screenshot/IMG_0001.png": "shot",
		"sorted/photo/IMG_0002.jpg":      "photo",
	} {
		if tree[path] != want {
			t.Errorf("%s = %q, want %q", path, tree[path], want)
		}
	}

	if len(tree) != 2 {
		t.Errorf("the archive holds %d files, want 2: %v", len(tree), tree)
	}

	// The database follows the files, so a later scan does not see them as new.
	stamps, err := f.db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	if _, ok := stamps["sorted/screenshot/IMG_0001.png"]; !ok {
		t.Errorf("the location was not updated: %v", stamps)
	}
}

// TestApplyLeavesUndecidedFilesAlone is the plan selector doing its job: a file nothing is known
// about must not be filed anywhere.
func TestApplyLeavesUndecidedFilesAlone(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"2019/decided.png":   "a",
			"2019/undecided.png": "b",
		},
		map[string]map[string]string{"2019/decided.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	f.run(t, true)

	tree := f.tree(t)

	if tree["2019/undecided.png"] != "b" {
		t.Error("an undecided file was moved")
	}

	if tree["sorted/screenshot/decided.png"] != "a" {
		t.Error("the decided file was not moved")
	}
}

// TestCompanionsFollowTheirOwner is docs/ARCHITECTURE.md#apply-manifests-and-undo. Without it, applying a plan would separate 201 Live
// Photos in the reference archive from their motion halves.
func TestCompanionsFollowTheirOwner(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"2021/IMG_2114.heic": "still",
			// A Live Photo motion file: same stem, an extension sicht does not index.
			"2021/IMG_2114.MP4": "motion",
			// A Takeout sidecar, named by the measured rule.
			"2021/IMG_2114.heic.supplemental-metadata.json": `{"title":"IMG_2114.heic"}`,
		},
		map[string]map[string]string{"2021/IMG_2114.heic": {"kind": "photo"}},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	if result.Companions != 2 {
		t.Errorf("Companions = %d, want 2 (the motion file and the sidecar)", result.Companions)
	}

	tree := f.tree(t)

	for _, want := range []string{
		"sorted/photo/IMG_2114.heic",
		"sorted/photo/IMG_2114.MP4",
		"sorted/photo/IMG_2114.heic.supplemental-metadata.json",
	} {
		if _, ok := tree[want]; !ok {
			t.Errorf("%s is missing; the archive holds %v", want, keysOf(tree))
		}
	}

	if len(tree) != 3 {
		t.Errorf("the archive holds %d files, want 3: %v", len(tree), keysOf(tree))
	}
}

// TestCompanionsStayWhenTheOwnerDoesNotMove: separating a sidecar from its file is the thing
// companion handling exists to prevent, so a companion must not move on its own.
func TestCompanionsStayWhenTheOwnerDoesNotMove(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"2021/IMG_2114.heic": "still",
			"2021/IMG_2114.MP4":  "motion",
		},
		// No decision, so the selector does not choose it.
		nil,
	)
	f.plan(simplePlan())

	f.run(t, true)

	tree := f.tree(t)

	if tree["2021/IMG_2114.MP4"] != "motion" {
		t.Error("a companion moved although its owner stayed put")
	}
}

// TestOneCompanionHasOneOwner is docs/ARCHITECTURE.md#apply-manifests-and-undo's tie-break. Four stems in the reference archive are
// indexed twice over, and without this both would try to move the same motion file.
func TestOneCompanionHasOneOwner(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"2018/IMG_0007.heic": "heic original",
			"2018/IMG_0007.jpeg": "jpeg transcode",
			"2018/IMG_0007.MP4":  "motion",
		},
		map[string]map[string]string{
			"2018/IMG_0007.heic": {"kind": "photo"},
			"2018/IMG_0007.jpeg": {"kind": "photo"},
		},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	// Exactly one companion operation, not two.
	if result.Companions != 1 {
		t.Errorf("Companions = %d, want 1: a companion belongs to one file", result.Companions)
	}

	m, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	var owners []string

	for _, op := range m.Operations {
		if op.Op == manifest.OpCompanion {
			owners = append(owners, op.Owner)
		}
	}

	if len(owners) != 1 {
		t.Fatalf("companion operations = %v, want exactly one", owners)
	}

	// HEIC wins by the documented preference order, because where Google produced both it is
	// the original and the JPEG is the transcode.
	if owners[0] != "2018/IMG_0007.heic" {
		t.Errorf("the companion went to %q, want the HEIC by extension preference", owners[0])
	}
}

// TestCompanionClaimingIsDeterministic: the choice must not depend on directory iteration order,
// or a re-run would produce a different manifest.
func TestCompanionClaimingIsDeterministic(t *testing.T) {
	t.Parallel()

	for range 10 {
		f := newFixture(t,
			map[string]string{
				"x/IMG_1.heic": "a",
				"x/IMG_1.jpeg": "b",
				"x/IMG_1.MP4":  "c",
			},
			map[string]map[string]string{
				"x/IMG_1.heic": {"kind": "photo"},
				"x/IMG_1.jpeg": {"kind": "photo"},
			},
		)
		f.plan(simplePlan())

		result := f.run(t, false)

		m, err := manifest.Load(result.ManifestPath)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}

		for _, op := range m.Operations {
			if op.Op == manifest.OpCompanion && op.Owner != "x/IMG_1.heic" {
				t.Fatalf("companion owner = %q, want it stable at x/IMG_1.heic", op.Owner)
			}
		}
	}
}

// TestCompanionsAreNotMovedWhenTheKnobIsOff covers the deprecated-alias path too, since the key
// was renamed and an old sicht.yaml must keep working.
func TestCompanionsHonourBothKeys(t *testing.T) {
	t.Parallel()

	tests := map[string]config.Plan{
		"new key": {
			Where: `decided("kind")`, Target: `sorted/{{ .kind }}`, Mode: "move",
			OnConflict: "suffix", IncludeCompanions: true,
		},
		"former key": {
			Where: `decided("kind")`, Target: `sorted/{{ .kind }}`, Mode: "move",
			OnConflict: "suffix", IncludeSidecars: true,
		},
	}

	for name, plan := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t,
				map[string]string{
					"a/IMG_1.jpg":                            "img",
					"a/IMG_1.jpg.supplemental-metadata.json": `{"title":"IMG_1.jpg"}`,
				},
				map[string]map[string]string{"a/IMG_1.jpg": {"kind": "photo"}},
			)
			f.plan(plan)

			if got := f.run(t, true).Companions; got != 1 {
				t.Errorf("Companions = %d, want 1", got)
			}
		})
	}

	t.Run("off", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t,
			map[string]string{
				"a/IMG_1.jpg":                            "img",
				"a/IMG_1.jpg.supplemental-metadata.json": `{"title":"IMG_1.jpg"}`,
			},
			map[string]map[string]string{"a/IMG_1.jpg": {"kind": "photo"}},
		)
		f.plan(config.Plan{
			Where: `decided("kind")`, Target: `sorted/{{ .kind }}`, Mode: "move",
			OnConflict: "suffix",
		})

		result := f.run(t, true)

		if result.Companions != 0 {
			t.Errorf("Companions = %d, want 0", result.Companions)
		}

		if f.tree(t)["a/IMG_1.jpg.supplemental-metadata.json"] == "" {
			t.Error("the sidecar moved although the plan did not ask for it")
		}
	})
}

// TestDuplicatesAreRecordedNotMoved is docs/ARCHITECTURE.md#apply-manifests-and-undo: the same content in two places is moved once, from
// its first path, and the other locations are recorded.
func TestDuplicatesAreRecordedNotMoved(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"backup/a.png": "identical",
			"photos/a.png": "identical",
		},
		map[string]map[string]string{"backup/a.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	if result.DuplicatesLeft != 1 {
		t.Errorf("DuplicatesLeft = %d, want 1", result.DuplicatesLeft)
	}

	tree := f.tree(t)

	// The first path by sort order is moved; the other stays.
	if tree["sorted/screenshot/a.png"] != "identical" {
		t.Errorf("the first location was not moved: %v", keysOf(tree))
	}

	if tree["photos/a.png"] != "identical" {
		t.Errorf("the second location was moved too: %v", keysOf(tree))
	}

	m, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	var found bool

	for _, op := range m.Operations {
		if op.Op == manifest.OpDuplicateLeft && op.From == "photos/a.png" {
			found = true
		}
	}

	if !found {
		t.Errorf("the untouched duplicate is not recorded in the manifest: %+v", m.Operations)
	}
}

func TestOrphanCompanionsAreReportedNotMoved(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"a/IMG_1.jpg": "img",
			// A sidecar for a video, whose extension sicht does not index.
			"a/IMG_9.MP4.supplemental-metadata.json": `{"title":"IMG_9.MP4"}`,
		},
		map[string]map[string]string{"a/IMG_1.jpg": {"kind": "photo"}},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	if result.OrphanCompanions != 1 {
		t.Errorf("OrphanCompanions = %d, want 1", result.OrphanCompanions)
	}

	if f.tree(t)["a/IMG_9.MP4.supplemental-metadata.json"] == "" {
		t.Error("an orphan companion was moved; guessing at an owner is how files get lost")
	}
}

func TestApplyConflictSuffix(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"a/IMG_1.png":                 "new",
			"sorted/screenshot/IMG_1.png": "existing",
		},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	result := f.run(t, true)

	tree := f.tree(t)

	if tree["sorted/screenshot/IMG_1.png"] != "existing" {
		t.Error("the existing file was overwritten")
	}

	if tree["sorted/screenshot/IMG_1 (2).png"] != "new" {
		t.Errorf("the moved file is not at the suffixed path: %v", keysOf(tree))
	}

	// The manifest must record where the file really went, or an undo would look in the
	// wrong place.
	m, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	for _, op := range m.Operations {
		if op.Op == manifest.OpMove && op.Status == manifest.OpDone {
			if op.To != "sorted/screenshot/IMG_1 (2).png" {
				t.Errorf("manifest records To = %q, want the path actually used", op.To)
			}
		}
	}
}

// TestCopyModeLeavesTheSource covers the other mode, and the manifest records a copy rather than
// a move so that undo correctly refuses to reverse it.
func TestCopyMode(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "contents"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(config.Plan{
		Where: `decided("kind")`, Target: `sorted/{{ .kind }}`, Mode: "copy",
		OnConflict: "suffix",
	})

	result := f.run(t, true)

	tree := f.tree(t)

	if tree["a/IMG_1.png"] != "contents" {
		t.Error("copy mode removed the source")
	}

	if tree["sorted/screenshot/IMG_1.png"] != "contents" {
		t.Error("copy mode did not write the target")
	}

	m, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if m.Operations[0].Op != manifest.OpCopy {
		t.Errorf("operation = %q, want %q", m.Operations[0].Op, manifest.OpCopy)
	}

	if m.Operations[0].Reversible() {
		t.Error("a copy reports itself as reversible; undoing it would mean deleting a file")
	}

	// A copy moves bytes, so it must be verified.
	if !m.Operations[0].Verified {
		t.Error("a copy was not verified")
	}
}

// TestUndoRestoresByteForByte is M1's acceptance criterion.
func TestUndoRestoresByteForByte(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"2019/IMG_0001.png":                            "shot one",
		"2019/IMG_0002.jpg":                            "photo two",
		"2019/IMG_0002.jpg.supplemental-metadata.json": `{"title":"IMG_0002.jpg"}`,
		"2021/IMG_2114.heic":                           "still",
		"2021/IMG_2114.MP4":                            "motion",
		"2021/untouched.png":                           "no decision",
	}

	f := newFixture(t, files, map[string]map[string]string{
		"2019/IMG_0001.png":  {"kind": "screenshot"},
		"2019/IMG_0002.jpg":  {"kind": "photo"},
		"2021/IMG_2114.heic": {"kind": "photo"},
	})
	f.plan(simplePlan())

	before := f.tree(t)

	result := f.run(t, true)

	if result.Done == 0 {
		t.Fatal("nothing was moved, so the undo proves nothing")
	}

	moved := f.tree(t)
	if sameTree(before, moved) {
		t.Fatal("apply changed nothing")
	}

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: result.BatchID,
		Execute: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo() error = %v, want nil", err)
	}

	if undone.Failed != 0 {
		t.Errorf("Failed = %d, want 0: %v", undone.Failed, undone.Errors)
	}

	after := f.tree(t)

	if !sameTree(before, after) {
		t.Errorf("the archive was not restored\nbefore: %v\nafter:  %v",
			keysOf(before), keysOf(after))
	}

	// Content, not just paths.
	for path, body := range before {
		if after[path] != body {
			t.Errorf("%s = %q, want %q", path, after[path], body)
		}
	}

	// The database follows too, so a later scan finds nothing changed.
	stamps, err := f.db.Stamps(context.Background())
	if err != nil {
		t.Fatalf("Stamps(): %v", err)
	}

	for path := range before {
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".json" || ext == ".mp4" {
			continue
		}

		if _, ok := stamps[path]; !ok {
			t.Errorf("location %s was not restored in the database: %v", path, keysOf2(stamps))
		}
	}
}

// TestUndoMarksTheOriginal so the history reads in both directions.
func TestUndoMarksTheOriginal(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	original, err := manifest.Load(applied.ManifestPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if original.Status != manifest.StatusUndone {
		t.Errorf("original status = %q, want %q", original.Status, manifest.StatusUndone)
	}

	if original.UndoneBy != undone.UndoBatchID {
		t.Errorf("UndoneBy = %q, want %q", original.UndoneBy, undone.UndoBatchID)
	}

	undoManifest, err := manifest.Load(undone.ManifestPath)
	if err != nil {
		t.Fatalf("Load(undo): %v", err)
	}

	if undoManifest.UndoOf != applied.BatchID {
		t.Errorf("UndoOf = %q, want %q", undoManifest.UndoOf, applied.BatchID)
	}
}

// TestUndoRefusesToOverwriteAChangedFile is docs/ARCHITECTURE.md#apply-manifests-and-undo's safety requirement. Moving a file back over a
// newer version of itself would destroy work.
func TestUndoRefusesToOverwriteAChangedFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "original"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	// Edit the file where it was filed.
	filed := filepath.Join(f.ws.Root(), "sorted", "screenshot", "IMG_1.png")
	if err := os.WriteFile(filed, []byte("edited since filing"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if undone.Changed != 1 {
		t.Errorf("Changed = %d, want 1", undone.Changed)
	}

	if undone.Done != 0 {
		t.Errorf("Done = %d, want 0 - the edited file must not be moved back", undone.Done)
	}

	// The edit survives where it is.
	raw, err := os.ReadFile(filed)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(raw) != "edited since filing" {
		t.Error("the edited file was moved or overwritten")
	}
}

func TestUndoForceMovesAChangedFileAnyway(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "original"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	filed := filepath.Join(f.ws.Root(), "sorted", "screenshot", "IMG_1.png")
	if err := os.WriteFile(filed, []byte("edited"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID,
		Execute: true, Force: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if undone.Done != 1 {
		t.Errorf("Done = %d, want 1 with --force", undone.Done)
	}

	if f.tree(t)["a/IMG_1.png"] != "edited" {
		t.Error("the file was not moved back")
	}
}

func TestUndoReportsAMissingFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	if err := os.Remove(filepath.Join(f.ws.Root(), "sorted", "screenshot", "IMG_1.png")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if undone.Missing != 1 || undone.Skipped != 1 {
		t.Errorf("Missing = %d, Skipped = %d, want 1 and 1", undone.Missing, undone.Skipped)
	}
}

func TestUndoDryRun(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	moved := f.tree(t)

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if undone.Executed {
		t.Error("Executed = true for a dry run")
	}

	if undone.Reversible != 1 {
		t.Errorf("Reversible = %d, want 1", undone.Reversible)
	}

	if !sameTree(moved, f.tree(t)) {
		t.Error("a dry-run undo moved files")
	}
}

func TestUndoRefusesADryRunBatch(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	planned := f.run(t, false)

	_, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: planned.BatchID, Execute: true, ToolVersion: "test",
	})
	if err == nil {
		t.Error("Undo() error = nil for a dry-run batch, want a refusal")
	}
}

func TestUndoRefusesTwice(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	opts := apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	}

	if _, err := apply.Undo(context.Background(), opts); err != nil {
		t.Fatalf("first Undo(): %v", err)
	}

	if _, err := apply.Undo(context.Background(), opts); err == nil {
		t.Error("second Undo() error = nil, want a refusal")
	}
}

func TestUndoAcceptsABatchIDPrefix(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a/IMG_1.png": "x"},
		map[string]map[string]string{"a/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	// Nobody types a 32-character batch id correctly.
	prefix := applied.BatchID[:12]

	if _, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: prefix, ToolVersion: "test",
	}); err != nil {
		t.Errorf("Undo() with a prefix error = %v, want nil", err)
	}
}

func TestUndoRejectsAnUnknownBatch(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a/IMG_1.png": "x"}, nil)

	_, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: "nonsense", ToolVersion: "test",
	})
	if !errors.Is(err, manifest.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestApplyRejectsAnUnknownPlan(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a/IMG_1.png": "x"}, nil)

	_, err := apply.Run(context.Background(), apply.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, PlanName: "nope", ToolVersion: "test",
	})
	if err == nil {
		t.Error("Run() error = nil for an unknown plan, want an error naming the configured ones")
	}
}

// TestApplyValidatesEveryTargetFirst is docs/ARCHITECTURE.md#apply-manifests-and-undo's requirement: a plan that would produce one invalid
// path must not have already moved a thousand files by the time anyone finds out.
func TestApplyValidatesEveryTargetFirst(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{
			"a/IMG_1.png": "x",
			"a/IMG_2.png": "y",
		},
		map[string]map[string]string{
			"a/IMG_1.png": {"kind": "screenshot"},
			// A facet value that escapes the workspace.
			"a/IMG_2.png": {"kind": "../../outside"},
		},
	)
	f.plan(simplePlan())

	_, err := apply.Run(context.Background(), apply.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg,
		PlanName: "default", Execute: true, ToolVersion: "test",
	})
	if err == nil {
		t.Fatal("Run() error = nil for a target that escapes the workspace, want an error")
	}

	// And nothing moved, because validation happens before execution.
	tree := f.tree(t)

	if tree["a/IMG_1.png"] != "x" || tree["a/IMG_2.png"] != "y" {
		t.Errorf("files moved despite the invalid plan: %v", keysOf(tree))
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func keysOf2(m map[string]store.Stamp) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if b[k] != v {
			return false
		}
	}

	return true
}

// TestUndoRemovesDirectoriesItEmptied completes the restore. Without it a round trip leaves
// "sorted/photo/unknown" behind: every file is back byte for byte, and the layout still is not
// what it was.
func TestUndoRemovesDirectoriesItEmptied(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"2019/IMG_1.png": "x"},
		map[string]map[string]string{"2019/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	before := directories(t, f.ws.Root())

	applied := f.run(t, true)

	undone, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	})
	if err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if undone.DirectoriesRemoved == 0 {
		t.Error("DirectoriesRemoved = 0, want the directories apply created to be cleaned up")
	}

	after := directories(t, f.ws.Root())

	if !equalStrings(before, after) {
		t.Errorf("the directory layout was not restored\nbefore: %v\nafter:  %v", before, after)
	}
}

// TestUndoKeepsADirectoryThatStillHasFiles is the guard on that cleanup: it must never remove a
// directory the user put something in.
func TestUndoKeepsADirectoryThatStillHasFiles(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"2019/IMG_1.png": "x"},
		map[string]map[string]string{"2019/IMG_1.png": {"kind": "screenshot"}},
	)
	f.plan(simplePlan())

	applied := f.run(t, true)

	// Something else arrives in the target directory after the apply.
	stranger := filepath.Join(f.ws.Root(), "sorted", "screenshot", "not-ours.txt")
	if err := os.WriteFile(stranger, []byte("someone else's file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	}); err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("the undo removed a directory that still held a file: %v", err)
	}
}

// TestUndoNeverRemovesTheWorkspaceRoot is the obvious catastrophe to rule out.
func TestUndoNeverRemovesTheWorkspaceRoot(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"IMG_1.png": "x"},
		map[string]map[string]string{"IMG_1.png": {"kind": "screenshot"}},
	)
	// A target of one level, so the only directory involved is near the root.
	f.plan(config.Plan{
		Where: `decided("kind")`, Target: `{{ .kind }}`, Mode: "move", OnConflict: "suffix",
	})

	applied := f.run(t, true)

	if _, err := apply.Undo(context.Background(), apply.UndoOptions{
		Workspace: f.ws, DB: f.db, BatchID: applied.BatchID, Execute: true, ToolVersion: "test",
	}); err != nil {
		t.Fatalf("Undo(): %v", err)
	}

	if _, err := os.Stat(f.ws.Root()); err != nil {
		t.Fatalf("the workspace root is gone: %v", err)
	}

	if _, err := os.Stat(f.ws.Dir()); err != nil {
		t.Errorf("the .sicht directory is gone: %v", err)
	}

	if f.tree(t)["IMG_1.png"] != "x" {
		t.Error("the file was not restored")
	}
}

// directories lists every directory under root, relative, excluding .sicht.
func directories(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." || strings.HasPrefix(rel, ".sicht") {
			//nolint:nilerr // Same: an unrelatable path is skipped, not fatal.
			return nil
		}

		out = append(out, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	sort.Strings(out)

	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
