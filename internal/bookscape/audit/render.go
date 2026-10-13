package audit

import (
	"fmt"
	"io"
	"strings"
)

// barWidth is the widest a proportional bar gets, so the output stays readable in a standard
// terminal alongside a label and two percentages.
const barWidth = 24

// Render writes the report.
//
// The ordering is deliberate: what the file contains, then what the current configuration does
// with it, then what is left over and what would fix it. Someone tuning rules reads from the
// bottom, and someone checking whether a file is safe to import reads from the top.
func Render(w io.Writer, r Report, cfg Options) error {
	out := &writer{w: w}

	out.printf("Document\n")
	out.printf("  title            %s\n", quoteOrDash(r.Title))
	out.printf("  bookmarks        %d\n", r.Entries)
	out.printf("  folders          %d\n", r.Folders)

	// The count reconciliation and the date range check are the two things that make an
	// import refuse, so audit reports them before anything else: this is the cheap way to
	// find out whether the file is importable at all.
	if r.Dropped > 0 {
		out.printf("  anchors in file  %d  ** %d MORE than were parsed **\n", r.AnchorTags, r.Dropped)
		out.printf("\n  `bookscape import` will refuse this file. The parser matches element and attribute\n")
		out.printf("  names case-sensitively and drops anything else without an error; check for lowercase\n")
		out.printf("  <a> or href in the export.\n")
	}

	if n := len(r.SuspectDates); n > 0 {
		out.printf("  implausible dates %d  ** import will refuse **\n", n)

		for _, s := range r.SuspectDates[:min(n, 3)] {
			out.printf("      %s -> %s\n", s.Field, s.Decoded.Format("2006-01-02T15:04:05Z"))
		}
	}

	if r.Undated > 0 {
		out.printf("  undated          %d  (will export without an ADD_DATE)\n", r.Undated)
	}

	if r.EmptyTitles > 0 {
		out.printf("  empty titles     %d  (a generated description is the only way to tell these apart)\n",
			r.EmptyTitles)
	}

	if r.NormalizeErrors > 0 {
		out.printf("  unparsable URLs  %d  (import will refuse: these cannot get a comparison key)\n",
			r.NormalizeErrors)
	}

	out.printf("\nDeduplication, under the configured normalise policy\n")
	out.printf("  duplicate groups %d\n", r.DuplicateGroups)
	out.printf("  rows collapsed   %d\n", r.DuplicateRows)
	out.printf("  survivors        %d  <- everything below is measured against these\n", r.Survivors)

	out.section("Schemes", r.Schemes, false)
	// Hosts are shown as the rules match them - normalised, so lowercased and (by default)
	// without "www." - because the point of this list is to write rules from it.
	out.section("Top domains (as the rules match them)", r.Domains, true)
	out.section("Top original folders", r.Paths, true)

	if len(r.Tags) > 0 {
		out.section("Tags already in the file", r.Tags, false)
	}

	out.section("Saved per year", r.Years, false)

	// --- what the current configuration does ---
	out.printf("\nRule coverage, with the current configuration\n")
	out.printf("  resolved by rules  %4d  (%.0f%%)  free, deterministic\n", r.Resolved, r.ResolvedShare())

	if r.TaggedOnly > 0 {
		out.printf("  tagged but not     %4d           matched a tag-only rule, still needs a collection\n",
			r.TaggedOnly)
	}

	out.printf("  left for the model %4d  (%.0f%%)  this is what a model pass would cost you\n",
		r.Unresolved, share(r.Unresolved, r.Survivors))

	if len(r.RuleHits) == 0 {
		out.printf("\n  No rules are configured. Everything goes to the model.\n")
	} else {
		out.printf("\n  %-4s %-9s %-9s %s\n", "rule", "matched", "decided", "matches on")

		for _, h := range r.RuleHits {
			flag := ""
			if h.N == 0 {
				flag = "  <- never matches"
			} else if h.Decisive == 0 && h.Collection != "" {
				// A rule that matches but never decides is shadowed by an earlier
				// one. Worth flagging: it is almost always a mistake in rule order.
				flag = "  <- always shadowed by an earlier rule"
			}

			out.printf("  %-4d %-9d %-9d %s%s\n", h.Index, h.N, h.Decisive, h.Match, flag)
		}
	}

	// --- what is left, and what would fix it ---
	if r.Unresolved > 0 {
		out.section("Uncovered domains (share of the unresolved remainder)", r.UncoveredDomains, true)
		out.section("Uncovered folders (share of the unresolved remainder)", r.UncoveredPaths, true)
	}

	if len(r.Suggestions) > 0 {
		out.printf("\nSuggested rules\n")
		out.printf("  Adopting the domain rules below would take rule coverage from %d to %d of %d links\n",
			r.Resolved, r.Projected, r.Survivors)
		out.printf("  (%.0f%% to %.0f%%), at no token cost. Folder suggestions are listed separately because\n",
			r.ResolvedShare(), r.ProjectedShare())
		out.printf("  they may overlap the domain rules, and overstating the gain would be worse than\n")
		out.printf("  understating it.\n\n")
		out.printf("  Paste into bookscape.yaml under classify.rules, then set each collection:\n\n")

		for _, s := range r.Suggestions {
			switch {
			case s.MatchDomain != "":
				out.printf("    - match_domain: [%s]\n", s.MatchDomain)
			default:
				out.printf("    - match_source_path: [%q]\n", s.MatchSourcePath+"/*")
			}

			out.printf("      collection: # ? %d link%s\n", s.N, plural(s.N))
		}

		out.printf("\n  Every collection must already be listed in classify.collections, or the config is\n")
		out.printf("  rejected - that is the closed set doing its job.\n")
	}

	if cfg.Suggest > 0 && len(r.Suggestions) == 0 && r.Unresolved > 0 {
		out.printf("\nNo rule is worth suggesting: no uncovered domain or folder holds at least %d links.\n",
			max(cfg.MinSuggest, 1))
		out.printf("The remainder is a long tail, which is exactly what the model pass is for.\n")
	}

	return out.err
}

// section renders one counted breakdown.
func (w *writer) section(title string, counts []Count, cumulative bool) {
	if len(counts) == 0 {
		return
	}

	width, maxN := 0, 0

	for _, c := range counts {
		if len(c.Label) > width {
			width = len(c.Label)
		}

		if c.N > maxN {
			maxN = c.N
		}
	}

	width = min(width, 52)

	w.printf("\n%s\n", title)

	for _, c := range counts {
		label := c.Label
		if len(label) > width {
			label = label[:width-1] + "…"
		}

		if cumulative {
			w.printf("  %-*s %6d  %5.1f%%  %5.1f%% cum  %s\n",
				width, label, c.N, c.Share, c.Cumulative, bar(c.N, maxN))

			continue
		}

		w.printf("  %-*s %6d  %5.1f%%  %s\n", width, label, c.N, c.Share, bar(c.N, maxN))
	}
}

// writer accumulates the first write error so Render does not check every printf.
type writer struct {
	w   io.Writer
	err error
}

func (w *writer) printf(format string, args ...any) {
	if w.err != nil {
		return
	}

	if _, err := fmt.Fprintf(w.w, format, args...); err != nil {
		w.err = fmt.Errorf("write audit report: %w", err)
	}
}

func bar(n, maxN int) string {
	if maxN <= 0 {
		return ""
	}

	filled := n * barWidth / maxN
	if filled == 0 && n > 0 {
		filled = 1
	}

	return strings.Repeat("#", filled)
}

func quoteOrDash(s string) string {
	if s == "" {
		return "(none)"
	}

	return fmt.Sprintf("%q", s)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}
