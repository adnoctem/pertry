package classify_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/classify"
	"github.com/adnoctem/pertry/internal/bookscape/config"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

func TestApplyWithDefaultRules(t *testing.T) {
	t.Parallel()

	engine := classify.NewRuleEngine(config.Default().Classify.Rules)

	tests := map[string]struct {
		target     store.ClassifyTarget
		collection string
		tags       []string
	}{
		"code host": {
			target:     store.ClassifyTarget{Host: "github.com"},
			collection: "Development",
			tags:       []string{"repo"},
		},
		"another code host": {
			target:     store.ClassifyTarget{Host: "codeberg.org"},
			collection: "Development",
			tags:       []string{"repo"},
		},
		"q&a site": {
			target:     store.ClassifyTarget{Host: "stackoverflow.com"},
			collection: "Reference",
			tags:       []string{"qa"},
		},
		"stackexchange subdomain": {
			target:     store.ClassifyTarget{Host: "superuser.stackexchange.com"},
			collection: "Reference",
			tags:       []string{"qa"},
		},
		"video platform": {
			target:     store.ClassifyTarget{Host: "youtu.be"},
			collection: "Media",
			tags:       []string{"video"},
		},
		"folder path only": {
			target:     store.ClassifyTarget{Host: "example.com", SourcePaths: []string{"Dev/Go"}},
			collection: "Development",
		},
		"domain and folder path both match": {
			// First match wins for the collection; tags accumulate from every match.
			target:     store.ClassifyTarget{Host: "github.com", SourcePaths: []string{"Dev/Go"}},
			collection: "Development",
			tags:       []string{"repo"},
		},
		"nothing matches": {
			target: store.ClassifyTarget{Host: "example.com", SourcePaths: []string{"Random"}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := engine.Apply(tc.target)

			if got.Collection != tc.collection {
				t.Errorf("Collection = %q, want %q", got.Collection, tc.collection)
			}

			if diff := cmp.Diff(tc.tags, got.Tags, cmp.Comparer(equalStringSlices)); diff != "" {
				t.Errorf("Tags mismatch (-want +got):\n%s", diff)
			}

			if want := tc.collection != ""; got.Decided() != want {
				t.Errorf("Decided() = %v, want %v", got.Decided(), want)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestApplyAccumulatesTagsAcrossRules states the asymmetry: one collection, many tags.
func TestApplyAccumulatesTagsAcrossRules(t *testing.T) {
	t.Parallel()

	engine := classify.NewRuleEngine([]config.Rule{
		{MatchDomain: []string{"github.com"}, Collection: "Development", Tags: []string{"repo"}},
		{MatchSourcePath: []string{"Dev/*"}, Tags: []string{"dev"}},
		{MatchSourcePath: []string{"Reading/*"}, Collection: "Reading", Tags: []string{"later"}},
	})

	got := engine.Apply(store.ClassifyTarget{
		Host:        "github.com",
		SourcePaths: []string{"Dev/Go", "Reading/Queue"},
	})

	if got.Collection != "Development" {
		t.Errorf("Collection = %q, want %q (the first matching rule that names one)", got.Collection, "Development")
	}

	if diff := cmp.Diff([]string{"repo", "dev", "later"}, got.Tags); diff != "" {
		t.Errorf("Tags mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]int{0, 1, 2}, got.Matched); diff != "" {
		t.Errorf("Matched mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyTagOnlyRuleLeavesTheBookmarkUndecided matters because the collection is the one axis
// export projects onto: a tagged but uncollected bookmark must still reach the model pass.
func TestApplyTagOnlyRuleLeavesTheBookmarkUndecided(t *testing.T) {
	t.Parallel()

	engine := classify.NewRuleEngine([]config.Rule{
		{MatchDomain: []string{"example.com"}, Tags: []string{"noted"}},
	})

	got := engine.Apply(store.ClassifyTarget{Host: "example.com"})

	if got.Decided() {
		t.Error("Decided() = true, want false for a rule that only assigned tags")
	}

	if diff := cmp.Diff([]string{"noted"}, got.Tags); diff != "" {
		t.Errorf("Tags mismatch (-want +got):\n%s", diff)
	}
}

func TestMatchDomain(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pattern string
		host    string
		want    bool
	}{
		"exact":                     {pattern: "github.com", host: "github.com", want: true},
		"case-insensitive pattern":  {pattern: "GitHub.com", host: "github.com", want: true},
		"different host":            {pattern: "github.com", host: "gitlab.com", want: false},
		"subdomain is not exact":    {pattern: "github.com", host: "gist.github.com", want: false},
		"suffix is not a match":     {pattern: "github.com", host: "notgithub.com", want: false},
		"wildcard subdomain":        {pattern: "*.stackexchange.com", host: "superuser.stackexchange.com", want: true},
		"wildcard deep subdomain":   {pattern: "*.stackexchange.com", host: "a.b.stackexchange.com", want: true},
		"wildcard excludes apex":    {pattern: "*.stackexchange.com", host: "stackexchange.com", want: false},
		"wildcard needs the suffix": {pattern: "*.stackexchange.com", host: "stackexchange.com.evil.test", want: false},
		"empty pattern":             {pattern: "", host: "github.com", want: false},
		"empty host":                {pattern: "github.com", host: "", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			engine := classify.NewRuleEngine([]config.Rule{
				{MatchDomain: []string{tc.pattern}, Collection: "X"},
			})

			got := engine.Apply(store.ClassifyTarget{Host: tc.host}).Decided()
			if got != tc.want {
				t.Errorf("matchDomain(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
			}
		})
	}
}

func TestMatchSourcePath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pattern string
		path    string
		want    bool
	}{
		"exact":                   {pattern: "Dev", path: "Dev", want: true},
		"exact is case-blind":     {pattern: "dev", path: "Dev", want: true},
		"exact excludes children": {pattern: "Dev", path: "Dev/Go", want: false},
		"glob includes the folder itself": {
			// A link filed directly in Dev carries the same signal as one in Dev/Go.
			pattern: "Dev/*", path: "Dev", want: true,
		},
		"glob includes children":      {pattern: "Dev/*", path: "Dev/Go", want: true},
		"glob includes grandchildren": {pattern: "Dev/*", path: "Dev/Go/stdlib", want: true},
		"glob is case-blind":          {pattern: "dev/*", path: "DEV/Go", want: true},
		"glob does not match a sibling prefix": {
			// "Development" must not match "Dev/*".
			pattern: "Dev/*", path: "Development", want: false,
		},
		"glob does not match elsewhere": {pattern: "Dev/*", path: "Reading/Dev", want: false},
		"empty pattern":                 {pattern: "", path: "Dev", want: false},
		"empty path":                    {pattern: "Dev", path: "", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			engine := classify.NewRuleEngine([]config.Rule{
				{MatchSourcePath: []string{tc.pattern}, Collection: "X"},
			})

			got := engine.Apply(store.ClassifyTarget{SourcePaths: []string{tc.path}}).Decided()
			if got != tc.want {
				t.Errorf("matchSourcePath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// TestApplyMatchesAnyOfSeveralSourcePaths is why source_paths is multi-valued: a link filed
// under two folders carries both signals, and either one may be the one a rule recognises.
func TestApplyMatchesAnyOfSeveralSourcePaths(t *testing.T) {
	t.Parallel()

	engine := classify.NewRuleEngine([]config.Rule{
		{MatchSourcePath: []string{"Programming/*"}, Collection: "Development"},
	})

	got := engine.Apply(store.ClassifyTarget{
		SourcePaths: []string{"Random bookmarks", "Programming/Rust"},
	})

	if got.Collection != "Development" {
		t.Errorf("Collection = %q, want %q", got.Collection, "Development")
	}
}

func TestApplyWithNoRules(t *testing.T) {
	t.Parallel()

	got := classify.NewRuleEngine(nil).Apply(store.ClassifyTarget{Host: "github.com"})

	if got.Decided() {
		t.Error("Decided() = true, want false with no rules configured")
	}
}
