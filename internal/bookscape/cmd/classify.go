package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/classify"
	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/anthropicbackend"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
	"github.com/adnoctem/pertry/pkg/infer/openaibackend"
)

var _ CLIOpt = NewClassifyCommand // assure type compatibility

// Pass names for --pass.
const (
	passRules = "rules"
	passModel = "model"
	passAll   = "all"
)

var passes = []string{passAll, passRules, passModel}

// NewClassifyCommand creates the 'classify' subcommand.
func NewClassifyCommand(a *App) *cobra.Command {
	var (
		pass   = passAll
		limit  int
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "classify",
		Short: "Assign a collection, tags and a description to each bookmark",
		Long: "Two passes, cheap first.\n\n" +
			"The rules pass applies the deterministic rules from bookscape.yaml. It is free, it is\n" +
			"instant, and in a typical decade-old collection it resolves a large share of the corpus -\n" +
			"code hosts, Q&A sites, documentation domains, video platforms. Re-running it picks up\n" +
			"edits to the rules without disturbing anything the model or a human already decided.\n\n" +
			"The model pass handles only what the rules could not decide, batched, with the collection\n" +
			"list supplied so the model assigns rather than invents. Every result records the model,\n" +
			"the run and a hash of the prompt, because the prompt will be iterated on. Results below\n" +
			"classify.min_confidence are stored but do not reach the export until a human confirms\n" +
			"them in `bookscape review`.\n\n" +
			"The model pass costs money. Use --dry-run to see what it would send, and --limit to try a\n" +
			"small batch first.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(passes, pass) {
				return usagef("unknown --pass %q\nhint: one of %s", pass, strings.Join(passes, ", "))
			}

			return a.runClassify(cmd.Context(), pass, limit, dryRun)
		},
	}

	cmd.Flags().StringVar(&pass, "pass", pass,
		"Which passes to run: "+strings.Join(passes, ", "))
	cmd.Flags().IntVar(&limit, "limit", 0,
		"Classify at most this many bookmarks in the model pass (0 means no limit)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Report what the model pass would send without calling the API or writing anything")

	return cmd
}

func (a *App) runClassify(ctx context.Context, pass string, limit int, dryRun bool) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if pass == passAll || pass == passRules {
		if err := a.runRulesPass(ctx, db, dryRun); err != nil {
			return err
		}
	}

	if pass == passAll || pass == passModel {
		if err := a.runModelPass(ctx, db, limit, dryRun); err != nil {
			return err
		}
	}

	return a.printClassifySummary(ctx, db)
}

// runRulesPass applies the configured rules to everything a rule may still decide.
func (a *App) runRulesPass(ctx context.Context, db *store.DB, dryRun bool) error {
	targets, err := db.ClassifyTargets(ctx, store.ClassifyScopeRuleOrNone, 0)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		a.printf("Rules pass: nothing to do.\n")

		return nil
	}

	engine := classify.NewRuleEngine(a.Config.Classify.Rules)

	var (
		writes  []store.ClassificationWrite
		tagOnly int
	)

	for _, t := range targets {
		match := engine.Apply(t)

		if !match.Decided() {
			if len(match.Tags) > 0 {
				// Tagged but not collected: still the model's problem, because the
				// collection is the axis export projects onto.
				tagOnly++
			}

			continue
		}

		a.Log.Debug("rule matched",
			"url", t.URL, "collection", match.Collection, "rules", match.Matched)

		writes = append(writes, store.ClassificationWrite{
			BookmarkID: t.ID,
			Classification: store.Classification{
				Collection: match.Collection,
				// A rule either matched or it did not; there is nothing to be
				// partially confident about.
				Confidence: 1,
				Source:     store.ClassifySourceRule,
			},
			Tags: match.Tags,
		})
	}

	if dryRun {
		a.printf("Rules pass would classify %d of %s considered.\n",
			len(writes), plural(len(targets), "bookmark", "bookmarks"))

		return nil
	}

	written, err := db.RecordClassifications(ctx, writes)
	if err != nil {
		return err
	}

	a.printf("Rules pass: classified %d of %s considered.\n",
		written, plural(len(targets), "bookmark", "bookmarks"))

	if tagOnly > 0 {
		a.printf("  %s matched a tag-only rule and still need a collection.\n",
			plural(tagOnly, "bookmark", "bookmarks"))
	}

	return nil
}

// runModelPass classifies the remainder with the configured model.
// inferenceBackend builds the completion backend named by classify.provider.
//
// Only the model pass calls a model at all, so this is the single place a provider is resolved.
// Credentials are each SDK's own business: the environment checks here are advisory, because a
// configured CLI profile works without an environment variable and failing early on an unset one
// would break that.
func (a *App) inferenceBackend() (infer.Completer, error) {
	cfg := a.Config.Classify

	switch cfg.Provider {
	case config.ProviderAnthropic:
		if !anthropicbackend.HasCredentialsEnv() {
			a.Log.Warn("no ANTHROPIC_API_KEY in the environment; relying on an `ant auth login` profile")
		}

		return anthropicbackend.New(), nil

	case config.ProviderOpenAI:
		if !openaibackend.HasCredentialsEnv() {
			a.Log.Warn("no OPENAI_API_KEY in the environment; the request will fail without one")
		}

		return openaibackend.New(), nil

	case config.ProviderOllama:
		backend, err := ollamabackend.New()
		if err != nil {
			return nil, fmt.Errorf("configure the ollama backend: %w\n"+
				"hint: set OLLAMA_HOST, or switch classify.provider to anthropic", err)
		}

		return backend, nil

	default:
		// Unreachable: config validation rejects an unknown provider.
		return nil, usagef("unknown classify.provider %q\nhint: one of %s",
			cfg.Provider, strings.Join(config.Providers, ", "))
	}
}

func (a *App) runModelPass(ctx context.Context, db *store.DB, limit int, dryRun bool) error {
	targets, err := db.ClassifyTargets(ctx, store.ClassifyScopeUnclassified, limit)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		a.printf("Model pass: nothing left for the model.\n")

		return nil
	}

	batchSize := a.Config.Classify.BatchSize
	batches := (len(targets) + batchSize - 1) / batchSize

	if dryRun {
		a.printf("Model pass would send %s to %s via %s in %s of up to %d.\n",
			plural(len(targets), "bookmark", "bookmarks"), a.Config.Classify.Model,
			a.Config.Classify.Provider, plural(batches, "batch", "batches"), batchSize)
		a.printf("Nothing was sent and nothing was written.\n")

		return nil
	}

	backend := a.ClassifyBackend
	if backend == nil {
		var err error
		if backend, err = a.inferenceBackend(); err != nil {
			return err
		}
	}

	runID, err := db.NextRunID(ctx)
	if err != nil {
		return err
	}

	a.Log.Info("starting model pass",
		"provider", a.Config.Classify.Provider,
		"model", a.Config.Classify.Model,
		"links", len(targets),
		"batches", batches,
		"batch_size", batchSize,
		"run_id", runID)

	classifier := classify.NewClassifier(a.Config.Classify, backend, a.Log)

	writes, stats, runErr := classifier.Run(ctx, targets, runID)

	// Persist whatever completed, even on failure: the batches that succeeded cost tokens,
	// and discarding them would make a retry pay for them twice.
	written, writeErr := db.RecordClassifications(ctx, writes)
	if writeErr != nil {
		return errors.Join(runErr, writeErr)
	}

	a.printf("Model pass: classified %s in %s with %s via %s (run %d).\n",
		plural(written, "bookmark", "bookmarks"), plural(stats.Batches, "batch", "batches"),
		a.Config.Classify.Model, backend.Name(), runID)

	if stats.Rejected > 0 {
		a.printf("  %s named a collection outside the configured set and were re-queued.\n",
			plural(stats.Rejected, "result", "results"))
	}

	if stats.LowConfidence > 0 {
		a.printf("  %s scored below min_confidence (%.2f) and need review before export.\n",
			plural(stats.LowConfidence, "result", "results"), a.Config.Classify.MinConfidence)
	}

	if stats.Unanswered > 0 {
		a.printf("  %s got no usable result and stay unclassified for the next run.\n",
			plural(stats.Unanswered, "bookmark", "bookmarks"))
	}

	if runErr != nil {
		return fmt.Errorf("%w\nhint: every classification that completed is saved, "+
			"so re-running `bookscape classify --pass=model` resumes from here", runErr)
	}

	return nil
}

// printClassifySummary reports the whole corpus's classification state.
func (a *App) printClassifySummary(ctx context.Context, db *store.DB) error {
	counts, err := db.ClassificationCounts(ctx)
	if err != nil {
		return err
	}

	total := 0
	for _, n := range counts {
		total += n
	}

	if total == 0 {
		a.printf("\nThe database has no bookmarks yet. Run `bookscape import <file.html>` first.\n")

		return nil
	}

	a.printf("\nClassification state across %s:\n", plural(total, "surviving link", "surviving links"))

	for _, key := range []string{"rule", "model", "manual", "unclassified"} {
		if n, ok := counts[key]; ok {
			a.printf("  %-13s %d\n", key, n)
		}
	}

	if counts["unclassified"] > 0 {
		a.printf("\nNext: bookscape classify --pass=model\n")
	} else {
		a.printf("\nNext: bookscape review\n")
	}

	return nil
}
