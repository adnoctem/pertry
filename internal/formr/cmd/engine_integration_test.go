//go:build integration

package cmd_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/cmd"
	"github.com/adnoctem/pertry/internal/formr/formrtest"
)

// TestRealEngines drives each installed engine through the real os/exec adapter: version
// parsing, `init -backend=false -input=false` and `providers schema -json`. The module declares
// no providers, so nothing is downloaded and the test runs offline; what it proves is that both
// engines accept the same commands and flags, which the adapter assumes rather than branches on.
func TestRealEngines(t *testing.T) {
	engines := map[string]string{"terraform": "terraform", "tofu": "opentofu"}
	ran := 0

	for bin, name := range engines {
		path, err := exec.LookPath(bin)
		if err != nil {
			t.Logf("%s not on PATH; skipped", bin)
			continue
		}
		ran++

		t.Run(bin, func(t *testing.T) {
			dir := formrtest.WriteModule(t, map[string]string{
				"main.tf": "terraform {\n  required_version = \">= 1.0\"\n}\n\nvariable \"name\" {\n  type = string\n}\n",
			})
			report := filepath.Join(dir, "SCHEMA.md")

			var out, errOut bytes.Buffer
			app := &cmd.App{Out: &out, Err: &errOut}
			if code := cmd.Execute(t.Context(), app, []string{"schema", dir, "--init", "--engine", path, "--output", report}); code != cmd.ExitOK {
				t.Fatalf("schema exited %d:\n%s", code, errOut.String())
			}

			out.Reset()
			errOut.Reset()
			if code := cmd.Execute(t.Context(), app, []string{"check", dir, "--engine", path, "--report", report}); code != cmd.ExitOK {
				t.Fatalf("check exited %d:\n%s", code, errOut.String())
			}
			if !strings.Contains(out.String(), "is current") {
				t.Errorf("check output: %s", out.String())
			}

			text := readFile(t, report)
			if !strings.Contains(text, "| Engine        | "+name+" ") {
				t.Errorf("the report does not name the engine as %s:\n%s", name, text)
			}
		})
	}

	if ran == 0 {
		t.Skip("neither terraform nor tofu is on PATH")
	}
}
