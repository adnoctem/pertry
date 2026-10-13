// Package cmd implements the sicht command-line interface.
//
// It is one of two thin frontends over internal/sicht/core, the other being the web UI. Neither
// calls the other, and core does not import either (docs/ARCHITECTURE.md#repository-layout). There is no global
// state: a *core.Workspace is passed explicitly.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/doctor"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// Name is the application name.
const Name = "sicht"

// Exit codes (cmd/sicht/README.md). Wider than bookscape's, which has only the first three:
// sicht distinguishes "the environment is not ready" so that a wrapper can run `doctor` and act on
// the answer, and "some items failed" so that a partial pass over ten thousand images is not
// indistinguishable from a total failure.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotReady = 3
	ExitPartial  = 4
)

// App is the state every subcommand shares.
type App struct {
	// Out is where human-facing output goes; a field so tests can capture it.
	Out io.Writer

	// Log is the structured logger at the level --log-level asked for.
	Log *slog.Logger

	// Workspace is resolved before each subcommand runs, and is nil when none was found.
	// Subcommands that require one say so rather than assuming.
	Workspace *core.Workspace

	// Config is the parsed sicht.yaml, nil when there is no workspace or no config.
	Config *config.Config

	workspaceFlag string
	configFlag    string
	logLevel      string
	noColor       bool
}

// CLIOpt builds a subcommand against the shared state.
type CLIOpt func(a *App) *cobra.Command

// commands is every subcommand. The CLI reference in cmd/sicht/README.md lists the full target set; these are the
// ones M0 implements.
var commands = []CLIOpt{
	NewInitCommand,
	NewDoctorCommand,
	NewScanCommand,
	NewRulesCommand,
	NewStatsCommand,
	NewApplyCommand,
	NewUndoCommand,
	NewExportCommand,
	NewRunCommand,
	NewModelCommand,
	NewTaxonomyCommand,
	NewReviewCommand,
	NewVersionCommand,
}

// NewRootCommand builds the sicht root command.
func NewRootCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   Name,
		Short: "Local-first iterative image sorter",
		Long: "Sort a large image archive by sifting it: metadata and deterministic rules first, a local\n" +
			"vision model only for what the rules cannot decide, a human review pass for what the\n" +
			"model was unsure about, and only then any file movement - recorded in a manifest that\n" +
			"can be undone.\n\n" +
			"Nothing leaves this machine. Inference runs against a locally installed Ollama.",
		SilenceErrors:     true,
		SilenceUsage:      true,
		TraverseChildren:  true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.setup(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Usage()
			}

			// An unknown subcommand must not be quietly successful. Returning nil
			// here printed nothing and exited 0, so a typo in a script looked like a
			// clean run - the one outcome worse than an error.
			return usagef("unknown command %q\nrun `%s --help` to see the available commands",
				args[0], Name)
		},
	}

	cmd.PersistentFlags().StringVar(&a.workspaceFlag, "workspace", "",
		"Workspace root (default: discovered by walking up from the current directory)")
	cmd.PersistentFlags().StringVar(&a.configFlag, "config", "",
		"Path to sicht.yaml (default: <workspace>/.sicht/sicht.yaml)")
	cmd.PersistentFlags().StringVar(&a.logLevel, "log-level", "info",
		"Log verbosity: debug, info, warn or error")
	cmd.PersistentFlags().BoolVar(&a.noColor, "no-color", false,
		"Disable coloured output")

	// Cobra reports a malformed flag as an ordinary error, which would exit 1. How the command
	// was invoked is a usage error, so a caller can tell "you typed it wrong" apart from "it
	// ran and failed". Set on the root, which is where cobra looks when a subcommand has none.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	for _, opt := range commands {
		cmd.AddCommand(opt(a))
	}

	return cmd
}

// setup resolves logging, the workspace and the config before any subcommand runs.
//
// A missing workspace is not an error here. `init` creates one and `doctor` reports on its
// absence, so failing in the shared setup would make both impossible to use.
func (a *App) setup(cmd *cobra.Command) error {
	level, err := parseLevel(a.logLevel)
	if err != nil {
		return err
	}

	if a.Out == nil {
		a.Out = cmd.OutOrStdout()
	}

	// Logs go to stderr so that subcommand output stays pipeable.
	a.Log = slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

	if err := a.resolveWorkspace(); err != nil {
		return err
	}

	return a.resolveConfig()
}

func (a *App) resolveWorkspace() error {
	if a.workspaceFlag != "" {
		// Named explicitly, so its absence is an error rather than something to discover.
		w, err := core.Open(a.workspaceFlag)
		if err != nil {
			return &usageError{err: err}
		}

		a.Workspace = w

		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine the current directory: %w", err)
	}

	w, err := core.Discover(cwd)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Left nil deliberately. `init` needs to run without one and `doctor` needs
			// to report on it.
			return nil
		}

		return err
	}

	a.Workspace = w

	return nil
}

func (a *App) resolveConfig() error {
	path := a.configFlag

	if path == "" {
		if a.Workspace == nil {
			return nil
		}

		path = a.Workspace.ConfigPath()

		if _, err := os.Stat(path); err != nil {
			//nolint:nilerr // No config yet: `init` writes it. Failing here would make
			// both `init` and `doctor` impossible to run in a fresh workspace.
			return nil
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		// A broken config is something the user has to fix, not a run that failed.
		return &usageError{err: err}
	}

	a.Config = cfg

	return nil
}

// openStore opens the workspace database, applying any pending migrations.
func (a *App) openStore(ctx context.Context, w *core.Workspace) (*store.DB, error) {
	a.Log.Debug("opening database", "path", w.DatabasePath())

	return store.Open(ctx, w.DatabasePath())
}

// databaseOpener adapts the store to doctor's narrow interface.
func (a *App) databaseOpener() doctor.DatabaseOpener {
	return func(ctx context.Context, path string) (doctor.Database, error) {
		return store.Open(ctx, path)
	}
}

// printf writes human-facing output.
func (a *App) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.Out, format, args...)
}

// color reports whether to colourise output: not when --no-color, and not when NO_COLOR is set,
// which is the convention every other tool on the machine follows.
func (a *App) color() bool {
	if a.noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}

	return true
}

// paint wraps text in an ANSI colour when colour is enabled, and returns it unchanged otherwise.
//
// Takes the SGR code rather than a named colour, because the set of things worth colouring is
// small and naming them would be more indirection than it saves.
func (a *App) paint(text, code string) string {
	if !a.color() {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, usagef("unknown --log-level %q\nhint: one of debug, info, warn, error", name)
	}
}

// Execute runs the root command and returns the process exit code.
func Execute(ctx context.Context, a *App) int {
	if a.Out == nil {
		a.Out = os.Stdout
	}

	root := NewRootCommand(a)
	root.SetOut(a.Out)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	fmt.Fprintf(os.Stderr, "%s: %v\n", Name, err)

	return ExitCodeFor(err)
}

// ExitCodeFor maps an error to the process exit code it should produce.
//
// Separate from Execute so that the mapping can be asserted without a test having to reach into
// how the error is printed: the codes are the part callers depend on (cmd/sicht/README.md), and a wrapper script
// that branches on 3 will keep working only if this stays stable.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}

	var notReady *notReadyError
	if errors.As(err, &notReady) {
		return ExitNotReady
	}

	var partial *partialError
	if errors.As(err, &partial) {
		return ExitPartial
	}

	return ExitError
}

// requireWorkspace returns the resolved workspace, or a usage error explaining how to get one.
func (a *App) requireWorkspace() (*core.Workspace, error) {
	if a.Workspace == nil {
		return nil, usagef("no workspace found\n" +
			"hint: run `sicht init` in the directory holding your images, or pass --workspace")
	}

	return a.Workspace, nil
}

// requireConfig returns the parsed configuration, or a usage error.
func (a *App) requireConfig() (*config.Config, error) {
	if a.Config == nil {
		return nil, usagef("no configuration found\n" +
			"hint: run `sicht init` to write a default sicht.yaml, or pass --config")
	}

	return a.Config, nil
}
