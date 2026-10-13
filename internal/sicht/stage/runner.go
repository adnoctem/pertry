package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/confidence"
	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/imageprep"
	"github.com/adnoctem/pertry/internal/sicht/schema"
	"github.com/adnoctem/pertry/internal/sicht/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

// runner holds what every request needs, so the per-file path takes one argument.
type runner struct {
	opts    Options
	stage   config.Stage
	built   *schema.Schema
	schema  map[string]any
	prompt  string
	result  *Result
	log     *slog.Logger
	started time.Time

	// mu guards result and pending, which the workers write to.
	mu      sync.Mutex
	pending []store.Result
	done    int
}

// process runs the stage over the selected files.
//
// Workers prepare images and issue requests concurrently. Inference is serialised by the server
// unless OLLAMA_NUM_PARALLEL is raised, so the concurrency is mostly there to keep the next image
// decoded and scaled while the model works on the current one - which measurably matters when the
// archive is on a translated filesystem and reading a file is slower than classifying it.
func (r *runner) process(ctx context.Context, files []candidate) error {
	workers := r.opts.Workers
	if workers <= 0 {
		workers = r.opts.Config.Model.Workers
	}

	if workers <= 0 {
		workers = min(2, runtime.NumCPU())
	}

	queue := make(chan candidate)

	var (
		wg       sync.WaitGroup
		firstErr error
		errOnce  sync.Once
	)

	// A cancellable context for the workers, so that a fatal error stops the others rather
	// than letting a doomed run grind through ten thousand images.
	group, cancel := context.WithCancel(ctx)
	defer cancel()

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for c := range queue {
				if err := r.one(group, c); err != nil {
					errOnce.Do(func() {
						firstErr = err

						cancel()
					})

					return
				}
			}
		}()
	}

	total := len(files)

feed:
	for _, c := range files {
		select {
		case queue <- c:
		case <-group.Done():
			break feed
		}
	}

	close(queue)
	wg.Wait()

	// Whatever the outcome, the work already done is written: inference is expensive enough
	// that discarding a partial run would be its own bug.
	if err := r.flush(ctx); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}

	if firstErr != nil {
		return firstErr
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	_ = total

	return nil
}

// one processes a single file. A per-file failure is recorded and does not stop the run; only an
// error that would affect every file is returned.
func (r *runner) one(ctx context.Context, c candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	prepared, err := imageprep.Prepare(r.opts.Workspace.Abs(c.path), imageprep.Options{
		MaxEdge: r.opts.Config.Model.MaxImageEdge,
		Tall:    imageprep.TallStrategy(r.opts.Config.Model.TallStrategy),
	})
	if err != nil {
		r.record(func(res *Result) {
			if errors.Is(err, imageprep.ErrUndecodable) {
				res.Undecodable++
			} else {
				res.Failed++
				res.Errors = append(res.Errors, err)
			}
		})

		r.log.Debug("skipping a file that cannot be prepared", "path", c.path, "error", err)

		r.advance(c.path, 0)

		//nolint:nilerr // Counted in the result and logged above. One undecodable image
		// must not abort a run over the rest of the archive.
		return nil
	}

	req := infer.Request{
		Model:          r.opts.Config.Model.Name,
		System:         r.prompt,
		Prompt:         r.userPrompt(c),
		Images:         prepared.Images,
		Schema:         r.schema,
		DecisionFields: r.stage.DecisionFields(),
		// Passed through verbatim, as docs/ARCHITECTURE.md#model-stages requires: temperature 0 and a fixed seed are
		// what make a re-run reproducible, which is what lets a prompt change be
		// attributed to the prompt.
		Options:   r.opts.Config.Model.Options,
		KeepAlive: r.opts.Config.Model.KeepAlive,
		// docs/ARCHITECTURE.md#confidence asks for five alternatives at the decision's first token, which is what
		// lets the review UI offer "did you mean" rather than only "this looks uncertain".
		Logprobs:    true,
		TopLogprobs: 5,
		Think:       r.opts.Config.Model.Think,
	}

	// One retry on a response that does not validate, as docs/ARCHITECTURE.md#inference-backend-and-model-choice requires. A second failure is
	// recorded rather than retried again: if constrained decoding produced something invalid
	// twice, a third attempt is unlikely to differ and the budget is better spent elsewhere.
	payload, resp, err := r.completeAndValidate(ctx, req, 2)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		r.record(func(res *Result) {
			if errors.Is(err, errInvalid) {
				res.Invalid++
			} else {
				res.Failed++
			}

			res.Errors = append(res.Errors, fmt.Errorf("%s: %w", c.path, err))
		})

		r.log.Warn("stage request failed", "path", c.path, "error", err)

		r.advance(c.path, 0)

		return nil
	}

	results := r.resultsFor(c, payload, resp)

	r.mu.Lock()
	r.pending = append(r.pending, results...)
	needFlush := len(r.pending) >= saveBatch
	r.mu.Unlock()

	if needFlush {
		if err := r.flush(ctx); err != nil {
			return err
		}
	}

	r.advance(c.path, 1)

	return nil
}

// errInvalid marks a response that failed schema validation, so the caller can count it apart
// from a transport failure.
var errInvalid = errors.New("response did not match the schema")

func (r *runner) completeAndValidate(
	ctx context.Context,
	req infer.Request,
	attempts int,
) (map[string]any, infer.Response, error) {
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := r.opts.Completer.Complete(ctx, req)

		switch {
		case errors.Is(err, infer.ErrEmptyResponse) && attempt < attempts:
			// Retryable, and distinct from a transport failure: the request succeeded
			// and the model spent its whole context reasoning. A second attempt often
			// lands, and discarding the file on the first would lose an answer that was
			// available.
			lastErr = err

			r.log.Debug("empty response, retrying", "attempt", attempt, "error", err)

			continue

		case err != nil:
			return nil, infer.Response{}, err
		}

		payload, err := schema.ValidateJSON(r.built, resp.JSON)
		if err == nil {
			r.record(func(res *Result) { res.Processed++ })

			return payload, resp, nil
		}

		lastErr = err

		r.log.Debug("response failed validation", "attempt", attempt, "error", err)
	}

	r.record(func(res *Result) { res.Processed++ })

	return nil, infer.Response{}, fmt.Errorf("%w: %w", errInvalid, lastErr)
}

// userPrompt is the per-file turn. The system prompt carries the instructions; this carries the
// context docs/ARCHITECTURE.md#model-stages's `context:` names, so that a later stage can see what an earlier one decided.
func (r *runner) userPrompt(c candidate) string {
	if len(r.stage.Context) == 0 {
		return "Classify this image."
	}

	// Context values are looked up from the environment the selection already built, so no
	// further queries are needed here.
	var lines []string

	for _, facet := range r.stage.Context {
		if value := r.contextValue(c, facet); value != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", facet, value))
		}
	}

	if len(lines) == 0 {
		return "Classify this image."
	}

	return "What is already known about this image:\n" + joinLines(lines) +
		"\n\nClassify this image."
}

func (r *runner) contextValue(c candidate, facet string) string {
	return c.context[facet]
}

func joinLines(lines []string) string {
	var out strings.Builder

	for i, l := range lines {
		if i > 0 {
			out.WriteString("\n")
		}

		out.WriteString("  ")
		out.WriteString(l)
	}

	return out.String()
}

// resultsFor turns a validated response into the rows to store.
//
// Decision fields become facet values; everything else is kept as the stage's payload, which docs/ARCHITECTURE.md#model-stages
// calls for so that an observation the model made is available for display and search even though
// nothing files by it.
func (r *runner) resultsFor(c candidate, payload map[string]any, resp infer.Response) []store.Result {
	now := time.Now().UTC()

	observations := map[string]any{}

	decisionNames := map[string]struct{}{}
	for _, name := range r.stage.DecisionFields() {
		decisionNames[name] = struct{}{}
		decisionNames[schema.OtherFieldFor(name)] = struct{}{}
	}

	for name, value := range payload {
		if _, isDecision := decisionNames[name]; !isDecision {
			observations[name] = value
		}
	}

	payloadJSON := ""

	if len(observations) > 0 {
		if encoded, err := json.Marshal(observations); err == nil {
			payloadJSON = string(encoded)
		}
	}

	var out []store.Result

	for _, field := range r.stage.DecisionFields() {
		// Only a field with an escape hatch can have used it. Elsewhere "other" is just a value.
		value, uncurated := schema.ResolveValue(field, payload)
		if !schema.HasEscapeHatch(r.stage, field) {
			value, _ = payload[field].(string)
			uncurated = false
		}
		if value == "" {
			continue
		}

		row := store.Result{
			Hash:        c.hash,
			Facet:       field,
			Value:       value,
			Source:      store.ModelSource(r.result.Model),
			Stage:       r.stage.Name,
			Model:       r.result.Model,
			ModelDigest: r.result.ModelDigest,
			PromptHash:  r.result.PromptHash,
			SchemaHash:  r.result.SchemaHash,
			PayloadJSON: payloadJSON,
			CreatedAt:   now,
		}

		score, err := confidence.Derive(resp, confidence.Options{
			Facet:   field,
			Allowed: r.allowedValues(field),
		})
		if err != nil {
			r.log.Debug("could not derive confidence", "path", c.path, "facet", field, "error", err)
		}

		if score.Derived {
			row.Confidence = score.Confidence
			row.HasConfidence = true

			if len(score.Alternatives) > 0 {
				if encoded, err := json.Marshal(score.Alternatives); err == nil {
					row.AlternativesJSON = string(encoded)
				}
			}
		}

		threshold := r.stage.ReviewBelow
		if threshold <= 0 {
			threshold = r.opts.Config.Model.ReviewBelow
		}

		r.record(func(res *Result) {
			res.Decided++

			if !score.Derived {
				res.NoConfidence++
			}

			// A value the model had to invent is reviewed whatever its confidence, and
			// whether or not a confidence could be derived at all: it means the curated
			// taxonomy does not cover this file, which is a fact about the taxonomy
			// rather than about the model's certainty. Checked before the score, because
			// an early return on a missing score used to skip it entirely.
			if uncurated {
				res.Queued++

				return
			}

			if score.Derived && score.Confidence < threshold {
				res.Queued++
			}
		})

		out = append(out, row)
	}

	return out
}

// allowedValues returns the legal value set for a decision field, which is what makes the
// confidence score renormalisable (docs/ARCHITECTURE.md#confidence). Empty for a free-text decision.
func (r *runner) allowedValues(field string) []string {
	for _, f := range r.stage.Fields {
		if f.Name != field {
			continue
		}

		if len(f.Values) > 0 {
			values := slices.Clone(f.Values)

			if r.stage.Mode == config.ModeClosed && !slices.Contains(values, schema.OtherValue) {
				values = append(values, schema.OtherValue)
			}

			return values
		}

		if f.Taxonomy != "" && r.stage.Mode == config.ModeClosed {
			values := slices.Clone(r.opts.Config.Taxonomies[f.Taxonomy])

			if !slices.Contains(values, schema.OtherValue) {
				values = append(values, schema.OtherValue)
			}

			return values
		}

		// Open mode: the decision is free text, so there is nothing to renormalise over.
		return nil
	}

	return nil
}

// flush writes the pending results.
func (r *runner) flush(ctx context.Context) error {
	r.mu.Lock()
	batch := r.pending
	r.pending = nil
	r.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	// A background context for the write, so that a cancelled run still records the
	// inference it already paid for. Bounded, so a hung database cannot keep the process
	// alive after a Ctrl-C.
	writeCtx := ctx

	if ctx.Err() != nil {
		var cancel context.CancelFunc

		writeCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}

	if err := r.opts.DB.SaveResults(writeCtx, batch); err != nil {
		return err
	}

	return nil
}

// record applies a mutation to the shared result under the lock.
func (r *runner) record(fn func(*Result)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fn(r.result)
}

// advance reports progress.
func (r *runner) advance(path string, delta int) {
	r.mu.Lock()
	r.done += max(delta, 0)
	done := r.done
	r.mu.Unlock()

	if r.opts.Progress == nil {
		return
	}

	r.opts.Progress(Progress{
		Done: done, Path: path, Elapsed: time.Since(r.started),
	})
}

// contextValues is the facet values a stage's `context:` makes available, resolved during
// selection so that no query happens per request.
func contextValues(facets map[string]string, wanted []string) map[string]string {
	if len(wanted) == 0 {
		return nil
	}

	out := make(map[string]string, len(wanted))

	for _, facet := range wanted {
		if value, ok := facets[facet]; ok {
			out[facet] = value
		}
	}

	return out
}
