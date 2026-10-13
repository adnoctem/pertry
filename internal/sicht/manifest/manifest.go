// Package manifest reads and writes the record of a file-moving batch (docs/ARCHITECTURE.md#apply-manifests-and-undo).
//
// A manifest is the only thing that makes `apply` reversible, so it is written before the files
// move, updated as they move, and written atomically every time. A crash must leave a manifest
// that says exactly which operations completed - never one that is truncated, and never one that
// claims more than happened.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version is the manifest schema version. A reader refuses a version it does not know rather than
// guessing, because an undo driven by a misread manifest moves files to the wrong places.
const Version = 1

// Status values for a manifest as a whole.
const (
	// StatusPlanned is a dry run: nothing was touched.
	StatusPlanned = "planned"

	// StatusRunning is written before the first file moves, so an interrupted run is
	// identifiable afterwards.
	StatusRunning = "running"

	// StatusCompleted means every operation reached a terminal state.
	StatusCompleted = "completed"

	// StatusFailed means the run stopped early.
	StatusFailed = "failed"

	// StatusUndone means an undo has reversed this batch.
	StatusUndone = "undone"
)

// Operation kinds.
const (
	// OpMove and OpCopy relocate an indexed file.
	OpMove = "move"
	OpCopy = "copy"

	// OpCompanion relocates a file that belongs to an indexed file but was never indexed
	// itself - a Takeout sidecar or a Live Photo motion file (docs/ARCHITECTURE.md#apply-manifests-and-undo).
	OpCompanion = "companion"

	// OpDuplicateLeft records a second location of content that was moved from elsewhere.
	// docs/ARCHITECTURE.md#apply-manifests-and-undo: `apply` moves the first location only, and says so about the others.
	OpDuplicateLeft = "duplicate_left"

	// OpCompanionOrphaned records a companion whose owner was not indexed, so nothing moved
	// it. Reported rather than moved, because guessing at an owner is how files get lost.
	OpCompanionOrphaned = "companion_orphaned"
)

// Operation statuses.
const (
	// OpPlanned is the state every operation starts in.
	OpPlanned = "planned"

	// OpDone means it completed.
	OpDone = "done"

	// OpSkipped means a conflict policy or a precondition declined to act.
	OpSkipped = "skipped"

	// OpFailed means it was attempted and failed.
	OpFailed = "failed"
)

// Manifest is one batch.
type Manifest struct {
	Version int    `json:"sicht_manifest_version"`
	BatchID string `json:"batch_id"`
	Plan    string `json:"plan"`

	// WorkspaceRootHint records where the workspace was when the batch ran. A hint and not a
	// key: docs/ARCHITECTURE.md#workspace-and-identity stores every path relative to the workspace precisely so that the archive can
	// be moved or read from another operating system, and an undo resolves paths against the
	// workspace it is run from, not against this string.
	WorkspaceRootHint string `json:"workspace_root_hint"`

	CreatedAt time.Time `json:"created_at"`

	// FinishedAt is set when the batch reaches a terminal status.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	Status string `json:"status"`

	ToolVersion string `json:"tool_version"`

	// ConfigHash identifies the configuration the plan came from, so that a batch produced by
	// a since-edited plan is distinguishable.
	ConfigHash string `json:"config_hash"`

	// UndoOf names the batch this one reverses, for a manifest written by `undo`.
	UndoOf string `json:"undo_of,omitempty"`

	// UndoneBy names the batch that reversed this one.
	UndoneBy string `json:"undone_by,omitempty"`

	Summary Summary `json:"summary"`

	Operations []Operation `json:"operations"`
}

// Summary counts operations by status.
type Summary struct {
	Planned int `json:"planned"`
	Done    int `json:"done"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// Operation is one file's journey.
type Operation struct {
	// Hash is the content hash of the file, empty for a companion, which was never indexed.
	Hash string `json:"hash,omitempty"`

	Op string `json:"op"`

	// From and To are workspace-relative, forward-slashed.
	From string `json:"from"`
	To   string `json:"to,omitempty"`

	// Facets and FacetSources record what the file was filed by and on whose authority, so a
	// manifest explains itself without the database.
	Facets       map[string]string `json:"facets,omitempty"`
	FacetSources map[string]string `json:"facet_sources,omitempty"`

	// Owner names the indexed file a companion belongs to.
	Owner string `json:"owner,omitempty"`

	Status string `json:"status"`

	// Verified is true when the destination's content was confirmed to match the source.
	// False for a same-volume rename, which moves no bytes.
	Verified bool `json:"verified,omitempty"`

	// CrossVolume records that a rename was impossible and a copy was used instead.
	CrossVolume bool `json:"cross_volume,omitempty"`

	// Error is the failure, when there was one.
	Error string `json:"error,omitempty"`
}

// Reversible reports whether an operation moved a file and can be undone.
//
// Only a completed move or companion move is reversible. A copy is not: undoing it would mean
// deleting a file, and docs/ARCHITECTURE.md#apply-manifests-and-undo never deletes anything. A skipped or failed operation did not happen.
func (o Operation) Reversible() bool {
	if o.Status != OpDone || o.To == "" || o.From == "" {
		return false
	}

	return o.Op == OpMove || o.Op == OpCompanion
}

// NewBatchID builds an identifier of the shape docs/ARCHITECTURE.md#apply-manifests-and-undo specifies: a timestamp, the plan name and a
// short random suffix.
//
// Sortable first, so that a directory listing of manifests is in chronological order, and unique
// last, so that two batches of the same plan in the same second do not collide.
func NewBatchID(when time.Time, plan, random string) string {
	return fmt.Sprintf("%s_%s_%s",
		when.UTC().Format("2006-01-02T15-04-05Z"), sanitisePlan(plan), random)
}

// sanitisePlan keeps a plan name usable in a filename.
func sanitisePlan(plan string) string {
	var b strings.Builder

	for _, r := range plan {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	if b.Len() == 0 {
		return "plan"
	}

	return b.String()
}

// FileName returns the manifest's filename.
func FileName(batchID string) string { return batchID + ".json" }

// Writer owns a manifest file and keeps it valid on disk at all times.
type Writer struct {
	path string
	m    *Manifest
}

// Create starts a new manifest at the given path.
//
// It writes immediately, before any file is touched, so that an `apply` killed between the first
// rename and the first flush still leaves a manifest naming the batch.
func Create(path string, m *Manifest) (*Writer, error) {
	if m.Version == 0 {
		m.Version = Version
	}

	w := &Writer{path: path, m: m}

	if err := w.Flush(); err != nil {
		return nil, err
	}

	return w, nil
}

// Manifest returns the in-memory manifest.
func (w *Writer) Manifest() *Manifest { return w.m }

// Add appends an operation and returns its index, for later update.
func (w *Writer) Add(op Operation) int {
	if op.Status == "" {
		op.Status = OpPlanned
	}

	w.m.Operations = append(w.m.Operations, op)

	return len(w.m.Operations) - 1
}

// Update replaces an operation in place.
func (w *Writer) Update(index int, op Operation) error {
	if index < 0 || index >= len(w.m.Operations) {
		return fmt.Errorf("operation index %d out of range", index)
	}

	w.m.Operations[index] = op

	return nil
}

// SetStatus sets the manifest's status, stamping the finish time for a terminal one.
func (w *Writer) SetStatus(status string) {
	w.m.Status = status

	switch status {
	case StatusCompleted, StatusFailed, StatusUndone:
		now := time.Now().UTC()
		w.m.FinishedAt = &now
	}
}

// Flush recomputes the summary and writes the manifest atomically.
//
// Atomic by temp-file-and-rename, so a reader never sees a partial document and a crash during
// the write leaves the previous version intact. Called after every operation during `apply`: the
// cost is one small write per file, and the alternative is a manifest that lies about what
// happened.
func (w *Writer) Flush() error {
	w.m.Summary = summarise(w.m.Operations)

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}

	encoded, err := json.MarshalIndent(w.m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	encoded = append(encoded, '\n')

	temp, err := os.CreateTemp(filepath.Dir(w.path), ".sicht-manifest-*")
	if err != nil {
		return fmt.Errorf("create temporary manifest: %w", err)
	}

	tempName := temp.Name()

	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)

		return fmt.Errorf("write manifest: %w", err)
	}

	// The manifest is the record that makes the batch reversible, so it is flushed to disk
	// rather than left in the page cache: a crash that loses it loses the ability to undo.
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)

		return fmt.Errorf("flush manifest: %w", err)
	}

	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)

		return fmt.Errorf("close manifest: %w", err)
	}

	if err := os.Rename(tempName, w.path); err != nil {
		_ = os.Remove(tempName)

		return fmt.Errorf("move manifest into place: %w", err)
	}

	return nil
}

func summarise(ops []Operation) Summary {
	var s Summary

	for _, op := range ops {
		switch op.Status {
		case OpDone:
			s.Done++
		case OpSkipped:
			s.Skipped++
		case OpFailed:
			s.Failed++
		default:
			s.Planned++
		}
	}

	return s
}

// Load reads a manifest.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}

	var m Manifest

	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	if m.Version != Version {
		return nil, fmt.Errorf("manifest %s has version %d, and this build understands %d\n"+
			"hint: an undo driven by a misread manifest would move files to the wrong places, "+
			"so it is refused rather than attempted", path, m.Version, Version)
	}

	if m.BatchID == "" {
		return nil, fmt.Errorf("manifest %s has no batch id", path)
	}

	return &m, nil
}

// Save writes a manifest that is not being incrementally updated.
//
// Defaults the version exactly as Create does. Without that, saving a manifest whose Version was
// never set produced a file that Load then refused as an unknown version - a round trip through
// this package's own two functions that could not read its own output.
func Save(path string, m *Manifest) error {
	if m.Version == 0 {
		m.Version = Version
	}

	w := &Writer{path: path, m: m}

	return w.Flush()
}

// ErrNotFound is returned when no manifest matches an identifier.
var ErrNotFound = errors.New("no such batch")

// Find locates a manifest in a directory by batch id, accepting a unique prefix.
//
// A prefix is accepted because a full batch id is 32 characters of timestamp and random suffix,
// and nobody is going to type one correctly. An ambiguous prefix is an error rather than a guess.
func Find(dir, batchID string) (string, error) {
	if batchID == "" {
		return "", fmt.Errorf("%w: no batch id given", ErrNotFound)
	}

	exact := filepath.Join(dir, FileName(batchID))
	if _, err := os.Stat(exact); err == nil {
		return exact, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read manifest directory %s: %w", dir, err)
	}

	var matches []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		if strings.HasPrefix(e.Name(), batchID) {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNotFound, batchID)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)

		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, strings.TrimSuffix(filepath.Base(m), ".json"))
		}

		return "", fmt.Errorf("%q matches %d batches: %s",
			batchID, len(matches), strings.Join(names, ", "))
	}
}

// List returns every manifest in a directory, newest first.
func List(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read manifest directory %s: %w", dir, err)
	}

	var out []*Manifest

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		m, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			// One unreadable manifest must not hide the rest; a listing is a report,
			// not an operation.
			continue
		}

		out = append(out, m)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})

	return out, nil
}
