// Package stage runs one AI stage over the files its input expression selects
// (docs/ARCHITECTURE.md#model-stages).
//
// Everything expensive happens here, so the shape of the work matters. Image preparation is done
// on worker goroutines and inference is serialised by the server anyway unless OLLAMA_NUM_PARALLEL
// is raised, which means the workers are mostly there to keep the next image decoded and scaled
// while the model is busy with the current one. That is not a micro-optimisation: measured against
// the reference archive on a translated filesystem, reading and scaling an image took long enough
// that the GPU sat idle most of the time.
package stage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/rules"
	"github.com/adnoctem/pertry/internal/sicht/schema"
	"github.com/adnoctem/pertry/internal/sicht/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

// saveBatch is how many results accumulate before being written.
//
// Small, because inference is slow: a crash or a Ctrl-C should not discard minutes of GPU time,
// and twenty rows is a negligible write next to twenty model calls.
const saveBatch = 20

// Options configures a stage run.
type Options struct {
	Workspace *core.Workspace
	DB        *store.DB
	Config    *config.Config

	// Completer is the inference backend.
	Completer infer.Completer

	// StageName selects the stage from sicht.yaml.
	StageName string

	// Limit caps how many files are processed. Zero means all of them.
	Limit int

	// Rerun processes files whose existing result is still fresh. Without it a re-run only
	// covers what the model has not already answered under the current model, prompt and
	// schema (docs/ARCHITECTURE.md#model-stages).
	Rerun bool

	// DryRun reports what would be sent without calling the model.
	DryRun bool

	// Workers bounds concurrent requests. Zero uses the configured model.workers.
	Workers int

	Log *slog.Logger

	Progress func(Progress)
}

// Progress reports how far a run has got.
type Progress struct {
	Done  int
	Total int
	Path  string

	// Elapsed is how long the run has been going, for an estimate of what remains.
	Elapsed time.Duration
}

// Result summarises a run.
type Result struct {
	Stage string

	// Selected is how many files the input expression chose.
	Selected int

	// Fresh is how many were skipped because their result is already current.
	Fresh int

	// Processed is how many were sent to the model.
	Processed int

	// Decided is how many produced a stored facet value.
	Decided int

	// Queued is how many fell below the stage's review threshold.
	Queued int

	// Undecodable is how many could not be prepared as an image at all - a real HEIC, which
	// docs/ARCHITECTURE.md#sicht-known-limitations settles is reported rather than worked around.
	Undecodable int

	// Invalid is how many responses failed schema validation even after a retry.
	Invalid int

	// Failed is how many requests errored.
	Failed int

	// NoConfidence is how many results carry no confidence score, which is expected when the
	// server does not report logprobs and is the trigger for docs/ARCHITECTURE.md#confidence's fallback discussion.
	NoConfidence int

	// Model, ModelDigest, PromptHash and SchemaHash are what this run used.
	Model       string
	ModelDigest string
	PromptHash  string
	SchemaHash  string

	// Elapsed is the wall time of the run.
	Elapsed time.Duration

	// Errors are per-file failures, already logged.
	Errors []error
}

// Run executes one stage.
func Run(ctx context.Context, opts Options) (Result, error) {
	result := Result{Stage: opts.StageName}

	if opts.Workspace == nil || opts.DB == nil || opts.Config == nil {
		return result, errors.New("stage: workspace, database and configuration are required")
	}

	if opts.Completer == nil && !opts.DryRun {
		return result, errors.New("stage: an inference backend is required unless this is a dry run")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	stage, ok := opts.Config.Stage(opts.StageName)
	if !ok {
		names := make([]string, 0, len(opts.Config.Stages))
		for _, s := range opts.Config.Stages {
			names = append(names, s.Name)
		}

		return result, fmt.Errorf("no stage named %q in sicht.yaml\nhint: the configured stages are %v",
			opts.StageName, names)
	}

	built, err := schema.Build(stage, opts.Config.Taxonomies)
	if err != nil {
		return result, err
	}

	schemaJSON, err := built.JSON()
	if err != nil {
		return result, err
	}

	var schemaMap map[string]any
	if err := json.Unmarshal(schemaJSON, &schemaMap); err != nil {
		return result, fmt.Errorf("decode generated schema: %w", err)
	}

	result.SchemaHash, err = built.Hash()
	if err != nil {
		return result, err
	}

	prompt, err := readPrompt(opts.Workspace, stage)
	if err != nil {
		return result, err
	}

	result.PromptHash = hashString(prompt)
	result.Model = opts.Config.Model.Name

	// The exact weights, where the backend can report them, so a re-run under a changed model
	// is distinguishable rather than silently conflated (docs/ARCHITECTURE.md#model-stages).
	if manager, ok := opts.Completer.(infer.ModelManager); ok && !opts.DryRun {
		if info, err := manager.ModelInfo(ctx, opts.Config.Model.Name); err == nil {
			result.ModelDigest = info.Digest
		} else {
			log.Warn("could not read the model's digest; results will record none",
				"model", opts.Config.Model.Name, "error", err)
		}
	}

	candidates, err := selectFiles(ctx, opts, stage, result, log)
	if err != nil {
		return result, err
	}

	result.Selected = candidates.selected
	result.Fresh = candidates.fresh

	log.Info("stage selected its inputs",
		"stage", stage.Name, "selected", result.Selected, "fresh", result.Fresh,
		"to_process", len(candidates.files))

	if opts.DryRun || len(candidates.files) == 0 {
		return result, nil
	}

	started := time.Now()

	runner := &runner{
		opts:    opts,
		stage:   stage,
		built:   built,
		schema:  schemaMap,
		prompt:  prompt,
		result:  &result,
		log:     log,
		started: started,
	}

	if err := runner.process(ctx, candidates.files); err != nil {
		result.Elapsed = time.Since(started)

		return result, err
	}

	result.Elapsed = time.Since(started)

	log.Info("stage finished",
		"stage", stage.Name, "processed", result.Processed, "decided", result.Decided,
		"queued", result.Queued, "failed", result.Failed,
		"elapsed", result.Elapsed.Round(time.Millisecond))

	return result, nil
}

// candidate is one file the stage will look at.
type candidate struct {
	hash string
	path string
	env  rules.Env

	// context holds the prior facet values a stage's `context:` names, resolved during
	// selection so that no query is needed per request.
	context map[string]string
}

type selection struct {
	files    []candidate
	selected int
	fresh    int
}

// selectFiles evaluates the stage's input expression over every scanned file.
func selectFiles(
	ctx context.Context,
	opts Options,
	stage config.Stage,
	result Result,
	log *slog.Logger,
) (*selection, error) {
	input, err := rules.CompileInput(stage.Input, opts.Config)
	if err != nil {
		return nil, err
	}

	known := rules.KnownFacets(opts.Config)

	// Everything the input expression might read, loaded in three queries rather than three
	// per file.
	decisions, err := opts.DB.AllDecisions(ctx)
	if err != nil {
		return nil, err
	}

	confidences, err := opts.DB.AllConfidence(ctx)
	if err != nil {
		return nil, err
	}

	out := &selection{}

	// Freshness is per decision field, and a stage with two decision fields is fresh only if
	// every one of them is.
	provenance := map[string]map[string]store.Provenance{}

	for _, field := range stage.DecisionFields() {
		p, err := opts.DB.ModelProvenance(ctx, field)
		if err != nil {
			return nil, err
		}

		provenance[field] = p
	}

	err = opts.DB.EachFileForRules(ctx, func(in store.RuleInput) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if in.Path == "" {
			return nil
		}

		env := rules.EnvFor(in, opts.Config, log)

		matched, err := input.Match(rules.InputEnv(env, rules.Facets{
			Values:     decisions[in.Hash],
			Confidence: confidences[in.Hash],
		}, known))
		if err != nil {
			return err
		}

		if !matched {
			return nil
		}

		out.selected++

		if !opts.Rerun && isFresh(stage, provenance, in.Hash, result) {
			out.fresh++

			return nil
		}

		if opts.Limit > 0 && len(out.files) >= opts.Limit {
			return nil
		}

		out.files = append(out.files, candidate{
			hash: in.Hash, path: in.Path, env: env,
			context: contextValues(decisions[in.Hash], stage.Context),
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// isFresh reports whether every decision field already has a result from the current conditions.
func isFresh(
	stage config.Stage,
	provenance map[string]map[string]store.Provenance,
	hash string,
	result Result,
) bool {
	for _, field := range stage.DecisionFields() {
		existing, ok := provenance[field][hash]
		if !ok {
			return false
		}

		if !existing.Fresh(result.Model, result.ModelDigest, result.PromptHash, result.SchemaHash) {
			return false
		}
	}

	return true
}

// readPrompt loads a stage's prompt file.
func readPrompt(ws *core.Workspace, stage config.Stage) (string, error) {
	if stage.PromptFile == "" {
		return "", fmt.Errorf("stage %q has no prompt_file", stage.Name)
	}

	// The configured path is relative to the workspace's own directory, and docs/ARCHITECTURE.md#model-stages writes it as
	// `prompts/kind.md`, so it is joined onto .sicht rather than onto the archive root.
	path := filepath.Join(ws.Dir(), filepath.FromSlash(stage.PromptFile))

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read prompt for stage %q: %w\n"+
			"hint: `sicht init` writes the default prompts; a stage's prompt_file is relative "+
			"to the .sicht directory", stage.Name, err)
	}

	return string(raw), nil
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))

	return "sha256:" + hex.EncodeToString(sum[:])
}
