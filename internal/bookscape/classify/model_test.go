package classify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/classify"
	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// backend wraps infer.Fake so a test can answer in terms of classify.Result rather than raw JSON,
// while still going through the real wire format: the items are encoded into the prompt and the
// results come back as the {"results": …} envelope the schema describes.
//
// No test here is allowed to reach a real API. The model pass costs money, and a test that could
// spend it by accident is a trap.
type backend struct {
	fake  *infer.Fake
	calls int
}

// newBackend answers every request with the given function, which sees the decoded items.
func newBackend(answer func(items []classify.Item) ([]classify.Result, error)) *backend {
	b := &backend{}

	b.fake = &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		b.calls++

		items, err := itemsIn(req)
		if err != nil {
			return infer.Response{}, err
		}

		results, err := answer(items)
		if err != nil {
			return infer.Response{}, err
		}

		encoded, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			return infer.Response{}, err
		}

		return infer.Response{JSON: encoded, Model: req.Model}, nil
	}}

	return b
}

// Name and Complete make *backend an infer.Completer by delegating to the embedded fake.
func (b *backend) Name() string { return b.fake.Name() }

func (b *backend) Complete(ctx context.Context, req infer.Request) (infer.Response, error) {
	return b.fake.Complete(ctx, req)
}

// requests returns every request the backend received.
func (b *backend) requests() []infer.Request { return b.fake.Requests() }

// batchSizes is how many items each request carried, in order.
func (b *backend) batchSizes(t *testing.T) []int {
	t.Helper()

	var out []int

	for _, req := range b.requests() {
		items, err := itemsIn(req)
		if err != nil {
			t.Fatalf("decode the items out of a request: %v", err)
		}

		out = append(out, len(items))
	}

	return out
}

// itemsIn decodes the batch out of a request's prompt. Parsing the prompt rather than inspecting a
// struct field is deliberate: it asserts on what actually goes over the wire.
func itemsIn(req infer.Request) ([]classify.Item, error) {
	_, encoded, ok := strings.Cut(req.Prompt, "\n\n")
	if !ok {
		return nil, fmt.Errorf("prompt has no JSON payload: %.80s", req.Prompt)
	}

	var items []classify.Item
	if err := json.Unmarshal([]byte(encoded), &items); err != nil {
		return nil, fmt.Errorf("decode items from the prompt: %w", err)
	}

	return items, nil
}

// echoAll answers every item with a valid result.
func echoAll(collection string, confidence float64) func([]classify.Item) ([]classify.Result, error) {
	return func(items []classify.Item) ([]classify.Result, error) {
		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			results = append(results, classify.Result{
				ID:          item.ID,
				Collection:  collection,
				Description: "a description of " + item.URL,
				Tags:        []string{"tagged"},
				Confidence:  confidence,
			})
		}

		return results, nil
	}
}

func targets(n int) []store.ClassifyTarget {
	out := make([]store.ClassifyTarget, 0, n)
	for i := range n {
		out = append(out, store.ClassifyTarget{
			ID:    int64(i + 1),
			URL:   "https://example.test/" + string(rune('a'+i%26)),
			Title: "title",
		})
	}

	return out
}

func testClassify() config.Classify {
	cfg := config.Default().Classify
	cfg.BatchSize = 3

	return cfg
}

func TestRunClassifiesEveryTarget(t *testing.T) {
	t.Parallel()

	backend := newBackend(echoAll("Development", 0.9))

	writes, stats, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(7), 42)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if len(writes) != 7 {
		t.Errorf("writes = %d, want 7", len(writes))
	}

	// 7 targets at a batch size of 3 is 3 batches, and nothing to re-queue.
	if stats.Batches != 3 {
		t.Errorf("Stats.Batches = %d, want 3", stats.Batches)
	}

	if stats.Unanswered != 0 || stats.Rejected != 0 {
		t.Errorf("Stats = %+v, want nothing unanswered or rejected", stats)
	}

	t.Run("provenance is recorded on every write", func(t *testing.T) {
		for _, w := range writes {
			if w.Source != store.ClassifySourceModel {
				t.Errorf("Source = %q, want %q", w.Source, store.ClassifySourceModel)
			}

			if w.Model != testClassify().Model {
				t.Errorf("Model = %q, want %q", w.Model, testClassify().Model)
			}

			if w.RunID != 42 {
				t.Errorf("RunID = %d, want 42", w.RunID)
			}

			// Without the prompt hash there is no way to tell which results came
			// from which version of the prompt.
			if w.PromptHash == "" {
				t.Error("PromptHash is empty")
			}
		}
	})
}

// TestRunRespectsTheBatchSize matters because single-link requests are wasteful and produce
// worse results than batches, which give the model comparative context.
func TestRunRespectsTheBatchSize(t *testing.T) {
	t.Parallel()

	cfg := testClassify()
	cfg.BatchSize = 4

	backend := newBackend(echoAll("Reference", 0.8))

	if _, _, err := classify.NewClassifier(cfg, backend, discardLogger()).
		Run(context.Background(), targets(10), 1); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if diff := cmp.Diff([]int{4, 4, 2}, backend.batchSizes(t)); diff != "" {
		t.Errorf("batch sizes mismatch (-want +got):\n%s", diff)
	}
}

// TestRunRejectsACollectionOutsideTheSet is the validation section 5.5 insists on. Constrained
// decoding is not a substitute for it: the whole point of a closed set is that a value outside
// it never becomes a collection.
func TestRunRejectsACollectionOutsideTheSet(t *testing.T) {
	t.Parallel()

	var round int

	backend := newBackend(func(items []classify.Item) ([]classify.Result, error) {
		round++

		// First call invents a collection; the re-queue answers properly.
		collection := "Invented"
		if round > 1 {
			collection = "Archive"
		}

		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			results = append(results, classify.Result{
				ID: item.ID, Collection: collection, Confidence: 0.9,
			})
		}

		return results, nil
	})

	writes, stats, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(2), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if stats.Rejected != 2 {
		t.Errorf("Stats.Rejected = %d, want 2", stats.Rejected)
	}

	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2 after the re-queue", len(writes))
	}

	for _, w := range writes {
		if w.Collection != "Archive" {
			t.Errorf("Collection = %q, want %q", w.Collection, "Archive")
		}
	}
}

// TestRunGivesUpAfterOneRequeue stops a persistently wrong model from looping forever. The
// links stay unclassified and the next run picks them up.
func TestRunGivesUpAfterOneRequeue(t *testing.T) {
	t.Parallel()

	backend := newBackend(echoAll("Still invented", 0.9))

	writes, stats, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(2), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if len(writes) != 0 {
		t.Errorf("writes = %d, want 0", len(writes))
	}

	if stats.Unanswered != 2 {
		t.Errorf("Stats.Unanswered = %d, want 2", stats.Unanswered)
	}

	// One initial batch plus one re-queue, and then it stops.
	if backend.calls != 2 {
		t.Errorf("backend calls = %d, want 2", backend.calls)
	}
}

// TestRunSurvivesAFailedBatch is why a pass is not all-or-nothing: one bad batch out of forty
// should not discard the other thirty-nine, which cost tokens.
func TestRunSurvivesAFailedBatch(t *testing.T) {
	t.Parallel()

	var calls int

	backend := newBackend(func(items []classify.Item) ([]classify.Result, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("503 from the API")
		}

		return echoAll("Media", 0.7)(items)
	})

	writes, stats, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(6), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil; a failed batch is not a failed pass", err)
	}

	// 6 targets, batch size 3: batch one fails, batch two succeeds, then the three failed
	// links are re-queued and succeed.
	if len(writes) != 6 {
		t.Errorf("writes = %d, want 6", len(writes))
	}

	if stats.Unanswered != 0 {
		t.Errorf("Stats.Unanswered = %d, want 0", stats.Unanswered)
	}
}

// TestRunDiscardsUnknownIDs guards against a response that invents an id, which would otherwise
// write a classification onto an unrelated bookmark.
func TestRunDiscardsUnknownIDs(t *testing.T) {
	t.Parallel()

	backend := newBackend(func([]classify.Item) ([]classify.Result, error) {
		return []classify.Result{
			{ID: 999, Collection: "Development", Confidence: 0.9},
		}, nil
	})

	writes, stats, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(1), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if len(writes) != 0 {
		t.Errorf("writes = %d, want 0; a result for an id outside the batch must be discarded", len(writes))
	}

	if stats.Unanswered != 1 {
		t.Errorf("Stats.Unanswered = %d, want 1", stats.Unanswered)
	}
}

// TestRunCountsLowConfidence is what routes a result to review instead of to export.
func TestRunCountsLowConfidence(t *testing.T) {
	t.Parallel()

	cfg := testClassify()
	cfg.MinConfidence = 0.6

	backend := newBackend(func(items []classify.Item) ([]classify.Result, error) {
		results := make([]classify.Result, 0, len(items))
		for _, item := range items {
			// Keyed on the id, not the position in the batch, so the expectation
			// does not depend on where the batch boundaries happen to fall.
			confidence := 0.95
			if item.ID%2 == 0 {
				confidence = 0.3
			}

			results = append(results, classify.Result{
				ID: item.ID, Collection: "Archive", Confidence: confidence,
			})
		}

		return results, nil
	})

	_, stats, err := classify.NewClassifier(cfg, backend, discardLogger()).
		Run(context.Background(), targets(4), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if stats.LowConfidence != 2 {
		t.Errorf("Stats.LowConfidence = %d, want 2", stats.LowConfidence)
	}
}

func TestRunClampsConfidence(t *testing.T) {
	t.Parallel()

	backend := newBackend(func(items []classify.Item) ([]classify.Result, error) {
		return []classify.Result{
			{ID: items[0].ID, Collection: "Archive", Confidence: 7},
			{ID: items[1].ID, Collection: "Archive", Confidence: -3},
		}, nil
	})

	writes, _, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(2), 1)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	for _, w := range writes {
		if w.Confidence < 0 || w.Confidence > 1 {
			t.Errorf("Confidence = %v, want it clamped to [0,1]", w.Confidence)
		}
	}
}

// TestRunStopsOnCancellation keeps an interrupted pass's completed work: the caller persists
// whatever Run returns.
func TestRunStopsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int

	backend := newBackend(func(items []classify.Item) ([]classify.Result, error) {
		calls++
		if calls == 2 {
			cancel()
		}

		return echoAll("Development", 0.9)(items)
	})

	writes, _, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(ctx, targets(30), 1)
	if err == nil {
		t.Fatal("Run() error = nil, want a cancellation error")
	}

	if len(writes) == 0 {
		t.Error("writes is empty, want the results that completed before cancellation")
	}

	if len(writes) >= 30 {
		t.Errorf("writes = %d, want fewer than 30; the run was not cut short", len(writes))
	}
}

// TestSchemaOrdersObservationBeforeDecision pins the ordering choice. Generation is
// autoregressive, so the collection is better chosen after the model has written down what it
// thinks the page is.
func TestSchemaOrdersObservationBeforeDecision(t *testing.T) {
	t.Parallel()

	backend := newBackend(echoAll("Development", 0.9))

	if _, _, err := classify.NewClassifier(testClassify(), backend, discardLogger()).
		Run(context.Background(), targets(1), 1); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	schema := backend.requests()[0].Schema

	results, ok := schema["properties"].(map[string]any)["results"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no results array: %#v", schema)
	}

	item, ok := results["items"].(map[string]any)
	if !ok {
		t.Fatalf("results has no item schema: %#v", results)
	}

	required, ok := item["required"].([]any)
	if !ok {
		t.Fatalf("item schema has no required list: %#v", item)
	}

	if diff := cmp.Diff([]any{"id", "description", "collection", "tags", "confidence"}, required); diff != "" {
		t.Errorf("required order mismatch (-want +got):\n%s", diff)
	}

	// A collection outside the configured set must not be representable in the first place.
	props, ok := item["properties"].(map[string]any)
	if !ok {
		t.Fatalf("item schema has no properties: %#v", item)
	}

	collection, ok := props["collection"].(map[string]any)
	if !ok {
		t.Fatalf("item schema has no collection property: %#v", props)
	}

	if _, ok := collection["enum"]; !ok {
		t.Error("the collection property has no enum, so the closed set is not enforced at decode time")
	}

	if item["additionalProperties"] != false {
		t.Error("additionalProperties is not false, so the model may add fields")
	}
}

// TestSystemPromptCarriesTheCollectionList is the other half of the closed set: the enum
// constrains what the model can emit, and the prompt explains what the options mean.
func TestSystemPromptCarriesTheCollectionList(t *testing.T) {
	t.Parallel()

	cfg := testClassify()
	cfg.Collections = []string{"Alpha", "Beta"}

	backend := newBackend(echoAll("Alpha", 0.9))

	if _, _, err := classify.NewClassifier(cfg, backend, discardLogger()).
		Run(context.Background(), targets(1), 1); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	system := backend.requests()[0].System

	for _, name := range cfg.Collections {
		if !strings.Contains(system, name) {
			t.Errorf("system prompt does not mention the collection %q", name)
		}
	}
}

// TestPromptHashChangesWithThePrompt is what makes the stored provenance useful: an edited
// prompt has to produce a different hash, or the column says nothing.
func TestPromptHashChangesWithThePrompt(t *testing.T) {
	t.Parallel()

	schema := map[string]any{"type": "object"}

	base := classify.PromptHash("classify these", schema)

	if same := classify.PromptHash("classify these", schema); same != base {
		t.Error("PromptHash is not stable for identical inputs")
	}

	if changed := classify.PromptHash("classify these differently", schema); changed == base {
		t.Error("PromptHash did not change when the prompt did")
	}

	if changed := classify.PromptHash("classify these", map[string]any{"type": "array"}); changed == base {
		t.Error("PromptHash did not change when the schema did")
	}
}
