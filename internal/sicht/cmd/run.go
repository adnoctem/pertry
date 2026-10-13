package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/stage"
	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

var _ CLIOpt = NewRunCommand // assure type compatibility

// NewRunCommand creates the 'run' subcommand.
func NewRunCommand(a *App) *cobra.Command {
	var (
		dryRun  bool
		rerun   bool
		limit   int
		workers int
	)

	cmd := &cobra.Command{
		Use:   "run <stage>",
		Short: "Ask the local model about what the rules could not decide",
		Long: "Run one AI stage over the files its input expression selects.\n\n" +
			"Inference happens on this machine, against a locally installed Ollama. Nothing about\n" +
			"your images leaves it.\n\n" +
			"Run `sicht rules` first. What the rules decide is what the model never has to look at,\n" +
			"and `rules` reports how many files are left.\n\n" +
			"Re-running is cheap: a file whose answer was produced by the current model, prompt and\n" +
			"schema is skipped. Edit the prompt and they all become stale, which is the point.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runStage(cmd.Context(), args[0], stageFlags{
				dryRun: dryRun, rerun: rerun, limit: limit, workers: workers,
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Report what would be sent without calling the model")
	cmd.Flags().BoolVar(&rerun, "rerun", false,
		"Re-process files whose answer is already current")
	cmd.Flags().IntVar(&limit, "limit", 0, "Process at most this many files")
	cmd.Flags().IntVar(&workers, "workers", 0,
		"Concurrent requests (default: model.workers from sicht.yaml)")

	return cmd
}

type stageFlags struct {
	dryRun  bool
	rerun   bool
	limit   int
	workers int
}

func (a *App) runStage(ctx context.Context, name string, flags stageFlags) error {
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

	var completer infer.Completer

	if !flags.dryRun {
		backend, err := ollamabackend.New()
		if err != nil {
			return &notReadyError{err: fmt.Errorf("%w\nhint: run `sicht doctor`", err)}
		}

		// The model is ensured before any image is sent, with progress, so that a first run
		// does not look like a hang while six gigabytes download.
		if err := a.ensureModel(ctx, backend, cfg.Model.Name); err != nil {
			return err
		}

		completer = backend
	}

	result, err := stage.Run(ctx, stage.Options{
		Workspace: ws,
		DB:        db,
		Config:    cfg,
		Completer: completer,
		StageName: name,
		Limit:     flags.limit,
		Rerun:     flags.rerun,
		DryRun:    flags.dryRun,
		Workers:   flags.workers,
		Log:       a.Log,
		Progress:  a.stageProgress(),
	})
	if err != nil {
		a.printf("\n")
		a.printStageResult(result, flags.dryRun)

		return err
	}

	a.printf("\n")
	a.printStageResult(result, flags.dryRun)

	if result.Failed > 0 || result.Invalid > 0 {
		return &partialError{err: fmt.Errorf("%d file(s) did not produce a result",
			result.Failed+result.Invalid)}
	}

	return nil
}

// ensureModel pulls the configured model if it is not present.
func (a *App) ensureModel(ctx context.Context, backend *ollamabackend.Backend, name string) error {
	if _, err := backend.ModelInfo(ctx, name); err == nil {
		return nil
	}

	a.printf("Pulling %s. This happens once.\n", name)

	last := ""

	err := backend.EnsureModel(ctx, name, func(p infer.Progress) {
		// Only on a change of phase, so a slow download does not scroll the terminal.
		if p.Status == last {
			return
		}

		last = p.Status

		a.printf("  %s\n", p.Status)
	})
	if err != nil {
		return &notReadyError{err: fmt.Errorf("could not pull %s: %w\n"+
			"hint: run `sicht doctor` to check that Ollama is reachable", name, err)}
	}

	return nil
}

func (a *App) stageProgress() func(stage.Progress) {
	return func(p stage.Progress) {
		if p.Done == 0 {
			return
		}

		// A rate, because the useful question during a long run is how much longer.
		rate := p.Elapsed.Seconds() / float64(p.Done)

		a.printf("\r  %d done, %.1fs each", p.Done, rate)
	}
}

func (a *App) printStageResult(r stage.Result, dryRun bool) {
	if dryRun {
		a.printf("Dry run: the model was not called.\n\n")
	}

	a.printf("%-24s %s\n", "stage", r.Stage)
	a.printf("%-24s %d\n", "files selected", r.Selected)

	if r.Fresh > 0 {
		a.printf("%-24s %d  (already answered by this model, prompt and schema)\n",
			"already current", r.Fresh)
	}

	if dryRun {
		a.printf("%-24s %d\n", "would be sent", r.Selected-r.Fresh)
		a.printf("\nTo run it:\n  sicht run %s\n", r.Stage)

		return
	}

	a.printf("%-24s %d\n", "sent to the model", r.Processed)
	a.printf("%-24s %d\n", "decided", r.Decided)

	if r.Queued > 0 {
		a.printf("%-24s %d  (below the review threshold, or an invented value)\n",
			"for review", r.Queued)
	}

	if r.NoConfidence > 0 {
		a.printf("%-24s %d  (the server reported no logprobs)\n", "without a score", r.NoConfidence)
	}

	if r.Undecodable > 0 {
		a.printf("%-24s %d  (no pure-Go decoder, e.g. a real HEIC)\n", "undecodable", r.Undecodable)
	}

	if r.Invalid > 0 {
		a.printf("%-24s %d  (failed schema validation twice)\n", "invalid answers", r.Invalid)
	}

	if r.Failed > 0 {
		a.printf("%-24s %d\n", "failed", r.Failed)
	}

	a.printf("%-24s %s\n", "elapsed", r.Elapsed.Round(time.Millisecond))

	if r.Processed > 0 {
		a.printf("%-24s %s\n", "per image",
			(r.Elapsed / time.Duration(r.Processed)).Round(time.Millisecond))
	}

	a.printf("\n%-24s %s\n", "model", r.Model)

	if r.ModelDigest != "" {
		a.printf("%-24s %s\n", "weights", shorten(r.ModelDigest, 19))
	}

	a.printf("%-24s %s\n", "prompt", shorten(r.PromptHash, 19))
	a.printf("%-24s %s\n", "schema", shorten(r.SchemaHash, 19))

	if r.Queued > 0 {
		a.printf("\nTo look at what needs review:\n  sicht review %s --list\n", r.Stage)
	}
}

// shorten trims a hash for display, keeping its algorithm prefix.
func shorten(hash string, width int) string {
	if len(hash) <= width {
		return hash
	}

	return hash[:width] + "…"
}
