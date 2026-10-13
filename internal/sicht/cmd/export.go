package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/adnoctem/pertry/internal/sicht/manifest"
	"github.com/adnoctem/pertry/internal/sicht/rules"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

var _ CLIOpt = NewExportCommand // assure type compatibility

// NewExportCommand creates the 'export' subcommand and its topics.
func NewExportCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Emit manifests or results for other tools",
		Long: "Write what sicht knows in a form something else can read.\n\n" +
			"Everything goes to stdout, so it pipes. Logs go to stderr.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
	}

	cmd.AddCommand(newExportManifestCommand(a), newExportResultsCommand(a), newExportBatchesCommand(a))

	return cmd
}

func newExportManifestCommand(a *App) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "manifest <batch-id>",
		Short: "A batch's manifest",
		Long: "Print the manifest for a batch.\n\n" +
			"JSON is the canonical form and is what `undo` reads. YAML is a rendering for\n" +
			"reading, and is not read back.\n\n" +
			"The batch id may be abbreviated to any unique prefix.",
		Args:             cobra.ExactArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runExportManifest(cmd.Context(), args[0], format)
		},
	}

	cmd.Flags().StringVar(&format, "format", "json", "json or yaml")

	return cmd
}

func newExportResultsCommand(a *App) *cobra.Command {
	var (
		format string
		where  string
	)

	cmd := &cobra.Command{
		Use:   "results",
		Short: "One row per decided file",
		Long: "Emit what has been decided, one row per file, with its path and facet values.\n\n" +
			"JSONL by default, one object per line, which streams. CSV is available for a\n" +
			"spreadsheet.\n\n" +
			"Narrow it with --where, using the same expression language as a rule.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runExportResults(cmd.Context(), format, where)
		},
	}

	cmd.Flags().StringVar(&format, "format", "jsonl", "jsonl or csv")
	cmd.Flags().StringVar(&where, "where", "", "Only include files matching this expression")

	return cmd
}

func newExportBatchesCommand(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "batches",
		Short: "Every apply and undo run",
		Long: "List the batches this workspace has recorded, newest first, with the id you would\n" +
			"pass to `sicht undo`.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runExportBatches(cmd.Context())
		},
	}

	return cmd
}

func (a *App) runExportManifest(_ context.Context, batchID, format string) error {
	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	path, err := manifest.Find(ws.ManifestsPath(), batchID)
	if err != nil {
		return &usageError{err: err}
	}

	m, err := manifest.Load(path)
	if err != nil {
		return err
	}

	switch format {
	case "json":
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")

		return encoder.Encode(m)

	case "yaml":
		// Round-tripped through JSON so that the YAML carries the same field names as the
		// canonical form, rather than Go's field names.
		raw, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("encode manifest: %w", err)
		}

		var generic any

		if err := json.Unmarshal(raw, &generic); err != nil {
			return fmt.Errorf("re-encode manifest: %w", err)
		}

		encoder := yaml.NewEncoder(a.Out)
		encoder.SetIndent(2)

		if err := encoder.Encode(generic); err != nil {
			return fmt.Errorf("write YAML: %w", err)
		}

		return encoder.Close()

	default:
		return usagef("unknown format %q, want json or yaml", format)
	}
}

// resultRow is one exported file.
type resultRow struct {
	Hash   string            `json:"hash"`
	Path   string            `json:"path"`
	Ext    string            `json:"ext"`
	Format string            `json:"format"`
	Width  int               `json:"width,omitempty"`
	Height int               `json:"height,omitempty"`
	Size   int64             `json:"size"`
	Facets map[string]string `json:"facets"`

	// Sources records which source each facet value came from, so a consumer can tell a
	// human decision from a model's guess.
	Sources map[string]string `json:"sources"`
}

func (a *App) runExportResults(ctx context.Context, format, where string) error {
	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	filter, err := rules.CompileFilter(where)
	if err != nil {
		return &usageError{err: err}
	}

	switch format {
	case "jsonl", "csv":
	default:
		return usagef("unknown format %q, want jsonl or csv", format)
	}

	return a.withStore(ctx, func(db *store.DB) error {
		var rows []resultRow

		facetNames := map[string]struct{}{}

		err := db.EachFileForRules(ctx, func(in store.RuleInput) error {
			env := rules.EnvFor(in, cfg, a.Log)

			if filter != nil {
				ok, err := filter.Match(env)
				if err != nil {
					return err
				}

				if !ok {
					return nil
				}
			}

			decisions, err := db.DecisionsForHash(ctx, in.Hash)
			if err != nil {
				return err
			}

			row := resultRow{
				Hash: in.Hash, Path: in.Path, Ext: env.Ext, Format: env.Format,
				Width: in.Width, Height: in.Height, Size: in.Size,
				Facets:  make(map[string]string, len(decisions)),
				Sources: make(map[string]string, len(decisions)),
			}

			for _, d := range decisions {
				row.Facets[d.Facet] = d.Value
				row.Sources[d.Facet] = d.Source

				facetNames[d.Facet] = struct{}{}
			}

			// JSONL streams, so a row is written as soon as it is built rather than
			// held: an export of a large archive should not need the whole thing in
			// memory.
			if format == "jsonl" {
				return writeJSONL(a.Out, row)
			}

			rows = append(rows, row)

			return nil
		})
		if err != nil {
			return err
		}

		if format == "jsonl" {
			return nil
		}

		return writeCSV(a.Out, rows, sortedSet(facetNames))
	})
}

func writeJSONL(w io.Writer, row resultRow) error {
	encoded, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode row: %w", err)
	}

	if _, err := fmt.Fprintf(w, "%s\n", encoded); err != nil {
		return fmt.Errorf("write row: %w", err)
	}

	return nil
}

// writeCSV emits a column per facet, which needs the full facet set up front - the reason CSV
// buffers where JSONL streams.
func writeCSV(w io.Writer, rows []resultRow, facets []string) error {
	writer := csv.NewWriter(w)

	header := []string{"hash", "path", "ext", "format", "width", "height", "size"}
	header = append(header, facets...)

	for _, facet := range facets {
		header = append(header, facet+"_source")
	}

	if err := writer.Write(header); err != nil {
		return fmt.Errorf("write CSV header: %w", err)
	}

	for _, row := range rows {
		record := []string{
			row.Hash, row.Path, row.Ext, row.Format,
			strconv.Itoa(row.Width), strconv.Itoa(row.Height),
			strconv.FormatInt(row.Size, 10),
		}

		for _, facet := range facets {
			record = append(record, row.Facets[facet])
		}

		for _, facet := range facets {
			record = append(record, row.Sources[facet])
		}

		if err := writer.Write(record); err != nil {
			return fmt.Errorf("write CSV row: %w", err)
		}
	}

	writer.Flush()

	if err := writer.Error(); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}

	return nil
}

func (a *App) runExportBatches(ctx context.Context) error {
	return a.withStore(ctx, func(db *store.DB) error {
		batches, err := db.Batches(ctx)
		if err != nil {
			return err
		}

		if len(batches) == 0 {
			a.printf("No batches yet. `sicht apply --execute` creates one.\n")

			return nil
		}

		a.printf("%-34s %-12s %-10s %s\n", "batch", "plan", "status", "created")

		for _, b := range batches {
			a.printf("%-34s %-12s %-10s %s\n",
				b.BatchID, b.Plan, b.Status, b.CreatedAt.Format("2006-01-02 15:04:05"))
		}

		return nil
	})
}

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
