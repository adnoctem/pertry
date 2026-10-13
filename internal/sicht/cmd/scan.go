package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/meta"
	"github.com/adnoctem/pertry/internal/sicht/scan"
)

var _ CLIOpt = NewScanCommand // assure type compatibility

// NewScanCommand creates the 'scan' subcommand.
func NewScanCommand(a *App) *cobra.Command {
	var (
		root      string
		full      bool
		prune     bool
		workers   int
		batchSize int
	)

	cmd := &cobra.Command{
		Use:   "scan [directory]",
		Short: "Index files, metadata and hashes",
		Long: "Walk the workspace, hash every file with an indexed extension, read its metadata with\n" +
			"exiftool, pair it with its Google Takeout sidecar, and record all of it.\n\n" +
			"Your files are not moved, renamed or modified. Everything is written under .sicht/.\n\n" +
			"Re-running is cheap: a file whose path, size and modification time are unchanged since\n" +
			"the last scan is skipped without being read. Pass --full to re-read everything anyway.\n\n" +
			"No folder structure is assumed. The directory names in your archive are recorded as part\n" +
			"of each path and are never interpreted - a Takeout export's folder names are localized,\n" +
			"and a file's own metadata is a better claim about it than the folder it sits in.",
		Args:             cobra.MaximumNArgs(1),
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				root = args[0]
			}

			return a.runScan(cmd.Context(), scanFlags{
				root:      root,
				full:      full,
				prune:     prune,
				workers:   workers,
				batchSize: batchSize,
			})
		},
	}

	cmd.Flags().StringVar(&root, "root", "",
		"Limit the scan to this subdirectory of the workspace")
	cmd.Flags().BoolVar(&full, "full", false,
		"Re-read every file, even if it looks unchanged")
	cmd.Flags().BoolVar(&prune, "prune", false,
		"Forget files that are no longer on disk (scoped to what this run walked)")
	cmd.Flags().IntVar(&workers, "workers", 0,
		"Concurrent hashing workers (default: one per CPU)")
	cmd.Flags().IntVar(&batchSize, "batch-size", 0,
		fmt.Sprintf("Files per exiftool call and per transaction (default %d)", scan.DefaultBatchSize))

	return cmd
}

type scanFlags struct {
	root      string
	full      bool
	prune     bool
	workers   int
	batchSize int
}

func (a *App) runScan(ctx context.Context, flags scanFlags) error {
	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	// exiftool is checked here rather than left to fail mid-scan, because the failure is
	// certain and the fix is a package install: finding out after twenty minutes of hashing
	// would be a waste of the user's time.
	version, err := exiftoolVersion(ctx)
	if err != nil {
		return &notReadyError{err: fmt.Errorf("%w\nhint: run `sicht doctor`", err)}
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

	result, err := scan.Run(ctx, scan.Options{
		Workspace:     ws,
		DB:            db,
		Reader:        &meta.Exiftool{BatchSize: flags.batchSize},
		Extensions:    cfg.Extensions,
		Root:          flags.root,
		Workers:       flags.workers,
		BatchSize:     flags.batchSize,
		Full:          flags.full,
		Prune:         flags.prune,
		ExtractedWith: "exiftool " + version,
		Log:           a.Log,
		Progress:      a.scanProgress(),
	})
	if err != nil {
		// A cancelled scan has still committed everything it finished, so report what was
		// achieved rather than only the interruption.
		a.printf("\n")
		a.printScanResult(result, time.Since(started))

		return err
	}

	a.printf("\n")
	a.printScanResult(result, time.Since(started))

	if result.Failed > 0 {
		return &partialError{err: fmt.Errorf("%d file(s) could not be read", result.Failed)}
	}

	return nil
}

// scanProgress returns a progress callback that overwrites one line, or nil when output is not a
// place to draw on.
func (a *App) scanProgress() func(scan.Progress) {
	return func(p scan.Progress) {
		if p.Total == 0 {
			return
		}

		percent := p.Done * 100 / p.Total

		// \r so a long scan does not produce thousands of lines. The final newline is
		// printed by the caller.
		a.printf("\r  scanning %d/%d (%d%%)", p.Done, p.Total, percent)
	}
}

func (a *App) printScanResult(r scan.Result, elapsed time.Duration) {
	a.printf("  %-18s %d\n", "candidates", r.Candidates)
	a.printf("  %-18s %d\n", "scanned", r.Scanned)
	a.printf("  %-18s %d\n", "skipped", r.Skipped)

	if r.Failed > 0 {
		a.printf("  %-18s %d\n", "failed", r.Failed)
	}

	a.printf("  %-18s %d\n", "sidecars paired", r.Sidecars)

	if r.OrphanSidecars > 0 {
		a.printf("  %-18s %d  (a sidecar for a file whose extension is not indexed, e.g. a video)\n",
			"unclaimed sidecars", r.OrphanSidecars)
	}

	if r.NoDimensions > 0 {
		a.printf("  %-18s %d\n", "no dimensions", r.NoDimensions)
	}

	if r.NoPerceptualHash > 0 {
		a.printf("  %-18s %d  (not decodable in a pure-Go build, e.g. HEIC)\n",
			"no visual hash", r.NoPerceptualHash)
	}

	if r.Pruned > 0 {
		a.printf("  %-18s %d\n", "forgotten", r.Pruned)
	}

	a.printf("  %-18s %s\n", "elapsed", elapsed.Round(time.Millisecond))
}

// exiftoolVersion returns the installed exiftool's version string.
func exiftoolVersion(ctx context.Context) (string, error) {
	path, err := exec.LookPath("exiftool")
	if err != nil {
		return "", fmt.Errorf("exiftool is not on PATH, and metadata extraction has no fallback: %w", err)
	}

	out, err := exec.CommandContext(ctx, path, "-ver").Output()
	if err != nil {
		return "", fmt.Errorf("exiftool is installed at %s but `-ver` failed: %w", path, err)
	}

	return strings.TrimSpace(string(out)), nil
}
