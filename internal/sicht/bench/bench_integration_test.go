//go:build integration

package bench_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/imageprep"
	"github.com/adnoctem/pertry/internal/sicht/schema"
	"github.com/adnoctem/pertry/internal/sicht/store"
	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

var (
	workspaceFlag = flag.String("bench.workspace", "",
		"A scanned sicht workspace to draw labelled images from")
	modelsFlag = flag.String("bench.models", "qwen3-vl:4b,qwen3-vl:8b,gemma3:4b",
		"Comma-separated models to compare")
	sampleFlag = flag.Int("bench.sample", 20,
		"Images per class per model")
)

// labelled is one image with its metadata-derived ground truth.
type labelled struct {
	path   string
	want   string
	width  int
	height int
}

// TestBenchmarkModels measures accuracy and seconds per image for each model, against ground truth
// the deterministic rules already established.
//
// Run it as, for example:
//
//	go test -tags integration -timeout 3h ./internal/sicht/bench/ \
//	  -run TestBenchmarkModels -v \
//	  -bench.workspace '/path/to/archive' -bench.sample 25
func TestBenchmarkModels(t *testing.T) {
	root := *workspaceFlag
	if root == "" {
		root = os.Getenv("SICHT_TEST_ARCHIVE")
	}

	if root == "" {
		t.Skip("set -bench.workspace or SICHT_TEST_ARCHIVE to a scanned workspace")
	}

	ws, err := core.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	defer func() { _ = db.Close() }()

	images, err := sampleLabelled(t, ws, db, *sampleFlag)
	if err != nil {
		t.Fatalf("sample images: %v", err)
	}

	if len(images) == 0 {
		t.Skip("no labelled images in that workspace; run `sicht scan` and `sicht rules` first")
	}

	byClass := map[string]int{}
	for _, img := range images {
		byClass[img.want]++
	}

	t.Logf("benchmark set: %d images %v, ground truth from deterministic rules", len(images), byClass)

	backend, err := ollamabackend.New()
	if err != nil {
		t.Fatalf("ollama backend: %v", err)
	}

	stage := kindStage()

	built, err := schema.Build(stage, nil)
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	schemaJSON, err := built.JSON()
	if err != nil {
		t.Fatalf("encode schema: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(schemaJSON, &asMap); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	type outcome struct {
		model        string
		correct      int
		total        int
		failures     int
		seconds      []float64
		evalTokens   []int
		confusion    map[string]map[string]int
		logprobsSeen bool
	}

	// Every image is prepared ONCE, before any model runs. Two reasons: reading and scaling
	// an image from the reference archive takes long enough that doing it three times tripled
	// the benchmark's wall time for no information, and preparing once guarantees all three
	// models see byte-identical input, which is the only way the comparison means anything.
	t.Log("preparing images once for all models...")

	type ready struct {
		labelled

		images []infer.Image
	}

	prepStart := time.Now()

	var prepared []ready

	for _, img := range images {
		out, err := imageprep.Prepare(img.path, imageprep.Options{MaxEdge: 1280})
		if err != nil {
			if !errors.Is(err, imageprep.ErrUndecodable) {
				t.Logf("  prepare %s: %v", filepath.Base(img.path), err)
			}

			continue
		}

		prepared = append(prepared, ready{labelled: img, images: out.Images})
	}

	t.Logf("prepared %d of %d images in %s", len(prepared), len(images),
		time.Since(prepStart).Round(time.Millisecond))

	if len(prepared) == 0 {
		t.Skip("no image could be prepared")
	}

	var results []outcome

	for _, model := range strings.Split(*modelsFlag, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		out := outcome{model: model, confusion: map[string]map[string]int{}}

		t.Logf("--- %s ---", model)

		for i, img := range prepared {
			req := infer.Request{
				Model:       model,
				System:      kindPrompt,
				Prompt:      "Classify this image.",
				Images:      img.images,
				Schema:      asMap,
				Logprobs:    true,
				TopLogprobs: 5,
				// What the tool will actually send, so the benchmark measures the
				// real thing rather than Ollama's defaults.
				// 4096 and a short hold, matching the shipped defaults: measured
				// worst-case usage over this archive was 2786 tokens, and the KV
				// cache scales with the context.
				Options:   map[string]any{"temperature": 0, "seed": 42, "num_ctx": 4096},
				KeepAlive: 2 * time.Minute,
			}

			start := time.Now()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			resp, err := backend.Complete(ctx, req)

			cancel()

			elapsed := time.Since(start).Seconds()

			if err != nil {
				out.failures++

				t.Logf("  [%d/%d] %s FAILED: %v", i+1, len(prepared), filepath.Base(img.path), err)

				continue
			}

			// Progress on every image, flushed as it goes: a benchmark that prints only
			// at the end tells you nothing while it runs, and the previous attempt ran
			// for 45 minutes without a single line of output.
			t.Logf("  [%d/%d] %-28s %5.1fs want=%-10s", i+1, len(prepared),
				truncate(filepath.Base(img.path), 28), elapsed, img.want)

			out.seconds = append(out.seconds, elapsed)
			out.evalTokens = append(out.evalTokens, int(resp.Usage.OutputTokens))

			if len(resp.Logprobs) > 0 {
				out.logprobsSeen = true
			}

			var payload map[string]any
			if err := json.Unmarshal(resp.JSON, &payload); err != nil {
				out.failures++

				continue
			}

			got, _ := payload["kind"].(string)

			out.total++

			if out.confusion[img.want] == nil {
				out.confusion[img.want] = map[string]int{}
			}

			out.confusion[img.want][got]++

			if got == img.want {
				out.correct++
			}
		}

		results = append(results, out)

		// Release the model before loading the next one. Without this, a three-model run
		// holds all three at once: on a 16 GiB WSL guest that exhausted the host and
		// killed the VM outright, which is how this line came to exist.
		if err := backend.Unload(context.Background(), model); err != nil {
			t.Logf("  could not unload %s: %v", model, err)
		} else {
			t.Logf("  unloaded %s", model)
		}
	}

	t.Log("")
	t.Log("================ RESULTS ================")
	t.Logf("%-14s %7s %8s %9s %9s %9s %8s", "model", "n", "accuracy", "mean s", "median s", "p90 s", "tokens")

	for _, r := range results {
		accuracy := 0.0
		if r.total > 0 {
			accuracy = float64(r.correct) * 100 / float64(r.total)
		}

		t.Logf("%-14s %7d %7.1f%% %9.2f %9.2f %9.2f %8.0f",
			r.model, r.total, accuracy,
			mean(r.seconds), percentile(r.seconds, 0.5), percentile(r.seconds, 0.9),
			mean(toFloat(r.evalTokens)))
	}

	t.Log("")

	for _, r := range results {
		t.Logf("%s confusion (rows = truth):", r.model)

		for _, truth := range sortedKeys(r.confusion) {
			parts := make([]string, 0, len(r.confusion[truth]))

			for _, got := range sortedKeys2(r.confusion[truth]) {
				parts = append(parts, fmt.Sprintf("%s=%d", got, r.confusion[truth][got]))
			}

			t.Logf("  %-14s %s", truth, strings.Join(parts, "  "))
		}

		if r.failures > 0 {
			t.Logf("  failures: %d", r.failures)
		}

		t.Logf("  logprobs reported: %v", r.logprobsSeen)
	}
}

// sampleLabelled draws a balanced sample of images whose kind the rules already decided.
func sampleLabelled(t *testing.T, ws *core.Workspace, db *store.DB, perClass int) ([]labelled, error) {
	t.Helper()

	ctx := context.Background()

	decisions, err := db.DecisionsForFacet(ctx, "kind")
	if err != nil {
		return nil, err
	}

	byHash := make(map[string]string, len(decisions))

	for _, d := range decisions {
		// Only rule decisions: a model result would be circular ground truth.
		if store.SourceKind(d.Source) != "rule" {
			continue
		}

		byHash[d.Hash] = d.Value
	}

	counts := map[string]int{}

	var out []labelled

	err = db.EachFileForRules(ctx, func(in store.RuleInput) error {
		want, ok := byHash[in.Hash]
		if !ok || counts[want] >= perClass {
			return nil
		}

		// A real HEIC cannot be sent to a model at all, so it is not part of a benchmark
		// about model accuracy.
		if strings.EqualFold(in.Ext, "heic") || strings.EqualFold(in.Ext, "heif") {
			return nil
		}

		counts[want]++

		out = append(out, labelled{
			path: ws.Abs(in.Path), want: want, width: in.Width, height: in.Height,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// kindPrompt is the shipped prompt, so the benchmark measures what the tool will actually do.
const kindPrompt = `Classify what kind of image this is.

Look at the whole frame before deciding. A screenshot has interface furniture somewhere in it -
a status bar, a navigation bar, a keyboard, a scroll bar, a cursor, text rendered crisply at a
consistent size. A photograph has optical evidence instead: depth of field, uneven lighting,
noise, motion blur, a horizon.

A photograph *of* a screen is a photograph. A screenshot of a photograph is a screenshot.

Describe the interface you can see before you decide, and describe nothing if there is none.`

func kindStage() config.Stage {
	return config.Stage{
		Name: "kind", Mode: config.ModeClosed,
		Fields: []config.Field{
			{Name: "visible_ui", Type: config.FieldString, Max: 200},
			{
				Name: "kind", Type: config.FieldEnum, Decision: true,
				Values: []string{"screenshot", "photo", "scan_document", "other"},
			},
		},
	}
}

func mean(in []float64) float64 {
	if len(in) == 0 {
		return 0
	}

	total := 0.0
	for _, v := range in {
		total += v
	}

	return total / float64(len(in))
}

func percentile(in []float64, p float64) float64 {
	if len(in) == 0 {
		return 0
	}

	sorted := make([]float64, len(in))
	copy(sorted, in)
	sort.Float64s(sorted)

	idx := int(math.Round(p * float64(len(sorted)-1)))

	return sorted[idx]
}

func toFloat(in []int) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}

	return out
}

func sortedKeys(m map[string]map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func sortedKeys2(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// truncate shortens a string for a fixed-width log column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n-1] + "…"
}
