package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/apply"
	"github.com/adnoctem/pertry/pkg/stamp"
)

var _ CLIOpt = NewUndoCommand // assure type compatibility

// NewUndoCommand creates the 'undo' subcommand.
func NewUndoCommand(a *App) *cobra.Command {
	var (
		execute bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "undo <batch-id>",
		Short: "Put a batch's files back where they were",
		Long: "Reverse an `apply` batch, using the manifest it wrote.\n\n" +
			"A dry run by default, like `apply`. Re-run with --execute to move the files.\n\n" +
			"A file whose content has changed since it was filed is reported and skipped, not moved\n" +
			"back over the newer version. --force overrides that, which is almost never what you\n" +
			"want.\n\n" +
			"Only moves are reversed. A copy is left alone, because undoing a copy would mean\n" +
			"deleting a file and sicht does not delete files.\n\n" +
			"The batch id may be abbreviated to any unique prefix.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runUndo(cmd.Context(), args[0], execute, force)
		},
	}

	cmd.Flags().BoolVar(&execute, "execute", false, "Actually move the files back")
	cmd.Flags().BoolVar(&force, "force", false,
		"Move a file back even if its content has changed since it was filed")

	return cmd
}

func (a *App) runUndo(ctx context.Context, batchID string, execute, force bool) error {
	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	db, err := a.openStore(ctx, ws)
	if err != nil {
		return err
	}

	defer func() {
		if err := db.Close(); err != nil {
			a.Log.Warn("closing the database failed", "error", err)
		}
	}()

	started := time.Now()

	result, err := apply.Undo(ctx, apply.UndoOptions{
		Workspace:   ws,
		DB:          db,
		BatchID:     batchID,
		Execute:     execute,
		Force:       force,
		ToolVersion: stamp.Version,
		Log:         a.Log,
		Progress:    a.undoProgress(),
	})
	if err != nil {
		return &usageError{err: err}
	}

	a.printUndoResult(result, time.Since(started))

	if result.Failed > 0 {
		return &partialError{err: fmt.Errorf("%d operation(s) failed", result.Failed)}
	}

	return nil
}

func (a *App) undoProgress() func(apply.Progress) {
	return func(p apply.Progress) {
		if p.Total == 0 {
			return
		}

		a.printf("\r  restoring %d/%d", p.Done, p.Total)
	}
}

func (a *App) printUndoResult(r apply.UndoResult, elapsed time.Duration) {
	if r.Executed {
		a.printf("\n")
	}

	a.printf("%-24s %s\n", "batch", r.BatchID)
	a.printf("%-24s %d\n", "reversible operations", r.Reversible)

	if r.Missing > 0 {
		a.printf("%-24s %d  (not where the batch put them)\n", "missing", r.Missing)
	}

	if r.Changed > 0 {
		a.printf("%-24s %d  (edited since being filed; left alone)\n", "changed", r.Changed)
	}

	if !r.Executed {
		a.printf("\nNothing was moved. To reverse it:\n")
		a.printf("  sicht undo %s --execute\n", r.BatchID)

		return
	}

	a.printf("%-24s %d\n", "restored", r.Done)

	if r.Skipped > 0 {
		a.printf("%-24s %d\n", "skipped", r.Skipped)
	}

	if r.Failed > 0 {
		a.printf("%-24s %d\n", "failed", r.Failed)
	}

	if r.DirectoriesRemoved > 0 {
		a.printf("%-24s %d  (emptied by the undo)\n", "directories removed", r.DirectoriesRemoved)
	}

	a.printf("%-24s %s\n", "elapsed", elapsed.Round(time.Millisecond))
	a.printf("%-24s %s\n", "undo manifest", r.ManifestPath)
}
