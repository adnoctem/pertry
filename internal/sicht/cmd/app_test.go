package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/cmd"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/doctor"
)

// run executes the root command in-process and returns stdout, stderr and the error.
func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	app := &cmd.App{Out: &out}
	root := cmd.NewRootCommand(app)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())

	return out.String(), errOut.String(), err
}

// exitCode runs the root command the way main does, so the mapping from error type to exit code is
// what is under test rather than the error itself.
func exitCode(t *testing.T, args ...string) int {
	t.Helper()

	var out bytes.Buffer

	app := &cmd.App{Out: &out}
	root := cmd.NewRootCommand(app)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	if err := root.ExecuteContext(context.Background()); err != nil {
		return cmd.ExitCodeFor(err)
	}

	return cmd.ExitOK
}

func TestInitCreatesTheWorkspace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	out, _, err := run(t, "init", dir)
	if err != nil {
		t.Fatalf("init error = %v, want nil", err)
	}

	w, err := core.Open(dir)
	if err != nil {
		t.Fatalf("Open() after init error = %v", err)
	}

	for _, path := range []string{
		w.ConfigPath(), w.DatabasePath(), w.ThumbsPath(), w.ManifestsPath(),
		w.LogsPath(), w.PromptsPath(),
		filepath.Join(w.PromptsPath(), "kind.md"),
		filepath.Join(w.PromptsPath(), "platform.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("init did not create %s: %v", path, err)
		}
	}

	// What init says about its defaults is stated at init time, not buried in a comment in a
	// file nobody opens. screen_resolutions is empty because a plausible-looking value would be
	// worse than an obviously empty one. The model warning names the trap the benchmark found:
	// qwen3-vl's plain tags are thinking variants that can run out of room before answering.
	for _, want := range []string{"screen_resolutions is empty", "qwen3-vl:8b-instruct", "thinking variants"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output is missing %q:\n%s", want, out)
		}
	}

	if !strings.Contains(out, "sicht doctor") {
		t.Errorf("init output does not say what to do next:\n%s", out)
	}
}

// TestInitIsIdempotent is documented behaviour, and the reason it matters is the sentence below:
// by the time anyone re-runs init, sicht.yaml holds a curated taxonomy and hand-tuned rules.
func TestInitIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("first init error = %v", err)
	}

	w, err := core.Open(dir)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	const marker = "# hand-tuned, must survive a second init\n"

	raw, err := os.ReadFile(w.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if err := os.WriteFile(w.ConfigPath(), append(raw, marker...), 0o600); err != nil {
		t.Fatalf("append to config: %v", err)
	}

	out, _, err := run(t, "init", dir)
	if err != nil {
		t.Fatalf("second init error = %v, want nil", err)
	}

	if !strings.Contains(out, "left alone") {
		t.Errorf("second init output does not say the config was kept:\n%s", out)
	}

	after, err := os.ReadFile(w.ConfigPath())
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}

	if !strings.Contains(string(after), marker) {
		t.Error("the second init overwrote a hand-edited sicht.yaml")
	}
}

// TestInitRestoresAMissingPrompt is the other half of idempotency: a deleted prompt file comes
// back, because a stage refers to it by name and would otherwise fail at run time.
func TestInitRestoresAMissingPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("first init error = %v", err)
	}

	w, err := core.Open(dir)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	prompt := filepath.Join(w.PromptsPath(), "kind.md")

	if err := os.Remove(prompt); err != nil {
		t.Fatalf("remove prompt: %v", err)
	}

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("second init error = %v", err)
	}

	if _, err := os.Stat(prompt); err != nil {
		t.Errorf("init did not restore %s: %v", prompt, err)
	}
}

// TestInitDoesNotTouchAnEditedPrompt is why prompts are written only when absent: a tuned prompt
// is as much the user's work as the config.
func TestInitDoesNotTouchAnEditedPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("first init error = %v", err)
	}

	w, err := core.Open(dir)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	prompt := filepath.Join(w.PromptsPath(), "kind.md")

	const edited = "my own prompt\n"

	if err := os.WriteFile(prompt, []byte(edited), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("second init error = %v", err)
	}

	raw, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}

	if string(raw) != edited {
		t.Errorf("init overwrote an edited prompt: %q", raw)
	}
}

func TestInitRejectsTooManyArguments(t *testing.T) {
	t.Parallel()

	if _, _, err := run(t, "init", t.TempDir(), t.TempDir()); err == nil {
		t.Error("init with two directories error = nil, want a usage error")
	}
}

func TestInitLeavesImagesAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	image := filepath.Join(dir, "IMG_0001.jpg")
	if err := os.WriteFile(image, []byte("not really a jpeg"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("init error = %v", err)
	}

	raw, err := os.ReadFile(image)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}

	if string(raw) != "not really a jpeg" {
		t.Error("init modified a file in the archive; it must only write under .sicht/")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	// Exactly the image and .sicht/, nothing else scattered in the archive root.
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Errorf("init left %v in the archive root, want only the image and .sicht", names)
	}
}

// TestDoctorExitsNotReady is the contract a wrapper script depends on: a distinct code for "the
// environment is not ready", so that it can run doctor and act on the answer.
//
// Not parallel, because it pins OLLAMA_HOST at a port nothing listens on. An earlier version
// relied on Ollama simply not running wherever the tests ran, which made the assertion a statement
// about the machine rather than about the exit code - and it duly broke the day Ollama was
// installed. Forcing the failure is the only way this means anything.
func TestDoctorExitsNotReady(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("init error = %v", err)
	}

	if got := exitCode(t, "--workspace", dir, "doctor"); got != cmd.ExitNotReady {
		t.Errorf("doctor exit code = %d, want %d (not ready)", got, cmd.ExitNotReady)
	}
}

// TestDoctorExitsOKWhenNothingFails is the other half of the contract. It constructs the report
// through the doctor package rather than the CLI, because the CLI's result depends on whether
// exiftool and Ollama are installed on the machine running the tests, and a test that changes
// answer with the environment is worse than no test.
func TestDoctorExitsOKWhenNothingFails(t *testing.T) {
	t.Parallel()

	report := doctor.Report{Results: []doctor.Result{
		{Name: "environment", Status: doctor.StatusOK},
		{Name: "exiftool", Status: doctor.StatusWarn},
		{Name: "model", Status: doctor.StatusSkip},
	}}

	if report.Failed() {
		t.Fatal("Failed() = true with no fail results, want false")
	}

	if got := cmd.ExitCodeFor(nil); got != cmd.ExitOK {
		t.Errorf("exit code = %d, want %d", got, cmd.ExitOK)
	}
}

func TestDoctorJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("init error = %v", err)
	}

	out, _, _ := run(t, "--workspace", dir, "doctor", "--json")

	var report struct {
		Results []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"results"`
	}

	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, out)
	}

	if len(report.Results) == 0 {
		t.Fatal("doctor --json returned no results")
	}

	seen := make(map[string]string, len(report.Results))
	for _, r := range report.Results {
		seen[r.Name] = r.Status
	}

	// Every check appears whatever the outcome, so a consumer can rely on the shape.
	for _, name := range []string{
		"environment", "exiftool", "workspace", "disk", "ollama", "ollama exposure",
		"inference", "model", "vram",
	} {
		if _, ok := seen[name]; !ok {
			t.Errorf("doctor --json is missing the %q check; got %v", name, seen)
		}
	}

	if seen["workspace"] != "ok" {
		t.Errorf("workspace status = %q, want ok for a freshly initialised workspace", seen["workspace"])
	}

	// --json emits nothing but JSON, so it can be piped.
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("doctor --json wrote something before the JSON:\n%s", out)
	}
}

// TestDoctorWithoutAWorkspace has to work, because the environment checks are exactly what someone
// wants before they run init.
func TestDoctorWithoutAWorkspace(t *testing.T) {
	t.Parallel()

	out, _, _ := run(t, "--workspace", "", "doctor", "--json")

	if !strings.Contains(out, `"name": "environment"`) && !strings.Contains(out, `"name":"environment"`) {
		t.Errorf("doctor outside a workspace did not report the environment:\n%s", out)
	}
}

// TestWorkspaceFlagMustExist distinguishes a path the user named from one sicht went looking for:
// a missing --workspace is a usage error, while finding no workspace by discovery is not.
func TestWorkspaceFlagMustExist(t *testing.T) {
	t.Parallel()

	absent := filepath.Join(t.TempDir(), "not-a-workspace")

	if got := exitCode(t, "--workspace", absent, "doctor"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage) for a --workspace that does not exist", got, cmd.ExitUsage)
	}
}

func TestLogLevelIsValidated(t *testing.T) {
	t.Parallel()

	if got := exitCode(t, "--log-level", "shouting", "doctor"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage) for an unknown log level", got, cmd.ExitUsage)
	}

	for _, level := range []string{"debug", "info", "warn", "error", "DEBUG", "Info"} {
		dir := t.TempDir()

		if _, _, err := run(t, "--log-level", level, "init", dir); err != nil {
			t.Errorf("--log-level %q error = %v, want it accepted", level, err)
		}
	}
}

// TestLogsGoToStderr keeps subcommand output pipeable: a log line in the middle of `doctor --json`
// would make the JSON unparsable.
func TestLogsGoToStderr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	out, _, err := run(t, "--log-level", "debug", "init", dir)
	if err != nil {
		t.Fatalf("init error = %v", err)
	}

	if strings.Contains(out, "level=") {
		t.Errorf("a log line reached stdout:\n%s", out)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()

	if got := exitCode(t, "nonsense"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage) for an unknown subcommand", got, cmd.ExitUsage)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	t.Parallel()

	if got := exitCode(t, "doctor", "--nonsense"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage) for an unknown flag", got, cmd.ExitUsage)
	}
}

// TestNoColorSuppressesEscapes covers the flag; the NO_COLOR environment variable is covered
// separately because it cannot run in parallel.
func TestNoColorSuppressesEscapes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("init error = %v", err)
	}

	out, _, _ := run(t, "--workspace", dir, "--no-color", "doctor")

	if strings.Contains(out, "\x1b[") {
		t.Errorf("doctor --no-color emitted an ANSI escape:\n%q", out)
	}
}

func TestNoColorEnvironmentVariable(t *testing.T) {
	// Not parallel: it sets an environment variable for the process.
	dir := t.TempDir()

	if _, _, err := run(t, "init", dir); err != nil {
		t.Fatalf("init error = %v", err)
	}

	t.Setenv("NO_COLOR", "1")

	out, _, _ := run(t, "--workspace", dir, "doctor")

	if strings.Contains(out, "\x1b[") {
		t.Errorf("doctor emitted an ANSI escape with NO_COLOR set:\n%q", out)
	}
}

// TestWorkspaceIsDiscoveredFromASubdirectory is what makes the tool usable from wherever someone
// happens to be inside a large archive.
func TestWorkspaceIsDiscoveredFromASubdirectory(t *testing.T) {
	// Not parallel: it changes the working directory.
	root := t.TempDir()

	if _, _, err := run(t, "init", root); err != nil {
		t.Fatalf("init error = %v", err)
	}

	deep := filepath.Join(root, "2023", "holiday")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("chdir back: %v", err)
		}
	})

	if err := os.Chdir(deep); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	out, _, _ := run(t, "doctor", "--json")

	var report struct {
		Results []struct {
			Name, Status string
		} `json:"results"`
	}

	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}

	for _, r := range report.Results {
		if r.Name == "workspace" {
			if r.Status != "ok" {
				t.Errorf("workspace status = %q from a subdirectory, want ok", r.Status)
			}

			return
		}
	}

	t.Error("no workspace check in the report")
}

// TestHelpListsTheImplementedCommands guards against a subcommand being written and never
// registered, which is a mistake with no other symptom.
func TestHelpListsTheImplementedCommands(t *testing.T) {
	t.Parallel()

	out, _, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help error = %v, want nil", err)
	}

	for _, name := range []string{"init", "doctor"} {
		if !strings.Contains(out, name) {
			t.Errorf("--help does not list %q:\n%s", name, out)
		}
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()

	// The numbers themselves are the contract (cmd/sicht/README.md), so they are asserted rather than
	// referred to.
	for name, got := range map[string]int{
		"ok": cmd.ExitOK, "error": cmd.ExitError, "usage": cmd.ExitUsage,
		"not ready": cmd.ExitNotReady, "partial": cmd.ExitPartial,
	} {
		want := map[string]int{"ok": 0, "error": 1, "usage": 2, "not ready": 3, "partial": 4}[name]
		if got != want {
			t.Errorf("Exit%s = %d, want %d", name, got, want)
		}
	}
}
