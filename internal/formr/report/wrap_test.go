package report_test

import (
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/formrtest"
	"github.com/adnoctem/pertry/internal/formr/report"
)

func TestWrap(t *testing.T) {
	in := strings.Join([]string{
		"# A heading that is much longer than the width and must never be wrapped at all",
		"",
		"A paragraph of prose that is long enough to wrap, with `a code span that has spaces` in it.",
		"",
		"- **warning** (`.`): a list item that is long enough to wrap onto a continuation line or two",
		"",
		"| Field | Input |",
		"| ----- | ----- |",
		"| `x`   | a table row is never wrapped however long it is, because a row cannot span lines |",
		"",
	}, "\n")

	got := report.Wrap(in, 40)
	lines := strings.Split(got, "\n")

	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "#"), strings.HasPrefix(l, "|"):
		case len([]rune(l)) > 40:
			t.Errorf("line exceeds 40: %q", l)
		}
	}
	if !strings.Contains(got, "`a code span that has spaces`") {
		t.Errorf("a code span was split:\n%s", got)
	}
	for i, l := range lines {
		if i > 0 && strings.HasPrefix(lines[i-1], "- ") && l != "" && !strings.HasPrefix(l, "  ") {
			t.Errorf("list continuation %q is not indented", l)
		}
	}
	if report.Wrap(got, 40) != got {
		t.Error("wrapping is not idempotent")
	}
	if report.Wrap(in, 0) != in {
		t.Error("width 0 changed the text")
	}
}

// A wrapped report is still the same report: only prose line breaks may differ.
func TestWrappedReportKeepsItsRows(t *testing.T) {
	plain := render(t, formrtest.Main)
	wrapped := report.Wrap(plain, 80)
	if changes := report.Changes(plain, wrapped); len(changes) == 0 {
		t.Fatal("wrapping at 80 changed nothing; the fixture has no long prose to test with")
	}
	for _, c := range report.Changes(plain, wrapped) {
		if strings.HasPrefix(c.Row, "`") {
			t.Errorf("wrapping changed table row %s: %s", c.Section, c.Row)
		}
	}
}

func TestChanges(t *testing.T) {
	before := "### `x.this`\n\n| Field | Input |\n| ----- | ----- |\n| `a`   | direct: `var.a` |\n| `b`   | omitted |\n"
	after := "### `x.this`\n\n| Field | Input |\n| ----- | ----- |\n| `a`   | omitted |\n| `c`   | fixed |\n"

	got := report.Changes(before, after)
	want := []report.Change{
		{Kind: report.Changed, Section: "`x.this`", Row: "`a`", Columns: []string{"Input: direct: `var.a` → omitted"}},
		{Kind: report.Removed, Section: "`x.this`", Row: "`b`"},
		{Kind: report.Added, Section: "`x.this`", Row: "`c`"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Section != want[i].Section || got[i].Row != want[i].Row ||
			strings.Join(got[i].Columns, ";") != strings.Join(want[i].Columns, ";") {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Re-padding a table because one cell grew is not a change to its other rows.
func TestChangesIgnorePadding(t *testing.T) {
	before := "## S\n\n| Field | Input |\n| ----- | ----- |\n| `a`   | x     |\n| `b`   | y     |\n"
	after := "## S\n\n| Field | Input      |\n| ----- | ---------- |\n| `a`   | x          |\n| `b`   | y but more |\n"
	got := report.Changes(before, after)
	if len(got) != 1 || got[0].Row != "`b`" {
		t.Errorf("changes = %+v, want only `b`", got)
	}
}

func TestChangesQuoteTheDifference(t *testing.T) {
	long := strings.Repeat("`var.load_balancers[].pools`, ", 6)
	before := "## S\n\n| Field | Input |\n| ----- | ----- |\n| `a`   | derived: " + long + "`var.x` |\n"
	after := "## S\n\n| Field | Input |\n| ----- | ----- |\n| `a`   | derived: " + long + "`var.y` |\n"
	got := report.Changes(before, after)
	if len(got) != 1 || len(got[0].Columns) != 1 {
		t.Fatalf("changes = %+v", got)
	}
	if col := got[0].Columns[0]; !strings.Contains(col, "var.x") || !strings.Contains(col, "var.y") {
		t.Errorf("the excerpt does not show the difference: %s", col)
	}
}
