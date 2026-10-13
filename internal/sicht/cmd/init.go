package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
)

var _ CLIOpt = NewInitCommand // assure type compatibility

// NewInitCommand creates the 'init' subcommand.
func NewInitCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Create a workspace in a directory of images",
		Long: "Create .sicht/ in a directory, with a default sicht.yaml, the prompt templates the stages\n" +
			"refer to, and an empty database.\n\n" +
			"The directory is the workspace root: everything sicht knows lives under .sicht/ beside\n" +
			"your images, which are left exactly where they are. Run it in the directory holding the\n" +
			"archive, and every later command finds it by walking up from wherever you happen to be.\n\n" +
			"Idempotent. Re-running it creates whatever is missing and never overwrites a sicht.yaml,\n" +
			"because by then it holds a curated taxonomy and hand-tuned rules.",
		Args:             cobra.MaximumNArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}

			return a.runInit(cmd.Context(), dir)
		},
	}

	return cmd
}

func (a *App) runInit(ctx context.Context, dir string) error {
	w, err := core.Create(dir)
	if err != nil {
		return err
	}

	a.printf("Workspace at %s\n", w.Root())
	a.printf("  %s\n", w.Dir())

	// The config is written only if absent. By the time anyone re-runs init it holds a
	// curated taxonomy and tuned rules, and clobbering that silently would be unrecoverable.
	switch _, err := os.Stat(w.ConfigPath()); {
	case err == nil:
		a.printf("  %-28s already exists, left alone\n", core.ConfigName)
	case errors.Is(err, fs.ErrNotExist):
		if err := config.Write(w.ConfigPath(), config.Default()); err != nil {
			return err
		}

		a.printf("  %-28s written\n", core.ConfigName)
	default:
		return fmt.Errorf("stat %s: %w", w.ConfigPath(), err)
	}

	written, err := writePrompts(w)
	if err != nil {
		return err
	}

	for _, name := range written {
		a.printf("  %-28s written\n", filepath.Join(core.PromptsDir, name))
	}

	// Opening the database creates it and applies the migrations, so that `doctor` has
	// something to report on and the first real command does not pay for it.
	db, err := a.openStore(ctx, w)
	if err != nil {
		return err
	}

	if err := db.Close(); err != nil {
		return err
	}

	a.printf("  %-28s created\n", core.DatabaseName)

	a.printf("\nOne thing in the default configuration is deliberately not filled in:\n")
	a.printf("  screen_resolutions is empty. Rules must not match invented device dimensions;\n")
	a.printf("  populate it from your own files once you have scanned them.\n")
	a.printf("\nmodel.name is qwen3-vl:8b-instruct, chosen by benchmark. If your GPU cannot hold it,\n")
	a.printf("qwen3-vl:4b-instruct is the smaller choice - not the plain qwen3-vl tags, which are\n")
	a.printf("thinking variants that can reason until there is no room left to answer.\n")

	a.printf("\nNext: sicht doctor\n")

	return nil
}
