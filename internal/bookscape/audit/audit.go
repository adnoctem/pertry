// Package audit inspects a Netscape bookmark export without importing it, and reports what the
// configured rules would and would not resolve.
//
// It exists so that tuning bookscape.yaml is self-service and deterministic. The classification
// rules in section 5.5 are only as good as the corpus they were written for, and the default set
// is written for nobody's corpus in particular - on a real export it resolved under a quarter of
// the links, because the folders were named things like "Products" and "DSGN" rather than "Dev".
// Finding that out by running the model pass and reading the bill is the expensive way.
//
// Nothing here touches the database or the network. `audit` is a pure function of the file and the
// config, so it can be re-run freely while editing rules.
package audit

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/adnoctem/pertry/internal/bookscape/classify"
	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/dedupe"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// Count is one labelled tally, with its share of the whole and the running total up to and
// including it. Cumulative share is the number that actually guides tuning: it answers "how much
// of the corpus do I cover if I write rules down to here".
type Count struct {
	Label      string
	N          int
	Share      float64
	Cumulative float64
}

// RuleHit is how often one configured rule matched, and what it did.
type RuleHit struct {
	// Index is the rule's position in classify.rules, which is how the config identifies it.
	Index int

	// Match is a short description of what the rule matches on, for the report.
	Match string

	// Collection is what it assigns, empty for a tag-only rule.
	Collection string

	// N is how many links it matched. A rule matching zero links is the single most useful
	// thing this report can tell you.
	N int

	// Decisive is how many links it was the *first* matching rule for, and so actually
	// decided the collection of. A rule can match often and decide nothing if an earlier
	// rule always wins.
	Decisive int
}

// Suggestion is a proposed rule for an uncovered domain or folder.
type Suggestion struct {
	// MatchDomain or MatchSourcePath, exactly one of which is set.
	MatchDomain     string
	MatchSourcePath string

	// N is how many currently-unresolved links it would resolve.
	N int
}

// Report is everything audit found.
type Report struct {
	// Parse results, straight from the netscape wrapper.
	Title   string
	Entries int

	// Survivors is how many links remain after deduplication under the configured
	// normalise policy. Rule coverage is measured against this, not against Entries,
	// because it is what `classify` will see.
	Survivors int

	AnchorTags   int
	Dropped      int
	Folders      int
	Undated      int
	EmptyTitles  int
	SuspectDates []netscape.SuspectDate

	// Normalisation outcome. Deterministic from the configured policy, so this is what
	// `dedupe` would do without having to import first.
	NormalizeErrors int
	DuplicateGroups int
	DuplicateRows   int

	Schemes []Count
	Domains []Count
	Paths   []Count
	Years   []Count
	Tags    []Count

	// Rule coverage under the current config.
	Resolved   int
	TaggedOnly int
	Unresolved int
	RuleHits   []RuleHit

	// What is left over, and what would fix it.
	UncoveredDomains []Count
	UncoveredPaths   []Count
	Suggestions      []Suggestion

	// Projected is the rule coverage that would result from adopting every suggestion.
	Projected int
}

// ResolvedShare is the fraction of surviving links the configured rules would classify.
func (r Report) ResolvedShare() float64 {
	return share(r.Resolved, r.Survivors)
}

// ProjectedShare is the fraction the rules would classify with the suggestions adopted.
func (r Report) ProjectedShare() float64 {
	return share(r.Projected, r.Survivors)
}

// Options bound the report's size.
type Options struct {
	// Top caps each breakdown. Zero means no cap.
	Top int

	// Suggest caps how many rules to propose. Zero disables suggestions.
	Suggest int

	// MinSuggest is the fewest links a domain or folder must hold before it is worth a rule
	// of its own. Below this the long tail is the model's job.
	MinSuggest int
}

// Analyze inspects a parsed document against a configuration.
func Analyze(entries []netscape.Entry, parsed netscape.Report, cfg *config.Config, opts Options) Report {
	r := Report{
		Title:        parsed.Title,
		Entries:      len(entries),
		AnchorTags:   parsed.AnchorTags,
		Dropped:      parsed.Dropped(len(entries)),
		Folders:      parsed.Folders,
		Undated:      parsed.UndatedEntries,
		SuspectDates: parsed.SuspectDates,
	}

	normalizer := dedupe.NewNormalizer(cfg.Normalize)
	engine := classify.NewRuleEngine(cfg.Classify.Rules)

	var (
		schemes = tally{}
		domains = tally{}
		paths   = tally{}
		years   = tally{}
		tags    = tally{}
		norms   = tally{}

		// Links no rule resolved, kept so that suggestions are measured against the
		// remainder rather than the whole corpus.
		unresolvedDomains = tally{}
		unresolvedPaths   = tally{}
	)

	hits := make([]RuleHit, len(cfg.Classify.Rules))
	for i, rule := range cfg.Classify.Rules {
		hits[i] = RuleHit{Index: i, Match: describeMatch(rule), Collection: rule.Collection}
	}

	// Survivors, keyed by comparison key, each carrying the union of its group's folder
	// paths. This reproduces what `dedupe` will do, so that rule coverage below is measured
	// against what `classify` will actually see rather than against the raw file.
	type survivor struct {
		host  string
		paths map[string]struct{}
	}

	survivors := map[string]*survivor{}

	for _, e := range entries {
		if e.Title == "" {
			r.EmptyTitles++
		}

		schemes.add(schemeOf(e.URL))

		for _, t := range e.Tags {
			tags.add(t)
		}

		if e.AddDate != nil {
			years.add(yearOf(*e.AddDate))
		}

		norm, err := normalizer.Normalize(e.URL)
		if err != nil {
			// No comparison key means `import` will refuse the file, so this is
			// counted and the entry is excluded from everything downstream.
			r.NormalizeErrors++

			continue
		}

		norms.add(norm)

		// Hosts and paths are tallied as the *rules* see them, not as the file spells
		// them: the host comes from the normalised URL, so it has already been
		// lowercased and - under the default policy - had "www." stripped. Tallying the
		// raw host would list "www.youtube.com" as uncovered while the rule matching
		// "youtube.com" resolves it, which is exactly backwards for someone writing
		// rules.
		host := hostOf(norm)
		if host != "" {
			domains.add(host)
		}

		if e.Path != "" {
			paths.add(e.Path)
		}

		s, ok := survivors[norm]
		if !ok {
			s = &survivor{host: host, paths: map[string]struct{}{}}
			survivors[norm] = s
		}

		if e.Path != "" {
			s.paths[e.Path] = struct{}{}
		}
	}

	for _, group := range norms {
		if group > 1 {
			r.DuplicateGroups++
			r.DuplicateRows += group - 1
		}
	}

	r.Survivors = len(survivors)

	for _, s := range survivors {
		target := store.ClassifyTarget{Host: s.host, SourcePaths: slices.Sorted(maps.Keys(s.paths))}

		match := engine.Apply(target)

		for _, i := range match.Matched {
			hits[i].N++
		}

		if decisive := firstDeciding(cfg.Classify.Rules, match.Matched); decisive >= 0 {
			hits[decisive].Decisive++
		}

		if match.Decided() {
			r.Resolved++

			continue
		}

		if len(match.Tags) > 0 {
			r.TaggedOnly++
		}

		r.Unresolved++

		if s.host != "" {
			unresolvedDomains.add(s.host)
		}

		for path := range s.paths {
			unresolvedPaths.add(path)
		}
	}

	r.Schemes = schemes.counts(0, len(entries))
	r.Domains = domains.counts(opts.Top, len(entries))
	r.Paths = paths.counts(opts.Top, len(entries))
	r.Tags = tags.counts(opts.Top, len(entries))
	r.Years = years.chronological(len(entries))
	r.RuleHits = hits

	r.UncoveredDomains = unresolvedDomains.counts(opts.Top, r.Unresolved)
	r.UncoveredPaths = unresolvedPaths.counts(opts.Top, r.Unresolved)
	r.Suggestions, r.Projected = suggest(unresolvedDomains, unresolvedPaths, r.Resolved, opts)

	return r
}

// suggest proposes rules for the heaviest uncovered domains, then the heaviest uncovered folders.
//
// Domains first because a domain rule is unambiguous - a host either matches or it does not -
// whereas a folder name is the user's own vocabulary and may mean something a rule cannot infer.
// Folder suggestions are therefore only offered once the domains are exhausted.
func suggest(domains, paths tally, resolved int, opts Options) ([]Suggestion, int) {
	if opts.Suggest <= 0 {
		return nil, resolved
	}

	minimum := max(opts.MinSuggest, 1)

	var out []Suggestion

	projected := resolved

	for _, c := range domains.ordered() {
		if len(out) >= opts.Suggest || c.N < minimum {
			break
		}

		out = append(out, Suggestion{MatchDomain: c.Label, N: c.N})
		projected += c.N
	}

	// Folder suggestions would double-count any link already covered by a domain suggestion
	// above, so they are offered as a separate list and deliberately not added to the
	// projection. Overstating the projected gain would be worse than understating it.
	for _, c := range paths.ordered() {
		if len(out) >= opts.Suggest || c.N < minimum {
			break
		}

		out = append(out, Suggestion{MatchSourcePath: c.Label, N: c.N})
	}

	return out, projected
}

// firstDeciding returns the index of the first matched rule that assigns a collection, which is
// the one that actually decided the link, or -1 when none did.
func firstDeciding(rules []config.Rule, matched []int) int {
	for _, i := range matched {
		if rules[i].Collection != "" {
			return i
		}
	}

	return -1
}

// describeMatch renders a rule's match criteria for the report.
func describeMatch(rule config.Rule) string {
	var parts []string

	if len(rule.MatchDomain) > 0 {
		parts = append(parts, "domain "+strings.Join(rule.MatchDomain, ", "))
	}

	if len(rule.MatchSourcePath) > 0 {
		parts = append(parts, "path "+strings.Join(rule.MatchSourcePath, ", "))
	}

	return strings.Join(parts, " / ")
}

// tally counts labels.
type tally map[string]int

func (t tally) add(label string) {
	if label != "" {
		t[label]++
	}
}

// ordered returns the tally sorted by count descending, ties broken by label so the output is
// stable across runs.
func (t tally) ordered() []Count {
	out := make([]Count, 0, len(t))
	for label, n := range t {
		out = append(out, Count{Label: label, N: n})
	}

	slices.SortFunc(out, func(a, b Count) int {
		if a.N != b.N {
			return b.N - a.N
		}

		return strings.Compare(a.Label, b.Label)
	})

	return out
}

// counts returns the top entries with shares filled in. Cumulative share is computed over the
// whole tally before truncation, so "the top 20 cover 42%" stays true regardless of the cap.
func (t tally) counts(top, total int) []Count {
	out := t.ordered()

	running := 0
	for i := range out {
		running += out[i].N
		out[i].Share = share(out[i].N, total)
		out[i].Cumulative = share(running, total)
	}

	if top > 0 && len(out) > top {
		out = out[:top]
	}

	return out
}

// chronological returns the tally sorted by label, for the year histogram.
func (t tally) chronological(total int) []Count {
	out := t.ordered()

	slices.SortFunc(out, func(a, b Count) int { return strings.Compare(a.Label, b.Label) })

	for i := range out {
		out[i].Share = share(out[i].N, total)
	}

	return out
}

// Distinct is how many labels a tally holds, which Analyze reports for domains.
func (t tally) Distinct() int { return len(t) }

func share(n, total int) float64 {
	if total == 0 {
		return 0
	}

	return float64(n) / float64(total) * 100
}

// schemeOf extracts a URL's scheme for the breakdown, without a full parse: a bookmark file
// contains plenty of URLs net/url rejects, and they still need counting.
func schemeOf(rawURL string) string {
	if i := strings.Index(rawURL, ":"); i > 0 {
		return strings.ToLower(rawURL[:i])
	}

	return "(none)"
}

// hostOf returns the lowercased hostname, or the empty string for a URL with no host - a
// javascript: or place: bookmark, of which a real collection has plenty. Port and userinfo are
// excluded, so this matches what domain rules compare against.
func hostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}

	return strings.ToLower(u.Hostname())
}

// yearOf renders a unix timestamp as a four-digit year for the histogram.
func yearOf(unixSeconds int64) string {
	return time.Unix(unixSeconds, 0).UTC().Format("2006")
}
