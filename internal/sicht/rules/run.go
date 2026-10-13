package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/meta"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// saveBatch is how many results accumulate before being written.
const saveBatch = 500

// RunOptions configures a rules pass.
type RunOptions struct {
	// DB is the workspace database. Required.
	DB *store.DB

	// Config supplies the rules and the screen-resolution list. Required.
	Config *config.Config

	// Retract removes every existing rule result before evaluating.
	//
	// On by default at the CLI, because leaving them is actively wrong: an edited rule set
	// changes what each index means, and a result from "rule:3" survives after rule 3 has
	// been deleted or rewritten, deciding files by a rule that no longer exists.
	Retract bool

	// DryRun evaluates and reports without writing anything.
	DryRun bool

	// Limit stops after this many files. Zero means all of them.
	Limit int

	Log *slog.Logger

	Progress func(RunProgress)
}

// RunProgress reports how far a pass has got.
type RunProgress struct {
	Done  int
	Total int
}

// RunResult summarises a pass.
type RunResult struct {
	// Files is how many files were evaluated.
	Files int

	// Decided is how many files got at least one decision.
	Decided int

	// Decisions is how many facet values were produced in total.
	Decisions int

	// Retracted is how many previous rule results were removed.
	Retracted int64

	// PerFacet counts decisions by facet.
	PerFacet map[string]int

	// PerRule counts decisions by rule index, which is what makes a rule's real yield
	// visible - a rule that never fires is worth knowing about.
	PerRule map[int]int

	// PerValue counts decisions by facet and value.
	PerValue map[string]map[string]int

	// Undecided counts, per facet the rule set mentions, how many files it could not decide.
	// This is the number that sizes the model pass, and therefore its cost.
	Undecided map[string]int

	// Errors are per-file evaluation failures, already logged.
	Errors []error
}

// Run evaluates the configured rules over every scanned file.
//
// Rules are deterministic and cheap, so this re-evaluates everything rather than trying to work
// out what changed: over the reference archive it is a sub-second operation, and a clever
// incremental version would be a source of staleness bugs for no benefit.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	result := RunResult{
		PerFacet:  map[string]int{},
		PerRule:   map[int]int{},
		PerValue:  map[string]map[string]int{},
		Undecided: map[string]int{},
	}

	if opts.DB == nil || opts.Config == nil {
		return result, errors.New("rules: database and configuration are required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	set, err := Compile(opts.Config)
	if err != nil {
		return result, err
	}

	if set.Len() == 0 {
		return result, nil
	}

	if opts.Retract && !opts.DryRun {
		// Every rule result, not just the ones this pass would replace: a file that no
		// longer matches any rule must lose its decision, and an upsert alone cannot
		// express that.
		retracted, err := opts.DB.DeleteResultsBySource(ctx, "rule:")
		if err != nil {
			return result, err
		}

		result.Retracted = retracted

		log.Info("retracted previous rule results", "count", retracted)
	}

	counts, err := opts.DB.Count(ctx)
	if err != nil {
		return result, err
	}

	now := time.Now().UTC()

	pending := make([]store.Result, 0, saveBatch)

	flush := func() error {
		if opts.DryRun || len(pending) == 0 {
			pending = pending[:0]

			return nil
		}

		if err := opts.DB.SaveResults(ctx, pending); err != nil {
			return err
		}

		pending = pending[:0]

		return nil
	}

	stop := errors.New("limit reached")

	err = opts.DB.EachFileForRules(ctx, func(in store.RuleInput) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if opts.Limit > 0 && result.Files >= opts.Limit {
			return stop
		}

		result.Files++

		env := envFor(in, opts.Config, log)

		decisions, evalErr := set.Evaluate(env)
		if evalErr != nil {
			result.Errors = append(result.Errors, evalErr)

			log.Warn("rule evaluation failed for a file", "path", in.Path, "error", evalErr)
		}

		if len(decisions) > 0 {
			result.Decided++
		}

		for _, d := range decisions {
			result.Decisions++
			result.PerFacet[d.Facet]++
			result.PerRule[d.RuleIndex]++

			if result.PerValue[d.Facet] == nil {
				result.PerValue[d.Facet] = map[string]int{}
			}

			result.PerValue[d.Facet][d.Value]++

			pending = append(pending, store.Result{
				Hash:          in.Hash,
				Facet:         d.Facet,
				Value:         d.Value,
				Confidence:    d.Confidence,
				HasConfidence: d.Confidence > 0,
				Source:        d.Source(),
				CreatedAt:     now,
			})
		}

		if len(pending) >= saveBatch {
			if err := flush(); err != nil {
				return err
			}
		}

		if opts.Progress != nil {
			opts.Progress(RunProgress{Done: result.Files, Total: counts.Files})
		}

		return nil
	})

	switch {
	case errors.Is(err, stop):
		// Not an error: --limit did what it was asked.
	case err != nil:
		return result, err
	}

	if err := flush(); err != nil {
		return result, err
	}

	// What the rules could not decide, per facet they cover. In a dry run the database still
	// holds the previous pass's results, so the honest answer is computed from this pass
	// rather than queried.
	for _, facet := range set.Facets() {
		if opts.DryRun {
			result.Undecided[facet] = result.Files - result.PerFacet[facet]

			continue
		}

		undecided, err := opts.DB.UndecidedHashes(ctx, facet)
		if err != nil {
			return result, err
		}

		result.Undecided[facet] = len(undecided)
	}

	log.Info("rules finished",
		"files", result.Files,
		"decided", result.Decided,
		"decisions", result.Decisions)

	return result, nil
}

// envFor builds the evaluation environment for one stored file.
func envFor(in store.RuleInput, cfg *config.Config, log *slog.Logger) Env {
	file := File{
		Path:              in.Path,
		Dir:               path.Dir(in.Path),
		Name:              path.Base(in.Path),
		Ext:               in.Ext,
		Size:              in.Size,
		Width:             in.Width,
		Height:            in.Height,
		ScreenResolutions: cfg.ScreenResolutions,
	}

	if file.Dir == "." {
		// path.Dir returns "." for a bare filename; "" reads better in a rule.
		file.Dir = ""
	}

	if in.ExifJSON != "" {
		var tags map[string]any

		if err := json.Unmarshal([]byte(in.ExifJSON), &tags); err != nil {
			log.Warn("stored metadata is not valid JSON", "path", in.Path, "error", err)
		} else {
			file.Exif = tags
		}
	}

	if in.SidecarJSON != "" {
		file.HasSidecar = true

		sidecar, err := meta.ParseSidecar([]byte(in.SidecarJSON))
		if err != nil {
			log.Warn("stored sidecar is not valid JSON", "path", in.Path, "error", err)
		} else if taken, ok := sidecar.TakenAt(); ok {
			file.Taken = taken
		}
	}

	// EXIF is the fallback for capture time when there is no sidecar, or the sidecar had no
	// usable timestamp. DateTimeOriginal is what the camera recorded; it has no timezone, so
	// it is read as UTC - which is wrong by at most a day's offset and right for the year,
	// which is all any rule here uses it for.
	if file.Taken.IsZero() && file.Exif != nil {
		if taken, ok := exifTime(file.Exif); ok {
			file.Taken = taken
		}
	}

	return NewEnv(file)
}

// exifDateLayout is EXIF's own date format. Not RFC 3339: it uses colons in the date.
const exifDateLayout = "2006:01:02 15:04:05"

func exifTime(exif map[string]any) (time.Time, bool) {
	for _, key := range []string{
		"ExifIFD:DateTimeOriginal", "DateTimeOriginal",
		"ExifIFD:CreateDate", "CreateDate",
		"IFD0:ModifyDate", "ModifyDate",
	} {
		raw, ok := exif[key].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}

		// A camera that never had its clock set writes all zeroes, which parses to year 0
		// and would otherwise be reported as a capture date.
		if strings.HasPrefix(raw, "0000") {
			continue
		}

		if parsed, err := time.Parse(exifDateLayout, strings.TrimSpace(raw)); err == nil {
			return parsed.UTC(), true
		}
	}

	return time.Time{}, false
}

// RuleYield pairs a rule with how many decisions it produced, for reporting.
type RuleYield struct {
	Index      int
	Facet      string
	Value      string
	Confidence float64
	Source     string
	Count      int
}

// Yields returns one entry per configured rule, in configuration order, with its decision count.
//
// Rules that fired zero times are included on purpose: a rule that never matches is the single
// most useful thing a report can point at, and omitting it would hide it.
func (s *Set) Yields(counts map[int]int) []RuleYield {
	out := make([]RuleYield, 0, len(s.rules))

	for _, r := range s.rules {
		out = append(out, RuleYield{
			Index:      r.Index,
			Facet:      r.Facet,
			Value:      r.Value,
			Confidence: r.Confidence,
			Source:     r.Source,
			Count:      counts[r.Index],
		})
	}

	return out
}

// Describe renders a rule's expression, shortened for a report.
func (y RuleYield) Describe(maxLen int) string {
	expression := strings.Join(strings.Fields(y.Source), " ")

	if maxLen > 3 && len(expression) > maxLen {
		expression = expression[:maxLen-1] + "…"
	}

	return fmt.Sprintf("%s = %s", y.Facet, y.Value) + "  " + expression
}
