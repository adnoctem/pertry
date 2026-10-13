package apply

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/adnoctem/pertry/internal/fsops"
	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/manifest"
	"github.com/adnoctem/pertry/internal/sicht/rules"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// Options configures an apply run.
type Options struct {
	Workspace *core.Workspace
	DB        *store.DB
	Config    *config.Config

	// PlanName selects the plan from sicht.yaml.
	PlanName string

	// Execute performs the operations. Without it the manifest is written with every
	// operation "planned" and nothing is touched, which is the default: docs/ARCHITECTURE.md#apply-manifests-and-undo makes the
	// dry run the thing you get unless you ask otherwise.
	Execute bool

	// Limit caps how many indexed files are planned. Companions do not count against it,
	// because moving a still without its motion file is worse than moving one more file.
	Limit int

	ToolVersion string

	Log *slog.Logger

	Progress func(Progress)
}

// Progress reports how far execution has got.
type Progress struct {
	Done  int
	Total int
	Path  string
}

// Result summarises a run.
type Result struct {
	// BatchID identifies the manifest.
	BatchID string

	// ManifestPath is where the manifest was written.
	ManifestPath string

	// Planned, Done, Skipped and Failed count operations.
	Planned int
	Done    int
	Skipped int
	Failed  int

	// Files is how many indexed files the plan selected.
	Files int

	// Companions is how many companion files travel with them.
	Companions int

	// DuplicatesLeft is how many additional locations of already-moved content were recorded
	// rather than moved (docs/ARCHITECTURE.md#apply-manifests-and-undo).
	DuplicatesLeft int

	// OrphanCompanions is how many companion-looking files have no indexed owner.
	OrphanCompanions int

	// Executed is false for a dry run.
	Executed bool

	// Errors are per-operation failures.
	Errors []error
}

// Run plans and optionally executes a batch.
func Run(ctx context.Context, opts Options) (Result, error) {
	var result Result

	if opts.Workspace == nil || opts.DB == nil || opts.Config == nil {
		return result, errors.New("apply: workspace, database and configuration are required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	plan, ok := opts.Config.Plans[opts.PlanName]
	if !ok {
		return result, fmt.Errorf("no plan named %q in sicht.yaml\nhint: the configured plans are %v",
			opts.PlanName, planNames(opts.Config))
	}

	selector, err := rules.CompileSelector(plan.Where)
	if err != nil {
		return result, err
	}

	target, err := CompileTarget(plan.Target)
	if err != nil {
		return result, err
	}

	// Every target is rendered and validated before anything moves. docs/ARCHITECTURE.md#apply-manifests-and-undo requires this: a
	// plan that would produce one invalid path must not have already moved a thousand files
	// by the time anyone finds out.
	planned, err := buildOperations(ctx, opts, plan, selector, target, log)
	if err != nil {
		return result, err
	}

	result.Files = planned.files
	result.Companions = planned.companions
	result.DuplicatesLeft = planned.duplicates
	result.OrphanCompanions = len(planned.orphans)

	batchID := manifest.NewBatchID(time.Now(), opts.PlanName, randomSuffix())
	manifestPath := path.Join(opts.Workspace.ManifestsPath(), manifest.FileName(batchID))

	result.BatchID = batchID
	result.ManifestPath = manifestPath

	status := manifest.StatusPlanned
	if opts.Execute {
		status = manifest.StatusRunning
	}

	configHash, err := hashConfig(opts.Config)
	if err != nil {
		return result, err
	}

	m := &manifest.Manifest{
		Version:           manifest.Version,
		BatchID:           batchID,
		Plan:              opts.PlanName,
		WorkspaceRootHint: opts.Workspace.Root(),
		CreatedAt:         time.Now().UTC(),
		Status:            status,
		ToolVersion:       opts.ToolVersion,
		ConfigHash:        configHash,
	}

	// The manifest is created before the first file moves, so that an apply killed between
	// the first rename and the first flush still leaves a record naming the batch.
	writer, err := manifest.Create(manifestPath, m)
	if err != nil {
		return result, err
	}

	for _, op := range planned.operations {
		writer.Add(op)
	}

	if err := writer.Flush(); err != nil {
		return result, err
	}

	if !opts.Execute {
		result.Planned = len(planned.operations)

		log.Info("plan written", "batch", batchID, "operations", result.Planned, "executed", false)

		return result, nil
	}

	result.Executed = true

	if err := execute(ctx, opts, writer, &result, log); err != nil {
		writer.SetStatus(manifest.StatusFailed)

		if flushErr := writer.Flush(); flushErr != nil {
			log.Error("could not record the failure in the manifest", "error", flushErr)
		}

		return result, err
	}

	writer.SetStatus(manifest.StatusCompleted)

	if err := writer.Flush(); err != nil {
		return result, err
	}

	if err := recordBatch(ctx, opts.DB, batchID, opts.PlanName, manifestPath, manifest.StatusCompleted); err != nil {
		return result, err
	}

	log.Info("apply finished",
		"batch", batchID, "done", result.Done, "skipped", result.Skipped, "failed", result.Failed)

	return result, nil
}

// plannedBatch is the result of evaluating a plan without touching anything.
type plannedBatch struct {
	operations []manifest.Operation

	files      int
	companions int
	duplicates int
	orphans    []Companion
}

func buildOperations(
	ctx context.Context,
	opts Options,
	plan config.Plan,
	selector *rules.Selector,
	target *Target,
	log *slog.Logger,
) (*plannedBatch, error) {
	out := &plannedBatch{}

	// Which files the plan selects, and where each goes.
	type selected struct {
		hash    string
		from    string
		to      string
		facets  map[string]string
		sources map[string]string
	}

	var chosen []selected

	indexed := indexedPaths{}

	err := opts.DB.EachFileForRules(ctx, func(in store.RuleInput) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if in.Path == "" {
			// A file with no location cannot be moved. Pruning should prevent this.
			return nil
		}

		indexed[in.Path] = struct{}{}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Companions are resolved once, from the filesystem, for every indexed file - not only
	// the selected ones, so that an orphan is identified as an orphan rather than as a
	// companion of an unselected file.
	companions, err := buildCompanionIndex(opts.Workspace, indexed)
	if err != nil {
		return nil, err
	}

	out.orphans = companions.orphans

	err = opts.DB.EachFileForRules(ctx, func(in store.RuleInput) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if in.Path == "" {
			return nil
		}

		if opts.Limit > 0 && len(chosen) >= opts.Limit {
			return nil
		}

		facets, err := opts.DB.DecisionMap(ctx, in.Hash)
		if err != nil {
			return err
		}

		env := rules.NewPlanEnv(rules.EnvFor(in, opts.Config, log), facets)

		matched, err := selector.Match(env)
		if err != nil {
			return err
		}

		if !matched {
			return nil
		}

		sources, err := facetSources(ctx, opts.DB, in.Hash)
		if err != nil {
			return err
		}

		dir, err := target.Render(facets, map[string]any{
			"ext": env.Ext, "format": env.Format, "year": env.Year,
			"camera": env.Camera, "dims": env.Dims, "name": env.Name,
		})
		if err != nil {
			return err
		}

		chosen = append(chosen, selected{
			hash:    in.Hash,
			from:    in.Path,
			to:      path.Join(dir, path.Base(in.Path)),
			facets:  facets,
			sources: sources,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	opName := manifest.OpMove
	if plan.Mode == string(fsops.ModeCopy) {
		opName = manifest.OpCopy
	}

	for _, s := range chosen {
		out.files++

		out.operations = append(out.operations, manifest.Operation{
			Hash:         s.hash,
			Op:           opName,
			From:         s.from,
			To:           s.to,
			Facets:       s.facets,
			FacetSources: s.sources,
			Status:       manifest.OpPlanned,
		})

		// docs/ARCHITECTURE.md#apply-manifests-and-undo: additional locations of the same content are recorded, not moved.
		locations, err := opts.DB.LocationsForHash(ctx, s.hash)
		if err != nil {
			return nil, err
		}

		for _, loc := range locations {
			if loc.Path == s.from {
				continue
			}

			out.duplicates++

			out.operations = append(out.operations, manifest.Operation{
				Hash:   s.hash,
				Op:     manifest.OpDuplicateLeft,
				From:   loc.Path,
				Status: manifest.OpSkipped,
			})
		}

		// docs/ARCHITECTURE.md#apply-manifests-and-undo: companions follow their owner, when the plan says so.
		if !plan.MovesCompanions() {
			continue
		}

		for _, c := range companions.byOwner[s.from] {
			out.companions++

			out.operations = append(out.operations, manifest.Operation{
				Op:     manifest.OpCompanion,
				From:   c.Path,
				To:     companionTarget(s.to, c.Path),
				Owner:  s.from,
				Status: manifest.OpPlanned,
			})
		}
	}

	for _, orphan := range out.orphans {
		out.operations = append(out.operations, manifest.Operation{
			Op:     manifest.OpCompanionOrphaned,
			From:   orphan.Path,
			Status: manifest.OpSkipped,
		})
	}

	return out, nil
}

// execute performs the planned operations, updating the manifest after each one.
func execute(ctx context.Context, opts Options, writer *manifest.Writer, result *Result, log *slog.Logger) error {
	m := writer.Manifest()

	plan := opts.Config.Plans[opts.PlanName]
	mode := fsops.Mode(plan.Mode)
	conflict := fsops.Conflict(plan.OnConflict)

	total := 0

	for _, op := range m.Operations {
		if op.Status == manifest.OpPlanned {
			total++
		}
	}

	done := 0

	// Moved records where each owner ended up, so a companion follows the path actually used
	// rather than the one planned - a conflict policy may have renamed it.
	moved := map[string]string{}

	for i, op := range m.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}

		if op.Status != manifest.OpPlanned {
			continue
		}

		// A companion follows its owner's real destination.
		if op.Op == manifest.OpCompanion {
			actual, ok := moved[op.Owner]
			if !ok {
				// The owner was skipped or failed, so the companion stays put:
				// separating a sidecar from its file is the thing this exists to
				// prevent.
				op.Status = manifest.OpSkipped
				op.Error = "owner was not moved"

				if err := writer.Update(i, op); err != nil {
					return err
				}

				result.Skipped++

				continue
			}

			op.To = companionTarget(actual, op.From)
		}

		outcome, err := fsops.Relocate(
			localPath(opts.Workspace, op.From),
			localPath(opts.Workspace, op.To),
			mode, conflict,
		)

		switch {
		case err != nil:
			op.Status = manifest.OpFailed
			op.Error = err.Error()

			result.Failed++
			result.Errors = append(result.Errors, err)

			log.Warn("operation failed", "from", op.From, "to", op.To, "error", err)

		case outcome.Skipped:
			op.Status = manifest.OpSkipped
			op.Error = "target already exists"

			result.Skipped++

		default:
			op.Status = manifest.OpDone
			op.Verified = outcome.Verified
			op.CrossVolume = outcome.CrossVolume

			// The path actually used, which a conflict policy may have changed. The
			// manifest must record where the file really is, or an undo would look in
			// the wrong place.
			if rel, relErr := opts.Workspace.Rel(outcome.To); relErr == nil {
				op.To = rel
			}

			result.Done++

			if op.Op == manifest.OpMove || op.Op == manifest.OpCopy {
				moved[op.From] = op.To
			}
		}

		if err := writer.Update(i, op); err != nil {
			return err
		}

		// Flushed after every operation, so a crash leaves a manifest that says exactly
		// which ones completed. One small write per file is the price of being able to
		// undo at all.
		if err := writer.Flush(); err != nil {
			return err
		}

		if op.Status == manifest.OpDone && op.Op == manifest.OpMove {
			if err := opts.DB.MoveLocation(ctx, op.From, op.To); err != nil {
				return err
			}
		}

		done++

		if opts.Progress != nil {
			opts.Progress(Progress{Done: done, Total: total, Path: op.From})
		}
	}

	return nil
}

func facetSources(ctx context.Context, db *store.DB, hash string) (map[string]string, error) {
	decisions, err := db.DecisionsForHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(decisions))
	for _, d := range decisions {
		out[d.Facet] = d.Source
	}

	return out, nil
}

func planNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Plans))
	for name := range cfg.Plans {
		out = append(out, name)
	}

	return out
}

// hashConfig identifies the configuration a batch came from, so a manifest produced by a
// since-edited plan is distinguishable from one produced by the current configuration.
func hashConfig(cfg *config.Config) (string, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("hash configuration: %w", err)
	}

	sum := sha256.Sum256(encoded)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// randomSuffix returns a short random string, so two batches of the same plan in the same second
// do not collide.
func randomSuffix() string {
	var buf [2]byte

	if _, err := rand.Read(buf[:]); err != nil {
		// Only reachable if the system entropy source fails, in which case the timestamp
		// alone still identifies the batch.
		return "0000"
	}

	return hex.EncodeToString(buf[:])
}

func recordBatch(ctx context.Context, db *store.DB, batchID, plan, manifestPath, status string) error {
	return db.SaveBatch(ctx, store.Batch{
		BatchID:      batchID,
		Plan:         plan,
		Status:       status,
		ManifestPath: manifestPath,
		CreatedAt:    time.Now().UTC(),
	})
}
