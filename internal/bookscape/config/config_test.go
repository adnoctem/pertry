package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/config"
)

func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), config.DefaultFile)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// TestDefaultIsValid is the invariant that lets Load tolerate a missing file: the documented
// defaults have to be a configuration the pipeline will actually accept.
func TestDefaultIsValid(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() error = %v, want nil", err)
	}
}

// TestLoadLayersOverDefaults is what makes a three-line bookscape.yaml usable: a partial file
// must override only the keys it sets.
func TestLoadLayersOverDefaults(t *testing.T) {
	t.Parallel()

	path := write(t, `
database: /tmp/custom.db
check:
  concurrency: 8
`)

	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Database != "/tmp/custom.db" {
		t.Errorf("Database = %q, want %q", cfg.Database, "/tmp/custom.db")
	}

	if cfg.Check.Concurrency != 8 {
		t.Errorf("Check.Concurrency = %d, want 8", cfg.Check.Concurrency)
	}

	// Untouched keys keep their defaults, including siblings inside the same block.
	def := config.Default()

	if cfg.Check.PerHostConcurrency != def.Check.PerHostConcurrency {
		t.Errorf("Check.PerHostConcurrency = %d, want the default %d",
			cfg.Check.PerHostConcurrency, def.Check.PerHostConcurrency)
	}

	if cfg.Check.Timeout != def.Check.Timeout {
		t.Errorf("Check.Timeout = %s, want the default %s", cfg.Check.Timeout, def.Check.Timeout)
	}

	if diff := cmp.Diff(def.Classify.Collections, cfg.Classify.Collections); diff != "" {
		t.Errorf("Classify.Collections mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	t.Parallel()

	path := write(t, "check:\n  timeout: 2500ms\n")

	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if want := 2500 * time.Millisecond; cfg.Check.Timeout != want {
		t.Errorf("Check.Timeout = %s, want %s", cfg.Check.Timeout, want)
	}
}

// TestLoadMissingFile covers the asymmetry: the default path may be absent, a path the user
// named may not.
func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	absent := filepath.Join(t.TempDir(), "nope.yaml")

	t.Run("default path falls back to defaults", func(t *testing.T) {
		t.Parallel()

		cfg, err := config.Load(absent, false)
		if err != nil {
			t.Fatalf("Load(explicit=false) error = %v, want nil", err)
		}

		if diff := cmp.Diff(config.Default(), *cfg); diff != "" {
			t.Errorf("config mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("explicit path is an error", func(t *testing.T) {
		t.Parallel()

		if _, err := config.Load(absent, true); err == nil {
			t.Error("Load(explicit=true) error = nil, want an error")
		}
	})
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	t.Parallel()

	path := write(t, "database: [this is not a string\n")

	_, err := config.Load(path, true)
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}

	// The error is what a user sees after a typo, so it has to point at the schema.
	if !strings.Contains(err.Error(), "cmd/bookscape/README.md") {
		t.Errorf("Load() error = %q, want it to point at the documented schema", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate func(*config.Config)
		want   string
	}{
		"empty database":        {mutate: func(c *config.Config) { c.Database = "" }, want: "'database'"},
		"zero concurrency":      {mutate: func(c *config.Config) { c.Check.Concurrency = 0 }, want: "check.concurrency"},
		"zero host concurrency": {mutate: func(c *config.Config) { c.Check.PerHostConcurrency = 0 }, want: "check.per_host_concurrency"},
		"zero timeout":          {mutate: func(c *config.Config) { c.Check.Timeout = 0 }, want: "check.timeout"},
		"negative redirects":    {mutate: func(c *config.Config) { c.Check.MaxRedirects = -1 }, want: "check.max_redirects"},
		"zero batch size":       {mutate: func(c *config.Config) { c.Classify.BatchSize = 0 }, want: "classify.batch_size"},
		"confidence above one":  {mutate: func(c *config.Config) { c.Classify.MinConfidence = 1.5 }, want: "classify.min_confidence"},
		"confidence below zero": {mutate: func(c *config.Config) { c.Classify.MinConfidence = -0.1 }, want: "classify.min_confidence"},
		"no collections":        {mutate: func(c *config.Config) { c.Classify.Collections = nil }, want: "classify.collections"},
		"unknown export format": {mutate: func(c *config.Config) { c.Export.Format = "yaml" }, want: "export.format"},
		"empty export format":   {mutate: func(c *config.Config) { c.Export.Format = "" }, want: "export.format"},
		"negative description":  {mutate: func(c *config.Config) { c.Export.MaxDescriptionLength = -1 }, want: "export.max_description_length"},
		"rule matches nothing": {
			mutate: func(c *config.Config) {
				c.Classify.Rules = []config.Rule{{Collection: "Development"}}
			},
			want: "matches nothing",
		},
		"rule assigns nothing": {
			mutate: func(c *config.Config) {
				c.Classify.Rules = []config.Rule{{MatchDomain: []string{"example.com"}}}
			},
			want: "assigns nothing",
		},
		// The closed collection set is the whole point of configuring one; a rule naming a
		// collection outside it would create one at export time.
		"rule names an unknown collection": {
			mutate: func(c *config.Config) {
				c.Classify.Rules = []config.Rule{{
					MatchDomain: []string{"example.com"},
					Collection:  "Nonexistent",
				}}
			},
			want: "not in 'classify.collections'",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Default()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want an error mentioning %q", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestClassifyHasCollection(t *testing.T) {
	t.Parallel()

	c := config.Default().Classify

	if !c.HasCollection("Development") {
		t.Error(`HasCollection("Development") = false, want true`)
	}

	// Matching is exact: a near-miss must not pass, or the closed set stops being closed.
	for _, name := range []string{"development", "Dev", "", "Nonexistent"} {
		if c.HasCollection(name) {
			t.Errorf("HasCollection(%q) = true, want false", name)
		}
	}
}
