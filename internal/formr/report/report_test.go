package report_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/addr"
	"github.com/adnoctem/pertry/internal/formr/analysis"
	"github.com/adnoctem/pertry/internal/formr/formrtest"
	"github.com/adnoctem/pertry/internal/formr/module"
	"github.com/adnoctem/pertry/internal/formr/report"
	"github.com/adnoctem/pertry/internal/formr/schema"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func render(t *testing.T, files map[string]string) string {
	t.Helper()

	tree, diags := module.LoadTree(formrtest.WriteModule(t, files))
	if tree == nil {
		t.Fatalf("load: %v", diags)
	}
	set, err := schema.Decode([]byte(formrtest.Schema))
	if err != nil {
		t.Fatal(err)
	}
	res := analysis.Analyze(tree, set)
	res.Diagnostics = append(diags, res.Diagnostics...)

	p, _ := addr.ParseProvider(formrtest.Provider)

	return report.Render(report.Input{
		Analysis:     res,
		Provenance:   schema.Provenance{Engine: schema.EngineInfo{Name: "terraform", Version: "1.16.5"}, Providers: map[addr.Provider]string{p: "1.2.3"}},
		SchemaFormat: set.FormatVersion,
	})
}

func TestGolden(t *testing.T) {
	got := render(t, formrtest.Main)
	path := filepath.Join("testdata", "main.golden.md")

	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if diff := report.Diff(string(want), got); len(diff) > 0 {
		t.Errorf("report differs from %s:\n%s", path, strings.Join(diff, "\n"))
	}
}

// The same module must produce the same bytes on every run and whatever its files are called.
func TestDeterministic(t *testing.T) {
	first := render(t, formrtest.Main)
	if again := render(t, formrtest.Main); again != first {
		t.Fatal("two runs over the same module differ")
	}

	renamed := formrtest.With(formrtest.Main, map[string]string{
		"main.tf":    "",
		"zz_main.tf": formrtest.Main["main.tf"],
		"outputs.tf": "",
		"a_out.tf":   formrtest.Main["outputs.tf"],
	})
	if got := render(t, renamed); got != first {
		t.Errorf("renaming files changed the report:\n%s", strings.Join(report.Diff(first, got), "\n"))
	}
}

// No value from the module ever reaches the report: not a literal, not a sensitive default,
// not user data. Every such value in the fixture contains SYNTHETIC.
func TestNoValuesInReport(t *testing.T) {
	got := render(t, formrtest.Main)
	if strings.Contains(got, "SYNTHETIC") {
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "SYNTHETIC") {
				t.Errorf("value leaked into the report: %s", line)
			}
		}
	}
	// No machine path either.
	if strings.Contains(got, os.TempDir()) {
		t.Error("the report contains a temporary-directory path")
	}
}

func TestTablesAreAligned(t *testing.T) {
	got := render(t, formrtest.Main)
	var width int
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "|") {
			width = 0
			continue
		}
		w := len([]rune(line))
		if width == 0 {
			width = w
			continue
		}
		if w != width {
			t.Errorf("table row is %d wide, its table %d: %s", w, width, line)
		}
	}
}

func TestDiff(t *testing.T) {
	if d := report.Diff("a\nb\nc\n", "a\nb\nc\n"); len(d) != 0 {
		t.Errorf("identical inputs differ: %v", d)
	}
	got := strings.Join(report.Diff("a\nb\nc\n", "a\nx\nc\n"), " ")
	if got != "-b +x" {
		t.Errorf("diff = %q, want \"-b +x\"", got)
	}
}
