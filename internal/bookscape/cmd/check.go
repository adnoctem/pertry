package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adnoctem/pertry/internal/bookscape/liveness"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

var _ CLIOpt = NewCheckCommand // assure type compatibility

// NewCheckCommand creates the 'check' subcommand, which probes bookmarks for liveness.
func NewCheckCommand(a *App) *cobra.Command {
	only := string(store.ScopeUnchecked)

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Probe bookmarks to see whether they still resolve",
		Long: "Probe surviving bookmarks with bounded concurrency and record whether each one is alive,\n" +
			"dead or uncertain.\n\n" +
			"The result is three-state on purpose. A decade-old collection is full of links that are\n" +
			"neither clearly alive nor clearly gone - bot protection, rate limits, servers that reject\n" +
			"HEAD, hosts that are down today. Those are recorded as uncertain and are included in the\n" +
			"export by default; only a 404, a 410 or a host that does not resolve counts as dead.\n\n" +
			"Resumable: results are written per link as they complete, and the default scope covers only\n" +
			"links with no result yet. Re-run with --only=uncertain later to retry the inconclusive\n" +
			"bucket, which is where a cleared rate limit shows up.",
		Args:             cobra.NoArgs,
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, err := store.ParseScope(only)
			if err != nil {
				return usagef("%w\nhint: --only must be one of %s", err, scopeList())
			}

			return a.runCheck(cmd.Context(), scope)
		},
	}

	cmd.Flags().StringVar(&only, "only", only,
		"Which bookmarks to probe: "+scopeList())

	return cmd
}

func scopeList() string {
	names := make([]string, 0, len(store.Scopes))
	for _, s := range store.Scopes {
		names = append(names, string(s))
	}

	return strings.Join(names, ", ")
}

func (a *App) runCheck(ctx context.Context, scope store.Scope) error {
	db, err := a.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	targets, err := db.LivenessTargets(ctx, scope)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		a.printf("Nothing to check in scope %q.\n", scope)

		return a.printLivenessSummary(ctx, db)
	}

	a.Log.Info("starting liveness pass",
		"scope", scope,
		"targets", len(targets),
		"concurrency", a.Config.Check.Concurrency,
		"per_host_concurrency", a.Config.Check.PerHostConcurrency,
		"timeout", a.Config.Check.Timeout)

	probes := make([]liveness.Target, 0, len(targets))
	for _, t := range targets {
		probes = append(probes, liveness.Target{ID: t.ID, URL: t.URL})
	}

	checker := liveness.New(a.Config.Check, db, a.Log)

	stats, err := checker.Run(ctx, probes)

	// Report what completed even when the pass was cut short. Every result is already
	// persisted, so an interrupted run is progress rather than a loss, and saying so is the
	// difference between "re-run it" and "start over".
	if stats.Total() > 0 {
		a.printf("Checked %s in scope %q:\n", plural(stats.Total(), "link", "links"), scope)
		a.printf("  alive      %d\n", stats.Alive)
		a.printf("  dead       %d\n", stats.Dead)
		a.printf("  uncertain  %d\n", stats.Uncertain)

		if stats.Skipped > 0 {
			a.printf("  skipped    %d  (not fetchable over HTTP)\n", stats.Skipped)
		}
	}

	if err != nil {
		if remaining := len(targets) - stats.Total(); remaining > 0 {
			return fmt.Errorf("%w\nhint: %s were not reached; every completed result is saved, "+
				"so re-running `bookscape check` resumes from here", err,
				plural(remaining, "link", "links"))
		}

		return err
	}

	a.printf("\n")

	if stats.Uncertain > 0 {
		a.printf("Re-run `bookscape check --only=uncertain` later to retry the inconclusive bucket.\n")
	}

	a.printf("Next: bookscape classify\n")

	return nil
}

// printLivenessSummary reports the whole corpus's state, which is the useful thing to show when
// the requested scope turned out to be empty.
func (a *App) printLivenessSummary(ctx context.Context, db *store.DB) error {
	counts, err := db.LivenessCounts(ctx)
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

	a.printf("\nCorpus state across %s:\n", plural(total, "surviving link", "surviving links"))

	for _, key := range []string{"alive", "uncertain", "dead", "skipped", "unchecked"} {
		if n, ok := counts[key]; ok {
			a.printf("  %-10s %d\n", key, n)
		}
	}

	return nil
}
