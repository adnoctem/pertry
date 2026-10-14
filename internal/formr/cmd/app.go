// Package cmd implements the formr command-line interface.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/formr/analysis"
	"github.com/adnoctem/pertry/internal/formr/diag"
	"github.com/adnoctem/pertry/internal/formr/module"
	"github.com/adnoctem/pertry/internal/formr/report"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

// Name is the application name.
const Name = "formr"

// Exit codes. Narrower than sicht's on purpose: a CI job needs to tell "the module's coverage
// changed or fell short" (1) from "formr could not do its job" (2), and nothing more.
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitError  = 2
)

// App is the state every subcommand shares.
type App struct {
	Out io.Writer
	Err io.Writer
	Log *slog.Logger

	// Runner overrides the engine runner; tests use it to record what formr asks the engine to do.
	Runner schema.Runner

	logLevel string
	noColor  bool
}

// CLIOpt builds a subcommand against the shared state.
type CLIOpt func(a *App) *cobra.Command

// commands is every subcommand, in the order help lists them.
var commands = []CLIOpt{
	NewSchemaCommand,
	NewCheckCommand,
	NewSnapshotCommand,
	NewVersionCommand,
}

// failure marks a run that completed and found the module wanting: drift or a policy failure.
type failure struct{ msg string }

func (f *failure) Error() string { return f.msg }

// NewRootCommand builds the formr root command.
func NewRootCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   Name,
		Short: "Terraform module provider-schema coverage",
		Long: "Report how completely a Terraform module exposes the providers it uses: every field of\n" +
			"every provider configuration, resource and data source in the module tree, and what the\n" +
			"module does with it.\n\n" +
			"formr reads the module's HCL and its providers' schemas. It never configures a provider,\n" +
			"plans or applies, and it never prints a value from the module.",
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.setup(cmd)
		},
	}

	cmd.PersistentFlags().StringVar(&a.logLevel, "log-level", "info", "Log verbosity: debug, info, warn or error")
	cmd.PersistentFlags().BoolVar(&a.noColor, "no-color", false, "Disable coloured output")

	for _, c := range commands {
		cmd.AddCommand(c(a))
	}

	return cmd
}

func (a *App) setup(cmd *cobra.Command) error {
	if a.Out == nil {
		a.Out = cmd.OutOrStdout()
	}
	if a.Err == nil {
		a.Err = cmd.ErrOrStderr()
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(a.logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: use debug, info, warn or error", a.logLevel)
	}
	a.Log = slog.New(slog.NewTextHandler(a.Err, &slog.HandlerOptions{Level: level}))

	return nil
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, a *App, args []string) int {
	root := NewRootCommand(a)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	errOut := a.Err
	if errOut == nil {
		errOut = os.Stderr
	}
	fmt.Fprintf(errOut, "%s: %v\n", Name, err)

	var f *failure
	if errors.As(err, &f) {
		return ExitFailed
	}

	return ExitError
}

// sourceFlags are the flags every command that needs schemas shares.
type sourceFlags struct {
	init       bool
	engine     string
	schemaFile string
	timeout    time.Duration
}

func (s *sourceFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&s.init, "init", false, "Run `init -backend=false -input=false` first (may download providers)")
	cmd.Flags().StringVar(&s.engine, "engine", "terraform", "Engine executable: terraform, tofu, or a path to either")
	cmd.Flags().StringVar(&s.schemaFile, "schema-file", "", "Read schemas from a snapshot instead of running the engine")
	cmd.Flags().DurationVar(&s.timeout, "timeout", 5*time.Minute, "Timeout for each engine command")
}

// acquired is everything a command needs to render a report.
type acquired struct {
	raw      []byte
	set      *schema.Set
	prov     schema.Provenance
	warnings []string
}

func (a *App) acquire(ctx context.Context, dir string, s sourceFlags) (*acquired, error) {
	if s.schemaFile != "" {
		if s.init {
			return nil, errors.New("--init and --schema-file are mutually exclusive: a snapshot needs no engine")
		}
		data, err := os.ReadFile(s.schemaFile)
		if err != nil {
			return nil, err
		}
		raw, prov, warnings, err := schema.ReadSnapshot(data)
		if err != nil {
			return nil, err
		}
		set, err := schema.Decode(raw)
		if err != nil {
			return nil, err
		}

		return &acquired{raw: raw, set: set, prov: prov, warnings: warnings}, nil
	}

	runner := a.Runner
	if runner == nil {
		runner = schema.ExecRunner{Path: s.engine}
	}
	eng := schema.Engine{Runner: runner, Timeout: s.timeout}

	info, err := eng.Info(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	if s.init {
		a.Log.Info("initialising", "engine", info.Name, "dir", dir)
		if err := eng.Init(ctx, dir); err != nil {
			return nil, fmt.Errorf("init: %w", err)
		}
	}
	raw, err := eng.ProvidersSchema(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("providers schema: %w", err)
	}
	set, err := schema.Decode(raw)
	if err != nil {
		return nil, err
	}
	locked, err := schema.ReadLock(filepath.Join(dir, schema.LockFile))
	if err != nil {
		return nil, err
	}

	// Logged rather than written into the report: the provenance table already says "unknown"
	// for each release, and a report line here would make the same schemas render differently
	// from a snapshot than from the engine.
	if len(locked) == 0 {
		a.Log.Warn("no dependency lock file: provider releases are unknown", "file", schema.LockFile)
	}

	return &acquired{raw: raw, set: set, prov: schema.Provenance{Engine: info, Providers: locked}}, nil
}

// build loads the module tree and renders its report.
func (a *App) build(dir string, acq *acquired) (string, *analysis.Result, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("%s is not a directory", dir)
	}

	tree, diags := module.LoadTree(dir)
	if tree == nil {
		return "", nil, errors.New(diags.Sorted()[0].Summary)
	}
	res := analysis.Analyze(tree, acq.set)
	res.Diagnostics = append(diags, res.Diagnostics...)

	a.printDiagnostics(res.Diagnostics)

	return report.Render(report.Input{
		Analysis:      res,
		Provenance:    acq.prov,
		SchemaFormat:  acq.set.FormatVersion,
		ExtraWarnings: acq.warnings,
	}), res, nil
}

// printDiagnostics writes every diagnostic with its source range, which the report omits.
func (a *App) printDiagnostics(diags diag.Diagnostics) {
	for _, d := range diags.Sorted() {
		where := d.Module
		if d.Subject != nil {
			where = fmt.Sprintf("%s:%d,%d", d.Subject.Filename, d.Subject.Start.Line, d.Subject.Start.Column)
		}
		msg := d.Summary
		if d.Detail != "" {
			msg += ": " + strings.TrimSpace(d.Detail)
		}
		fmt.Fprintf(a.Err, "%s: %s: %s\n", d.Severity, where, msg)
	}
}

func singleDir(args []string) string {
	if len(args) == 0 {
		return "."
	}

	return args[0]
}
