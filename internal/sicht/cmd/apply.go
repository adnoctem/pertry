package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/apply"
	"github.com/adnoctem/pertry/pkg/stamp"
)

var _ CLIOpt = NewApplyCommand // assure type compatibility

// NewApplyCommand creates the 'apply' subcommand.
func NewApplyCommand(a *App) *cobra.Command {
	var (
		plan    string
		execute bool
		limit   int
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Move files according to a plan",
		Long: "File your images into directories derived from what has been decided about them.\n\n" +
			"A dry run by default: it writes a manifest describing every move it would make and\n" +
			"touches nothing. Read it, then re-run with --execute.\n\n" +
			"Every move is recorded in the manifest as it happens, so `sicht undo <batch>` can put\n" +
			"everything back - and the manifest stays valid even if the run is interrupted.\n\n" +
			"A file whose content is already somewhere else in the archive is moved once, from its\n" +
			"first path; the other locations are recorded rather than moved. A Takeout sidecar or a\n" +
			"Live Photo motion file travels with the image it belongs to.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runApply(cmd.Context(), plan, execute, limit)
		},
	}

	cmd.Flags().StringVar(&plan, "plan", "default", "The plan from sicht.yaml to apply")
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually move the files")
	cmd.Flags().IntVar(&limit, "limit", 0, "Plan at most this many files")

	return cmd
}

func (a *App) runApply(ctx context.Context, plan string, execute bool, limit int) error {
	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	cfg, err := a.requireConfig()
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

	result, err := apply.Run(ctx, apply.Options{
		Workspace: ws,
		DB:        db,
		Config:    cfg,
		PlanName:  plan,
		Execute:   execute,
		Limit:     limit,
		// The link-time version, so a manifest says which build wrote it. An
		// unstamped build records "redacted", which is the honest answer.
		ToolVersion: stamp.Version,
		Log:         a.Log,
		Progress:    a.applyProgress(),
	})
	if err != nil {
		if result.BatchID != "" {
			a.printf("\nThe manifest records what completed: %s\n", result.ManifestPath)
		}

		return err
	}

	a.printApplyResult(result, plan, time.Since(started))

	if result.Failed > 0 {
		return &partialError{err: fmt.Errorf("%d operation(s) failed", result.Failed)}
	}

	return nil
}

func (a *App) applyProgress() func(apply.Progress) {
	return func(p apply.Progress) {
		if p.Total == 0 {
			return
		}

		a.printf("\r  moving %d/%d", p.Done, p.Total)
	}
}

func (a *App) printApplyResult(r apply.Result, plan string, elapsed time.Duration) {
	if r.Executed {
		a.printf("\n")
	}

	a.printf("%-24s %s\n", "plan", plan)
	a.printf("%-24s %s\n", "batch", r.BatchID)
	a.printf("%-24s %d\n", "files selected", r.Files)

	if r.Companions > 0 {
		a.printf("%-24s %d  (sidecars and Live Photo motion files)\n", "companions", r.Companions)
	}

	if r.DuplicatesLeft > 0 {
		a.printf("%-24s %d  (same content elsewhere; moved once, from its first path)\n",
			"duplicates recorded", r.DuplicatesLeft)
	}

	if r.OrphanCompanions > 0 {
		a.printf("%-24s %d  (no indexed owner, so left alone)\n",
			"unclaimed companions", r.OrphanCompanions)
	}

	if !r.Executed {
		a.printf("%-24s %d\n", "operations planned", r.Planned)
		a.printf("%-24s %s\n", "manifest", r.ManifestPath)
		a.printf("\nNothing was moved. Read the manifest, then:\n")
		a.printf("  sicht apply --plan %s --execute\n", plan)

		return
	}

	a.printf("%-24s %d\n", "moved", r.Done)

	if r.Skipped > 0 {
		a.printf("%-24s %d\n", "skipped", r.Skipped)
	}

	if r.Failed > 0 {
		a.printf("%-24s %d\n", "failed", r.Failed)
	}

	a.printf("%-24s %s\n", "elapsed", elapsed.Round(time.Millisecond))
	a.printf("%-24s %s\n", "manifest", r.ManifestPath)
	a.printf("\nTo put everything back:\n")
	a.printf("  sicht undo %s --execute\n", r.BatchID)
}
