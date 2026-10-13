package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

var _ CLIOpt = NewModelCommand // assure type compatibility

// NewModelCommand creates the 'model' subcommand and its topics.
func NewModelCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Inspect or fetch the configured vision model",
		Long: "The model named in sicht.yaml is a candidate, not a recommendation. Which local vision\n" +
			"model is actually best at this is settled by benchmarking against your own files, not\n" +
			"by reading model cards.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
	}

	cmd.AddCommand(newModelInfoCommand(a), newModelPullCommand(a))

	return cmd
}

func newModelInfoCommand(a *App) *cobra.Command {
	return &cobra.Command{
		Use:              "info",
		Short:            "What the server knows about the configured model",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runModelInfo(cmd.Context())
		},
	}
}

func newModelPullCommand(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "pull",
		Short: "Fetch the configured model",
		Long: "Download the model named in sicht.yaml. `sicht run` does this automatically on first\n" +
			"use; this exists so the download can be done deliberately rather than in the middle of\n" +
			"a run.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runModelPull(cmd.Context())
		},
	}
}

func (a *App) runModelInfo(ctx context.Context) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	backend, err := ollamabackend.New()
	if err != nil {
		return &notReadyError{err: err}
	}

	info, err := backend.ModelInfo(ctx, cfg.Model.Name)
	if err != nil {
		return &notReadyError{err: fmt.Errorf("%s is not available: %w\n"+
			"hint: run `sicht model pull`", cfg.Model.Name, err)}
	}

	a.printf("%-22s %s\n", "name", cfg.Model.Name)

	if info.Digest != "" {
		a.printf("%-22s %s\n", "weights", info.Digest)
	}

	if len(info.Capabilities) > 0 {
		a.printf("%-22s %v\n", "capabilities", info.Capabilities)
	}

	// Vision is not optional: a text-only model cannot classify a screenshot, and finding
	// that out from nonsense output would be far worse than finding it out here.
	if len(info.Capabilities) > 0 && !info.HasCapability(ollamabackend.CapabilityVision) {
		a.printf("\n%s\n", a.paint("This model does not report the vision capability. "+
			"sicht classifies images, so it cannot be used.", "31"))
	}

	if info.Size > 0 {
		a.printf("%-22s %s\n", "size", humanBytes(info.Size))
	}

	if info.SizeVRAM > 0 {
		a.printf("%-22s %s\n", "in VRAM", humanBytes(info.SizeVRAM))

		if info.SizeVRAM < info.Size {
			a.printf("\n%s\n", a.paint("Part of the model is on the CPU, which is much slower.", "33"))
		}
	}

	a.printf("\nConfigured options, passed through verbatim:\n")

	for _, key := range sortedKeys(cfg.Model.Options) {
		a.printf("  %-20s %v\n", key, cfg.Model.Options[key])
	}

	a.printf("  %-20s %s\n", "keep_alive", cfg.Model.KeepAlive)
	a.printf("  %-20s %d\n", "max_image_edge", cfg.Model.MaxImageEdge)
	a.printf("  %-20s %d\n", "workers", cfg.Model.Workers)

	logprobs, err := backend.SupportsLogprobs(ctx, cfg.Model.Name)
	if err != nil {
		a.Log.Debug("could not test logprob support", "error", err)
	} else {
		a.printf("\n%-22s %v\n", "reports logprobs", logprobs)

		if !logprobs {
			a.printf("%s\n", a.paint("Without logprobs there is no confidence score, so "+
				"nothing can be prioritised for review.", "33"))
		}
	}

	return nil
}

func (a *App) runModelPull(ctx context.Context) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	backend, err := ollamabackend.New()
	if err != nil {
		return &notReadyError{err: err}
	}

	return a.ensureModel(ctx, backend, cfg.Model.Name)
}
