package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/config"
)

func TestDefaultIsValid(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default() is not valid: %v", err)
	}
}

// TestDefaultShipsNoScreenResolutions guards a deliberate emptiness. Section 5.2 forbids
// hardcoding or inventing device dimensions, and a plausible-looking list of invented ones would
// make the second `kind` rule fire wrongly and silently. Anyone tempted to "fix" the empty list
// has to come through this test and read why.
func TestDefaultShipsNoScreenResolutions(t *testing.T) {
	t.Parallel()

	if got := config.Default().ScreenResolutions; len(got) != 0 {
		t.Errorf("Default().ScreenResolutions = %v, want empty: docs/ARCHITECTURE.md#rules forbids invented "+
			"device dimensions; the list is populated from verified data or `sicht stats dims`", got)
	}
}

// TestDefaultRoundTrips is what makes `sicht init` trustworthy: the file it writes must load.
func TestDefaultRoundTrips(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sicht.yaml")

	if err := config.Write(path, config.Default()); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := config.Default()

	if got.Model.Name != want.Model.Name {
		t.Errorf("model.name = %q, want %q", got.Model.Name, want.Model.Name)
	}

	if got.Model.KeepAlive != want.Model.KeepAlive {
		t.Errorf("model.keep_alive = %v, want %v", got.Model.KeepAlive, want.Model.KeepAlive)
	}

	if len(got.Stages) != len(want.Stages) {
		t.Fatalf("len(stages) = %d, want %d", len(got.Stages), len(want.Stages))
	}

	if len(got.Rules) != len(want.Rules) {
		t.Errorf("len(rules) = %d, want %d", len(got.Rules), len(want.Rules))
	}
}

// TestWriteRefusesToOverwrite protects a curated taxonomy and hand-tuned rules from a re-run of
// `sicht init`, which is documented as safe to repeat.
func TestWriteRefusesToOverwrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sicht.yaml")

	if err := config.Write(path, config.Default()); err != nil {
		t.Fatalf("first Write() error = %v, want nil", err)
	}

	if err := config.Write(path, config.Default()); err == nil {
		t.Fatal("second Write() error = nil, want a refusal to overwrite")
	}

	// And the original survives intact.
	if _, err := config.Load(path); err != nil {
		t.Errorf("Load() after the refused overwrite error = %v, want the original to be intact", err)
	}
}

// TestLoadLayersOverDefault is why a user can write three lines of YAML instead of the whole file.
func TestLoadLayersOverDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sicht.yaml")

	const partial = "model:\n  name: llama4-vision:16b\n  workers: 8\n"

	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Model.Name != "llama4-vision:16b" {
		t.Errorf("model.name = %q, want the override", cfg.Model.Name)
	}

	if cfg.Model.Workers != 8 {
		t.Errorf("model.workers = %d, want the override 8", cfg.Model.Workers)
	}

	// Untouched keys keep their defaults rather than becoming zero values.
	if cfg.Model.MaxImageEdge != config.Default().Model.MaxImageEdge {
		t.Errorf("model.max_image_edge = %d, want the default %d",
			cfg.Model.MaxImageEdge, config.Default().Model.MaxImageEdge)
	}

	if len(cfg.Stages) != len(config.Default().Stages) {
		t.Errorf("len(stages) = %d, want the default %d", len(cfg.Stages), len(config.Default().Stages))
	}
}

func TestLoadReportsAMissingFileWithAHint(t *testing.T) {
	t.Parallel()

	_, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}

	if !strings.Contains(err.Error(), "sicht init") {
		t.Errorf("Load() error = %q, want it to point at `sicht init`", err)
	}
}

func TestLoadRejectsAnInvalidFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sicht.yaml")

	if err := os.WriteFile(path, []byte("extensions: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := config.Load(path); err == nil {
		t.Error("Load() error = nil, want validation to reject an empty extension list")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// mutate turns a valid default into the case under test.
		mutate func(*config.Config)
		// want is a substring of the expected message, so a case asserts the diagnosis and
		// not merely that something failed.
		want string
	}{
		{
			name:   "empty model name",
			mutate: func(c *config.Config) { c.Model.Name = "  " },
			want:   "model.name",
		},
		{
			name:   "zero workers",
			mutate: func(c *config.Config) { c.Model.Workers = 0 },
			want:   "model.workers",
		},
		{
			name:   "negative image edge",
			mutate: func(c *config.Config) { c.Model.MaxImageEdge = -1 },
			want:   "model.max_image_edge",
		},
		{
			name:   "no extensions",
			mutate: func(c *config.Config) { c.Extensions = nil },
			want:   "extensions",
		},
		{
			name:   "extension with a leading dot",
			mutate: func(c *config.Config) { c.Extensions = []string{".jpg"} },
			want:   "without a leading dot",
		},
		{
			name:   "rule with no facet",
			mutate: func(c *config.Config) { c.Rules[0].Facet = "" },
			want:   "rules[0]",
		},
		{
			name:   "rule with no expression",
			mutate: func(c *config.Config) { c.Rules[0].When = "" },
			want:   "when",
		},
		{
			name:   "rule confidence out of range",
			mutate: func(c *config.Config) { c.Rules[0].Confidence = 1.5 },
			want:   "between 0 and 1",
		},
		{
			name:   "unknown stage mode",
			mutate: func(c *config.Config) { c.Stages[0].Mode = "halfway" },
			want:   "mode",
		},
		{
			name:   "duplicate stage name",
			mutate: func(c *config.Config) { c.Stages[1].Name = c.Stages[0].Name },
			want:   "appears twice",
		},
		{
			name:   "stage with no fields",
			mutate: func(c *config.Config) { c.Stages[0].Fields = nil },
			want:   "fields",
		},
		{
			// A stage that decides nothing spends inference and records no facet value.
			name: "stage with no decision field",
			mutate: func(c *config.Config) {
				for i := range c.Stages[0].Fields {
					c.Stages[0].Fields[i].Decision = false
				}
			},
			want: "decide nothing",
		},
		{
			name:   "unknown field type",
			mutate: func(c *config.Config) { c.Stages[0].Fields[0].Type = "blob" },
			want:   "type",
		},
		{
			name: "enum with neither values nor taxonomy",
			mutate: func(c *config.Config) {
				c.Stages[0].Fields[1].Values = nil
				c.Stages[0].Fields[1].Taxonomy = ""
			},
			want: "either 'values' or 'taxonomy'",
		},
		{
			name: "enum with both values and taxonomy",
			mutate: func(c *config.Config) {
				c.Stages[0].Fields[1].Taxonomy = "kind"
			},
			want: "not both",
		},
		{
			// Closing a stage before curating its taxonomy is the ordering mistake section
			// 5.5 warns about, so the message has to name the way out.
			name: "closed stage over a missing taxonomy",
			mutate: func(c *config.Config) {
				c.Stages[1].Mode = config.ModeClosed
			},
			want: "sicht taxonomy",
		},
		{
			name: "enum over the 50-value cap",
			mutate: func(c *config.Config) {
				values := make([]string, 51)
				for i := range values {
					values[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
				}

				c.Stages[0].Fields[1].Values = values
			},
			want: "50-value cap",
		},
		{
			name: "taxonomy over the 50-value cap",
			mutate: func(c *config.Config) {
				values := make([]string, 51)
				for i := range values {
					values[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
				}

				c.Taxonomies = map[string][]string{"platform": values}
			},
			want: "50-value cap",
		},
		{
			name:   "plan with no target",
			mutate: func(c *config.Config) { p := c.Plans["default"]; p.Target = ""; c.Plans["default"] = p },
			want:   "target",
		},
		{
			name:   "plan with an unknown mode",
			mutate: func(c *config.Config) { p := c.Plans["default"]; p.Mode = "symlink"; c.Plans["default"] = p },
			want:   "mode",
		},
		{
			name:   "plan with an unknown conflict policy",
			mutate: func(c *config.Config) { p := c.Plans["default"]; p.OnConflict = "clobber"; c.Plans["default"] = p },
			want:   "on_conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Default()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want an error mentioning %q", tt.want)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestOpenStageMayUseAnUncuratedTaxonomy is the other half of the closed-stage case: open mode is
// precisely how a taxonomy gets populated, so it must not require one.
func TestOpenStageMayUseAnUncuratedTaxonomy(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Stages[1].Mode != config.ModeOpen {
		t.Fatalf("fixture assumption broken: stages[1].mode = %q, want open", cfg.Stages[1].Mode)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want an open stage to be valid with no taxonomy yet", err)
	}
}

func TestStageLookup(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if _, ok := cfg.Stage("kind"); !ok {
		t.Error(`Stage("kind") not found, want the default stage`)
	}

	if _, ok := cfg.Stage("nonexistent"); ok {
		t.Error(`Stage("nonexistent") found, want not ok`)
	}
}

func TestDecisionFields(t *testing.T) {
	t.Parallel()

	stage := config.Stage{Fields: []config.Field{
		{Name: "visible_ui"},
		{Name: "kind", Decision: true},
		{Name: "platform", Decision: true},
	}}

	got := stage.DecisionFields()

	want := []string{"kind", "platform"}
	if len(got) != len(want) {
		t.Fatalf("DecisionFields() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DecisionFields()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestMovesCompanionsAcceptsTheFormerKey is why the deprecated field still exists. YAML parsing
// ignores unknown keys, so renaming include_sidecars without keeping it would have turned the
// behaviour off silently in any sicht.yaml written before the rename - the exact class of failure
// that is only discovered after `apply` has already left 201 motion files behind.
func TestMovesCompanionsAcceptsTheFormerKey(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		yaml string
		want bool
	}{
		"current key":    {"plans:\n  default:\n    target: x\n    mode: move\n    on_conflict: skip\n    include_companions: true\n", true},
		"former key":     {"plans:\n  default:\n    target: x\n    mode: move\n    on_conflict: skip\n    include_sidecars: true\n", true},
		"both":           {"plans:\n  default:\n    target: x\n    mode: move\n    on_conflict: skip\n    include_companions: true\n    include_sidecars: true\n", true},
		"neither":        {"plans:\n  default:\n    target: x\n    mode: move\n    on_conflict: skip\n", false},
		"explicit false": {"plans:\n  default:\n    target: x\n    mode: move\n    on_conflict: skip\n    include_companions: false\n", false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "sicht.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if got := cfg.Plans["default"].MovesCompanions(); got != tt.want {
				t.Errorf("MovesCompanions() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDefaultMovesCompanions pins the default, because the cost of getting it wrong is asymmetric:
// moving a companion that did not need moving is tidy, while leaving one behind breaks a pairing
// the user cannot easily reconstruct.
func TestDefaultMovesCompanions(t *testing.T) {
	t.Parallel()

	plan, ok := config.Default().Plans["default"]
	if !ok {
		t.Fatal("no default plan")
	}

	if !plan.MovesCompanions() {
		t.Error("the default plan does not move companions, want it to")
	}
}
