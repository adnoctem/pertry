package stage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/stage"
	"github.com/adnoctem/pertry/internal/sicht/store"
	"github.com/adnoctem/pertry/pkg/infer"
)

// fixture is a workspace with images on disk and matching database rows.
type fixture struct {
	ws  *core.Workspace
	db  *store.DB
	cfg *config.Config
}

func newFixture(t *testing.T, files map[string]string, decided map[string]map[string]string) *fixture {
	t.Helper()

	root := t.TempDir()

	ws, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	// The prompts a stage refers to.
	if err := os.MkdirAll(ws.PromptsPath(), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}

	for _, name := range []string{"kind.md", "platform.md"} {
		path := filepath.Join(ws.PromptsPath(), name)

		if err := os.WriteFile(path, []byte("Classify the image. Prompt: "+name), 0o644); err != nil {
			t.Fatalf("write prompt: %v", err)
		}
	}

	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	var (
		scanned []store.Scanned
		results []store.Result
	)

	for rel, hash := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(path, pngBytes(t, 40, 30), 0o644); err != nil {
			t.Fatalf("write image: %v", err)
		}

		scanned = append(scanned, store.Scanned{
			File: store.File{
				Hash: hash, Size: 100, Ext: strings.TrimPrefix(filepath.Ext(rel), "."),
				Width: 1242, Height: 2688, FirstSeen: now,
			},
			Location: store.Location{Path: rel, Hash: hash, MTime: now, SeenAt: now},
			Metadata: store.Metadata{Hash: hash, ExifJSON: `{"File:FileTypeExtension":"png"}`},
		})

		for facet, value := range decided[rel] {
			results = append(results, store.Result{
				Hash: hash, Facet: facet, Value: value,
				Source: store.RuleSource(0), CreatedAt: now,
			})
		}
	}

	if err := db.SaveScanned(context.Background(), scanned); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	if err := db.SaveResults(context.Background(), results); err != nil {
		t.Fatalf("SaveResults(): %v", err)
	}

	cfg := config.Default()

	return &fixture{ws: ws, db: db, cfg: &cfg}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			v := uint8((x * 255) / max(w-1, 1))
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	var buf bytes.Buffer

	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	return buf.Bytes()
}

// answer builds a fake that returns a fixed payload, optionally with logprobs for the value.
func answer(payload string, valueLogprob float64, alternatives ...infer.Alternative) *infer.Fake {
	return &infer.Fake{
		CompleteFunc: func(req infer.Request) (infer.Response, error) {
			resp := infer.Response{JSON: json.RawMessage(payload), Model: req.Model}

			if req.Logprobs {
				// A realistic stream: a grammar-forced opening, then the value.
				resp.Logprobs = []infer.TokenLogprob{
					{Token: payload[:strings.Index(payload, `"kind"`)+8], Logprob: -15.8},
					{Token: valueOf(payload), Logprob: valueLogprob, Top: alternatives},
					{Token: `"}`, Logprob: -0.001},
				}
			}

			return resp, nil
		},
	}
}

// valueOf pulls the kind value out of a fixture payload, so the fake's token stream matches it.
func valueOf(payload string) string {
	var decoded map[string]any

	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return ""
	}

	value, _ := decoded["kind"].(string)

	return value
}

func kindOnlyConfig(cfg *config.Config) {
	cfg.Stages = []config.Stage{{
		Name: "kind", Mode: config.ModeClosed,
		Input:       `undecided("kind")`,
		PromptFile:  "prompts/kind.md",
		ReviewBelow: 0.75,
		Fields: []config.Field{
			{Name: "visible_ui", Type: config.FieldString, Max: 200},
			{
				Name: "kind", Type: config.FieldEnum, Decision: true,
				Values: []string{"screenshot", "photo", "scan_document", "other"},
			},
		},
	}}
}

func TestRunDecidesUndecidedFiles(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a.png": "aaa", "b.png": "bbb"},
		// b is already decided by a rule, so the stage input excludes it.
		map[string]map[string]string{"b.png": {"kind": "photo"}},
	)
	kindOnlyConfig(f.cfg)

	fake := answer(`{"visible_ui":"a status bar","kind":"screenshot"}`, math.Log(0.95),
		infer.Alternative{Token: "screenshot", Logprob: math.Log(0.95)},
		infer.Alternative{Token: "photo", Logprob: math.Log(0.05)},
	)

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if result.Selected != 1 {
		t.Errorf("Selected = %d, want 1 - the rule-decided file must be excluded", result.Selected)
	}

	if result.Decided != 1 {
		t.Errorf("Decided = %d, want 1", result.Decided)
	}

	decisions, err := f.db.DecisionMap(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if decisions["kind"] != "screenshot" {
		t.Errorf("stored kind = %q, want screenshot", decisions["kind"])
	}

	// The observation is kept as the stage's payload, so it is available for display and
	// search even though nothing files by it.
	stored, err := f.db.DecisionsForHash(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash(): %v", err)
	}

	if !strings.Contains(stored[0].PayloadJSON, "visible_ui") {
		t.Errorf("PayloadJSON = %q, want the observation retained", stored[0].PayloadJSON)
	}

	// Provenance, so a re-run under a changed model or prompt is distinguishable.
	if stored[0].Model == "" || stored[0].Source != store.ModelSource(f.cfg.Model.Name) {
		t.Errorf("result = %+v, want the model recorded as its source", stored[0])
	}
}

// TestConfidenceIsRenormalised ties docs/ARCHITECTURE.md#confidence to the stored row: the value token's raw probability is
// 0.95 here, and the alternatives are both legal, so the renormalised figure is what lands.
func TestConfidenceIsStored(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	// 0.8 renormalised over {0.8, 0.1} is about 0.889 - comfortably above the 0.75 threshold,
	// rather than exactly on it, because an exact-boundary assertion would turn floating-point
	// noise into a flaky test.
	fake := answer(`{"visible_ui":"none","kind":"photo"}`, math.Log(0.8),
		infer.Alternative{Token: "photo", Logprob: math.Log(0.8)},
		infer.Alternative{Token: "screenshot", Logprob: math.Log(0.1)},
	)

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	stored, err := f.db.DecisionsForHash(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash(): %v", err)
	}

	if len(stored) != 1 {
		t.Fatalf("stored %d decisions, want 1", len(stored))
	}

	if !stored[0].HasConfidence {
		t.Fatal("no confidence was stored")
	}

	// 0.8 renormalised over {0.8, 0.1} is 8/9.
	if math.Abs(stored[0].Confidence-8.0/9.0) > 0.02 {
		t.Errorf("Confidence = %v, want about 0.889 renormalised over the legal values",
			stored[0].Confidence)
	}

	// The alternative is kept for the review UI.
	if !strings.Contains(stored[0].AlternativesJSON, "screenshot") {
		t.Errorf("AlternativesJSON = %q, want the alternative retained", stored[0].AlternativesJSON)
	}

	if result.Queued != 0 {
		t.Errorf("Queued = %d, want 0 for a confident answer", result.Queued)
	}
}

func TestLowConfidenceIsQueuedForReview(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	fake := answer(`{"visible_ui":"none","kind":"photo"}`, math.Log(0.3),
		infer.Alternative{Token: "photo", Logprob: math.Log(0.3)},
		infer.Alternative{Token: "screenshot", Logprob: math.Log(0.3)},
	)

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Queued != 1 {
		t.Errorf("Queued = %d, want 1 - a 50/50 answer belongs in the review queue", result.Queued)
	}
}

// TestNoLogprobsIsNotZeroConfidence: a backend that reports none must not have every result
// treated as maximally uncertain, which would send the whole archive to review.
func TestNoLogprobsIsNotZeroConfidence(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	fake := &infer.Fake{Fixture: json.RawMessage(`{"visible_ui":"none","kind":"photo"}`)}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.NoConfidence != 1 {
		t.Errorf("NoConfidence = %d, want 1", result.NoConfidence)
	}

	if result.Queued != 0 {
		t.Errorf("Queued = %d, want 0 - an absent score is not a low one", result.Queued)
	}

	stored, err := f.db.DecisionsForHash(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash(): %v", err)
	}

	if stored[0].HasConfidence {
		t.Error("a confidence was stored although none could be derived")
	}
}

// TestFreshResultsAreSkipped is docs/ARCHITECTURE.md#model-stages's staleness rule: a second run does no work when nothing
// about the model, prompt or schema has changed.
func TestFreshResultsAreSkipped(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	// A stage whose input does not exclude already-decided files, so freshness is the only
	// thing that can skip it.
	kindOnlyConfig(f.cfg)
	f.cfg.Stages[0].Input = `true`

	fake := answer(`{"visible_ui":"none","kind":"photo"}`, math.Log(0.9))

	first, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	if first.Processed != 1 {
		t.Fatalf("first run Processed = %d, want 1", first.Processed)
	}

	second, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	if second.Fresh != 1 || second.Processed != 0 {
		t.Errorf("second run Fresh = %d, Processed = %d; want 1 and 0", second.Fresh, second.Processed)
	}
}

// TestAChangedPromptMakesResultsStale is the other half: the whole point of recording a prompt
// hash is that editing the prompt means the answers were produced under different conditions.
func TestAChangedPromptMakesResultsStale(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)
	f.cfg.Stages[0].Input = `true`

	fake := answer(`{"visible_ui":"none","kind":"photo"}`, math.Log(0.9))

	opts := stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	}

	if _, err := stage.Run(context.Background(), opts); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	// Edit the prompt.
	prompt := filepath.Join(f.ws.PromptsPath(), "kind.md")
	if err := os.WriteFile(prompt, []byte("A different instruction entirely."), 0o644); err != nil {
		t.Fatalf("rewrite prompt: %v", err)
	}

	second, err := stage.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	if second.Processed != 1 {
		t.Errorf("Processed = %d, want 1 - an edited prompt makes the result stale", second.Processed)
	}
}

func TestRerunIgnoresFreshness(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)
	f.cfg.Stages[0].Input = `true`

	fake := answer(`{"visible_ui":"none","kind":"photo"}`, math.Log(0.9))

	opts := stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	}

	if _, err := stage.Run(context.Background(), opts); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	opts.Rerun = true

	second, err := stage.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	if second.Processed != 1 {
		t.Errorf("Processed = %d with --rerun, want 1", second.Processed)
	}
}

// TestDryRunSendsNothing covers the estimate-before-you-spend path.
func TestDryRunSendsNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa", "b.png": "bbb"}, nil)
	kindOnlyConfig(f.cfg)

	var calls int

	fake := &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		calls++

		return infer.Response{JSON: json.RawMessage(`{}`)}, nil
	}}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", DryRun: true,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if calls != 0 {
		t.Errorf("the model was called %d times in a dry run", calls)
	}

	if result.Selected != 2 {
		t.Errorf("Selected = %d, want 2 - a dry run still reports what it would do", result.Selected)
	}

	counts, err := f.db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Results != 0 {
		t.Errorf("Count().Results = %d, want 0 in a dry run", counts.Results)
	}
}

// TestInvalidResponseIsRetriedOnce is docs/ARCHITECTURE.md#inference-backend-and-model-choice's requirement, and the retry count matters: if
// constrained decoding produced something invalid twice, a third attempt is unlikely to differ.
func TestInvalidResponseIsRetriedOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var (
		mu    sync.Mutex
		calls int
	)

	fake := &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n == 1 {
			// A value outside the enum, which validation must catch.
			return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"meme"}`)}, nil
		}

		return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"photo"}`)}, nil
	}}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if calls != 2 {
		t.Errorf("the model was called %d times, want 2 - one retry", calls)
	}

	if result.Decided != 1 {
		t.Errorf("Decided = %d, want the retry to succeed", result.Decided)
	}
}

func TestPersistentlyInvalidResponseIsRecorded(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	fake := &infer.Fake{Fixture: json.RawMessage(`{"visible_ui":"x","kind":"meme"}`)}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want a per-file failure not to fail the run", err)
	}

	if result.Invalid != 1 {
		t.Errorf("Invalid = %d, want 1", result.Invalid)
	}

	if result.Decided != 0 {
		t.Errorf("Decided = %d, want 0", result.Decided)
	}
}

// TestOtherFollowsItsCompanion keeps the taxonomy from filling up with "other": the companion
// field holds what the model actually saw, and that is the label worth aggregating.
func TestOtherFollowsItsCompanion(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)

	f.cfg.Stages = []config.Stage{{
		Name: "platform", Mode: config.ModeClosed,
		Input: `true`, PromptFile: "prompts/platform.md", ReviewBelow: 0.75,
		Fields: []config.Field{
			{Name: "ui_description", Type: config.FieldString, Max: 100},
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
		},
	}}
	f.cfg.Taxonomies = map[string][]string{"platform": {"instagram", "reddit"}}

	fake := &infer.Fake{Fixture: json.RawMessage(
		`{"ui_description":"a purple timeline","platform":"other","platform_other":"Mastodon"}`)}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "platform", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	decisions, err := f.db.DecisionMap(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if decisions["platform"] != "mastodon" {
		t.Errorf("platform = %q, want the companion's value lowercased", decisions["platform"])
	}

	// An invented value means the curated taxonomy does not cover this file, which is worth
	// reviewing whatever the confidence.
	if result.Queued != 1 {
		t.Errorf("Queued = %d, want 1 - an uncurated value belongs in the review queue", result.Queued)
	}
}

// TestOtherInAnInlineEnumIsAnOrdinaryValue is the other half of the test above, and exists because
// the default configuration got it wrong. "other" is the escape hatch only on a taxonomy field, the
// one kind of field Build gives a free-text companion. An inline enum may list "other" among its own
// values - the default `kind` stage did - and there it is an answer like any other. Treating it as
// the escape hatch marked every "other" as an invented value and sent it to review whatever its
// confidence, with no companion to resolve it into.
func TestOtherInAnInlineEnumIsAnOrdinaryValue(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)

	f.cfg.Stages = []config.Stage{{
		Name: "kind", Mode: config.ModeClosed,
		Input: `true`, PromptFile: "prompts/kind.md", ReviewBelow: 0.75,
		Fields: []config.Field{
			{Name: "visible_ui", Type: config.FieldString, Max: 100},
			{Name: "kind", Type: config.FieldEnum, Values: []string{"photo", "other"}, Decision: true},
		},
	}}

	fake := &infer.Fake{Fixture: json.RawMessage(`{"visible_ui":"none","kind":"other"}`)}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	decisions, err := f.db.DecisionMap(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if decisions["kind"] != "other" {
		t.Errorf("kind = %q, want other", decisions["kind"])
	}

	// The fake reports no logprobs, so no confidence is derived and nothing is below the
	// threshold. The only way this could be queued is by being taken for an invented value.
	if result.Queued != 0 {
		t.Errorf("Queued = %d, want 0 - an inline enum's own value is not an invented one", result.Queued)
	}
}

// TestContextIsPassedToTheModel covers docs/ARCHITECTURE.md#model-stages's `context:`, which is how a later stage sees what an
// earlier one decided.
func TestContextIsPassedToTheModel(t *testing.T) {
	t.Parallel()

	f := newFixture(t,
		map[string]string{"a.png": "aaa"},
		map[string]map[string]string{"a.png": {"kind": "screenshot"}},
	)

	f.cfg.Stages = []config.Stage{{
		Name: "platform", Mode: config.ModeOpen,
		Input: `kind == "screenshot"`, PromptFile: "prompts/platform.md",
		Context: []string{"kind"},
		Fields: []config.Field{
			{Name: "platform", Type: config.FieldEnum, Taxonomy: "platform", Decision: true},
		},
	}}

	var seen string

	fake := &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		seen = req.Prompt

		return infer.Response{JSON: json.RawMessage(`{"platform":"reddit"}`)}, nil
	}}

	if _, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "platform", Workers: 1,
	}); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if !strings.Contains(seen, "kind") || !strings.Contains(seen, "screenshot") {
		t.Errorf("prompt = %q, want the prior facet value included", seen)
	}
}

// TestTheSystemPromptIsTheStagePrompt checks the prompt file actually reaches the model, since a
// silently empty prompt would degrade every answer without failing anything.
func TestTheSystemPromptIsTheStagePrompt(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var system string

	fake := &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		system = req.System

		return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"photo"}`)}, nil
	}}

	if _, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	}); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if !strings.Contains(system, "kind.md") {
		t.Errorf("system prompt = %q, want the stage's prompt file", system)
	}
}

// TestImagesAreSent is the whole point: a stage that classified by filename would be worthless.
func TestImagesAreSent(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var images int

	fake := &infer.Fake{CompleteFunc: func(req infer.Request) (infer.Response, error) {
		images = len(req.Images)

		for _, img := range req.Images {
			if len(img.Data) == 0 {
				return infer.Response{}, errors.New("an image with no data was sent")
			}
		}

		return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"photo"}`)}, nil
	}}

	if _, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	}); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if images != 1 {
		t.Errorf("sent %d images, want 1", images)
	}
}

func TestLimit(t *testing.T) {
	t.Parallel()

	files := map[string]string{}
	for i := range 6 {
		files[string(rune('a'+i))+".png"] = strings.Repeat(string(rune('a'+i)), 3)
	}

	f := newFixture(t, files, nil)
	kindOnlyConfig(f.cfg)

	fake := answer(`{"visible_ui":"x","kind":"photo"}`, math.Log(0.9))

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1, Limit: 2,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Processed != 2 {
		t.Errorf("Processed = %d, want 2", result.Processed)
	}

	// Selected still reports the real total, so the report says what remains.
	if result.Selected != 6 {
		t.Errorf("Selected = %d, want 6", result.Selected)
	}
}

// TestCancellationKeepsCompletedWork: inference is expensive, so a Ctrl-C must not discard what
// the GPU already produced.
func TestCancellationKeepsCompletedWork(t *testing.T) {
	t.Parallel()

	files := map[string]string{}
	for i := range 10 {
		files[string(rune('a'+i))+".png"] = strings.Repeat(string(rune('a'+i)), 3)
	}

	f := newFixture(t, files, nil)
	kindOnlyConfig(f.cfg)

	ctx, cancel := context.WithCancel(context.Background())

	var (
		mu    sync.Mutex
		calls int
	)

	fake := &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n >= 3 {
			cancel()
		}

		return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"photo"}`)}, nil
	}}

	_, err := stage.Run(ctx, stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err == nil {
		t.Log("Run() returned no error; cancellation may have arrived after the queue drained")
	}

	counts, countErr := f.db.Count(context.Background())
	if countErr != nil {
		t.Fatalf("Count(): %v", countErr)
	}

	if counts.Results == 0 {
		t.Error("cancellation discarded every completed result")
	}
}

func TestRunRejectsAnUnknownStage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)

	_, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg,
		Completer: &infer.Fake{}, StageName: "nope",
	})
	if err == nil {
		t.Error("Run() error = nil for an unknown stage, want one naming the configured stages")
	}
}

func TestRunRejectsAMissingPrompt(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)
	f.cfg.Stages[0].PromptFile = "prompts/absent.md"

	_, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg,
		Completer: &infer.Fake{}, StageName: "kind",
	})
	if err == nil {
		t.Fatal("Run() error = nil for a missing prompt file")
	}

	if !strings.Contains(err.Error(), "sicht init") {
		t.Errorf("error = %q, want a hint about how to get the prompts back", err)
	}
}

// TestEmptyResponseIsRetried is the failure the first GPU benchmark run exposed: a model that
// reasons before answering can spend its whole context on reasoning, and what arrives is an empty
// content field. The request succeeded, so treating it like a transport failure discarded a file
// that a second attempt answers perfectly well.
func TestEmptyResponseIsRetried(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var (
		mu    sync.Mutex
		calls int
	)

	fake := &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n == 1 {
			return infer.Response{}, fmt.Errorf("ollama m: %w", infer.ErrEmptyResponse)
		}

		return infer.Response{JSON: json.RawMessage(`{"visible_ui":"x","kind":"photo"}`)}, nil
	}}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if calls != 2 {
		t.Errorf("the model was called %d times, want 2 - an empty response is retryable", calls)
	}

	if result.Decided != 1 {
		t.Errorf("Decided = %d, want the retry to succeed", result.Decided)
	}
}

// TestPersistentlyEmptyResponseIsRecorded: a second empty answer is recorded rather than retried
// again, because a context too small for the model does not get bigger by asking twice more.
func TestPersistentlyEmptyResponseIsRecorded(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var (
		mu    sync.Mutex
		calls int
	)

	fake := &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()

		return infer.Response{}, fmt.Errorf("ollama m: %w", infer.ErrEmptyResponse)
	}}

	result, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want a per-file failure not to fail the run", err)
	}

	if calls != 2 {
		t.Errorf("the model was called %d times, want exactly 2", calls)
	}

	if result.Failed != 1 {
		t.Errorf("Failed = %d, want 1", result.Failed)
	}
}

// TestATransportFailureIsNotRetried keeps the distinction meaningful: a server that is not there
// will not be there a moment later, and retrying every failure would turn one outage into two.
func TestATransportFailureIsNotRetried(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{"a.png": "aaa"}, nil)
	kindOnlyConfig(f.cfg)

	var (
		mu    sync.Mutex
		calls int
	)

	fake := &infer.Fake{CompleteFunc: func(infer.Request) (infer.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()

		return infer.Response{}, errors.New("connection refused")
	}}

	if _, err := stage.Run(context.Background(), stage.Options{
		Workspace: f.ws, DB: f.db, Config: f.cfg, Completer: fake,
		StageName: "kind", Workers: 1,
	}); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if calls != 1 {
		t.Errorf("the model was called %d times, want 1 - a transport failure is not retryable", calls)
	}
}
