package rules

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/adnoctem/pertry/internal/sicht/config"
)

// Facets is what is known about one file's facets: the decided value and its confidence.
type Facets struct {
	// Values are the decided facet values, keyed by facet.
	Values map[string]string

	// Confidence is the decided value's confidence, keyed by facet. A facet decided by a
	// source that reports none is absent rather than zero.
	Confidence map[string]float64
}

// InputEnv builds the variable map a stage's `input` expression is evaluated against.
//
// A map rather than the typed struct the rules use, because docs/ARCHITECTURE.md#model-stages writes a stage input as
// `kind == "screenshot"` - a **bare facet name**, which no fixed struct can express since the
// facets are whatever the configuration invents. Type checking is not given up for it: the
// expression is compiled against a sample environment containing every facet the configuration
// mentions, so a misspelled facet still fails at compile time rather than evaluating to nil on
// every file.
// known is every facet the configuration defines. All of them are present in the returned map,
// defaulting to the empty string, so that `platform == ""` reads as "undecided" rather than
// failing on a nil comparison - which is what it did before this argument existed.
func InputEnv(env Env, facets Facets, known []string) map[string]any {
	out := fileVariables(env)

	values := facets.Values
	if values == nil {
		values = map[string]string{}
	}

	confidence := facets.Confidence
	if confidence == nil {
		confidence = map[string]float64{}
	}

	// Every known facet as a bare name, so an undecided one is "" rather than absent. A facet
	// that collides with a file variable loses, because `ext` meaning the file's extension is
	// the stronger expectation and a facet called "ext" is a configuration mistake worth not
	// rewarding.
	for _, facet := range known {
		if _, taken := out[facet]; taken {
			continue
		}

		out[facet] = values[facet]
	}

	// And any facet that was decided but is not in the known set, which happens when a
	// configuration has been edited since the result was recorded.
	for facet, value := range values {
		if _, taken := out[facet]; taken {
			continue
		}

		out[facet] = value
	}

	out["facets"] = values

	out["decided"] = func(facet string) bool {
		value, ok := values[facet]

		return ok && value != ""
	}

	out["undecided"] = func(facet string) bool {
		value, ok := values[facet]

		return !ok || value == ""
	}

	out["facet"] = func(facet string) string { return values[facet] }

	// confidence(facet) is 0 for a facet that is undecided or whose source reported none.
	// Zero rather than an error, so that `confidence("platform") >= 0.6` reads naturally and
	// excludes exactly what it should.
	out["confidence"] = func(facet string) float64 { return confidence[facet] }

	return out
}

// fileVariables renders the rule environment as a map, so a stage input sees everything a rule
// does.
func fileVariables(env Env) map[string]any {
	exif := env.Exif
	if exif == nil {
		exif = map[string]any{}
	}

	return map[string]any{
		"ext":                env.Ext,
		"format":             env.Format,
		"mime":               env.MIME,
		"format_mismatch":    env.FormatMismatch,
		"name":               env.Name,
		"path":               env.Path,
		"dir":                env.Dir,
		"size":               env.Size,
		"width":              env.Width,
		"height":             env.Height,
		"dims":               env.Dims,
		"megapixels":         env.Megapixels,
		"portrait":           env.Portrait,
		"landscape":          env.Landscape,
		"exif":               exif,
		"screen_resolutions": env.ScreenResolutions,
		"has_sidecar":        env.HasSidecar,
		"taken":              env.Taken,
		"has_taken":          env.HasTaken,
		"year":               env.Year,
		"camera":             env.Camera,
		"software":           env.Software,
	}
}

// KnownFacets returns every facet the configuration mentions, sorted.
//
// The universe a stage input may legitimately refer to: facets the rules decide, and facets the
// stages decide. Used to build the sample environment an input is type-checked against, which is
// what turns a misspelled facet from a silent nil into a startup error.
func KnownFacets(cfg *config.Config) []string {
	seen := map[string]struct{}{}

	for _, r := range cfg.Rules {
		seen[r.Facet] = struct{}{}
	}

	for _, stage := range cfg.Stages {
		for _, f := range stage.Fields {
			if f.Decision {
				seen[f.Name] = struct{}{}
			}
		}

		// A stage's `context` names prior facets it reads, which are by definition facets
		// something decides - but a configuration may list one before the stage that
		// produces it exists, and a stage input referring to it should still compile.
		for _, facet := range stage.Context {
			seen[facet] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(seen))
}

// Input is a compiled stage input expression.
type Input struct {
	program *vm.Program
	source  string
}

// CompileInput compiles a stage's `input` expression.
//
// An empty expression is an error rather than "every file": a stage that sends every image in the
// archive to a model whether or not it needs looking at is never what someone meant, and docs/ARCHITECTURE.md#model-stages's
// own examples are always narrowing.
func CompileInput(expression string, cfg *config.Config) (*Input, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, errors.New("a stage needs an 'input' expression\n" +
			"hint: `undecided(\"kind\")` selects the files no rule could decide")
	}

	known := KnownFacets(cfg)
	sample := InputEnv(Env{}, Facets{Values: sampleFacets(cfg)}, known)

	program, err := expr.Compile(expression, append([]expr.Option{
		expr.Env(sample),
		expr.AsBool(),
		has(),
	}, groupAgnostic()...)...)
	if err != nil {
		return nil, fmt.Errorf("invalid stage input %q: %w\n"+
			"hint: a stage input sees everything a rule sees, plus the facet names this "+
			"configuration defines (%s), and decided(), undecided(), facet() and confidence()",
			expression, err, strings.Join(known, ", "))
	}

	return &Input{program: program, source: expression}, nil
}

// sampleFacets gives every known facet a value, so the sample environment types them as strings.
func sampleFacets(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, facet := range KnownFacets(cfg) {
		out[facet] = ""
	}

	return out
}

// Match reports whether a file is selected by the stage input.
func (i *Input) Match(env map[string]any) (bool, error) {
	result, err := expr.Run(i.program, env)
	if err != nil {
		path, _ := env["path"].(string)

		return false, fmt.Errorf("stage input %q failed on %q: %w", i.source, path, err)
	}

	matched, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("stage input %q returned %T, want bool", i.source, result)
	}

	return matched, nil
}

// String returns the expression's source.
func (i *Input) String() string {
	if i == nil {
		return ""
	}

	return i.source
}
