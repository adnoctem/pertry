// Package classify assigns a collection, tags and a description to each bookmark.
//
// Two passes, cheap first. The deterministic rules in bookscape.yaml run over everything and
// resolve a large share of a typical decade-old collection at zero token cost - code hosts,
// Q&A sites, documentation domains, video platforms. The model pass then sees only what the
// rules could not decide. Every link a rule resolves is a link not spent on tokens
// (docs/ARCHITECTURE.md#classification).
package classify

import (
	"strings"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

// RuleEngine evaluates the configured rules against a bookmark.
type RuleEngine struct {
	rules []config.Rule
}

// NewRuleEngine builds an engine for the configured rules, in the order they are written.
func NewRuleEngine(rules []config.Rule) *RuleEngine {
	return &RuleEngine{rules: rules}
}

// Match is what the rules decided about one bookmark.
type Match struct {
	// Collection is from the first matching rule that names one. Empty if no rule did,
	// which leaves the bookmark for the model pass even if a rule did tag it.
	Collection string

	// Tags accumulate from every matching rule, deduplicated. Tags are many-per-link by
	// nature, so there is no reason to let the first match suppress the rest: a
	// github.com link filed under Dev/Go is legitimately both a repo and a dev bookmark.
	Tags []string

	// Matched records which rules matched, by index, for logging and for explaining a
	// classification during review.
	Matched []int
}

// Decided reports whether the rules resolved the bookmark's collection. A bookmark that only
// picked up tags is not decided: the collection is the single "where does this belong" axis,
// and without one there is nothing to export under.
func (m Match) Decided() bool {
	return m.Collection != ""
}

// Apply evaluates every rule against a target.
func (e *RuleEngine) Apply(t store.ClassifyTarget) Match {
	var m Match

	for i, rule := range e.rules {
		if !matches(rule, t) {
			continue
		}

		m.Matched = append(m.Matched, i)

		// First match wins for the collection, because a link belongs in exactly one.
		if m.Collection == "" && rule.Collection != "" {
			m.Collection = rule.Collection
		}

		m.Tags = appendUnique(m.Tags, rule.Tags...)
	}

	return m
}

// matches reports whether a rule applies to a target. A rule with both match lists applies if
// either matches: the lists are alternatives, not conjuncts.
func matches(rule config.Rule, t store.ClassifyTarget) bool {
	for _, pattern := range rule.MatchDomain {
		if matchDomain(pattern, t.Host) {
			return true
		}
	}

	for _, pattern := range rule.MatchSourcePath {
		for _, path := range t.SourcePaths {
			if matchSourcePath(pattern, path) {
				return true
			}
		}
	}

	return false
}

// matchDomain matches a host against a pattern.
//
// A bare pattern is an exact hostname match. The host it is compared against comes from
// url_norm, so it is already lowercased and - with the default policy - www-stripped, which is
// why "github.com" matches a bookmark saved as "https://www.github.com/...".
//
// A "*." prefix matches subdomains only: "*.stackexchange.com" matches
// "superuser.stackexchange.com" but not "stackexchange.com" itself. That is the precise
// reading, and the config example in cmd/bookscape/README.md relies on it - the Q&A sites are the
// subdomains. List the bare domain as well to cover both.
func matchDomain(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = strings.ToLower(host)

	if pattern == "" || host == "" {
		return false
	}

	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}

	return host == pattern
}

// matchSourcePath matches an original folder path against a pattern.
//
// A bare pattern is an exact path match. A "/*" suffix matches the folder itself and everything
// below it: "Dev/*" matches "Dev", "Dev/Go" and "Dev/Go/stdlib". Including the folder itself is
// deliberate - a link filed directly in Dev carries the same signal as one filed in Dev/Go, and
// a pattern that missed it would be a trap.
//
// Matching is case-insensitive, because folder names are user-authored and a decade of them is
// not consistently capitalised.
func matchSourcePath(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || path == "" {
		return false
	}

	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.EqualFold(path, prefix) ||
			strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix)+"/")
	}

	return strings.EqualFold(path, pattern)
}

// appendUnique appends values not already present, preserving order.
func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}

		if !slicesContainsFold(dst, v) {
			dst = append(dst, v)
		}
	}

	return dst
}

func slicesContainsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}

	return false
}
