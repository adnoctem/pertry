// Package apply moves files according to a plan, recording everything it does in a manifest so
// that it can be reversed (docs/ARCHITECTURE.md#apply-manifests-and-undo).
//
// Nothing here moves a file until the whole plan has been evaluated and every target validated.
// docs/ARCHITECTURE.md#apply-manifests-and-undo requires that: a plan that would produce one invalid path must not have already moved a
// thousand files by the time anyone finds out.
package apply

import (
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/adnoctem/pertry/internal/fsops"
)

// targetFuncs are the functions a target template may use.
//
// Deliberately few. A target is a filing decision, not a program, and every function here exists
// because docs/ARCHITECTURE.md#apply-manifests-and-undo's own example needs it or because a facet value from a model needs taming before
// it becomes a directory name.
var targetFuncs = template.FuncMap{
	// default supplies a value for an undecided facet. docs/ARCHITECTURE.md#apply-manifests-and-undo's example is
	// `{{ .platform | default "unknown" }}`, and without it an undecided facet would
	// silently produce an empty path segment.
	"default": func(fallback string, value any) string {
		s := fmt.Sprint(value)
		if strings.TrimSpace(s) == "" || value == nil {
			return fallback
		}

		return s
	},

	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"title": strings.ToTitle,
	"trim":  strings.TrimSpace,

	"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },

	// slug makes a value safe and predictable as a directory name: lowercase, with runs of
	// anything awkward collapsed to a single hyphen. Useful for a model-supplied value like
	// "Twitter / X" that would otherwise become "Twitter _ X".
	"slug": slug,
}

// slug lowercases a value and reduces it to letters, digits and single hyphens.
func slug(s string) string {
	var b strings.Builder

	lastHyphen := false

	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)

			lastHyphen = false
		case r > 0x7f:
			// Keep non-ASCII letters: a German or Japanese facet value should not be
			// reduced to hyphens.
			b.WriteRune(r)

			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteRune('-')

				lastHyphen = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

// Target renders a plan's target path for one file.
type Target struct {
	tmpl   *template.Template
	source string
}

// CompileTarget parses a target template.
func CompileTarget(expression string) (*Target, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, errors.New("a plan needs a 'target' template\n" +
			"hint: `sorted/{{ .kind }}` files by the kind facet")
	}

	tmpl, err := template.New("target").
		Funcs(targetFuncs).
		// An undecided facet renders as empty rather than as "<no value>", which is what
		// makes `| default "unknown"` work at all.
		Option("missingkey=zero").
		Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("invalid target template %q: %w\n"+
			"hint: facet values are available as {{ .kind }} and so on; the functions are "+
			"default, lower, upper, title, trim, replace and slug", expression, err)
	}

	return &Target{tmpl: tmpl, source: expression}, nil
}

// Render produces a workspace-relative directory for a file, given its facet values.
//
// The result is sanitised segment by segment, so a facet value cannot introduce a directory level,
// escape the workspace, or produce a name Windows will not open. A template that renders to
// nothing usable is an error rather than a file dropped at the workspace root.
func (t *Target) Render(facets map[string]string, extra map[string]any) (string, error) {
	data := make(map[string]any, len(facets)+len(extra)+1)

	for k, v := range facets {
		data[k] = v
	}

	// File attributes live under a prefix so that a facet can never be shadowed by one. A
	// facet called "year" is a user's choice; `.file.year` is always the file's own.
	fileData := make(map[string]any, len(extra))
	for k, v := range extra {
		fileData[k] = v
	}

	data["file"] = fileData

	var out strings.Builder

	if err := t.tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render target %q: %w", t.source, err)
	}

	rendered := strings.TrimSpace(out.String())
	if rendered == "" {
		return "", fmt.Errorf("target %q rendered to nothing for facets %v", t.source, facets)
	}

	clean, err := fsops.SanitiseRelative(rendered)
	if err != nil {
		return "", fmt.Errorf("target %q rendered %q, which is not usable: %w",
			t.source, rendered, err)
	}

	return clean, nil
}

// String returns the template's source.
func (t *Target) String() string {
	if t == nil {
		return ""
	}

	return t.source
}
