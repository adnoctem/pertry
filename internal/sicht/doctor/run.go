package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

// Database is the subset of the store a check needs. An interface so that doctor does not import
// the store package - which would make the dependency run doctor -> store -> sqlite and leave the
// workspace check impossible to exercise without a real database file.
type Database interface {
	PendingMigrations(ctx context.Context) ([]string, error)
	Close() error
}

// DatabaseOpener opens the database at a path.
type DatabaseOpener func(ctx context.Context, path string) (Database, error)

// Options is what Run needs to do its work.
type Options struct {
	// Workspace is the workspace to check. May be nil when none was found, in which case the
	// workspace-dependent checks are skipped rather than guessed at.
	Workspace *core.Workspace

	// Config is the parsed sicht.yaml. May be nil for the same reason.
	Config *config.Config

	// OpenDatabase opens the workspace database. Required when Workspace is set.
	OpenDatabase DatabaseOpener

	// Backend is the inference backend to check. May be nil when it could not be built.
	Backend *ollamabackend.Backend

	// BackendErr is why Backend is nil, if it is.
	BackendErr error

	// MinDiskFree is the free space below which the disk check warns. Zero disables it.
	MinDiskFree int64
}

// Run executes every check and returns the report.
//
// The order is deliberate: the environment and the workspace first, because they are local facts
// that explain later failures, then inference, which depends on something the user runs. Checks
// whose preconditions failed are skipped rather than failed, so one unreachable server does not
// produce six failures that all mean the same thing.
func Run(ctx context.Context, opts Options) Report {
	var report Report

	root := ""
	if opts.Workspace != nil {
		root = opts.Workspace.Root()
	}

	report.Results = append(report.Results, Environment(root))
	report.Results = append(report.Results, Exiftool(ctx))

	switch {
	case opts.Workspace == nil:
		report.Results = append(report.Results,
			skip("workspace", "no workspace found; run `sicht init` or pass --workspace"))
	case opts.OpenDatabase == nil:
		report.Results = append(report.Results, skip("workspace", "no database opener supplied"))
	default:
		report.Results = append(report.Results, Workspace(ctx, opts.Workspace, opts.OpenDatabase))
		report.Results = append(report.Results, Disk(opts.Workspace.Root(), opts.MinDiskFree))
	}

	if opts.Backend == nil {
		detail := "the Ollama backend could not be configured"
		if opts.BackendErr != nil {
			detail = opts.BackendErr.Error()
		}

		report.Results = append(report.Results,
			fail("ollama", detail, "check OLLAMA_HOST"),
			skip("ollama exposure", "backend unavailable"),
			skip("inference", "backend unavailable"),
			skip("model", "backend unavailable"),
			skip("vram", "backend unavailable"),
			// The host's memory is a local fact and worth reporting even with no
			// server to ask about a model.
			MemoryHeadroom("", 0))

		return report
	}

	ollamaResult, info := Ollama(ctx, opts.Backend)
	report.Results = append(report.Results,
		ollamaResult,
		OllamaExposure(opts.Backend),
		Inference(info))

	// The model and VRAM checks need both a reachable server and a configured model name.
	switch {
	case ollamaResult.Status == StatusFail:
		report.Results = append(report.Results,
			skip("model", "Ollama is not reachable"),
			skip("vram", "Ollama is not reachable"),
			MemoryHeadroom("", 0))
	case opts.Config == nil:
		report.Results = append(report.Results,
			skip("model", "no configuration found"),
			skip("vram", "no configuration found"),
			MemoryHeadroom("", 0))
	default:
		name := opts.Config.Model.Name

		modelResult := Model(ctx, opts.Backend, name)

		report.Results = append(report.Results, modelResult, VRAM(ctx, opts.Backend, name))

		// Memory headroom is reported last and needs the model's size, which only the
		// backend knows. A model that is not pulled yet has no size to reason about, so
		// the check reports the host's memory without a verdict on fitting it.
		var size int64

		if info, err := opts.Backend.ModelInfo(ctx, name); err == nil {
			size = info.Size
		}

		report.Results = append(report.Results, MemoryHeadroom(name, size))
	}

	return report
}

// DefaultMinDiskFree is the free space below which the disk check warns.
//
// docs/ARCHITECTURE.md#doctor frames the check as "less than twice the size of planned copy operations", which is only
// computable once a plan exists. Until `apply` can supply one, a flat floor is the honest
// approximation: enough room for thumbnails and a database over a large archive.
const DefaultMinDiskFree = 2 << 30 // 2 GiB

// Disk checks free space on the workspace volume.
func Disk(root string, minFree int64) Result {
	const name = "disk"

	if minFree <= 0 {
		return skip(name, "no minimum configured")
	}

	free, err := FreeSpace(root)
	if err != nil {
		return skip(name, fmt.Sprintf("could not determine free space: %v", err))
	}

	if free < minFree {
		return warn(name, fmt.Sprintf("%s free on the workspace volume", bytes(free)),
			fmt.Sprintf("under %s. Thumbnails and the database need room, and `apply --execute` "+
				"in copy mode needs as much again as the files it copies.", bytes(minFree)))
	}

	return ok(name, fmt.Sprintf("%s free", bytes(free)))
}

// writable reports whether a directory can be written to, by writing to it. Checking the mode
// bits would be wrong on a read-only mount, and wrong again under an ACL.
func writable(dir string) error {
	file, err := os.CreateTemp(dir, ".sicht-writable-*")
	if err != nil {
		return err
	}

	name := file.Name()

	if err := file.Close(); err != nil {
		return err
	}

	return os.Remove(filepath.Clean(name))
}
