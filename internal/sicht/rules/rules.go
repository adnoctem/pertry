// Package rules evaluates the deterministic rules in sicht.yaml against a file's metadata
// (docs/ARCHITECTURE.md#rules).
//
// Rules run before any model call and decide what can be decided for free. Everything here is
// expression evaluation over data the scan already collected: no I/O, no inference, no network.
package rules

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// Env is the environment a rule expression sees.
//
// Field names come from the `expr` tags, so an expression reads `ext == "png"` rather than
// `Ext == "png"`, matching the examples in docs/ARCHITECTURE.md#rules and the configuration a user writes. The struct is
// typed rather than a map so that expressions are checked when they compile - a rule naming a
// variable that does not exist, or comparing a string to a number, is a configuration error worth
// reporting at startup rather than per file.
type Env struct {
	// Ext is the file *extension*, lowercased and without a dot.
	//
	// Prefer Format. The extension is what the filename claims, and in a Google Photos export
	// it is wrong for about one file in seventy: 16 of 25 files named ".HEIC" in the reference
	// archive are plain JPEGs that Google transcoded without renaming, and 3 of 23 named
	// ".gif" are WebP. A rule written against `ext` will quietly not fire for those.
	Ext string `expr:"ext"`

	// Format is the real format, from exiftool's File:FileTypeExtension, lowercased and
	// without a dot - "jpg" for a JPEG whatever the file is called.
	//
	// This is what a rule about file type should use.
	Format string `expr:"format"`

	// MIME is the real media type, from exiftool's File:MIMEType, e.g. "image/jpeg".
	MIME string `expr:"mime"`

	// FormatMismatch reports that Ext and Format disagree, so a rule can act on the fact
	// rather than be caught out by it.
	FormatMismatch bool `expr:"format_mismatch"`

	// Name is the base filename, as it is on disk.
	Name string `expr:"name"`

	// Path is the workspace-relative path with forward slashes.
	//
	// Available because a user may legitimately want a rule about their own layout. Nothing
	// in the shipped defaults uses it: docs/ARCHITECTURE.md#scan is explicit that folder structure carries no
	// meaning, because Takeout's names are localized.
	Path string `expr:"path"`

	// Dir is the workspace-relative directory, forward slashes, "" at the root.
	Dir string `expr:"dir"`

	// Size is the file size in bytes.
	Size int64 `expr:"size"`

	Width  int `expr:"width"`
	Height int `expr:"height"`

	// Dims is "<width>x<height>", the form `screen_resolutions` is written in.
	Dims string `expr:"dims"`

	// Megapixels is width*height/1e6, for rules that want a size class rather than an exact
	// match.
	Megapixels float64 `expr:"megapixels"`

	// Portrait and Landscape are conveniences over Width and Height. A square image is
	// neither.
	Portrait  bool `expr:"portrait"`
	Landscape bool `expr:"landscape"`

	// Exif holds exiftool's tags under their group-qualified names, such as "IFD0:Make".
	// Group prefixes are kept because a bare "Make" is ambiguous between a HEIC's IFD0 and
	// its QuickTime track.
	Exif map[string]any `expr:"exif"`

	// ScreenResolutions is the list from sicht.yaml. It ships empty on purpose; docs/ARCHITECTURE.md#rules forbids
	// inventing device dimensions.
	ScreenResolutions []string `expr:"screen_resolutions"`

	// HasSidecar reports whether a Takeout sidecar was found for this file.
	HasSidecar bool `expr:"has_sidecar"`

	// Taken is the capture time, from the sidecar's photoTakenTime or EXIF. Zero when unknown,
	// which `has_taken` distinguishes.
	Taken    time.Time `expr:"taken"`
	HasTaken bool      `expr:"has_taken"`

	// Year is the capture year, 0 when unknown.
	Year int `expr:"year"`

	// Camera is the camera make and model joined by a space, "" when the file names none. A
	// convenience over Exif for the common case.
	Camera string `expr:"camera"`

	// Software is the IFD0:Software tag. Worth its own field because a re-encoder having
	// touched a file is evidence about the file - docs/ARCHITECTURE.md#rules rejected two candidate screenshot
	// resolutions on exactly this ground.
	Software string `expr:"software"`
}

// NewEnv builds an environment from a file's facts.
//
// Derived fields are computed here rather than in each rule so that two rules cannot disagree
// about what `dims` means.
func NewEnv(f File) Env {
	env := Env{
		Ext:               normaliseExt(f.Ext),
		Name:              f.Name,
		Path:              f.Path,
		Dir:               f.Dir,
		Size:              f.Size,
		Width:             f.Width,
		Height:            f.Height,
		Exif:              f.Exif,
		ScreenResolutions: f.ScreenResolutions,
		HasSidecar:        f.HasSidecar,
		Taken:             f.Taken,
		HasTaken:          !f.Taken.IsZero(),
	}

	if env.Exif == nil {
		// A nil map would make `has(exif, …)` and `exif[…]` fail at run time rather than
		// evaluate to false, turning a file with no metadata into an error.
		env.Exif = map[string]any{}
	}

	if env.ScreenResolutions == nil {
		env.ScreenResolutions = []string{}
	}

	if f.Width > 0 && f.Height > 0 {
		env.Dims = fmt.Sprintf("%dx%d", f.Width, f.Height)
		env.Megapixels = float64(f.Width) * float64(f.Height) / 1e6
		env.Portrait = f.Height > f.Width
		env.Landscape = f.Width > f.Height
	}

	if env.HasTaken {
		env.Year = f.Taken.Year()
	}

	env.Camera = camera(env.Exif)
	env.Software = tagString(env.Exif, "IFD0:Software", "Software")

	// The real format, as exiftool read it from the bytes. Falls back to the extension when
	// there is no metadata, which is the honest answer rather than an empty string that every
	// comparison would fail against.
	env.Format = normaliseExt(tagString(env.Exif, "File:FileTypeExtension", "FileTypeExtension"))
	if env.Format == "" {
		env.Format = env.Ext
	}

	env.MIME = strings.ToLower(tagString(env.Exif, "File:MIMEType", "MIMEType"))
	env.FormatMismatch = env.Ext != "" && env.Format != "" && canonicalExt(env.Ext) != canonicalExt(env.Format)

	return env
}

// normaliseExt lowercases an extension and strips a leading dot.
func normaliseExt(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "."))
}

// canonicalExt collapses extensions that name the same format.
func canonicalExt(s string) string {
	if canonical, ok := jpegAliases[s]; ok {
		return canonical
	}

	return s
}

// File is what NewEnv needs to know about a file. A plain struct rather than a store row, so that
// rules can be evaluated in a test without a database.
type File struct {
	Path string
	Dir  string
	Name string
	Ext  string
	Size int64

	Width  int
	Height int

	Exif map[string]any

	HasSidecar bool
	Taken      time.Time

	ScreenResolutions []string
}

// jpegAliases are the extensions that mean the same format, so that a mismatch between them is
// not reported as one.
//
// Nothing else in the archive needed this, and keeping the list short is deliberate: the point of
// FormatMismatch is to surface a file that is not what it claims, and treating "jpeg" and "jpg" as
// different would bury that in noise.
var jpegAliases = map[string]string{"jpeg": "jpg"}

func camera(exif map[string]any) string {
	maker := tagString(exif, "IFD0:Make", "Make")
	model := tagString(exif, "IFD0:Model", "Model")

	switch {
	case maker == "" && model == "":
		return ""
	case maker == "":
		return model
	case model == "":
		return maker
	// Apple writes Make "Apple" and Model "iPhone 7", but some makers repeat the make in the
	// model ("NIKON CORPORATION" / "NIKON D7000"), and "NIKON CORPORATION NIKON D7000" is
	// worse than either.
	case strings.HasPrefix(strings.ToLower(model), strings.ToLower(maker)):
		return model
	default:
		return maker + " " + model
	}
}

// tagString reads the first of several candidate keys that holds a non-empty string.
func tagString(exif map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := exif[k]
		if !ok {
			continue
		}

		if s, ok := v.(string); ok && s != "" {
			return strings.TrimSpace(s)
		}
	}

	return ""
}

// Rule is one compiled rule.
type Rule struct {
	// Index is the rule's position in sicht.yaml, which is what `source = "rule:<index>"`
	// records. Kept through compilation so a result can be traced back to the line that
	// produced it.
	Index int

	Facet      string
	Value      string
	Confidence float64

	// Source is the expression as written, for diagnostics.
	Source string

	program *vm.Program
}

// Set is a compiled rule set, ready to evaluate.
type Set struct {
	rules []Rule
}

// Compile compiles every rule in a configuration.
//
// Compiling all of them and reporting the first failure with its index is deliberate: a typo in
// rule 7 should name rule 7, not fail somewhere in the middle of a scan over ten thousand files.
func Compile(cfg *config.Config) (*Set, error) {
	set := &Set{rules: make([]Rule, 0, len(cfg.Rules))}

	for i, r := range cfg.Rules {
		program, err := expr.Compile(r.When, append([]expr.Option{
			expr.Env(Env{}),
			// The expression must answer yes or no. Without this a rule whose expression
			// returns a string compiles and then behaves unpredictably.
			expr.AsBool(),
			// Note there is deliberately no expr.AllowUndefinedVariables(): with
			// expr.Env set, rejecting unknown variables is the default, and a rule that
			// reads a variable that does not exist should fail to compile rather than
			// quietly evaluate it as nil on every file.
			has(),
		}, groupAgnostic()...)...)
		if err != nil {
			return nil, fmt.Errorf("rules[%d] (%s = %q): %w\n"+
				"hint: the available variables are listed in docs/ARCHITECTURE.md#rules",
				i, r.Facet, r.Value, err)
		}

		set.rules = append(set.rules, Rule{
			Index:      i,
			Facet:      r.Facet,
			Value:      r.Value,
			Confidence: r.Confidence,
			Source:     r.When,
			program:    program,
		})
	}

	return set, nil
}

// has defines the has(map, key) function the rules in docs/ARCHITECTURE.md#rules use.
//
// expr has no built-in for it, and `exif["IFD0:Make"] != nil` is both uglier and wrong for a tag
// that is present and empty.
func has() expr.Option {
	return expr.Function("has",
		func(params ...any) (any, error) {
			if len(params) != 2 {
				return nil, fmt.Errorf("has() takes 2 arguments, got %d", len(params))
			}

			m, ok := params[0].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("has() expects a map as its first argument, got %T", params[0])
			}

			key, ok := params[1].(string)
			if !ok {
				return nil, fmt.Errorf("has() expects a string key, got %T", params[1])
			}

			_, found := m[key]

			return found, nil
		},
		new(func(map[string]any, string) bool),
	)
}

// groupAgnostic defines tags(), tag() and has_tag(), which look a tag up by its bare name across
// every group exiftool reported it under.
//
// These exist because the group prefix is not a detail. In the reference archive the screenshot
// marker lives in **two** groups with different coverage: 100 files have
// `ExifIFD:UserComment == "Screenshot"` and 105 have `XMP-exif:UserComment == "Screenshot"`, with
// 94 in both - a union of 111. A rule naming either group alone misses files, and the shipped rule
// that named ExifIFD saw 99 of 111. Writing `"Screenshot" in tags("UserComment")` catches all of
// them and keeps working when a format records the tag somewhere else again.
//
// has() and an explicit group key remain available for a rule that genuinely means one group.
func groupAgnostic() []expr.Option {
	values := func(m map[string]any, name string) []any {
		var out []any

		// Exact key first, then every group-qualified one, in sorted group order so the
		// result never depends on map iteration.
		if v, ok := m[name]; ok {
			out = append(out, v)
		}

		keys := make([]string, 0, len(m))
		suffix := ":" + name

		for k := range m {
			if strings.HasSuffix(k, suffix) {
				keys = append(keys, k)
			}
		}

		slices.Sort(keys)

		for _, k := range keys {
			out = append(out, m[k])
		}

		return out
	}

	return []expr.Option{
		// tags(exif, "UserComment") returns every group's value for that tag. Prefer it
		// for an equality test: `"Screenshot" in tags(exif, "UserComment")` is exact even
		// when two groups disagree.
		expr.Function("tags",
			func(params ...any) (any, error) {
				m, name, err := mapAndName("tags", params)
				if err != nil {
					return nil, err
				}

				return values(m, name), nil
			},
			new(func(map[string]any, string) []any),
		),

		// tag(exif, "Make") returns the first value, in sorted group order. Convenient when
		// a tag realistically appears once.
		expr.Function("tag",
			func(params ...any) (any, error) {
				m, name, err := mapAndName("tag", params)
				if err != nil {
					return nil, err
				}

				found := values(m, name)
				if len(found) == 0 {
					return nil, nil
				}

				return found[0], nil
			},
			new(func(map[string]any, string) any),
		),

		// has_tag(exif, "Make") reports presence in any group.
		expr.Function("has_tag",
			func(params ...any) (any, error) {
				m, name, err := mapAndName("has_tag", params)
				if err != nil {
					return nil, err
				}

				return len(values(m, name)) > 0, nil
			},
			new(func(map[string]any, string) bool),
		),
	}
}

// mapAndName validates the two arguments the group-agnostic helpers share.
func mapAndName(fn string, params []any) (map[string]any, string, error) {
	if len(params) != 2 {
		return nil, "", fmt.Errorf("%s() takes 2 arguments, got %d", fn, len(params))
	}

	m, ok := params[0].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("%s() expects a map as its first argument, got %T", fn, params[0])
	}

	name, ok := params[1].(string)
	if !ok {
		return nil, "", fmt.Errorf("%s() expects a string tag name, got %T", fn, params[1])
	}

	return m, name, nil
}

// Len returns how many rules are in the set.
func (s *Set) Len() int { return len(s.rules) }

// Facets returns the facets the set can decide, sorted and deduplicated. Used by `stats` and by
// `run` to work out which facets a stage still has to cover.
func (s *Set) Facets() []string {
	seen := map[string]struct{}{}

	var out []string

	for _, r := range s.rules {
		if _, dup := seen[r.Facet]; dup {
			continue
		}

		seen[r.Facet] = struct{}{}

		out = append(out, r.Facet)
	}

	return out
}

// Decision is one rule's verdict about one facet of one file.
type Decision struct {
	Facet      string
	Value      string
	Confidence float64

	// RuleIndex is the index of the rule that decided it, for `source = "rule:<index>"`.
	RuleIndex int
}

// Source renders the decision's provenance as it is stored in the results table.
func (d Decision) Source() string {
	return fmt.Sprintf("rule:%d", d.RuleIndex)
}

// Evaluate runs the set against one environment and returns at most one decision per facet.
//
// First match per facet wins, so rule order in sicht.yaml is significant (docs/ARCHITECTURE.md#rules). Later rules for
// an already-decided facet are skipped rather than evaluated, which is also what makes a cheap
// high-precision rule worth putting first.
func (s *Set) Evaluate(env Env) ([]Decision, error) {
	var (
		out     []Decision
		decided = map[string]struct{}{}
		errs    []error
	)

	for _, r := range s.rules {
		if _, done := decided[r.Facet]; done {
			continue
		}

		matched, err := r.match(env)
		if err != nil {
			// Collect rather than abort: one rule that trips over an unexpected tag type
			// must not stop the other facets from being decided for this file.
			errs = append(errs, err)

			continue
		}

		if !matched {
			continue
		}

		decided[r.Facet] = struct{}{}

		out = append(out, Decision{
			Facet:      r.Facet,
			Value:      r.Value,
			Confidence: r.Confidence,
			RuleIndex:  r.Index,
		})
	}

	return out, errors.Join(errs...)
}

func (r Rule) match(env Env) (bool, error) {
	result, err := expr.Run(r.program, env)
	if err != nil {
		return false, fmt.Errorf("rules[%d] (%s = %q) failed on %q: %w",
			r.Index, r.Facet, r.Value, env.Path, err)
	}

	matched, ok := result.(bool)
	if !ok {
		// expr.AsBool() makes this unreachable through Compile; it is here so that a
		// change to the compile options cannot turn a type error into a silent false.
		return false, fmt.Errorf("rules[%d] returned %T, want bool", r.Index, result)
	}

	return matched, nil
}

// Filter is a compiled standalone predicate over the rule environment.
//
// Used by `stats --where`, so that narrowing a report uses exactly the same language and the same
// variables as a rule. Learning one expression language for the whole tool is the point.
type Filter struct {
	program *vm.Program
	source  string
}

// CompileFilter compiles a predicate. An empty expression yields a nil Filter, which matches
// everything - so a caller need not special-case the absence of a filter.
func CompileFilter(expression string) (*Filter, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, nil
	}

	program, err := expr.Compile(expression, append([]expr.Option{
		expr.Env(Env{}),
		expr.AsBool(),
		has(),
	}, groupAgnostic()...)...)
	if err != nil {
		return nil, fmt.Errorf("invalid filter %q: %w\n"+
			"hint: the available variables are the same as a rule's, listed in "+
			"docs/ARCHITECTURE.md#rules", expression, err)
	}

	return &Filter{program: program, source: expression}, nil
}

// Match reports whether the environment satisfies the filter. A nil Filter matches everything.
func (f *Filter) Match(env Env) (bool, error) {
	if f == nil {
		return true, nil
	}

	result, err := expr.Run(f.program, env)
	if err != nil {
		return false, fmt.Errorf("filter %q failed on %q: %w", f.source, env.Path, err)
	}

	matched, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("filter %q returned %T, want bool", f.source, result)
	}

	return matched, nil
}

// String returns the filter's source, or "" for a nil filter.
func (f *Filter) String() string {
	if f == nil {
		return ""
	}

	return f.source
}

// EnvFor builds an evaluation environment for a stored file. Exported so that `stats` can filter
// with the same variables a rule sees.
func EnvFor(in store.RuleInput, cfg *config.Config, log *slog.Logger) Env {
	return envFor(in, cfg, log)
}
