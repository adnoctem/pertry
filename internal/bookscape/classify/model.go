package classify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

// Item is one bookmark presented to the model. URL, title and original folder path are the
// whole input: v1 does not fetch pages (docs/ARCHITECTURE.md#classification).
type Item struct {
	ID          int64    `json:"id"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	SourcePaths []string `json:"source_paths"`
}

// Result is one classification returned by the model.
type Result struct {
	// ID echoes the Item's id, so a response can be matched back to its input without
	// relying on array order.
	ID int64 `json:"id"`

	// Description comes before Collection deliberately — see schemaFor.
	Description string `json:"description"`

	// Collection must be one of the configured set. A value outside it is rejected and
	// re-queued rather than silently creating a new collection.
	Collection string `json:"collection"`

	Tags []string `json:"tags"`

	Confidence float64 `json:"confidence"`
}

// Classifier runs the model pass over the links the rules could not decide.
type Classifier struct {
	cfg     config.Classify
	backend infer.Completer
	log     *slog.Logger
}

// NewClassifier builds a Classifier over any pkg/infer completion backend.
//
// An interface rather than a concrete client so that the orchestration here - batching,
// validation, re-queueing - is testable without a network call or an API key, and so that the
// paid pass is never reachable from a test by accident. Since the move to pkg/infer it is also
// what lets the provider be a configuration choice.
func NewClassifier(cfg config.Classify, backend infer.Completer, log *slog.Logger) *Classifier {
	return &Classifier{cfg: cfg, backend: backend, log: log}
}

// ModelStats is what a model pass produced.
type ModelStats struct {
	// Batches is how many requests were sent, including the re-queue pass.
	Batches int

	// Classified is how many bookmarks got a valid result.
	Classified int

	// Rejected is how many results named a collection outside the configured set and were
	// re-queued.
	Rejected int

	// Unanswered is how many bookmarks the model returned nothing valid for, after the
	// re-queue. They stay unclassified and are picked up by the next run.
	Unanswered int

	// LowConfidence is how many valid results fell below classify.min_confidence. These are
	// stored but do not reach the export until a human confirms them in `review`.
	LowConfidence int
}

// PromptHash is the hash persisted with every result, over the system prompt and the schema
// together. The prompt will be iterated on, and without this there is no way to tell which
// results came from which version (docs/ARCHITECTURE.md#classification).
func PromptHash(system string, schema map[string]any) string {
	encoded, err := json.Marshal(schema)
	if err != nil {
		// A schema this package builds itself always marshals; fall back rather than
		// failing a pass over a hash.
		encoded = []byte("unmarshalable")
	}

	sum := sha256.Sum256(append([]byte(system+"\x00"), encoded...))

	return hex.EncodeToString(sum[:])
}

// Run classifies targets in batches, returning the writes to persist.
//
// Results are returned rather than written here so that the caller owns the transaction and so
// that a --dry-run can stop short of the database. A batch that fails is logged and skipped:
// one bad batch out of forty should not discard the other thirty-nine, which cost tokens.
func (c *Classifier) Run(ctx context.Context, targets []store.ClassifyTarget, runID int64) ([]store.ClassificationWrite, ModelStats, error) {
	var (
		stats  ModelStats
		writes []store.ClassificationWrite
	)

	system := c.systemPrompt()
	schema := c.schemaFor()
	hash := PromptHash(system, schema)

	pending := targets
	// Two rounds: the initial pass, then one re-queue of everything that came back invalid
	// or unanswered. A second failure is left for the next run rather than retried forever.
	for round := range 2 {
		if len(pending) == 0 {
			break
		}

		if round > 0 {
			c.log.Info("re-queueing links the first pass did not resolve", "links", len(pending))
		}

		got, unresolved, err := c.round(ctx, pending, runID, hash, system, schema, &stats)

		// Accumulate before checking the error. Every batch that came back cost tokens,
		// and the caller persists whatever is returned - discarding a cancelled run's
		// completed work would make the retry pay for it twice.
		writes = append(writes, got...)

		if err != nil {
			return writes, c.finalise(writes, pending, stats), err
		}

		pending = unresolved
	}

	return writes, c.finalise(writes, pending, stats), nil
}

// finalise fills in the stats that are derived from the outcome rather than counted during it.
func (c *Classifier) finalise(writes []store.ClassificationWrite, pending []store.ClassifyTarget, stats ModelStats) ModelStats {
	stats.Classified = len(writes)
	stats.Unanswered = len(pending)

	// A result below the threshold is stored, but does not reach the export until a human
	// confirms it in `review` (docs/ARCHITECTURE.md#classification).
	for _, w := range writes {
		if w.Confidence < c.cfg.MinConfidence {
			stats.LowConfidence++
		}
	}

	return stats
}

// round sends every batch once and splits the outcome into accepted writes and links to retry.
func (c *Classifier) round(
	ctx context.Context,
	targets []store.ClassifyTarget,
	runID int64,
	promptHash, system string,
	schema map[string]any,
	stats *ModelStats,
) ([]store.ClassificationWrite, []store.ClassifyTarget, error) {
	var (
		writes     []store.ClassificationWrite
		unresolved []store.ClassifyTarget
	)

	for batch := range batches(targets, c.cfg.BatchSize) {
		if err := ctx.Err(); err != nil {
			// Stop cleanly, keeping what completed. The caller persists it.
			return writes, nil, err
		}

		byID := make(map[int64]store.ClassifyTarget, len(batch))
		items := make([]Item, 0, len(batch))

		for _, t := range batch {
			byID[t.ID] = t
			items = append(items, Item{
				ID:          t.ID,
				URL:         t.URL,
				Title:       t.Title,
				SourcePaths: t.SourcePaths,
			})
		}

		stats.Batches++

		results, err := c.classifyBatch(ctx, system, schema, items)
		if err != nil {
			if ctx.Err() != nil {
				return writes, nil, err
			}

			// One failed batch is not a failed pass. The links stay pending and the
			// next run picks them up.
			c.log.Error("batch failed", "links", len(batch), "error", err)
			unresolved = append(unresolved, batch...)

			continue
		}

		answered := make(map[int64]struct{}, len(results))

		for _, r := range results {
			target, ok := byID[r.ID]
			if !ok {
				// The model invented or echoed an id that was not in this batch.
				c.log.Warn("discarding a result for an id that was not in the batch", "id", r.ID)

				continue
			}

			// Validate even though the response was schema-constrained. Constrained
			// decoding is not a substitute for rejecting a collection outside the
			// configured set (docs/ARCHITECTURE.md#classification).
			if !c.cfg.HasCollection(r.Collection) {
				c.log.Warn("rejecting a collection outside the configured set",
					"url", target.URL, "collection", r.Collection)

				stats.Rejected++

				continue
			}

			answered[r.ID] = struct{}{}

			writes = append(writes, store.ClassificationWrite{
				BookmarkID: r.ID,
				Classification: store.Classification{
					Collection:  r.Collection,
					Description: strings.TrimSpace(r.Description),
					Confidence:  clamp01(r.Confidence),
					Source:      store.ClassifySourceModel,
					Model:       c.cfg.Model,
					RunID:       runID,
					PromptHash:  promptHash,
				},
				Tags: r.Tags,
			})
		}

		for _, t := range batch {
			if _, ok := answered[t.ID]; !ok {
				unresolved = append(unresolved, t)
			}
		}
	}

	return writes, unresolved, nil
}

// classifyBatch sends one batch through the completion backend and decodes the results.
//
// The batch is handed to the model as a JSON array under a schema that wraps the results in an
// object, because a top-level array is awkward to constrain in JSON Schema and several providers
// reject it outright.
func (c *Classifier) classifyBatch(ctx context.Context, system string, schema map[string]any, items []Item) ([]Result, error) {
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("encode batch of %d: %w", len(items), err)
	}

	resp, err := c.backend.Complete(ctx, infer.Request{
		Model:  c.cfg.Model,
		System: system,
		Prompt: "Classify these bookmarks:\n\n" + string(encoded),
		Schema: schema,
		// The model's own confidence in the collection is part of the schema rather than
		// something the backend derives, so no DecisionFields are declared here.
	})
	if err != nil {
		return nil, fmt.Errorf("classify batch of %d with %s: %w", len(items), c.cfg.Model, err)
	}

	var parsed struct {
		Results []Result `json:"results"`
	}

	if err := json.Unmarshal(resp.JSON, &parsed); err != nil {
		return nil, fmt.Errorf("decode response from %s: %w", resp.Model, err)
	}

	return parsed.Results, nil
}

// batches yields successive chunks of at most size.
func batches[T any](items []T, size int) func(func([]T) bool) {
	if size < 1 {
		size = 1
	}

	return func(yield func([]T) bool) {
		for start := 0; start < len(items); start += size {
			end := min(start+size, len(items))

			if !yield(items[start:end]) {
				return
			}
		}
	}
}

// clamp01 bounds a confidence to [0,1]. The schema says so, but a value outside it would make
// the min_confidence comparison meaningless, which is worse than quietly correcting it.
func clamp01(v float64) float64 {
	return min(max(v, 0), 1)
}

// systemPrompt builds the instruction carrying the closed collection list.
//
// The list goes in the prompt as well as in the schema enum so that the model assigns from it
// rather than inventing: the enum constrains what it can emit, and the prompt explains what the
// options mean.
func (c *Classifier) systemPrompt() string {
	var b strings.Builder

	b.WriteString(`You are classifying bookmarks salvaged from roughly a decade of browser history, for import into a personal link archive.

For each bookmark you are given its URL, its title as the browser recorded it, and the folder path or paths the user originally filed it under. You cannot fetch the page; classify from these signals alone.

For each bookmark, return:

- "description": one sentence saying what the page actually is. This is the most valuable field you produce. Titles in a collection this old are frequently useless - "Home", "untitled", a bare repository name, "(1) LinkedIn" - and a plain-language summary is what makes the archive searchable. Describe the page, do not editorialise, and do not repeat the title verbatim when the title already says it.
- "collection": exactly one of the collections listed below. This is the single "where does this belong" axis.
- "tags": a few short lowercase keywords. Topic, technology, format - whatever would help find this again. Prefer reusing obvious tags across bookmarks over inventing a unique one for each.
- "confidence": how sure you are of the collection, from 0 to 1. Be honest: a URL whose purpose you genuinely cannot tell from these signals should score low, and will be routed to a human instead of being trusted.

The original folder path is often a better signal than the domain - the user put it there on purpose. Where the folder and the domain disagree, say so in the description.

The collections, which are a closed set:

`)

	for _, name := range c.cfg.Collections {
		fmt.Fprintf(&b, "- %s\n", name)
	}

	b.WriteString(`
Use exactly these names. If a bookmark fits none of them well, choose the closest and lower your confidence rather than inventing a collection.

Return one result per bookmark, echoing its "id" so each result can be matched to its input.`)

	return b.String()
}

// schemaFor builds the JSON schema the response is constrained to.
//
// Property order is deliberate: "description" comes before "collection", and "confidence"
// comes last. Generation is autoregressive, so a decision made after the model has written
// down what it thinks the page is, is conditioned on that observation - and a confidence
// stated after the decision is a judgement about a decision already made. The ClassifyResult
// struct in docs/ARCHITECTURE.md#classification lists the fields collection-first; the field
// set here is the same, only the order differs. The reasoning is the one docs/ARCHITECTURE.md
// docs/ARCHITECTURE.md#url-normalisation-and-deduplication sets out for the same situation.
func (c *Classifier) schemaFor() map[string]any {
	collections := make([]any, 0, len(c.cfg.Collections))
	for _, name := range c.cfg.Collections {
		collections = append(collections, name)
	}

	result := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":          map[string]any{"type": "integer"},
			"description": map[string]any{"type": "string", "maxLength": 300},
			"collection":  map[string]any{"type": "string", "enum": collections},
			"tags": map[string]any{
				"type":     "array",
				"maxItems": 8,
				"items":    map[string]any{"type": "string", "maxLength": 40},
			},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		"required":             []any{"id", "description", "collection", "tags", "confidence"},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"results": map[string]any{"type": "array", "items": result},
		},
		"required":             []any{"results"},
		"additionalProperties": false,
	}
}

// ErrNoCredentials is returned when the model pass is asked for without an API key.
var ErrNoCredentials = errors.New("no Anthropic credentials found")
