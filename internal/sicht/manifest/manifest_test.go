package manifest_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/manifest"
)

func newManifest() *manifest.Manifest {
	return &manifest.Manifest{
		BatchID:           "2026-10-12T10-00-00Z_default_abcd",
		Plan:              "default",
		WorkspaceRootHint: "/archive",
		CreatedAt:         time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Status:            manifest.StatusRunning,
		ToolVersion:       "test",
	}
}

// TestCreateWritesImmediately is why Create flushes: an apply killed between the first rename and
// the first update must still leave a manifest naming the batch.
func TestCreateWritesImmediately(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	if _, err := manifest.Create(path, newManifest()); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want the file to exist already", err)
	}

	if loaded.BatchID != newManifest().BatchID {
		t.Errorf("BatchID = %q, want it written", loaded.BatchID)
	}

	if loaded.Version != manifest.Version {
		t.Errorf("Version = %d, want %d", loaded.Version, manifest.Version)
	}
}

// TestEveryFlushLeavesValidJSON is the crash-safety property: a reader must never see a partial
// document, which is why the write is a temp file and a rename.
func TestEveryFlushLeavesValidJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	writer, err := manifest.Create(path, newManifest())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	for i := range 20 {
		index := writer.Add(manifest.Operation{
			Hash: "aaa", Op: manifest.OpMove,
			From: "a.png", To: "sorted/a.png",
		})

		if i%2 == 0 {
			op := writer.Manifest().Operations[index]
			op.Status = manifest.OpDone

			if err := writer.Update(index, op); err != nil {
				t.Fatalf("Update(): %v", err)
			}
		}

		if err := writer.Flush(); err != nil {
			t.Fatalf("Flush(): %v", err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		var generic map[string]any

		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatalf("after flush %d the file is not valid JSON: %v", i, err)
		}
	}
}

func TestSummaryIsRecomputedOnFlush(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	writer, err := manifest.Create(path, newManifest())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	for _, status := range []string{
		manifest.OpDone, manifest.OpDone, manifest.OpSkipped,
		manifest.OpFailed, manifest.OpPlanned,
	} {
		writer.Add(manifest.Operation{Op: manifest.OpMove, From: "a", To: "b", Status: status})
	}

	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush(): %v", err)
	}

	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	want := manifest.Summary{Planned: 1, Done: 2, Skipped: 1, Failed: 1}
	if loaded.Summary != want {
		t.Errorf("Summary = %+v, want %+v", loaded.Summary, want)
	}
}

// TestReversible is the rule that keeps undo from deleting anything: a copy is not reversible,
// because undoing it would mean removing a file.
func TestReversible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   manifest.Operation
		want bool
	}{
		{
			name: "a completed move",
			op:   manifest.Operation{Op: manifest.OpMove, From: "a", To: "b", Status: manifest.OpDone},
			want: true,
		},
		{
			name: "a completed companion move",
			op:   manifest.Operation{Op: manifest.OpCompanion, From: "a", To: "b", Status: manifest.OpDone},
			want: true,
		},
		{
			name: "a completed copy",
			op:   manifest.Operation{Op: manifest.OpCopy, From: "a", To: "b", Status: manifest.OpDone},
			want: false,
		},
		{
			name: "a skipped move",
			op:   manifest.Operation{Op: manifest.OpMove, From: "a", To: "b", Status: manifest.OpSkipped},
			want: false,
		},
		{
			name: "a failed move",
			op:   manifest.Operation{Op: manifest.OpMove, From: "a", To: "b", Status: manifest.OpFailed},
			want: false,
		},
		{
			name: "a recorded duplicate",
			op:   manifest.Operation{Op: manifest.OpDuplicateLeft, From: "a", Status: manifest.OpSkipped},
			want: false,
		},
		{
			name: "a done move with no destination",
			op:   manifest.Operation{Op: manifest.OpMove, From: "a", Status: manifest.OpDone},
			want: false,
		},
	}

	for _, tt := range tests {
		if got := tt.op.Reversible(); got != tt.want {
			t.Errorf("%s: Reversible() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestLoadRefusesAnUnknownVersion: an undo driven by a misread manifest moves files to the wrong
// places, so it is refused rather than attempted.
func TestLoadRefusesAnUnknownVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	m := newManifest()
	m.Version = 99

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	_, err := manifest.Load(path)
	if err == nil {
		t.Fatal("Load() error = nil for an unknown version")
	}

	if !strings.Contains(err.Error(), "wrong places") {
		t.Errorf("error = %q, want it to explain why this is refused rather than attempted", err)
	}
}

func TestLoadRejectsAManifestWithNoBatchID(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	if err := os.WriteFile(path, []byte(`{"sicht_manifest_version":1}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := manifest.Load(path); err == nil {
		t.Error("Load() error = nil for a manifest with no batch id")
	}
}

// TestFindAcceptsAPrefix exists because a batch id is 32 characters of timestamp and random
// suffix and nobody types one correctly.
func TestFindAcceptsAPrefix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	m := newManifest()
	if err := manifest.Save(filepath.Join(dir, manifest.FileName(m.BatchID)), m); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	for _, query := range []string{m.BatchID, m.BatchID[:12], "2026-10-12"} {
		got, err := manifest.Find(dir, query)
		if err != nil {
			t.Errorf("Find(%q) error = %v", query, err)

			continue
		}

		if filepath.Base(got) != manifest.FileName(m.BatchID) {
			t.Errorf("Find(%q) = %q, want the manifest", query, got)
		}
	}
}

// TestFindRefusesAnAmbiguousPrefix: guessing between two batches would move the wrong files.
func TestFindRefusesAnAmbiguousPrefix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, id := range []string{
		"2026-10-12T10-00-00Z_default_aaaa",
		"2026-10-12T10-00-00Z_default_bbbb",
	} {
		m := newManifest()
		m.BatchID = id

		if err := manifest.Save(filepath.Join(dir, manifest.FileName(id)), m); err != nil {
			t.Fatalf("Save(): %v", err)
		}
	}

	_, err := manifest.Find(dir, "2026-10-12")
	if err == nil {
		t.Fatal("Find() error = nil for an ambiguous prefix")
	}

	// The message names the candidates, since the user has to pick.
	if !strings.Contains(err.Error(), "aaaa") || !strings.Contains(err.Error(), "bbbb") {
		t.Errorf("error = %q, want it to list the matches", err)
	}
}

func TestFindNotFound(t *testing.T) {
	t.Parallel()

	_, err := manifest.Find(t.TempDir(), "nonsense")
	if !errors.Is(err, manifest.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestFindRejectsAnEmptyID(t *testing.T) {
	t.Parallel()

	if _, err := manifest.Find(t.TempDir(), ""); !errors.Is(err, manifest.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i, when := range []time.Time{
		time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
	} {
		m := newManifest()
		m.BatchID = manifest.NewBatchID(when, "default", string(rune('a'+i))+"000")
		m.CreatedAt = when

		if err := manifest.Save(filepath.Join(dir, manifest.FileName(m.BatchID)), m); err != nil {
			t.Fatalf("Save(): %v", err)
		}
	}

	listed, err := manifest.List(dir)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}

	if len(listed) != 3 {
		t.Fatalf("List() returned %d, want 3", len(listed))
	}

	for i := 1; i < len(listed); i++ {
		if listed[i].CreatedAt.After(listed[i-1].CreatedAt) {
			t.Errorf("List() is not newest-first: %v then %v",
				listed[i-1].CreatedAt, listed[i].CreatedAt)
		}
	}
}

// TestListSkipsAnUnreadableManifest: a listing is a report, so one bad file must not hide the rest.
func TestListSkipsAnUnreadableManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	m := newManifest()
	if err := manifest.Save(filepath.Join(dir, manifest.FileName(m.BatchID)), m); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	listed, err := manifest.List(dir)
	if err != nil {
		t.Fatalf("List() error = %v, want the good manifest to survive", err)
	}

	if len(listed) != 1 {
		t.Errorf("List() returned %d, want 1", len(listed))
	}
}

func TestListOnAMissingDirectory(t *testing.T) {
	t.Parallel()

	listed, err := manifest.List(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("List() error = %v, want nil for a directory that does not exist yet", err)
	}

	if len(listed) != 0 {
		t.Errorf("List() returned %d, want none", len(listed))
	}
}

func TestNewBatchIDIsSortableAndUnique(t *testing.T) {
	t.Parallel()

	earlier := manifest.NewBatchID(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), "default", "aaaa")
	later := manifest.NewBatchID(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), "default", "bbbb")

	// Sortable first, so a directory listing is in chronological order.
	if earlier >= later {
		t.Errorf("%q should sort before %q", earlier, later)
	}

	// Unique last, so two batches of the same plan in the same second do not collide.
	same := manifest.NewBatchID(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), "default", "cccc")
	if same == later {
		t.Error("two batches in the same second collided")
	}
}

func TestNewBatchIDSanitisesThePlanName(t *testing.T) {
	t.Parallel()

	id := manifest.NewBatchID(time.Now(), "my plan/with:junk", "aaaa")

	for _, bad := range []string{"/", ":", " "} {
		if strings.Contains(id, bad) {
			t.Errorf("batch id %q contains %q, which is not usable in a filename", id, bad)
		}
	}
}

func TestSetStatusStampsTheFinishTime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	writer, err := manifest.Create(path, newManifest())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	if writer.Manifest().FinishedAt != nil {
		t.Error("FinishedAt is set on a running manifest")
	}

	writer.SetStatus(manifest.StatusCompleted)

	if writer.Manifest().FinishedAt == nil {
		t.Error("FinishedAt is not set on a completed manifest")
	}
}

func TestUpdateRejectsABadIndex(t *testing.T) {
	t.Parallel()

	writer, err := manifest.Create(filepath.Join(t.TempDir(), "b.json"), newManifest())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	for _, index := range []int{-1, 0, 5} {
		if err := writer.Update(index, manifest.Operation{}); err == nil {
			t.Errorf("Update(%d) error = nil, want an out-of-range error", index)
		}
	}
}

// TestSaveProducesALoadableManifest is a round trip through this package's own two functions.
// Save used to leave Version at zero, which Load then refused as unknown - so the package could
// not read its own output.
func TestSaveProducesALoadableManifest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "batch.json")

	m := newManifest()
	m.Version = 0 // as a caller who never set it would leave it

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error = %v, want the round trip to work", err)
	}

	if loaded.Version != manifest.Version {
		t.Errorf("Version = %d, want %d", loaded.Version, manifest.Version)
	}
}
