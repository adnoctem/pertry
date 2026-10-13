package cmd

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/rules"
)

var _ CLIOpt = NewRulesCommand // assure type compatibility

// NewRulesCommand creates the 'rules' subcommand.
func NewRulesCommand(a *App) *cobra.Command {
	var (
		dryRun bool
		keep   bool
		limit  int
	)

	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Decide what can be decided without a model",
		Long: "Evaluate the rules in sicht.yaml against everything `scan` indexed, and record what they\n" +
			"decide. No inference, no network, no cost - just expressions over metadata.\n\n" +
			"Run this before any AI stage. What the rules decide is what the model never has to look\n" +
			"at, and the report tells you how many files are left, which is what sizes the model pass.\n\n" +
			"Previous rule results are retracted first, because an edited rule set changes what each\n" +
			"rule index means. A human decision or a model result is never touched: precedence is\n" +
			"resolved when decisions are read, not by deleting anything.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runRules(cmd.Context(), dryRun, !keep, limit)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Report what the rules would decide without writing anything")
	cmd.Flags().BoolVar(&keep, "keep-previous", false,
		"Do not retract previous rule results first (rarely what you want)")
	cmd.Flags().IntVar(&limit, "limit", 0,
		"Evaluate at most this many files")

	return cmd
}

func (a *App) runRules(ctx context.Context, dryRun, retract bool, limit int) error {
	ws, err := a.requireWorkspace()
	if err != nil {
		return err
	}

	cfg, err := a.requireConfig()
	if err != nil {
		return err
	}

	if len(cfg.Rules) == 0 {
		return usagef("no rules are configured\n" +
			"hint: sicht.yaml's `rules:` list is empty; the shipped default has three examples")
	}

	db, err := a.openStore(ctx, ws)
	if err != nil {
		return err
	}

	defer func() {
		if err := db.Close(); err != nil {
			a.Log.Warn("closing the database failed", "error", err)
		}
	}()

	counts, err := db.Count(ctx)
	if err != nil {
		return err
	}

	if counts.Files == 0 {
		return usagef("nothing has been scanned yet\nhint: run `sicht scan` first")
	}

	started := time.Now()

	result, err := rules.Run(ctx, rules.RunOptions{
		DB:      db,
		Config:  cfg,
		Retract: retract,
		DryRun:  dryRun,
		Limit:   limit,
		Log:     a.Log,
	})
	if err != nil {
		return err
	}

	a.printRulesResult(cfg, result, dryRun, time.Since(started))

	if len(result.Errors) > 0 {
		return &partialError{err: fmt.Errorf("%d file(s) failed rule evaluation", len(result.Errors))}
	}

	return nil
}

func (a *App) printRulesResult(cfg *config.Config, result rules.RunResult, dryRun bool, elapsed time.Duration) {
	if dryRun {
		a.printf("Dry run: nothing was written.\n\n")
	}

	a.printf("%-22s %d\n", "files evaluated", result.Files)
	a.printf("%-22s %d\n", "files decided", result.Decided)
	a.printf("%-22s %d\n", "decisions", result.Decisions)

	if result.Retracted > 0 {
		a.printf("%-22s %d\n", "previous retracted", result.Retracted)
	}

	a.printf("%-22s %s\n", "elapsed", elapsed.Round(time.Millisecond))

	// Per rule, in configuration order, including the ones that never fired - which is the
	// most useful thing here, since a rule that matches nothing looks identical to a correct
	// one in the configuration file.
	set, err := rules.Compile(cfg)
	if err != nil {
		return
	}

	a.printf("\nPer rule, in order. Earlier rules win, so a later rule for the same facet only\n")
	a.printf("sees what the earlier ones left.\n\n")

	for _, y := range set.Yields(result.PerRule) {
		marker := "  "
		if y.Count == 0 {
			marker = a.paint("<-", "33")
		}

		a.printf("  [%d] %7d  %s %s\n", y.Index, y.Count, marker, y.Describe(72))
	}

	if len(result.PerValue) > 0 {
		a.printf("\nPer facet:\n")

		facets := make([]string, 0, len(result.PerValue))
		for f := range result.PerValue {
			facets = append(facets, f)
		}

		sort.Strings(facets)

		for _, facet := range facets {
			a.printf("\n  %s\n", facet)

			values := result.PerValue[facet]

			type pair struct {
				value string
				count int
			}

			ordered := make([]pair, 0, len(values))
			for v, n := range values {
				ordered = append(ordered, pair{v, n})
			}

			sort.Slice(ordered, func(i, j int) bool {
				if ordered[i].count != ordered[j].count {
					return ordered[i].count > ordered[j].count
				}

				return ordered[i].value < ordered[j].value
			})

			for _, p := range ordered {
				share := 0.0
				if result.Files > 0 {
					share = float64(p.count) * 100 / float64(result.Files)
				}

				a.printf("    %-24s %6d  %5.1f%%\n", p.value, p.count, share)
			}

			if undecided, ok := result.Undecided[facet]; ok {
				share := 0.0
				if result.Files > 0 {
					share = float64(undecided) * 100 / float64(result.Files)
				}

				a.printf("    %-24s %6d  %5.1f%%   <- what a model pass would have to look at\n",
					"(undecided)", undecided, share)
			}
		}
	}

	if len(result.Errors) > 0 {
		a.printf("\n%d file(s) failed evaluation; see the log for details.\n", len(result.Errors))
	}
}
