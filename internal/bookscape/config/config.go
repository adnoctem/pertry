// Package config implements parsing for bookscape's bookscape.yaml configuration file, which
// declares the working database, the URL-normalisation policy used for deduplication, the
// liveness-check budget, the classification taxonomy and its deterministic rules, and the
// export filters.
//
// The schema is documented in cmd/bookscape/README.md. Every field has a default
// that matches that document, so an absent bookscape.yaml is equivalent to the documented
// defaults rather than an error - only `database` and the classify taxonomy are worth setting
// by hand for a first run.
package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the configuration file bookscape looks for when --config is not given.
const DefaultFile = "bookscape.yaml"

// Normalize is the URL-normalisation policy. Normalisation exists solely to make two URLs
// comparable for deduplication; the original URL is always retained and url_norm is a derived
// key (docs/ARCHITECTURE.md#url-normalisation-and-deduplication).
//
// Every rule is switchable because each one can over-collapse on some corner of a real corpus:
// forcing https merges hosts that genuinely differ, stripping www does the same, and sorting
// query parameters merges URLs where parameter order carries meaning.
type Normalize struct {
	// StripTrackingParams removes the built-in tracking-parameter list (see TrackingParams).
	StripTrackingParams bool `yaml:"strip_tracking_params"`

	// ExtraStripParams are additional query parameters to drop, on top of TrackingParams.
	ExtraStripParams []string `yaml:"extra_strip_params"`

	// ForceHTTPS rewrites an http scheme to https for comparison purposes only.
	ForceHTTPS bool `yaml:"force_https"`

	// StripWWW removes a leading "www." from the host.
	StripWWW bool `yaml:"strip_www"`

	// StripFragment discards the URL fragment.
	StripFragment bool `yaml:"strip_fragment"`

	// StripTrailingSlash removes a trailing slash from a path other than "/".
	StripTrailingSlash bool `yaml:"strip_trailing_slash"`

	// SortQueryParams canonicalises query-parameter order.
	SortQueryParams bool `yaml:"sort_query_params"`
}

// Check is the liveness-check budget and behaviour (docs/ARCHITECTURE.md#liveness).
type Check struct {
	// Concurrency is the global cap on in-flight requests.
	Concurrency int `yaml:"concurrency"`

	// PerHostConcurrency caps in-flight requests to any single host. A bookmark collection is
	// heavily skewed toward a handful of domains, so a global cap alone would hammer them.
	PerHostConcurrency int `yaml:"per_host_concurrency"`

	// Timeout bounds a single request, including redirects.
	Timeout time.Duration `yaml:"timeout"`

	// UserAgent is sent with every request. A plausible one clears a meaningful amount of bot
	// protection that would otherwise report a live page as uncertain.
	UserAgent string `yaml:"user_agent"`

	// RetryUncertainWithGET re-probes an uncertain HEAD result with a full GET.
	RetryUncertainWithGET bool `yaml:"retry_uncertain_with_get"`

	// FollowRedirects enables redirect following up to MaxRedirects.
	FollowRedirects bool `yaml:"follow_redirects"`

	// MaxRedirects caps a redirect chain.
	MaxRedirects int `yaml:"max_redirects"`
}

// Rule is one deterministic classification rule. Rules run before any model call; every link a
// rule resolves is a link not spent on tokens (docs/ARCHITECTURE.md#classification).
//
// A rule matches if any of its match lists matches. Patterns in MatchDomain may be prefixed
// with "*." to match subdomains; patterns in MatchSourcePath may be suffixed with "/*" to match
// a folder and everything under it.
type Rule struct {
	// MatchDomain matches against the bookmark's host.
	MatchDomain []string `yaml:"match_domain,omitempty"`

	// MatchSourcePath matches against any of the bookmark's original folder paths. The
	// original Chrome folder is often a better classification signal than the domain.
	MatchSourcePath []string `yaml:"match_source_path,omitempty"`

	// Collection is the collection assigned on a match. May be empty if the rule only tags.
	Collection string `yaml:"collection,omitempty"`

	// Tags are added on a match.
	Tags []string `yaml:"tags,omitempty"`
}

// Validate reports whether the rule can match anything and assigns something.
func (r Rule) Validate() error {
	if len(r.MatchDomain) == 0 && len(r.MatchSourcePath) == 0 {
		return errors.New("rule matches nothing: set at least one of 'match_domain' or 'match_source_path'")
	}

	if r.Collection == "" && len(r.Tags) == 0 {
		return errors.New("rule assigns nothing: set at least one of 'collection' or 'tags'")
	}

	return nil
}

// Inference providers bookscape can use for the model pass.
//
// Ollama is included deliberately: the pass classifies text, not images, so a local model is a
// genuine option for anyone who would rather spend CPU than money. It is not the default because
// the quality of the generated description is what the pass is for, and the hosted models are
// better at it.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOllama    = "ollama"
)

// Providers is every valid classify.provider value, for validation and help text.
var Providers = []string{ProviderAnthropic, ProviderOpenAI, ProviderOllama}

// Classify is the classification taxonomy and the two-pass configuration
// (docs/ARCHITECTURE.md#classification).
type Classify struct {
	// Provider selects the inference backend for the long-tail pass. One of the values in
	// Providers. The model pass is the only thing in bookscape that calls a model at all,
	// so this is the only place a provider is chosen.
	Provider string `yaml:"provider"`

	// Model is the provider's own model id for the long-tail pass. It is persisted with
	// every result so that rows classified under different models stay distinguishable.
	Model string `yaml:"model"`

	// BatchSize is how many links go into one model request. Single-link requests are
	// wasteful and produce worse results than batches, which give the model comparative
	// context.
	BatchSize int `yaml:"batch_size"`

	// MinConfidence is the floor below which a model result routes to review rather than to
	// export.
	MinConfidence float64 `yaml:"min_confidence"`

	// Collections is the closed set a classification must choose from. Keep it small and
	// shallow - 8 to 15 entries - as the single "where does this belong" axis, and put
	// everything else in tags.
	Collections []string `yaml:"collections"`

	// Rules are the deterministic first pass, evaluated in order.
	Rules []Rule `yaml:"rules"`
}

// HasCollection reports whether name is in the configured set. The model pass validates
// against this even though the request is schema-constrained: constrained decoding is not a
// substitute for rejecting a collection outside the configured set.
func (c Classify) HasCollection(name string) bool {
	return slices.Contains(c.Collections, name)
}

// Export is the projection from the database back into a document an importer reads
// (docs/ARCHITECTURE.md#export-and-linkwarden).
type Export struct {
	// Format is the document to produce: "json" for Linkwarden's own backup format, which
	// is the default and the only one of the two that preserves the generated description,
	// or "html" for Netscape bookmarks. See export.ParseFormat for the accepted values.
	Format string `yaml:"format"`

	// Output is the file the enriched document is written to. Empty means
	// ./enriched-bookmarks with the extension the format uses, so that changing the format
	// does not silently write JSON into a file named .html.
	Output string `yaml:"output"`

	// IncludeDead ships bookmarks whose liveness check concluded 'dead'.
	IncludeDead bool `yaml:"include_dead"`

	// IncludeUncertain ships bookmarks whose liveness check was inconclusive. These are
	// included by default: an uncertain result usually means bot protection, not a dead link.
	IncludeUncertain bool `yaml:"include_uncertain"`

	// MaxDescriptionLength truncates generated descriptions. Zero means no limit.
	MaxDescriptionLength int `yaml:"max_description_length"`

	// Title becomes the document's <TITLE> and <H1>.
	Title string `yaml:"title"`
}

// Config is the parsed contents of a bookscape.yaml file.
type Config struct {
	// Database is the path to the working SQLite database.
	Database string `yaml:"database"`

	Normalize Normalize `yaml:"normalize"`
	Check     Check     `yaml:"check"`
	Classify  Classify  `yaml:"classify"`
	Export    Export    `yaml:"export"`
}

// Default returns the configuration documented in cmd/bookscape/README.md. It is
// what Load starts from, so a partial bookscape.yaml overrides only the keys it sets.
//
// The classify taxonomy and rules are deliberately part of the default: a first run with no
// configuration file should still resolve the obvious domains for free rather than sending
// every link to the model.
func Default() Config {
	return Config{
		Database: "./bookscape.db",
		Normalize: Normalize{
			StripTrackingParams: true,
			ExtraStripParams:    []string{"ref", "source", "at_medium"},
			ForceHTTPS:          true,
			StripWWW:            true,
			StripFragment:       true,
			StripTrailingSlash:  true,
			SortQueryParams:     true,
		},
		Check: Check{
			Concurrency:           32,
			PerHostConcurrency:    4,
			Timeout:               10 * time.Second,
			UserAgent:             "Mozilla/5.0 (compatible; bookscape/0.1)",
			RetryUncertainWithGET: true,
			FollowRedirects:       true,
			MaxRedirects:          5,
		},
		Classify: Classify{
			Provider:      ProviderAnthropic,
			Model:         "claude-sonnet-5",
			BatchSize:     75,
			MinConfidence: 0.6,
			Collections: []string{
				"Development",
				"Infrastructure",
				"Reference",
				"Reading",
				"Media",
				"Hardware",
				"Finance",
				"Archive",
			},
			Rules: []Rule{
				{
					MatchDomain: []string{"github.com", "gitlab.com", "codeberg.org"},
					Collection:  "Development",
					Tags:        []string{"repo"},
				},
				{
					MatchDomain: []string{"stackoverflow.com", "*.stackexchange.com"},
					Collection:  "Reference",
					Tags:        []string{"qa"},
				},
				{
					MatchDomain: []string{"youtube.com", "youtu.be", "vimeo.com"},
					Collection:  "Media",
					Tags:        []string{"video"},
				},
				{
					MatchSourcePath: []string{"Dev/*", "Programming/*"},
					Collection:      "Development",
				},
			},
		},
		Export: Export{
			Format:               "json",
			Output:               "",
			IncludeDead:          false,
			IncludeUncertain:     true,
			MaxDescriptionLength: 200,
			Title:                "bookscape export",
		},
	}
}

// Load reads and parses a bookscape.yaml file at path, layered over Default.
//
// A missing file at the default path is not an error - it yields the defaults - because every
// subcommand needs a configuration and the documented defaults are a usable one. A missing file
// at an explicitly requested path is an error, since the user named something that is not
// there. Pass explicit=false for the former, true for the latter.
func Load(path string, explicit bool) (*Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Unmarshal over the defaults, so a partial file overrides only what it sets.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse config %q: %w\nhint: the schema is documented in cmd/bookscape/README.md", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// Defaults it is.
	default:
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return &cfg, nil
}

// Validate checks the invariants the pipeline relies on. It is called by Load, and is exported
// so that a caller constructing a Config in code can check it too.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Database) == "" {
		return errors.New("'database' must not be empty")
	}

	if c.Check.Concurrency < 1 {
		return fmt.Errorf("'check.concurrency' must be at least 1, got %d", c.Check.Concurrency)
	}

	if c.Check.PerHostConcurrency < 1 {
		return fmt.Errorf("'check.per_host_concurrency' must be at least 1, got %d", c.Check.PerHostConcurrency)
	}

	if c.Check.Timeout <= 0 {
		return fmt.Errorf("'check.timeout' must be positive, got %s", c.Check.Timeout)
	}

	if c.Check.MaxRedirects < 0 {
		return fmt.Errorf("'check.max_redirects' must not be negative, got %d", c.Check.MaxRedirects)
	}

	if !slices.Contains(Providers, c.Classify.Provider) {
		return fmt.Errorf("'classify.provider' is %q, want one of %v", c.Classify.Provider, Providers)
	}

	if strings.TrimSpace(c.Classify.Model) == "" {
		return errors.New("'classify.model' must not be empty")
	}

	if c.Classify.BatchSize < 1 {
		return fmt.Errorf("'classify.batch_size' must be at least 1, got %d", c.Classify.BatchSize)
	}

	if c.Classify.MinConfidence < 0 || c.Classify.MinConfidence > 1 {
		return fmt.Errorf("'classify.min_confidence' must be between 0 and 1, got %v", c.Classify.MinConfidence)
	}

	if len(c.Classify.Collections) == 0 {
		return errors.New("'classify.collections' must list at least one collection")
	}

	// A rule naming a collection outside the configured set would silently create one at
	// export time, which is the thing the closed set exists to prevent.
	for i, r := range c.Classify.Rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("'classify.rules[%d]': %w", i, err)
		}

		if r.Collection != "" && !c.Classify.HasCollection(r.Collection) {
			return fmt.Errorf("'classify.rules[%d]' assigns collection %q, which is not in 'classify.collections' %v",
				i, r.Collection, c.Classify.Collections)
		}
	}

	// export.output is allowed to be empty: that means ./enriched-bookmarks with the
	// extension the format uses. export.format is what must be a value the exporter knows,
	// and it is checked here rather than at export time so a typo is reported when the
	// configuration is read and not after a classify pass has already been paid for.
	if _, err := ParseExportFormat(c.Export.Format); err != nil {
		return fmt.Errorf("'export.format': %w", err)
	}

	if c.Export.MaxDescriptionLength < 0 {
		return fmt.Errorf("'export.max_description_length' must not be negative, got %d", c.Export.MaxDescriptionLength)
	}

	return nil
}
