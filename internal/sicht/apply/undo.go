package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/adnoctem/pertry/internal/fsops"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/manifest"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// UndoOptions configures an undo.
type UndoOptions struct {
	Workspace *core.Workspace
	DB        *store.DB

	// BatchID is the batch to reverse, accepting a unique prefix.
	BatchID string

	// Execute performs the reversal. Without it the undo is reported and nothing is touched.
	Execute bool

	// Force reverses an operation even when the file at the destination is not the one that
	// was put there. Off by default, because the usual cause is that the file was edited
	// after the move and moving it back would hide that.
	Force bool

	ToolVersion string

	Log *slog.Logger

	Progress func(Progress)
}

// UndoResult summarises an undo.
type UndoResult struct {
	// BatchID is the batch that was reversed.
	BatchID string

	// UndoBatchID identifies the undo's own manifest, which links back to the original.
	UndoBatchID  string
	ManifestPath string

	// Reversible is how many operations could be reversed.
	Reversible int

	Done    int
	Skipped int
	Failed  int

	// Changed is how many files were skipped because their content no longer matches what
	// the batch moved. This is the number worth looking at: it means someone edited a file
	// after it was filed.
	Changed int

	// Missing is how many files were not where the manifest says they were put.
	Missing int

	// DirectoriesRemoved is how many directories the batch created and the undo emptied.
	DirectoriesRemoved int

	Executed bool

	Errors []error
}

// Undo reverses a batch.
//
// Operations are reversed in the opposite order to the one they were applied in, so that a file
// moved into a directory created by an earlier operation is moved out before the directory is
// considered empty.
//
// Only completed moves are reversed. A copy is not undone, because undoing a copy means deleting
// a file and docs/ARCHITECTURE.md#apply-manifests-and-undo never deletes anything; a skipped or failed operation did not happen.
func Undo(ctx context.Context, opts UndoOptions) (UndoResult, error) {
	var result UndoResult

	if opts.Workspace == nil || opts.DB == nil {
		return result, errors.New("undo: workspace and database are required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	manifestPath, err := manifest.Find(opts.Workspace.ManifestsPath(), opts.BatchID)
	if err != nil {
		return result, err
	}

	original, err := manifest.Load(manifestPath)
	if err != nil {
		return result, err
	}

	result.BatchID = original.BatchID

	if original.Status == manifest.StatusUndone {
		return result, fmt.Errorf("batch %s has already been undone by %s",
			original.BatchID, original.UndoneBy)
	}

	if original.Status == manifest.StatusPlanned {
		return result, fmt.Errorf("batch %s was a dry run, so there is nothing to undo",
			original.BatchID)
	}

	// Reverse order, so a file moved into a directory an earlier operation created comes out
	// before anything assumes that directory is finished with.
	reversible := make([]manifest.Operation, 0, len(original.Operations))

	for _, op := range original.Operations {
		if op.Reversible() {
			reversible = append(reversible, op)
		}
	}

	slices.Reverse(reversible)

	result.Reversible = len(reversible)

	if !opts.Execute {
		// A dry run still checks every precondition, so that the report is about what
		// would really happen rather than about what the manifest claims.
		for _, op := range reversible {
			// The file is currently at the original operation's destination.
			switch inspect(opts.Workspace, op.To, op.Hash) {
			case stateMissing:
				result.Missing++
			case stateChanged:
				result.Changed++
			case stateOK, stateUnknown:
				// Nothing to report: it is where it should be.
			}
		}

		log.Info("undo planned", "batch", original.BatchID, "reversible", result.Reversible)

		return result, nil
	}

	result.Executed = true

	undoID := manifest.NewBatchID(time.Now(), "undo-"+original.Plan, randomSuffix())
	undoPath := path.Join(opts.Workspace.ManifestsPath(), manifest.FileName(undoID))

	result.UndoBatchID = undoID
	result.ManifestPath = undoPath

	undoManifest := &manifest.Manifest{
		Version:           manifest.Version,
		BatchID:           undoID,
		Plan:              original.Plan,
		WorkspaceRootHint: opts.Workspace.Root(),
		CreatedAt:         time.Now().UTC(),
		Status:            manifest.StatusRunning,
		ToolVersion:       opts.ToolVersion,
		ConfigHash:        original.ConfigHash,
		UndoOf:            original.BatchID,
	}

	writer, err := manifest.Create(undoPath, undoManifest)
	if err != nil {
		return result, err
	}

	for _, op := range reversible {
		// The reversal: what was the destination becomes the source.
		writer.Add(manifest.Operation{
			Hash:   op.Hash,
			Op:     op.Op,
			From:   op.To,
			To:     op.From,
			Owner:  op.Owner,
			Status: manifest.OpPlanned,
		})
	}

	if err := writer.Flush(); err != nil {
		return result, err
	}

	done := 0

	for i, op := range writer.Manifest().Operations {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// In an undo operation the file is at From - which is where the original batch
		// put it - and To is where it is going back to.
		switch state := inspect(opts.Workspace, op.From, op.Hash); {
		case state == stateMissing:
			op.Status = manifest.OpSkipped
			op.Error = "the file is not where the batch put it"

			result.Missing++
			result.Skipped++

			log.Warn("skipping: file missing", "expected_at", op.From)

		case state == stateChanged && !opts.Force:
			// docs/ARCHITECTURE.md#apply-manifests-and-undo requires this: undo fails safely when the target has been modified
			// since, and reports the file rather than overwriting the newer version.
			op.Status = manifest.OpSkipped
			op.Error = "the file has changed since it was moved; re-run with --force to move it anyway"

			result.Changed++
			result.Skipped++

			log.Warn("skipping: file changed since the move", "path", op.From)

		default:
			outcome, err := fsops.Relocate(
				localPath(opts.Workspace, op.From),
				localPath(opts.Workspace, op.To),
				fsops.ModeMove, fsops.ConflictSuffix,
			)

			switch {
			case err != nil:
				op.Status = manifest.OpFailed
				op.Error = err.Error()

				result.Failed++
				result.Errors = append(result.Errors, err)

			case outcome.Skipped:
				op.Status = manifest.OpSkipped

				result.Skipped++

			default:
				op.Status = manifest.OpDone
				op.Verified = outcome.Verified
				op.CrossVolume = outcome.CrossVolume

				if rel, relErr := opts.Workspace.Rel(outcome.To); relErr == nil {
					op.To = rel
				}

				result.Done++

				if op.Op == manifest.OpMove {
					if err := opts.DB.MoveLocation(ctx, op.From, op.To); err != nil {
						return result, err
					}
				}
			}
		}

		if err := writer.Update(i, op); err != nil {
			return result, err
		}

		if err := writer.Flush(); err != nil {
			return result, err
		}

		done++

		if opts.Progress != nil {
			opts.Progress(Progress{Done: done, Total: len(reversible), Path: op.From})
		}
	}

	// Directories the batch created and the undo has now emptied. Without this a round trip
	// leaves "sorted/photo/unknown" behind: every file is restored byte for byte, and the
	// layout still is not what it was, which is what docs/ARCHITECTURE.md#apply-manifests-and-undo promises.
	result.DirectoriesRemoved = removeEmptied(opts.Workspace, reversible, log)

	writer.SetStatus(manifest.StatusCompleted)

	if err := writer.Flush(); err != nil {
		return result, err
	}

	// The original is marked undone and linked to its reversal, so the history reads in both
	// directions.
	original.Status = manifest.StatusUndone
	original.UndoneBy = undoID

	if err := manifest.Save(manifestPath, original); err != nil {
		return result, err
	}

	if err := opts.DB.SetBatchStatus(ctx, original.BatchID, manifest.StatusUndone); err != nil {
		return result, err
	}

	if err := recordBatch(ctx, opts.DB, undoID, original.Plan, undoPath, manifest.StatusCompleted); err != nil {
		return result, err
	}

	log.Info("undo finished",
		"batch", original.BatchID, "undo", undoID,
		"done", result.Done, "skipped", result.Skipped, "failed", result.Failed)

	return result, nil
}

// removeEmptied deletes directories the batch created, now that the undo has emptied them.
//
// Deliberately narrow. Only directories that were an ancestor of one of this batch's targets are
// considered, only inside the workspace, never the workspace root itself, and only when
// os.Remove succeeds - which it does for an empty directory and refuses for one that still holds
// anything. So a directory the user created, or one that still has files in it, is never touched.
//
// Deepest first, because a parent only becomes empty once its children are gone.
func removeEmptied(ws *core.Workspace, operations []manifest.Operation, log *slog.Logger) int {
	candidates := map[string]struct{}{}

	for _, op := range operations {
		// op.To is where the batch put the file, so its directory is what the batch may
		// have created.
		for dir := path.Dir(op.To); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			if core.IsInternal(dir) {
				break
			}

			candidates[dir] = struct{}{}
		}
	}

	dirs := make([]string, 0, len(candidates))
	for dir := range candidates {
		dirs = append(dirs, dir)
	}

	// Deepest first: more slashes means deeper, and the comparison is reversed so that
	// children are removed before their parents.
	slices.SortFunc(dirs, func(a, b string) int {
		if depth := strings.Count(b, "/") - strings.Count(a, "/"); depth != 0 {
			return depth
		}

		return strings.Compare(b, a)
	})

	removed := 0

	for _, dir := range dirs {
		if err := os.Remove(localPath(ws, dir)); err != nil {
			// Expected for a directory that still holds something, which is most of
			// them: the year folders the files went back into are not empty.
			continue
		}

		removed++

		log.Debug("removed a directory the batch created", "path", dir)
	}

	return removed
}

// fileState is what an undo found at an operation's destination.
type fileState int

const (
	// stateOK means the file is there and is the one the batch moved.
	stateOK fileState = iota

	// stateMissing means nothing is at the path the batch moved the file to.
	stateMissing

	// stateChanged means a file is there but its content differs from what was moved.
	stateChanged

	// stateUnknown means the check could not be made - a companion has no recorded hash,
	// so there is nothing to compare against.
	stateUnknown
)

// inspect checks whether the file at relPath is still the one whose hash was recorded.
//
// Takes the path and the hash directly rather than an Operation, because the two callers mean
// different fields by it: planning an undo looks at the original operation's destination, while
// executing one looks at the undo operation's source. Passing an Operation and relying on the
// caller to swap From and To was a trap worth removing.
//
// The hash comparison is what docs/ARCHITECTURE.md#apply-manifests-and-undo means by failing safely: moving a file back over a newer
// version of itself would destroy work, and the only way to know is to look.
func inspect(ws *core.Workspace, relPath, hash string) fileState {
	abs := localPath(ws, relPath)

	info, err := os.Stat(abs)
	if err != nil {
		return stateMissing
	}

	if !info.Mode().IsRegular() {
		return stateMissing
	}

	// A companion is not indexed, so no hash was recorded for it and there is nothing to
	// compare. Its presence is all that can be checked.
	if hash == "" {
		return stateUnknown
	}

	got, err := fsops.Hash(abs)
	if err != nil {
		return stateUnknown
	}

	if got != hash {
		return stateChanged
	}

	return stateOK
}
