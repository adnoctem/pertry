package dedupe_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/adnoctem/pertry/internal/bookscape/dedupe"
	"github.com/adnoctem/pertry/internal/bookscape/store"
)

func ptr(v int64) *int64 { return &v }

// group builds a Group with candidates already in the election order
// store.DuplicateGroups produces: a known date beats none, then earliest, then lowest id.
func group(key string, candidates ...store.Candidate) store.Group {
	return store.Group{URLNorm: key, Candidates: candidates}
}

func TestBuildSkipsGroupsOfOne(t *testing.T) {
	t.Parallel()

	plan := dedupe.Build([]store.Group{
		group("https://a.test/", store.Candidate{ID: 1, URL: "https://a.test/"}),
	})

	if !plan.Empty() {
		t.Errorf("Plan has %d merges, want none for a group of one", len(plan.Merges))
	}
}

// TestBuildElectsTheEarliestSave is the rule from section 5.3: the survivor is the original
// save, not a later re-save.
func TestBuildElectsTheEarliestSave(t *testing.T) {
	t.Parallel()

	plan := dedupe.Build([]store.Group{group("https://a.test/",
		store.Candidate{ID: 5, URL: "https://a.test/one", AddDate: ptr(100)},
		store.Candidate{ID: 2, URL: "https://a.test/two", AddDate: ptr(200)},
		store.Candidate{ID: 9, URL: "https://a.test/three", AddDate: ptr(300)},
	)})

	if len(plan.Merges) != 1 {
		t.Fatalf("Plan has %d merges, want 1", len(plan.Merges))
	}

	m := plan.Merges[0]

	if m.SurvivorID != 5 {
		t.Errorf("SurvivorID = %d, want 5 (the earliest add_date)", m.SurvivorID)
	}

	if diff := cmp.Diff([]int64{2, 9}, m.LoserIDs); diff != "" {
		t.Errorf("LoserIDs mismatch (-want +got):\n%s", diff)
	}

	if plan.Collapsed != 2 {
		t.Errorf("Plan.Collapsed = %d, want 2", plan.Collapsed)
	}
}

func TestBuildMergesFieldValues(t *testing.T) {
	t.Parallel()

	plan := dedupe.Build([]store.Group{group("https://a.test/",
		store.Candidate{
			ID: 1, URL: "https://a.test/", AddDate: ptr(200), LastModified: ptr(250),
			Title: "a.test", SourcePaths: []string{"Bookmarks bar"}, Tags: []string{"b"},
		},
		store.Candidate{
			ID: 2, URL: "http://www.a.test/", AddDate: ptr(100), LastModified: ptr(900),
			Title: "The full and informative title", Description: "a description",
			SourcePaths: []string{"Reading List"}, Tags: []string{"a", "b"},
		},
		store.Candidate{
			ID: 3, URL: "https://a.test", AddDate: nil, Title: "",
			Private: true, SourcePaths: []string{"Bookmarks bar"},
		},
	)})

	m := plan.Merges[0]

	t.Run("add_date is the group minimum", func(t *testing.T) {
		if m.AddDate == nil || *m.AddDate != 100 {
			t.Errorf("AddDate = %v, want 100", m.AddDate)
		}
	})

	t.Run("last_modified is the group maximum", func(t *testing.T) {
		// Earliest creation, latest modification: the group's identity is both endpoints.
		if m.LastModified == nil || *m.LastModified != 900 {
			t.Errorf("LastModified = %v, want 900", m.LastModified)
		}
	})

	t.Run("title is the longest", func(t *testing.T) {
		// Chrome stores "" or the bare hostname often enough that the longest title is
		// almost always the most informative one.
		if want := "The full and informative title"; m.Title != want {
			t.Errorf("Title = %q, want %q", m.Title, want)
		}
	})

	t.Run("description is the longest non-empty", func(t *testing.T) {
		if want := "a description"; m.Description != want {
			t.Errorf("Description = %q, want %q", m.Description, want)
		}
	})

	t.Run("private if any row was", func(t *testing.T) {
		// Erring toward private is the only safe direction: un-marking a link the user
		// deliberately hid would leak it.
		if !m.Private {
			t.Error("Private = false, want true because one row in the group was private")
		}
	})

	t.Run("source paths are the union", func(t *testing.T) {
		// A link filed under two folders carries two classification signals; discarding
		// either loses information.
		if diff := cmp.Diff([]string{"Bookmarks bar", "Reading List"}, m.SourcePaths); diff != "" {
			t.Errorf("SourcePaths mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("tags are the union", func(t *testing.T) {
		if diff := cmp.Diff([]string{"a", "b"}, m.Tags); diff != "" {
			t.Errorf("Tags mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("the reason is recorded", func(t *testing.T) {
		if m.Reason != dedupe.Reason {
			t.Errorf("Reason = %q, want %q", m.Reason, dedupe.Reason)
		}
	})
}

// TestBuildWithNoDatesAtAll covers a group where nothing has an add_date: the survivor is still
// elected (by the ordering the store applied) and add_date stays nil rather than becoming zero.
func TestBuildWithNoDatesAtAll(t *testing.T) {
	t.Parallel()

	plan := dedupe.Build([]store.Group{group("https://a.test/",
		store.Candidate{ID: 4, URL: "https://a.test/"},
		store.Candidate{ID: 7, URL: "https://a.test"},
	)})

	m := plan.Merges[0]

	if m.SurvivorID != 4 {
		t.Errorf("SurvivorID = %d, want 4", m.SurvivorID)
	}

	if m.AddDate != nil {
		t.Errorf("AddDate = %d, want nil; an unknown date must not become a 1970 date", *m.AddDate)
	}

	if m.LastModified != nil {
		t.Errorf("LastModified = %d, want nil", *m.LastModified)
	}
}

// TestDescribePrintsBothOriginalURLs is what makes --dry-run auditable. Stripping "www." and
// forcing https will merge a small number of genuinely distinct hosts, and that is only
// visible by reading the originals.
func TestDescribePrintsBothOriginalURLs(t *testing.T) {
	t.Parallel()

	candidates := []store.Candidate{
		{ID: 1, URL: "https://a.test/page", AddDate: ptr(100), SourcePaths: []string{"Dev"}},
		{ID: 2, URL: "http://www.a.test/page/", AddDate: ptr(200), SourcePaths: []string{"Reading"}},
	}

	plan := dedupe.Build([]store.Group{group("https://a.test/page", candidates...)})
	out := dedupe.Describe(plan.Merges[0], candidates)

	for _, want := range []string{
		"https://a.test/page",
		"http://www.a.test/page/",
		"keep  [1]",
		"merge [2]",
		"Dev",
		"Reading",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe() = %q, want it to contain %q", out, want)
		}
	}
}
