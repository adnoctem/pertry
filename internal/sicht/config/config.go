// Package config implements parsing for sicht.yaml, the workspace's configuration and stage
// definitions (docs/ARCHITECTURE.md#model-stages).
//
// The full documented shape is modelled here even though M0 only reads and writes it: these are
// declarative structs, and a stub that later milestones replace would mean a config file written
// by `sicht init` today failing to load tomorrow. Validation is a different matter and is scoped
// to what each milestone actually relies on, so an unimplemented stage is a parse-time fact rather
// than a startup error.
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

// FieldType is the type of a stage output field (docs/ARCHITECTURE.md#model-stages).
type FieldType string

const (
	FieldString FieldType = "string"
	FieldEnum   FieldType = "enum"
	FieldNumber FieldType = "number"
	FieldBool   FieldType = "bool"
)

// FieldTypes is every valid field type, for validation and messages.
var FieldTypes = []FieldType{FieldString, FieldEnum, FieldNumber, FieldBool}

// StageMode is whether a stage's taxonomy is still being discovered or has been frozen.
type StageMode string

const (
	// ModeOpen lets a taxonomy field return free text, which `sicht taxonomy` then
	// aggregates for curation.
	ModeOpen StageMode = "open"

	// ModeClosed constrains it to the curated enum.
	ModeClosed StageMode = "closed"
)

// StageModes is every valid stage mode.
var StageModes = []StageMode{ModeOpen, ModeClosed}

// Model is the inference configuration.
//
// There is deliberately no provider key. sicht wires up Ollama and only Ollama: docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes
// require that nothing leaves the machine, because the archive contains third parties' data
// visible in screenshots. Allowing opt-in cloud backends is recorded in TODO.md as post-M4 work,
// and when it lands it adds a key here rather than changing this default.
type Model struct {
	// Name is the Ollama model tag.
	Name string `yaml:"name"`

	// KeepAlive is how long Ollama holds the model in memory between requests.
	//
	// A trade-off with a sharp edge. Reloading a vision model per image dominates the runtime,
	// so some hold is essential - but a held model pins gigabytes, and on a memory-capped host
	// that is how a long hold turns into an out-of-memory kill. Five minutes covers the gap
	// between images in a run without keeping 6 GB reserved for half an hour afterwards.
	//
	// Raise it when the machine is otherwise idle; lower it to zero to release the model after
	// every request, which is slow and very polite.
	KeepAlive time.Duration `yaml:"keep_alive"`

	// Options are passed through to Ollama verbatim: temperature, seed, num_ctx.
	Options map[string]any `yaml:"options"`

	// MaxImageEdge downscales an image's long edge before inference.
	MaxImageEdge int `yaml:"max_image_edge"`

	// Workers is how many requests are in flight client-side. Server-side parallelism is
	// Ollama's own OLLAMA_NUM_PARALLEL and is not set from here.
	//
	// Worth more than it looks even with a server that serialises inference: the workers keep
	// the next image decoded and scaled while the model works on the current one, and on a
	// translated filesystem reading a file can take longer than classifying it.
	Workers int `yaml:"workers"`

	// TallStrategy is how a screenshot far taller than it is wide is handled: "crop" keeps
	// the top region, "tile" sends overlapping bands as several images, "scale" squashes the
	// whole thing (docs/ARCHITECTURE.md#inference-backend-and-model-choice).
	//
	// Empty means crop. A tall screenshot scaled whole has illegible text, which defeats the
	// purpose, and tiling costs one request's worth of image tokens per tile.
	TallStrategy string `yaml:"tall_strategy"`

	// ReviewBelow is the default confidence threshold, used by a stage that sets none.
	ReviewBelow float64 `yaml:"review_below"`

	// Think turns the model's reasoning phase on or off. Unset leaves it to the model,
	// and for qwen3-vl that means on.
	//
	// It matters more than it looks. Reasoning shares the context window with the image
	// and the answer, so a model that reasons long enough runs out of room before it
	// writes anything: on the 2017 benchmark qwen3-vl:8b did exactly that on 22 of 87
	// files at num_ctx 4096 (docs/benchmarks/sicht-kind-2026-10-13.md).
	Think *bool `yaml:"think,omitempty"`
}

// Rule is one deterministic rule, evaluated before any model call (docs/ARCHITECTURE.md#rules).
type Rule struct {
	// Facet is the facet this rule decides.
	Facet string `yaml:"facet"`

	// Value is the value it assigns.
	Value string `yaml:"value"`

	// When is an expr-lang expression over the file's metadata environment.
	When string `yaml:"when"`

	// Confidence is recorded as the result's confidence. A rule either matched or it did
	// not, but the author may still want to express that a heuristic is weaker than an exact
	// tag match.
	Confidence float64 `yaml:"confidence"`
}

// Field is one output field of an AI stage.
type Field struct {
	Name string    `yaml:"name"`
	Type FieldType `yaml:"type"`

	// Max is the maximum length for a string field.
	Max int `yaml:"max,omitempty"`

	// Values is the fixed enum for a field whose options are known up front.
	Values []string `yaml:"values,omitempty"`

	// Taxonomy names a curated taxonomy for an enum field whose options are discovered from
	// the data instead (docs/ARCHITECTURE.md#model-stages).
	Taxonomy string `yaml:"taxonomy,omitempty"`

	// Nullable allows an explicit null.
	Nullable bool `yaml:"nullable,omitempty"`

	// Decision marks this field as a facet value rather than an observation. Observational
	// fields come first in the generated schema and are stored as stage payload; the decision
	// field is what lands in results (docs/ARCHITECTURE.md#model-stages).
	Decision bool `yaml:"decision,omitempty"`
}

// Stage is one AI pass over a selected set of files.
type Stage struct {
	Name string    `yaml:"name"`
	Mode StageMode `yaml:"mode"`

	// Input is an expression over prior results selecting this stage's inputs.
	Input string `yaml:"input"`

	// PromptFile is relative to the workspace's prompts directory.
	PromptFile string `yaml:"prompt_file"`

	// Context names prior facets whose values are injected into the prompt.
	Context []string `yaml:"context,omitempty"`

	// ReviewBelow is the confidence threshold under which a result goes to the review queue.
	ReviewBelow float64 `yaml:"review_below,omitempty"`

	Fields []Field `yaml:"fields"`
}

// DecisionFields returns the names of the stage's decision fields, which is what `run` writes to
// results and what a confidence score has to be derived for.
func (s Stage) DecisionFields() []string {
	var out []string

	for _, f := range s.Fields {
		if f.Decision {
			out = append(out, f.Name)
		}
	}

	return out
}

// Plan is a set of file moves, as a template over facet values (docs/ARCHITECTURE.md#apply-manifests-and-undo).
type Plan struct {
	// Where selects the files this plan covers.
	Where string `yaml:"where"`

	// Target is a Go text/template over the file's facet values.
	Target string `yaml:"target"`

	// Mode is "move" or "copy".
	Mode string `yaml:"mode"`

	// OnConflict is "suffix", "skip" or "fail".
	OnConflict string `yaml:"on_conflict"`

	// IncludeCompanions moves a file's companions along with it: the Takeout JSON sidecar and
	// the Live Photo motion file (docs/ARCHITECTURE.md#apply-manifests-and-undo). A companion is a file that belongs to an indexed file
	// but that sicht never indexed itself, so nothing else would move it.
	//
	// Without this, `apply` moves a still out from under its motion half and silently breaks
	// the pairing - 201 stills in the archive this was measured against have one.
	IncludeCompanions bool `yaml:"include_companions"`

	// IncludeSidecars is the former name of IncludeCompanions, honoured so that a sicht.yaml
	// written before the rename does not lose the setting. YAML parsing ignores keys it does
	// not know, so a plain rename would have turned the behaviour off silently rather than
	// reporting anything. Read it through MovesCompanions, never directly.
	//
	// Deprecated: write include_companions.
	IncludeSidecars bool `yaml:"include_sidecars"`
}

// Config is a parsed sicht.yaml.
type Config struct {
	Model Model `yaml:"model"`

	// Extensions are the file extensions `scan` indexes, without a leading dot.
	Extensions []string `yaml:"extensions"`

	// ScreenResolutions is the list rules match device dimensions against.
	//
	// Deliberately configuration and not a table in Go: docs/ARCHITECTURE.md#rules is explicit that device
	// resolutions must not be hardcoded or invented, and that this list is populated from
	// verified data or from the user's own files via `sicht stats dims`. It therefore ships
	// empty - an empty list is honest, whereas a plausible-looking guess would be worse than
	// nothing.
	ScreenResolutions []string `yaml:"screen_resolutions"`

	Rules []Rule `yaml:"rules"`

	Stages []Stage `yaml:"stages"`

	// Taxonomies are the curated value sets for closed-mode enum fields, keyed by facet.
	Taxonomies map[string][]string `yaml:"taxonomies,omitempty"`

	Plans map[string]Plan `yaml:"plans"`
}

// Stage returns the named stage.
func (c *Config) Stage(name string) (Stage, bool) {
	for _, s := range c.Stages {
		if s.Name == name {
			return s, true
		}
	}

	return Stage{}, false
}

// Default returns the configuration `sicht init` writes.
//
// The model is the -instruct variant on purpose (docs/benchmarks/sicht-kind-2026-10-13.md). qwen3-vl's plain tags are its
// thinking variants, and on the 2017 benchmark those returned no answer at all for a quarter of
// the files - they reasoned until the 4096-token context was full - at about 24 seconds an image.
// The instruct variants answered 86 of 87 in under a second each. The 8b over the 4b because it
// fits the reference GPU with room to spare and the owner asked for the most capable model that
// does; nothing measured separates the two on accuracy yet.
//
// The rules are the ones docs/ARCHITECTURE.md#rules gives as examples, measured against the reference archive in docs/ARCHITECTURE.md#rules.
func Default() Config {
	return Config{
		Model: Model{
			Name:      "qwen3-vl:8b-instruct",
			KeepAlive: 5 * time.Minute,
			Options: map[string]any{
				"temperature": 0,
				"seed":        42,
				// 4096 rather than 8192, from measurement. A 1280 px image costs
				// about 1100 prompt tokens and this model's reasoning 282 to 1686,
				// so 2786 was the worst case observed over the reference archive.
				// The KV cache scales linearly with this, so halving it halves the
				// memory a loaded model holds beyond its weights - which matters a
				// great deal on a host with a hard memory ceiling.
				"num_ctx": 4096,
			},
			MaxImageEdge: 1280,
			Workers:      2,
			TallStrategy: "crop",
			ReviewBelow:  0.75,
		},
		Extensions: []string{"jpg", "jpeg", "png", "heic", "heif", "webp", "gif"},
		// Empty on purpose. See the field comment.
		ScreenResolutions: []string{},
		// Each of these is as measured against a real export, not as first drafted. The
		// reasoning is in docs/ARCHITECTURE.md#rules; the short version is that the draft
		// rules named one EXIF group and one file extension, and both assumptions cost real
		// files.
		Rules: []Rule{
			{
				// No extension guard: one screenshot in the reference archive is a
				// JPG, and 5 of the tagged PNGs are really JPEGs that Google
				// transcoded without renaming (docs/ARCHITECTURE.md#scan).
				//
				// tags() rather than a group key: the marker lives in both
				// ExifIFD:UserComment (100 files) and XMP-exif:UserComment (105),
				// union 111. Naming ExifIFD alone matched 99 of them.
				//
				// Equality against a list member, never a substring: UserComment is a
				// junk drawer that also holds a PicsArt editor blob and a VSCO preset
				// string in this very archive.
				Facet:      "kind",
				Value:      "screenshot",
				When:       `!has_tag(exif, "Make") && "Screenshot" in tags(exif, "UserComment")`,
				Confidence: 0.99,
			},
			{
				// Fires only once screen_resolutions is populated, which `sicht stats
				// dims` does from the user's own files. It ships empty because docs/ARCHITECTURE.md#rules
				// forbids inventing device dimensions.
				Facet:      "kind",
				Value:      "screenshot",
				When:       `!has_tag(exif, "Make") && dims in screen_resolutions`,
				Confidence: 0.9,
			},
			{
				// A camera make and an aperture together are optical evidence: no
				// screenshot has them. Verified on the reference archive, where this
				// matches 300 files and not one tagged screenshot.
				Facet:      "kind",
				Value:      "photo",
				When:       `has_tag(exif, "Make") && has_tag(exif, "FNumber")`,
				Confidence: 0.98,
			},
		},
		Stages: []Stage{
			{
				Name:       "kind",
				Mode:       ModeClosed,
				Input:      `undecided("kind")`,
				PromptFile: "prompts/kind.md",
				Fields: []Field{
					{Name: "visible_ui", Type: FieldString, Max: 200},
					{
						Name: "kind",
						Type: FieldEnum,
						// "graphic", not "other". A value that names a thing gets chosen;
						// one that names an absence does not. On the 2017 benchmark the
						// instruct models never once answered "other", and filed 63 saved
						// images as photos and screenshots, until the value was renamed -
						// which moved accuracy from 25% to 92% (docs/benchmarks/sicht-kind-2026-10-13.md).
						Values:   []string{"screenshot", "photo", "scan_document", "graphic"},
						Decision: true,
					},
				},
			},
			{
				Name:       "platform",
				Mode:       ModeOpen,
				Input:      `kind == "screenshot"`,
				PromptFile: "prompts/platform.md",
				Fields: []Field{
					{Name: "visible_text_excerpt", Type: FieldString, Max: 300},
					{Name: "visible_url", Type: FieldString, Nullable: true},
					{Name: "ui_description", Type: FieldString, Max: 200},
					{Name: "platform", Type: FieldEnum, Taxonomy: "platform", Decision: true},
				},
			},
		},
		Plans: map[string]Plan{
			"default": {
				Where:             `decided("kind")`,
				Target:            `sorted/{{ .kind }}/{{ .platform | default "unknown" }}`,
				Mode:              "move",
				OnConflict:        "suffix",
				IncludeCompanions: true,
			},
		},
	}
}

// Load reads and parses a sicht.yaml, layered over Default so a partial file overrides only the
// keys it sets.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w\nhint: run `sicht init` to create one", path, err)
	}

	cfg := Default()

	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w\n"+
			"hint: the schema is documented in cmd/sicht/README.md", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return &cfg, nil
}

// Write renders a config to a file, failing if it already exists.
//
// Refusing to overwrite is the point: sicht.yaml holds a curated taxonomy and hand-tuned rules by
// the time anyone re-runs `init`, and clobbering that silently would be unrecoverable.
func Write(path string, cfg Config) error {
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create config %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(encoded); err != nil {
		return fmt.Errorf("write config %q: %w", path, err)
	}

	return nil
}

// Validate checks the invariants the implemented milestones rely on.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Model.Name) == "" {
		return errors.New("'model.name' must not be empty")
	}

	if c.Model.Workers < 1 {
		return fmt.Errorf("'model.workers' must be at least 1, got %d", c.Model.Workers)
	}

	if c.Model.MaxImageEdge < 1 {
		return fmt.Errorf("'model.max_image_edge' must be positive, got %d", c.Model.MaxImageEdge)
	}

	if len(c.Extensions) == 0 {
		return errors.New("'extensions' must list at least one file extension")
	}

	for i, e := range c.Extensions {
		if strings.HasPrefix(e, ".") {
			return fmt.Errorf("'extensions[%d]' is %q; write extensions without a leading dot", i, e)
		}
	}

	for i, r := range c.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("'rules[%d]': %w", i, err)
		}
	}

	seen := make(map[string]struct{}, len(c.Stages))

	for i, s := range c.Stages {
		if err := s.validate(c.Taxonomies); err != nil {
			return fmt.Errorf("'stages[%d]': %w", i, err)
		}

		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("'stages[%d]': stage name %q appears twice", i, s.Name)
		}

		seen[s.Name] = struct{}{}
	}

	for name, p := range c.Plans {
		if err := p.validate(); err != nil {
			return fmt.Errorf("'plans.%s': %w", name, err)
		}
	}

	return nil
}

func (r Rule) validate() error {
	if strings.TrimSpace(r.Facet) == "" {
		return errors.New("'facet' must not be empty")
	}

	if strings.TrimSpace(r.Value) == "" {
		return errors.New("'value' must not be empty")
	}

	if strings.TrimSpace(r.When) == "" {
		return errors.New("'when' must not be empty")
	}

	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("'confidence' must be between 0 and 1, got %v", r.Confidence)
	}

	return nil
}

func (s Stage) validate(taxonomies map[string][]string) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("'name' must not be empty")
	}

	if !slices.Contains(StageModes, s.Mode) {
		return fmt.Errorf("'mode' is %q, want one of %v", s.Mode, StageModes)
	}

	if s.ReviewBelow < 0 || s.ReviewBelow > 1 {
		return fmt.Errorf("'review_below' must be between 0 and 1, got %v", s.ReviewBelow)
	}

	if len(s.Fields) == 0 {
		return errors.New("'fields' must list at least one field")
	}

	decisions := 0

	for i, f := range s.Fields {
		if err := f.validate(s.Mode, taxonomies); err != nil {
			return fmt.Errorf("'fields[%d]': %w", i, err)
		}

		if f.Decision {
			decisions++
		}
	}

	// A stage with no decision field produces no facet value, so `run` would spend inference
	// and record nothing in results.
	if decisions == 0 {
		return errors.New("no field is marked 'decision: true', so the stage would decide nothing")
	}

	return nil
}

func (f Field) validate(mode StageMode, taxonomies map[string][]string) error {
	if strings.TrimSpace(f.Name) == "" {
		return errors.New("'name' must not be empty")
	}

	if !slices.Contains(FieldTypes, f.Type) {
		return fmt.Errorf("'type' is %q, want one of %v", f.Type, FieldTypes)
	}

	if f.Type != FieldEnum {
		return nil
	}

	switch {
	case len(f.Values) > 0 && f.Taxonomy != "":
		return errors.New("set either 'values' or 'taxonomy', not both")

	case len(f.Values) == 0 && f.Taxonomy == "":
		return errors.New("an enum field needs either 'values' or 'taxonomy'")

	case f.Taxonomy != "" && mode == ModeClosed:
		// A closed stage builds its enum from the curated taxonomy, so the taxonomy has
		// to exist. In open mode it does not yet - that is what `sicht taxonomy` is for.
		if len(taxonomies[f.Taxonomy]) == 0 {
			return fmt.Errorf("taxonomy %q is empty or missing, and the stage is closed\n"+
				"hint: run the stage in open mode, curate with `sicht taxonomy %s`, then close it",
				f.Taxonomy, f.Taxonomy)
		}
	}

	// Enum cardinality is capped at 50 per facet; beyond that docs/ARCHITECTURE.md#model-stages says to split the taxonomy
	// hierarchically into separate stages rather than widen one enum.
	const maxEnum = 50

	if n := len(f.Values); n > maxEnum {
		return fmt.Errorf("'values' has %d entries, which exceeds the %d-value cap; split the facet "+
			"into separate stages instead", n, maxEnum)
	}

	if n := len(taxonomies[f.Taxonomy]); f.Taxonomy != "" && n > maxEnum {
		return fmt.Errorf("taxonomy %q has %d entries, which exceeds the %d-value cap; split the facet "+
			"into separate stages instead", f.Taxonomy, n, maxEnum)
	}

	return nil
}

// MovesCompanions reports whether this plan moves a file's companions with it, accepting either
// the current key or its former name.
func (p Plan) MovesCompanions() bool {
	return p.IncludeCompanions || p.IncludeSidecars
}

func (p Plan) validate() error {
	if strings.TrimSpace(p.Target) == "" {
		return errors.New("'target' must not be empty")
	}

	if !slices.Contains([]string{"move", "copy"}, p.Mode) {
		return fmt.Errorf("'mode' is %q, want \"move\" or \"copy\"", p.Mode)
	}

	if !slices.Contains([]string{"suffix", "skip", "fail"}, p.OnConflict) {
		return fmt.Errorf("'on_conflict' is %q, want \"suffix\", \"skip\" or \"fail\"", p.OnConflict)
	}

	return nil
}
