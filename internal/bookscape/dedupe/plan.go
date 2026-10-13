package dedupe

import (
	"fmt"
	"strings"

	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// Reason is recorded in merge_log for every collapsed row. There is only one reason for now -
// the comparison keys matched - but the column exists so that a later, less mechanical merge
// rule stays distinguishable from this one.
const Reason = "url_norm match"

// Plan is the set of merges a corpus needs, plus what they would change.
type Plan struct {
	// Merges is one entry per duplicate group, in comparison-key order.
	Merges []store.Merge

	// Groups and Collapsed summarise the plan: how many keys have duplicates, and how many
	// rows would stop being survivors.
	Groups    int
	Collapsed int
}

// Empty reports whether there is nothing to do.
func (p Plan) Empty() bool {
	return len(p.Merges) == 0
}

// Build elects a survivor for every duplicate group and merges the group's field values onto
// it (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
//
// Groups of one are not merges and are skipped. Every other group produces an entry, even one
// that is already settled: whether anything actually changes is decided when the plan is
// applied, which is what makes dedupe re-runnable and what lets --dry-run over a settled
// corpus report zero changes rather than re-listing every past merge.
func Build(groups []store.Group) Plan {
	var plan Plan

	for _, g := range groups {
		if len(g.Candidates) < 2 {
			continue
		}

		merge := mergeGroup(g)
		plan.Merges = append(plan.Merges, merge)
		plan.Groups++
		plan.Collapsed += len(merge.LoserIDs)
	}

	return plan
}

// mergeGroup resolves one duplicate group.
//
// store.DuplicateGroups returns candidates in election order, so the survivor is the first
// one: a row with a known date beats one without, then earliest date wins, then lowest id.
// Choosing the earliest means the survivor is the original save rather than a later re-save.
func mergeGroup(g store.Group) store.Merge {
	survivor := g.Candidates[0]

	m := store.Merge{
		URLNorm:      g.URLNorm,
		SurvivorID:   survivor.ID,
		AddDate:      minDate(g.Candidates),
		LastModified: maxLastModified(g.Candidates),
		Title:        longest(g.Candidates, func(c store.Candidate) string { return c.Title }),
		Description:  longest(g.Candidates, func(c store.Candidate) string { return c.Description }),
		Private:      anyPrivate(g.Candidates),
		SourcePaths:  store.MergedPaths(g.Candidates),
		Tags:         store.MergedTags(g.Candidates),
		Reason:       Reason,
	}

	for _, c := range g.Candidates[1:] {
		m.LoserIDs = append(m.LoserIDs, c.ID)
	}

	return m
}

// minDate is the earliest known add_date in the group, or nil if no row has one. Taking the
// minimum rather than the survivor's own value matters when the earliest-dated row is not the
// one that wins election for some other reason.
func minDate(candidates []store.Candidate) *int64 {
	var best *int64

	for _, c := range candidates {
		if c.AddDate == nil {
			continue
		}

		if best == nil || *c.AddDate < *best {
			best = c.AddDate
		}
	}

	return best
}

// maxLastModified is the latest known last_modified in the group.
//
// The design document specifies the minimum for add_date and is silent on last_modified. The
// maximum is the complement of that choice and the only one that makes sense: the group's
// identity is "first saved at X, last touched at Y", so taking the earliest creation and the
// latest modification keeps both endpoints rather than collapsing them.
func maxLastModified(candidates []store.Candidate) *int64 {
	var best *int64

	for _, c := range candidates {
		if c.LastModified == nil {
			continue
		}

		if best == nil || *c.LastModified > *best {
			best = c.LastModified
		}
	}

	return best
}

// longest picks the longest non-empty value in the group.
//
// Chrome frequently stores "" or the bare hostname as a title, so the longest one is almost
// always the most informative. Ties keep the first, which is the survivor's own value by
// election order.
func longest(candidates []store.Candidate, field func(store.Candidate) string) string {
	best := ""

	for _, c := range candidates {
		if v := strings.TrimSpace(field(c)); len(v) > len(best) {
			best = v
		}
	}

	return best
}

// anyPrivate marks the survivor private if any row in the group was.
//
// Not specified by the design document. Erring toward private is the only safe direction: a
// user who marked one copy of a link private did so deliberately, and silently un-marking it
// because another copy was public would leak something they chose to hide.
func anyPrivate(candidates []store.Candidate) bool {
	for _, c := range candidates {
		if c.Private {
			return true
		}
	}

	return false
}

// Describe renders one merge for --dry-run.
//
// Every collapse prints both original URLs, because stripping "www." and forcing https will
// merge a small number of hosts that are genuinely distinct, and sorting query parameters will
// merge URLs where parameter order is meaningful. The merge log has to be auditable by eye
// before anything is written (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
func Describe(m store.Merge, candidates []store.Candidate) string {
	var b strings.Builder

	byID := make(map[int64]store.Candidate, len(candidates))
	for _, c := range candidates {
		byID[c.ID] = c
	}

	fmt.Fprintf(&b, "%s\n", m.URLNorm)
	fmt.Fprintf(&b, "  keep  [%d] %s\n", m.SurvivorID, byID[m.SurvivorID].URL)

	for _, id := range m.LoserIDs {
		fmt.Fprintf(&b, "  merge [%d] %s\n", id, byID[id].URL)
	}

	if paths := m.SourcePaths; len(paths) > 0 {
		fmt.Fprintf(&b, "  paths %s\n", strings.Join(paths, " | "))
	}

	if m.Title != "" {
		fmt.Fprintf(&b, "  title %s\n", m.Title)
	}

	return b.String()
}
