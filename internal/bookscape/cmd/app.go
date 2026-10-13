// Package cmd implements the bookscape command-line interface.
//
// Each pipeline stage is a subcommand that reads and writes the same database, and each one is
// independently re-runnable: the slow passes can be re-run, resumed or re-scoped without
// repeating the cheap ones (docs/ARCHITECTURE.md#process-model). cmd/bookscape/main.go is a
// thin entrypoint over NewRootCommand.
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

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

// Name is the application name, used for the root command and in messages.
const Name = "bookscape"

// Exit codes. Anything the user can act on gets its own code so that a wrapper script can tell
// "you asked for something impossible" apart from "the run failed".
const (
	// ExitOK is a successful run.
	ExitOK = 0

	// ExitError is a generic failure.
	ExitError = 1

	// ExitUsage is a usage error: bad flags, missing arguments, unparsable config.
	ExitUsage = 2
)

// App is the state every subcommand shares. There is no global state; this is passed
// explicitly.
type App struct {
	// Config is the resolved configuration, available once a subcommand's RunE is entered.
	Config *config.Config

	// Log is the structured logger, at the level the --log-level flag asked for.
	Log *slog.Logger

	// Out is where human-facing output goes. A field rather than a direct os.Stdout
	// reference so that tests can capture it.
	Out io.Writer

	// ClassifyBackend overrides the model-pass backend. Left nil in production, where it is
	// built from classify.provider; set by tests, so that a paid API is not reachable from
	// the suite by accident.
	ClassifyBackend infer.Completer

	// configPath and explicitConfig record what --config resolved to, so that a missing file
	// at the default path can be tolerated while a missing file at a named path cannot.
	configPath     string
	explicitConfig bool

	logLevel string
	dbPath   string
}

// CLIOpt builds a subcommand against the shared application state.
type CLIOpt func(a *App) *cobra.Command

// commands is every subcommand, in pipeline order (cmd/bookscape/README.md).
var commands = []CLIOpt{
	// `audit` comes first because it is what you run before anything else: it reads a file
	// without importing it and says whether the file is usable and what the rules will do
	// with it.
	NewAuditCommand,
	NewImportCommand,
	NewDedupeCommand,
	NewCheckCommand,
	NewClassifyCommand,
	NewReviewCommand,
	NewStatsCommand,
	NewExportCommand,
	NewVersionCommand,
}

// NewRootCommand builds the bookscape root command.
func NewRootCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   Name,
		Short: "Bookmark archival and enrichment pipeline",
		Long: "Ingest Netscape bookmark exports, deduplicate and enrich them in SQLite, and re-emit a\n" +
			"bookmark file for import into Linkwarden.\n\n" +
			"The input format and the output format are the same format: bookscape is a filter, not a\n" +
			"converter. Each subcommand is a pipeline stage over the same database and can be re-run.",
		SilenceErrors:    true,
		SilenceUsage:     true,
		TraverseChildren: true,
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
		// PersistentPreRunE resolves configuration and logging once, before any
		// subcommand's RunE. Doing it here rather than in each subcommand keeps the flag
		// plumbing in one place.
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

	cmd.PersistentFlags().StringVarP(&a.configPath, "config", "c", config.DefaultFile,
		"Path to the bookscape.yaml configuration file")
	cmd.PersistentFlags().StringVar(&a.dbPath, "database", "",
		"Path to the working SQLite database (overrides the config file)")
	cmd.PersistentFlags().StringVar(&a.logLevel, "log-level", "info",
		"Log verbosity: debug, info, warn or error")

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

// setup resolves logging and configuration. It runs before every subcommand.
func (a *App) setup(cmd *cobra.Command) error {
	level, err := parseLevel(a.logLevel)
	if err != nil {
		return err
	}

	if a.Out == nil {
		a.Out = cmd.OutOrStdout()
	}

	// Logs go to stderr so that a subcommand's actual output stays pipeable.
	a.Log = slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

	// An explicitly named config file must exist; the default one need not, because the
	// documented defaults are a usable configuration on their own.
	a.explicitConfig = cmd.Flags().Changed("config")

	cfg, err := config.Load(a.configPath, a.explicitConfig)
	if err != nil {
		// A bad configuration file is something the user has to fix, not a run that
		// happened to fail.
		return &usageError{err: err}
	}

	if a.dbPath != "" {
		cfg.Database = a.dbPath
	}

	a.Config = cfg

	return nil
}

// openStore opens the configured database, running any pending migrations.
func (a *App) openStore(ctx context.Context) (*store.DB, error) {
	a.Log.Debug("opening database", "path", a.Config.Database)

	db, err := store.Open(ctx, a.Config.Database)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// plural renders a count with the right noun form, so that progress lines do not read
// "1 entries".
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}

	return fmt.Sprintf("%d %s", n, pluralForm)
}

// printf writes human-facing output.
func (a *App) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.Out, format, args...)
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

// Execute runs the root command and returns the process exit code. Errors are reported here,
// once, rather than by each subcommand.
func Execute(ctx context.Context, a *App) int {
	if a.Out == nil {
		a.Out = os.Stdout
	}

	root := NewRootCommand(a)
	root.SetOut(a.Out)

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Name, err)

		return ExitCodeFor(err)
	}

	return ExitOK
}

// ExitCodeFor maps an error to the process exit code it should produce.
//
// Separate from Execute so the mapping can be asserted without a test having to reach into how
// the error is printed: the codes are the part callers depend on.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}

	return ExitError
}
