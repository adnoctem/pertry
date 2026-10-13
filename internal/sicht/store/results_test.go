package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/store"
)

func seedFile(t *testing.T, db *store.DB, hash string) {
	t.Helper()

	if err := db.SaveScanned(context.Background(), []store.Scanned{
		scanned(hash, "photos/"+hash+".jpg", 1024),
	}); err != nil {
		t.Fatalf("SaveScanned(%s): %v", hash, err)
	}
}

func TestSaveResults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")

	err := db.SaveResults(ctx, []store.Result{{
		Hash: "aaa", Facet: "kind", Value: "screenshot",
		Confidence: 0.99, HasConfidence: true,
		Source: store.RuleSource(0), Stage: "",
		CreatedAt: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}})
	if err != nil {
		t.Fatalf("SaveResults() error = %v, want nil", err)
	}

	got, err := db.DecisionsForHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash() error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("DecisionsForHash() returned %d, want 1", len(got))
	}

	if got[0].Value != "screenshot" || got[0].Source != "rule:0" {
		t.Errorf("decision = %+v, want screenshot from rule:0", got[0])
	}

	if !got[0].HasConfidence || got[0].Confidence != 0.99 {
		t.Errorf("confidence = (%v, %v), want 0.99", got[0].Confidence, got[0].HasConfidence)
	}
}

// TestSaveResultsSupersedesTheSameSource is forced by the primary key and is the right semantics:
// a correction from one source replaces that source's opinion rather than accumulating.
func TestSaveResultsSupersedesTheSameSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")

	for _, value := range []string{"photo", "screenshot"} {
		if err := db.SaveResults(ctx, []store.Result{{
			Hash: "aaa", Facet: "kind", Value: value, Source: store.SourceHuman,
			CreatedAt: time.Now(),
		}}); err != nil {
			t.Fatalf("SaveResults(%s) error = %v", value, err)
		}
	}

	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}

	if counts.Results != 1 {
		t.Errorf("Count().Results = %d, want 1 - the same source supersedes itself", counts.Results)
	}

	got, err := db.DecisionsForHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash() error = %v", err)
	}

	if got[0].Value != "screenshot" {
		t.Errorf("value = %q, want the correction", got[0].Value)
	}
}

// TestPrecedenceThroughTheStore checks that every reader gets docs/ARCHITECTURE.md#review's ordering, since it is
// enforced in SQL and not in Go.
func TestPrecedenceThroughTheStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")

	// Written worst-first with the model's row newest, so a naive "last write wins" would
	// give the wrong answer.
	err := db.SaveResults(ctx, []store.Result{
		{
			Hash: "aaa", Facet: "kind", Value: "photo",
			Source: store.SourceHuman, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			Hash: "aaa", Facet: "kind", Value: "scan_document",
			Source: store.RuleSource(3), CreatedAt: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		},
		{
			Hash: "aaa", Facet: "kind", Value: "screenshot",
			Source: store.ModelSource("qwen3-vl:8b"), Model: "qwen3-vl:8b",
			CreatedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("SaveResults() error = %v", err)
	}

	got, err := db.DecisionsForHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash() error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("DecisionsForHash() returned %d, want one winner per facet", len(got))
	}

	if got[0].Value != "photo" || got[0].Source != store.SourceHuman {
		t.Errorf("decision = (%q, %q), want the human's, despite it being the oldest",
			got[0].Value, got[0].Source)
	}

	// All three opinions survive, because the review UI shows what the model said and stats
	// measures agreement.
	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}

	if counts.Results != 3 {
		t.Errorf("Count().Results = %d, want all 3 kept", counts.Results)
	}
}

func TestDecisionMap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")

	if err := db.SaveResults(ctx, []store.Result{
		{Hash: "aaa", Facet: "kind", Value: "screenshot", Source: store.RuleSource(0), CreatedAt: time.Now()},
		{Hash: "aaa", Facet: "platform", Value: "reddit", Source: store.ModelSource("m"), CreatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveResults() error = %v", err)
	}

	got, err := db.DecisionMap(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionMap() error = %v", err)
	}

	if got["kind"] != "screenshot" || got["platform"] != "reddit" {
		t.Errorf("DecisionMap() = %v, want both facets", got)
	}
}

// TestDeleteResultsBySource is what `rules` uses to retract before re-evaluating. Without it, an
// edited rule set leaves results behind from a rule index that no longer exists.
func TestDeleteResultsBySource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")
	seedFile(t, db, "bbb")

	if err := db.SaveResults(ctx, []store.Result{
		{Hash: "aaa", Facet: "kind", Value: "a", Source: store.RuleSource(0), CreatedAt: time.Now()},
		{Hash: "bbb", Facet: "kind", Value: "b", Source: store.RuleSource(7), CreatedAt: time.Now()},
		{Hash: "aaa", Facet: "kind", Value: "c", Source: store.SourceHuman, CreatedAt: time.Now()},
		{Hash: "aaa", Facet: "platform", Value: "d", Source: store.ModelSource("m"), CreatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveResults() error = %v", err)
	}

	removed, err := db.DeleteResultsBySource(ctx, "rule:")
	if err != nil {
		t.Fatalf("DeleteResultsBySource() error = %v", err)
	}

	if removed != 2 {
		t.Errorf("DeleteResultsBySource() = %d, want 2", removed)
	}

	// The human decision and the model's are untouched: retracting rules must not discard
	// work from other sources.
	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}

	if counts.Results != 2 {
		t.Errorf("Count().Results = %d, want the human and model results kept", counts.Results)
	}
}

func TestUndecidedHashes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)

	for _, h := range []string{"aaa", "bbb", "ccc"} {
		seedFile(t, db, h)
	}

	if err := db.SaveResults(ctx, []store.Result{
		{Hash: "bbb", Facet: "kind", Value: "photo", Source: store.RuleSource(0), CreatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveResults() error = %v", err)
	}

	undecided, err := db.UndecidedHashes(ctx, "kind")
	if err != nil {
		t.Fatalf("UndecidedHashes() error = %v", err)
	}

	if len(undecided) != 2 {
		t.Fatalf("UndecidedHashes() = %v, want 2", undecided)
	}

	// Sorted, so that --limit and --sample take a stable prefix across runs.
	if undecided[0] != "aaa" || undecided[1] != "ccc" {
		t.Errorf("UndecidedHashes() = %v, want [aaa ccc] in order", undecided)
	}

	decided, err := db.DecidedHashes(ctx, "kind")
	if err != nil {
		t.Fatalf("DecidedHashes() error = %v", err)
	}

	if len(decided) != 1 || decided[0] != "bbb" {
		t.Errorf("DecidedHashes() = %v, want [bbb]", decided)
	}
}

// TestUndecidedCountsAFacetSeparately: a file decided for one facet is still undecided for
// another, which is what makes the platform stage run only over screenshots.
func TestUndecidedCountsAFacetSeparately(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := open(t)
	seedFile(t, db, "aaa")

	if err := db.SaveResults(ctx, []store.Result{
		{Hash: "aaa", Facet: "kind", Value: "screenshot", Source: store.RuleSource(0), CreatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveResults() error = %v", err)
	}

	for facet, want := range map[string]int{"kind": 0, "platform": 1} {
		got, err := db.UndecidedHashes(ctx, facet)
		if err != nil {
			t.Fatalf("UndecidedHashes(%q) error = %v", facet, err)
		}

		if len(got) != want {
			t.Errorf("UndecidedHashes(%q) = %v, want %d", facet, got, want)
		}
	}
}

func TestSourceHelpers(t *testing.T) {
	t.Parallel()

	if got := store.RuleSource(7); got != "rule:7" {
		t.Errorf("RuleSource(7) = %q, want %q", got, "rule:7")
	}

	if got := store.ModelSource("qwen3-vl:8b"); got != "model:qwen3-vl:8b" {
		t.Errorf("ModelSource() = %q, want %q", got, "model:qwen3-vl:8b")
	}

	for source, want := range map[string]string{
		"human":             "human",
		"rule:0":            "rule",
		"rule:12":           "rule",
		"model:qwen3-vl:8b": "model",
		"something else":    "other",
	} {
		if got := store.SourceKind(source); got != want {
			t.Errorf("SourceKind(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestSaveResultsEmpty(t *testing.T) {
	t.Parallel()

	if err := open(t).SaveResults(context.Background(), nil); err != nil {
		t.Errorf("SaveResults(nil) error = %v, want nil", err)
	}
}

// TestResultsRequireAKnownFile is the foreign key doing its job: a result about content that was
// never scanned is a bug in the caller, not a row to keep.
func TestResultsRequireAKnownFile(t *testing.T) {
	t.Parallel()

	err := open(t).SaveResults(context.Background(), []store.Result{{
		Hash: "never-scanned", Facet: "kind", Value: "photo",
		Source: store.RuleSource(0), CreatedAt: time.Now(),
	}})
	if err == nil {
		t.Error("SaveResults() error = nil, want a foreign key violation")
	}
}
