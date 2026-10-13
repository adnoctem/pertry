package rules

import (
	"errors"
	"fmt"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// PlanEnv is the environment a plan's `where` expression sees (docs/ARCHITECTURE.md#apply-manifests-and-undo).
//
// It is the rule environment plus what has been decided about the file. The three function-typed
// fields are what let a plan be written as `decided("kind")` rather than
// `has(facets, "kind")` - expr treats a func-typed field as a callable, so the expression reads
// the way docs/ARCHITECTURE.md#apply-manifests-and-undo writes it while still being type-checked when it compiles.
type PlanEnv struct {
	// File carries everything a rule can see. Named rather than embedded, because expr
	// resolves a tagged field by its tag and not through an embedded struct's promotion.
	File Env `expr:"file"`

	// The rule variables are repeated here so that a plan can use them directly, which is
	// what makes `where: 'format == "heic"'` work as it does in a rule.
	Ext        string         `expr:"ext"`
	Format     string         `expr:"format"`
	MIME       string         `expr:"mime"`
	Name       string         `expr:"name"`
	Path       string         `expr:"path"`
	Dir        string         `expr:"dir"`
	Size       int64          `expr:"size"`
	Width      int            `expr:"width"`
	Height     int            `expr:"height"`
	Dims       string         `expr:"dims"`
	Megapixels float64        `expr:"megapixels"`
	Portrait   bool           `expr:"portrait"`
	Landscape  bool           `expr:"landscape"`
	Exif       map[string]any `expr:"exif"`
	HasSidecar bool           `expr:"has_sidecar"`
	HasTaken   bool           `expr:"has_taken"`
	Year       int            `expr:"year"`
	Camera     string         `expr:"camera"`
	Software   string         `expr:"software"`

	// Facets holds the decided facet values, keyed by facet.
	Facets map[string]string `expr:"facets"`

	// Decided reports whether a facet has a value.
	Decided func(string) bool `expr:"decided"`

	// Undecided is its negation, so a plan that collects leftovers reads naturally.
	Undecided func(string) bool `expr:"undecided"`

	// Facet returns a facet's value, or "" when undecided.
	Facet func(string) string `expr:"facet"`
}

// NewPlanEnv builds a plan environment from a rule environment and the file's decided facets.
func NewPlanEnv(env Env, facets map[string]string) PlanEnv {
	if facets == nil {
		facets = map[string]string{}
	}

	return PlanEnv{
		File:       env,
		Ext:        env.Ext,
		Format:     env.Format,
		MIME:       env.MIME,
		Name:       env.Name,
		Path:       env.Path,
		Dir:        env.Dir,
		Size:       env.Size,
		Width:      env.Width,
		Height:     env.Height,
		Dims:       env.Dims,
		Megapixels: env.Megapixels,
		Portrait:   env.Portrait,
		Landscape:  env.Landscape,
		Exif:       env.Exif,
		HasSidecar: env.HasSidecar,
		HasTaken:   env.HasTaken,
		Year:       env.Year,
		Camera:     env.Camera,
		Software:   env.Software,
		Facets:     facets,
		Decided: func(facet string) bool {
			value, ok := facets[facet]

			return ok && value != ""
		},
		Undecided: func(facet string) bool {
			value, ok := facets[facet]

			return !ok || value == ""
		},
		Facet: func(facet string) string { return facets[facet] },
	}
}

// Selector is a compiled plan `where` expression.
type Selector struct {
	program *vm.Program
	source  string
}

// CompileSelector compiles a plan's `where`.
//
// An empty expression is an error rather than "match everything": a plan that moves every scanned
// file whether or not anything is known about it is never what someone meant, and docs/ARCHITECTURE.md#apply-manifests-and-undo's own
// example is `decided("kind")`.
func CompileSelector(expression string) (*Selector, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, errors.New("a plan needs a 'where' expression\n" +
			"hint: `decided(\"kind\")` selects every file whose kind is known")
	}

	program, err := expr.Compile(expression, append([]expr.Option{
		// The function-typed fields need a value to type-check against, so the sample
		// environment is built rather than zero.
		expr.Env(NewPlanEnv(Env{}, nil)),
		expr.AsBool(),
		has(),
	}, groupAgnostic()...)...)
	if err != nil {
		return nil, fmt.Errorf("invalid plan selector %q: %w\n"+
			"hint: a plan sees everything a rule sees, plus facets, decided(), undecided() "+
			"and facet(); see docs/ARCHITECTURE.md#apply-manifests-and-undo", expression, err)
	}

	return &Selector{program: program, source: expression}, nil
}

// Match reports whether the file is selected.
func (s *Selector) Match(env PlanEnv) (bool, error) {
	result, err := expr.Run(s.program, env)
	if err != nil {
		return false, fmt.Errorf("plan selector %q failed on %q: %w", s.source, env.Path, err)
	}

	matched, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("plan selector %q returned %T, want bool", s.source, result)
	}

	return matched, nil
}

// String returns the selector's source.
func (s *Selector) String() string {
	if s == nil {
		return ""
	}

	return s.source
}
