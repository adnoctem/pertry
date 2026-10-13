package cmd_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/cmd"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// modelResult builds a model decision with a confidence and alternatives.
func modelResult(hash, facet, value string, score float64, alternativesJSON, payloadJSON string) store.Result {
	return store.Result{
		Hash: hash, Facet: facet, Value: value,
		Confidence: score, HasConfidence: true,
		Source: store.ModelSource("qwen3-vl:8b"), Stage: facet,
		Model: "qwen3-vl:8b", ModelDigest: "sha256:dead",
		PromptHash: "sha256:p", SchemaHash: "sha256:s",
		AlternativesJSON: alternativesJSON, PayloadJSON: payloadJSON,
		CreatedAt: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}
}

func TestReviewListOrdersLeastConfidentFirst(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.png", 1242, 2688, `{}`),
			file("ccc", "c.png", 1242, 2688, `{}`),
		},
		[]store.Result{
			modelResult("aaa", "kind", "photo", 0.55, "", ""),
			modelResult("bbb", "kind", "screenshot", 0.20, "", ""),
			// Confident, so it is not in the queue at all.
			modelResult("ccc", "kind", "photo", 0.99, "", ""),
		},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind")
	if err != nil {
		t.Fatalf("review list error = %v", err)
	}

	if !strings.Contains(out, "b.png") || !strings.Contains(out, "a.png") {
		t.Errorf("output is missing the uncertain items:\n%s", out)
	}

	if strings.Contains(out, "c.png") {
		t.Errorf("a confident result is in the review queue:\n%s", out)
	}

	// Least confident first, because that is where a reviewer's attention is worth most.
	if strings.Index(out, "b.png") > strings.Index(out, "a.png") {
		t.Errorf("the queue is not ordered least-confident-first:\n%s", out)
	}
}

// TestReviewListIncludesResultsWithNoScore: "no score" is not "confident", and a result the
// backend could not score still needs a human look.
func TestReviewListIncludesResultsWithNoScore(t *testing.T) {
	t.Parallel()

	unscored := modelResult("aaa", "kind", "photo", 0, "", "")
	unscored.HasConfidence = false

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{unscored},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind")
	if err != nil {
		t.Fatalf("review list error = %v", err)
	}

	if !strings.Contains(out, "a.png") {
		t.Errorf("an unscored result is missing from the queue:\n%s", out)
	}

	if !strings.Contains(out, "none") {
		t.Errorf("output does not mark the missing score:\n%s", out)
	}
}

// TestReviewListNeverShowsAHumanDecision is docs/ARCHITECTURE.md#review: a human decision is final, and re-presenting it
// would invite someone to confirm their own earlier answer as though it were new evidence.
func TestReviewListNeverShowsAHumanDecision(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{
			modelResult("aaa", "kind", "photo", 0.10, "", ""),
			{
				Hash: "aaa", Facet: "kind", Value: "screenshot",
				Source: store.SourceHuman, CreatedAt: time.Now(),
			},
		},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind")
	if err != nil {
		t.Fatalf("review list error = %v", err)
	}

	if strings.Contains(out, "a.png") {
		t.Errorf("a file a human has already decided is in the queue:\n%s", out)
	}
}

// TestReviewListShowsAlternativesAndObservations is what makes a correction a keystroke rather
// than a decision from scratch.
func TestReviewListShowsAlternativesAndObservations(t *testing.T) {
	t.Parallel()

	alternatives := `[{"value":"screenshot","probability":0.35}]`
	payload := `{"visible_ui":"a navigation bar and a keyboard"}`

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{modelResult("aaa", "kind", "photo", 0.45, alternatives, payload)},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind")
	if err != nil {
		t.Fatalf("review list error = %v", err)
	}

	if !strings.Contains(out, "screenshot") {
		t.Errorf("the alternative is missing:\n%s", out)
	}

	if !strings.Contains(out, "navigation bar") {
		t.Errorf("what the model said it saw is missing:\n%s", out)
	}
}

func TestReviewListJSON(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{modelResult("aaa", "kind", "photo", 0.30, "", "")},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind", "--json")
	if err != nil {
		t.Fatalf("review list --json error = %v", err)
	}

	var items []struct {
		Hash       string  `json:"Hash"`
		Path       string  `json:"Path"`
		Value      string  `json:"Value"`
		Confidence float64 `json:"Confidence"`
	}

	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	if len(items) != 1 || items[0].Path != "a.png" {
		t.Errorf("items = %+v, want one for a.png", items)
	}
}

func TestReviewListEmptyQueue(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{modelResult("aaa", "kind", "photo", 0.99, "", "")},
	)

	out, _, err := run(t, "--workspace", root, "review", "list", "kind")
	if err != nil {
		t.Fatalf("review list error = %v", err)
	}

	if !strings.Contains(out, "Nothing to review") {
		t.Errorf("output does not say the queue is empty:\n%s", out)
	}
}

// TestReviewSetRecordsAHumanDecision is docs/ARCHITECTURE.md#review's core guarantee.
func TestReviewSetRecordsAHumanDecision(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{modelResult("aaa", "kind", "photo", 0.40, "", "")},
	)

	out, _, err := run(t, "--workspace", root, "review", "set", "a.png", "kind=screenshot")
	if err != nil {
		t.Fatalf("review set error = %v", err)
	}

	// The output says what changed, including what it used to be.
	if !strings.Contains(out, "photo") || !strings.Contains(out, "model") {
		t.Errorf("output does not report the previous value and its source:\n%s", out)
	}

	ws, err := core.Open(root)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	defer func() { _ = db.Close() }()

	decisions, err := db.DecisionMap(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if decisions["kind"] != "screenshot" {
		t.Errorf("decision = %q, want the human's", decisions["kind"])
	}

	// The model's opinion survives, so stats can still measure agreement.
	counts, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Results != 2 {
		t.Errorf("Count().Results = %d, want both opinions kept", counts.Results)
	}
}

// TestReviewSetCarriesNoConfidence: a human decision is not a probability, and storing 1.0 would
// make it indistinguishable from a model that was very sure.
func TestReviewSetCarriesNoConfidence(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)}, nil)

	if _, _, err := run(t, "--workspace", root, "review", "set", "a.png", "kind=photo"); err != nil {
		t.Fatalf("review set error = %v", err)
	}

	ws, _ := core.Open(root)

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	defer func() { _ = db.Close() }()

	decisions, err := db.DecisionsForHash(context.Background(), "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash(): %v", err)
	}

	if decisions[0].HasConfidence {
		t.Errorf("a human decision carries confidence %v, want none", decisions[0].Confidence)
	}
}

// TestReviewSetAcceptsAHashPrefix exists because nobody types sixty-four hex characters.
func TestReviewSetAcceptsAHashPrefix(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("abc123def456", "a.png", 1242, 2688, `{}`)},
		nil,
	)

	if _, _, err := run(t, "--workspace", root, "review", "set", "abc123", "kind=photo"); err != nil {
		t.Fatalf("review set with a hash prefix error = %v", err)
	}
}

func TestReviewSetRejectsBadInput(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)}, nil)

	tests := map[string][]string{
		"no equals sign": {"a.png", "kind"},
		"no facet":       {"a.png", "=photo"},
		"no value":       {"a.png", "kind="},
		"unknown file":   {"nonexistent.png", "kind=photo"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			full := append([]string{"--workspace", root, "review", "set"}, args...)

			if got := exitCode(t, full...); got != cmd.ExitUsage {
				t.Errorf("exit code = %d, want %d (usage)", got, cmd.ExitUsage)
			}
		})
	}
}

// TestReviewSetReportsAConfirmation: recording the same value the model chose is a confirmation,
// which reads differently from a correction and is worth saying.
func TestReviewSetReportsAConfirmation(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{modelResult("aaa", "kind", "photo", 0.40, "", "")},
	)

	out, _, err := run(t, "--workspace", root, "review", "set", "a.png", "kind=photo")
	if err != nil {
		t.Fatalf("review set error = %v", err)
	}

	if !strings.Contains(out, "confirms") {
		t.Errorf("output does not distinguish a confirmation from a correction:\n%s", out)
	}
}

func TestTaxonomyAggregatesNearDuplicates(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.png", 1242, 2688, `{}`),
			file("ccc", "c.png", 1242, 2688, `{}`),
			file("ddd", "d.png", 1242, 2688, `{}`),
		},
		[]store.Result{
			// Three spellings that docs/ARCHITECTURE.md#model-stages's normalisation groups together.
			modelResult("aaa", "platform", "Twitter / X", 0.9, "", ""),
			modelResult("bbb", "platform", "twitter/x", 0.9, "", ""),
			modelResult("ccc", "platform", "twitter x", 0.9, "", ""),
			modelResult("ddd", "platform", "reddit", 0.9, "", ""),
		},
	)

	out, _, err := run(t, "--workspace", root, "taxonomy", "platform")
	if err != nil {
		t.Fatalf("taxonomy error = %v", err)
	}

	// One group of three, one of one.
	if !strings.Contains(out, "twitter x") {
		t.Errorf("the normalised key is missing:\n%s", out)
	}

	if !strings.Contains(out, "2 distinct label(s)") {
		t.Errorf("the three spellings were not grouped:\n%s", out)
	}

	// The variants are shown, because the merge is the decision the user is being asked for.
	for _, variant := range []string{"Twitter / X", "twitter/x"} {
		if !strings.Contains(out, variant) {
			t.Errorf("variant %q is not shown:\n%s", variant, out)
		}
	}
}

func TestTaxonomyYAML(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.png", 1242, 2688, `{}`),
		},
		[]store.Result{
			modelResult("aaa", "platform", "reddit", 0.9, "", ""),
			modelResult("bbb", "platform", "instagram", 0.9, "", ""),
		},
	)

	out, _, err := run(t, "--workspace", root, "taxonomy", "platform", "--yaml")
	if err != nil {
		t.Fatalf("taxonomy --yaml error = %v", err)
	}

	for _, want := range []string{"taxonomies:", "platform:", `"reddit"`, `"instagram"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// The output has to say it needs curating, since pasting it unexamined is the mistake
	// docs/ARCHITECTURE.md#model-stages's workflow exists to prevent.
	if !strings.Contains(out, "Curate by hand") {
		t.Errorf("output does not say it needs curating:\n%s", out)
	}
}

func TestTaxonomyOnAnEmptyFacet(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)}, nil)

	out, _, err := run(t, "--workspace", root, "taxonomy", "platform")
	if err != nil {
		t.Fatalf("taxonomy error = %v", err)
	}

	if !strings.Contains(out, "sicht run") {
		t.Errorf("output does not say what to do first:\n%s", out)
	}
}

// TestRunDryRunNeedsNoOllama is why --dry-run does not construct a backend: estimating the work
// should not require the server to be up.
func TestRunDryRunNeedsNoOllama(t *testing.T) {
	// Not parallel: it pins OLLAMA_HOST so that a running server on this machine cannot make
	// the test pass for the wrong reason.
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)}, nil)

	out, _, err := run(t, "--workspace", root, "run", "kind", "--dry-run")
	if err != nil {
		t.Fatalf("run --dry-run error = %v, want it to work with no server", err)
	}

	if !strings.Contains(out, "the model was not called") {
		t.Errorf("output does not say nothing was sent:\n%s", out)
	}

	if !strings.Contains(out, "would be sent") {
		t.Errorf("output does not report what it would do:\n%s", out)
	}
}

func TestRunRejectsAnUnknownStage(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)}, nil)

	if got := exitCode(t, "--workspace", root, "run", "nonsense", "--dry-run"); got != cmd.ExitError {
		t.Errorf("exit code = %d, want %d", got, cmd.ExitError)
	}
}

func TestHelpListsTheM2Commands(t *testing.T) {
	t.Parallel()

	out, _, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help error = %v", err)
	}

	for _, name := range []string{"run", "model", "taxonomy", "review"} {
		if !strings.Contains(out, name) {
			t.Errorf("--help does not list %q:\n%s", name, out)
		}
	}
}
