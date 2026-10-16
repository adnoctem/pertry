package cmd_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adnoctem/pertry/internal/formr/cmd"
	"github.com/adnoctem/pertry/internal/formr/formrtest"
)

// fakeEngine answers the three commands formr may ask for and records every invocation.
type fakeEngine struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeEngine) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()

	switch args[0] {
	case "version":
		return []byte("Terraform v1.16.5\non linux_amd64\n"), nil
	case "providers":
		return []byte(formrtest.Schema), nil
	default:
		return nil, nil
	}
}

func run(t *testing.T, eng *fakeEngine, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cmd.Execute(t.Context(), &cmd.App{Out: &out, Err: &errOut, Runner: eng}, args)

	return code, out.String(), errOut.String()
}

func TestSchemaThenCheck(t *testing.T) {
	dir := formrtest.WriteModule(t, formrtest.Main)
	report := filepath.Join(dir, "SCHEMA.md")
	eng := &fakeEngine{}

	if code, _, stderr := run(t, eng, "schema", dir, "--output", report); code != cmd.ExitOK {
		t.Fatalf("schema exited %d: %s", code, stderr)
	}
	before, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}

	if code, stdout, stderr := run(t, eng, "check", dir, "--report", report); code != cmd.ExitOK {
		t.Fatalf("check exited %d: %s%s", code, stdout, stderr)
	}

	// check writes nothing.
	after, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("check modified the report")
	}

	// And it never asks the engine to plan, apply, refresh or import.
	for _, c := range eng.calls {
		switch c[0] {
		case "version", "providers":
		default:
			t.Errorf("formr ran `%s`", strings.Join(c, " "))
		}
	}
}

func TestCheckDetectsDrift(t *testing.T) {
	dir := formrtest.WriteModule(t, formrtest.Main)
	report := filepath.Join(dir, "SCHEMA.md")
	eng := &fakeEngine{}
	if code, _, stderr := run(t, eng, "schema", dir, "--output", report); code != cmd.ExitOK {
		t.Fatalf("schema exited %d: %s", code, stderr)
	}

	// Remove a mapped assignment: the report must notice.
	main := strings.Replace(formrtest.Main["main.tf"], "  size   = var.size\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := run(t, eng, "check", dir, "--report", report)
	if code != cmd.ExitFailed {
		t.Fatalf("check exited %d, want %d: %s", code, cmd.ExitFailed, stderr)
	}
	if !strings.Contains(stderr, "out of date") || !strings.Contains(stderr, "omitted") {
		t.Errorf("stderr does not explain the drift:\n%s", stderr)
	}
}

// Rewriting the report cannot make a policy failure pass: the policy is evaluated against the
// regenerated analysis.
func TestPolicyIgnoresTheCommittedReport(t *testing.T) {
	files := formrtest.With(formrtest.Main, map[string]string{"fn.tf": "resource \"acme_volume\" \"f\" {\n  name = file(\"x\")\n}\n"})
	dir := formrtest.WriteModule(t, files)
	report := filepath.Join(dir, "SCHEMA.md")
	eng := &fakeEngine{}
	if code, _, stderr := run(t, eng, "schema", dir, "--output", report); code != cmd.ExitOK {
		t.Fatalf("schema exited %d: %s", code, stderr)
	}

	code, _, stderr := run(t, eng, "check", dir, "--report", report)
	if code != cmd.ExitFailed || !strings.Contains(stderr, "unresolved") {
		t.Errorf("check exited %d, want %d for an unresolved mapping:\n%s", code, cmd.ExitFailed, stderr)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := formrtest.WriteModule(t, formrtest.Main)
	snap := filepath.Join(t.TempDir(), "providers.json")
	eng := &fakeEngine{}

	if code, _, stderr := run(t, eng, "snapshot", dir, "--output", snap); code != cmd.ExitOK {
		t.Fatalf("snapshot exited %d: %s", code, stderr)
	}
	_, fromEngine, _ := run(t, eng, "schema", dir)

	calls := len(eng.calls)
	_, fromSnapshot, stderr := run(t, eng, "schema", dir, "--schema-file", snap)
	if len(eng.calls) != calls {
		t.Error("--schema-file still ran the engine")
	}
	if fromEngine != fromSnapshot {
		t.Errorf("snapshot and engine reports differ: %s", stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	eng := &fakeEngine{}
	for _, args := range [][]string{
		{"check", ".", "--policy", "bogus"},
		{"schema", ".", "--init", "--schema-file", "x.json"},
		{"snapshot", "."},
	} {
		if code, _, _ := run(t, eng, args...); code != cmd.ExitError {
			t.Errorf("%v exited %d, want %d", args, code, cmd.ExitError)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}
