// Package dedupe normalises URLs into a comparison key and collapses the duplicates that key
// reveals.
//
// Deduplication is non-destructive. Losers are retained with canonical_id pointing at their
// survivor and the merge is recorded in merge_log; nothing is deleted
// (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
package dedupe

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adnoctem/pertry/internal/bookscape/config"
)

// TrackingParams are the query parameters stripped when
// config.Normalize.StripTrackingParams is set. They carry campaign attribution and never
// affect which page is served, so two URLs differing only in these are the same bookmark
// (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
var TrackingParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"fbclid", "gclid", "msclkid", "mc_cid", "mc_eid", "igshid", "ref_src",
}

// Normalizer derives a comparison key from a URL according to a configured policy.
//
// Normalisation exists solely to make two URLs comparable. The original URL is always retained
// in bookmarks.url and is what gets exported; url_norm is derived and never shown to the user.
// Each rule is individually switchable because each one can over-collapse on some corner of a
// real corpus - see the caution in docs/ARCHITECTURE.md#url-normalisation-and-deduplication - and the policy has
// to be tunable against the actual data.
type Normalizer struct {
	cfg config.Normalize

	// strip is the union of TrackingParams and ExtraStripParams, resolved once.
	strip map[string]struct{}
}

// NewNormalizer builds a Normalizer for the given policy.
func NewNormalizer(cfg config.Normalize) *Normalizer {
	strip := make(map[string]struct{})

	if cfg.StripTrackingParams {
		for _, p := range TrackingParams {
			strip[p] = struct{}{}
		}
	}

	// extra_strip_params applies regardless of strip_tracking_params: it is an explicit list
	// the user wrote, not part of the built-in policy.
	for _, p := range cfg.ExtraStripParams {
		if p = strings.TrimSpace(p); p != "" {
			strip[p] = struct{}{}
		}
	}

	return &Normalizer{cfg: cfg, strip: strip}
}

// Normalize returns the comparison key for raw.
//
// An unparsable URL is an error rather than a silently-passed-through string: url_norm is
// what grouping keys on, and a key that is really an error message would merge every broken
// URL in the corpus into one group.
func (n *Normalizer) Normalize(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", raw, err)
	}

	if n.cfg.ForceHTTPS && u.Scheme == "http" {
		// Collapsed for comparison purposes only. The original scheme is what gets
		// exported.
		u.Scheme = "https"
	}

	u.Host = strings.ToLower(u.Host)

	if n.cfg.StripWWW {
		u.Host = strings.TrimPrefix(u.Host, "www.")
	}

	if n.cfg.StripFragment {
		u.Fragment = ""
		u.RawFragment = ""
	}

	n.normalizeQuery(u)

	if n.cfg.StripTrailingSlash && u.Path != "/" {
		u.Path = strings.TrimSuffix(u.Path, "/")
		u.RawPath = ""
	}

	return u.String(), nil
}

// normalizeQuery drops stripped parameters and, if configured, canonicalises parameter order.
func (n *Normalizer) normalizeQuery(u *url.URL) {
	// A bare trailing "?" carries no parameters and no meaning, but url.URL records it as
	// ForceQuery and String() writes it back, which would give "…/doc" and "…/doc?"
	// different keys for the same page. Clearing it is not a policy choice, so it is not
	// behind a config flag.
	u.ForceQuery = false

	if u.RawQuery == "" {
		return
	}

	// Without sort_query_params the raw query is left exactly as it was, apart from removing
	// stripped parameters: url.Values.Encode always sorts, so round-tripping through it
	// would reorder the query whether or not the policy asked for that.
	if !n.cfg.SortQueryParams {
		if len(n.strip) == 0 {
			return
		}

		u.RawQuery = n.stripPreservingOrder(u.RawQuery)

		return
	}

	q := u.Query()
	for p := range n.strip {
		q.Del(p)
	}

	// Encode sorts by key, which is the canonicalisation.
	u.RawQuery = q.Encode()
}

// stripPreservingOrder removes stripped parameters from a raw query string without reordering
// or re-encoding the parameters that remain.
func (n *Normalizer) stripPreservingOrder(rawQuery string) string {
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]

	for _, part := range parts {
		name := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			name = part[:i]
		}

		// An un-decodable parameter name is kept rather than dropped: it cannot match a
		// configured strip entry, and dropping it would lose data.
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}

		if _, drop := n.strip[name]; drop {
			continue
		}

		kept = append(kept, part)
	}

	return strings.Join(kept, "&")
}
