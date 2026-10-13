package dedupe_test

import (
	"testing"

	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/dedupe"
)

func TestNormalizeDefaultPolicy(t *testing.T) {
	t.Parallel()

	n := dedupe.NewNormalizer(config.Default().Normalize)

	tests := map[string]struct {
		in   string
		want string
	}{
		"already canonical":      {in: "https://go.dev/doc", want: "https://go.dev/doc"},
		"http collapses":         {in: "http://go.dev/doc", want: "https://go.dev/doc"},
		"host lowercased":        {in: "https://Go.Dev/doc", want: "https://go.dev/doc"},
		"www stripped":           {in: "https://www.go.dev/doc", want: "https://go.dev/doc"},
		"fragment stripped":      {in: "https://go.dev/doc#install", want: "https://go.dev/doc"},
		"trailing slash":         {in: "https://go.dev/doc/", want: "https://go.dev/doc"},
		"root slash kept":        {in: "https://go.dev/", want: "https://go.dev/"},
		"tracking params":        {in: "https://go.dev/doc?utm_source=x&utm_medium=y", want: "https://go.dev/doc"},
		"extra strip params":     {in: "https://go.dev/doc?ref=hn&source=rss", want: "https://go.dev/doc"},
		"meaningful param kept":  {in: "https://go.dev/doc?v=2", want: "https://go.dev/doc?v=2"},
		"query order sorted":     {in: "https://go.dev/doc?b=2&a=1", want: "https://go.dev/doc?a=1&b=2"},
		"mixed strip and keep":   {in: "https://go.dev/doc?utm_source=x&id=42", want: "https://go.dev/doc?id=42"},
		"path case preserved":    {in: "https://go.dev/Doc", want: "https://go.dev/Doc"},
		"port preserved":         {in: "https://go.dev:8443/doc", want: "https://go.dev:8443/doc"},
		"userinfo preserved":     {in: "https://user@go.dev/doc", want: "https://user@go.dev/doc"},
		"whitespace trimmed":     {in: "  https://go.dev/doc  ", want: "https://go.dev/doc"},
		"non-http scheme":        {in: "ftp://files.go.dev/x", want: "ftp://files.go.dev/x"},
		"empty query preserved":  {in: "https://go.dev/doc?", want: "https://go.dev/doc"},
		"encoded path preserved": {in: "https://go.dev/a%20b", want: "https://go.dev/a%20b"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := n.Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v, want nil", tc.in, err)
			}

			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeCollapsesDuplicates states the property the whole package exists for: two URLs
// that differ only in ways the policy calls insignificant must produce the same key.
func TestNormalizeCollapsesDuplicates(t *testing.T) {
	t.Parallel()

	n := dedupe.NewNormalizer(config.Default().Normalize)

	groups := [][]string{
		{
			"http://www.github.com/golang/go/",
			"https://github.com/golang/go",
			"https://GitHub.com/golang/go#readme",
			"https://github.com/golang/go?utm_source=newsletter",
		},
		{
			"https://example.net/article?id=42&utm_medium=email",
			"https://example.net/article?utm_medium=email&id=42",
		},
	}

	for _, group := range groups {
		want, err := n.Normalize(group[0])
		if err != nil {
			t.Fatalf("Normalize(%q) error = %v", group[0], err)
		}

		for _, raw := range group[1:] {
			got, err := n.Normalize(raw)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", raw, err)
			}

			if got != want {
				t.Errorf("Normalize(%q) = %q, want it to match Normalize(%q) = %q", raw, got, group[0], want)
			}
		}
	}
}

// TestNormalizeDistinguishesDifferentPages is the other half: the policy must not collapse URLs
// that really do serve different content.
func TestNormalizeDistinguishesDifferentPages(t *testing.T) {
	t.Parallel()

	n := dedupe.NewNormalizer(config.Default().Normalize)

	pairs := [][2]string{
		{"https://go.dev/doc", "https://go.dev/ref"},
		{"https://go.dev/doc?v=1", "https://go.dev/doc?v=2"},
		{"https://go.dev/doc", "https://pkg.go.dev/doc"},
		{"https://go.dev/doc", "https://go.dev:8443/doc"},
		// Stripping "www." is a known over-collapse risk, but a different subdomain is
		// still a different host.
		{"https://blog.go.dev/x", "https://go.dev/x"},
	}

	for _, pair := range pairs {
		a, err := n.Normalize(pair[0])
		if err != nil {
			t.Fatalf("Normalize(%q) error = %v", pair[0], err)
		}

		b, err := n.Normalize(pair[1])
		if err != nil {
			t.Fatalf("Normalize(%q) error = %v", pair[1], err)
		}

		if a == b {
			t.Errorf("Normalize(%q) and Normalize(%q) both = %q, want different keys", pair[0], pair[1], a)
		}
	}
}

// TestNormalizePolicyIsSwitchable checks each rule can be turned off independently, which is
// what makes the policy tunable when a normalisation turns out to be merging genuinely
// distinct URLs.
func TestNormalizePolicyIsSwitchable(t *testing.T) {
	t.Parallel()

	const in = "http://www.go.dev/doc/?b=2&a=1&utm_source=x#frag"

	tests := map[string]struct {
		mutate func(*config.Normalize)
		want   string
	}{
		"everything on": {
			mutate: func(*config.Normalize) {},
			want:   "https://go.dev/doc?a=1&b=2",
		},
		"force_https off": {
			mutate: func(c *config.Normalize) { c.ForceHTTPS = false },
			want:   "http://go.dev/doc?a=1&b=2",
		},
		"strip_www off": {
			mutate: func(c *config.Normalize) { c.StripWWW = false },
			want:   "https://www.go.dev/doc?a=1&b=2",
		},
		"strip_fragment off": {
			mutate: func(c *config.Normalize) { c.StripFragment = false },
			want:   "https://go.dev/doc?a=1&b=2#frag",
		},
		"strip_trailing_slash off": {
			mutate: func(c *config.Normalize) { c.StripTrailingSlash = false },
			want:   "https://go.dev/doc/?a=1&b=2",
		},
		"sort_query_params off": {
			// Order is preserved, but stripping still happens.
			mutate: func(c *config.Normalize) { c.SortQueryParams = false },
			want:   "https://go.dev/doc?b=2&a=1",
		},
		"strip_tracking_params off": {
			mutate: func(c *config.Normalize) { c.StripTrackingParams = false },
			want:   "https://go.dev/doc?a=1&b=2&utm_source=x",
		},
		"nothing on": {
			mutate: func(c *config.Normalize) { *c = config.Normalize{} },
			want:   "http://www.go.dev/doc/?b=2&a=1&utm_source=x#frag",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Default().Normalize
			tc.mutate(&cfg)

			got, err := dedupe.NewNormalizer(cfg).Normalize(in)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v, want nil", in, err)
			}

			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", in, got, tc.want)
			}
		})
	}
}

// TestNormalizeRejectsUnparsableURLs guards against a broken URL becoming a comparison key
// that every other broken URL in the corpus also produces.
func TestNormalizeRejectsUnparsableURLs(t *testing.T) {
	t.Parallel()

	n := dedupe.NewNormalizer(config.Default().Normalize)

	for _, raw := range []string{"https://go.dev/\x7f", "ht tp://go.dev", "://nonsense"} {
		if _, err := n.Normalize(raw); err == nil {
			t.Errorf("Normalize(%q) error = nil, want an error", raw)
		}
	}
}
