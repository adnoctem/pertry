package doctor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/doctor"
	"github.com/adnoctem/pertry/pkg/infer"
	"github.com/adnoctem/pertry/pkg/infer/ollamabackend"
)

func TestCompareVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		got, want string
		cmp       int
	}{
		{"0.12.11", "0.12.11", 0},
		{"0.12.12", "0.12.11", 1},
		{"0.12.10", "0.12.11", -1},
		{"0.13.0", "0.12.11", 1},
		{"1.0.0", "0.12.11", 1},
		// Numeric, not lexicographic: "0.9.0" sorts after "0.12.0" as text and before it as
		// a version, and getting this wrong would warn on every up-to-date server.
		{"0.9.0", "0.12.0", -1},
		{"0.12.0", "0.9.0", 1},
		// Ollama reports a version with a leading "v" in some builds, and a build suffix in
		// others.
		{"v0.12.11", "0.12.11", 0},
		{"0.12.11-rc1", "0.12.11", 0},
		{"0.12.11 (abcdef)", "0.12.11", 0},
		// Unparsable input compares as equal, deliberately. "I cannot read this version"
		// is not evidence that the server is old, and warning on it would fire on every
		// build with an unusual version string. The Ollama check handles a missing version
		// separately, before it gets here.
		{"unknown", "0.12.11", 0},
		{"", "0.12.11", 0},
	}

	for _, tt := range tests {
		if got := doctor.CompareVersions(tt.got, tt.want); got != tt.cmp {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.got, tt.want, got, tt.cmp)
		}
	}
}

func TestReportFailed(t *testing.T) {
	t.Parallel()

	tests := map[doctor.Status]bool{
		doctor.StatusOK:   false,
		doctor.StatusWarn: false,
		doctor.StatusSkip: false,
		doctor.StatusFail: true,
	}

	for status, want := range tests {
		r := doctor.Report{Results: []doctor.Result{
			{Name: "a", Status: doctor.StatusOK},
			{Name: "b", Status: status},
		}}

		if got := r.Failed(); got != want {
			t.Errorf("Failed() with a %q result = %v, want %v", status, got, want)
		}
	}
}

// TestWarnDoesNotFail is the distinction that decides the exit code: a warning is advice, and
// exiting 3 on one would make `doctor` unusable in a script on a machine that works fine.
func TestWarnDoesNotFail(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{{Name: "a", Status: doctor.StatusWarn}}}

	if r.Failed() {
		t.Error("Failed() = true for a warn-only report, want false")
	}
}

func TestReportCounts(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{
		{Status: doctor.StatusOK},
		{Status: doctor.StatusOK},
		{Status: doctor.StatusWarn},
		{Status: doctor.StatusFail},
		{Status: doctor.StatusSkip},
		{Status: doctor.StatusSkip},
	}}

	counts := r.Counts()

	for status, want := range map[doctor.Status]int{
		doctor.StatusOK: 2, doctor.StatusWarn: 1, doctor.StatusFail: 1, doctor.StatusSkip: 2,
	} {
		if counts[status] != want {
			t.Errorf("Counts()[%q] = %d, want %d", status, counts[status], want)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{
		{Name: "exiftool", Status: doctor.StatusFail, Detail: "not found on PATH", Hint: "install it"},
		{Name: "disk", Status: doctor.StatusOK, Detail: "832 GiB free"},
	}}

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON() error = %v, want nil", err)
	}

	var decoded struct {
		Results []struct {
			Name, Status, Detail, Hint string
		} `json:"results"`
	}

	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("the output is not valid JSON: %v\n%s", err, buf.String())
	}

	if len(decoded.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(decoded.Results))
	}

	if decoded.Results[0].Name != "exiftool" || decoded.Results[0].Status != "fail" {
		t.Errorf("results[0] = %+v, want the exiftool failure", decoded.Results[0])
	}

	// The hint is omitted when empty, so a consumer can distinguish "no advice" from "".
	if decoded.Results[1].Hint != "" {
		t.Errorf("results[1].hint = %q, want it omitted", decoded.Results[1].Hint)
	}
}

func TestWriteText(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{
		{Name: "environment", Status: doctor.StatusOK, Detail: "linux/amd64"},
		{Name: "exiftool", Status: doctor.StatusFail, Detail: "not found on PATH", Hint: "install it"},
		{Name: "model", Status: doctor.StatusSkip, Detail: "Ollama is not reachable"},
	}}

	var buf bytes.Buffer
	if err := r.WriteText(&buf, false); err != nil {
		t.Fatalf("WriteText() error = %v, want nil", err)
	}

	out := buf.String()

	for _, want := range []string{"ok", "FAIL", "skip", "environment", "install it",
		"1 ok, 0 warn, 1 fail, 1 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteText() output is missing %q:\n%s", want, out)
		}
	}

	// With color off there must be no escape sequence, because the output is redirected to a
	// file or a log as often as to a terminal.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("WriteText(color=false) emitted an ANSI escape:\n%q", out)
	}
}

func TestWriteTextOmitsTheSkipCountWhenThereAreNone(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{{Name: "a", Status: doctor.StatusOK}}}

	var buf bytes.Buffer
	if err := r.WriteText(&buf, false); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	if strings.Contains(buf.String(), "skipped") {
		t.Errorf("WriteText() mentions skipped with none present:\n%s", buf.String())
	}
}

func TestWriteTextColor(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{{Name: "a", Status: doctor.StatusFail, Detail: "d"}}}

	var buf bytes.Buffer
	if err := r.WriteText(&buf, true); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	if !strings.Contains(buf.String(), "\x1b[31m") {
		t.Errorf("WriteText(color=true) did not colour a failure red:\n%q", buf.String())
	}
}

// TestWriteTextAlignsHintsUnderTheirCheck is cosmetic but it is what makes a report with several
// hints readable at all.
func TestWriteTextAlignsHintsUnderTheirCheck(t *testing.T) {
	t.Parallel()

	r := doctor.Report{Results: []doctor.Result{
		{Name: "ollama exposure", Status: doctor.StatusOK, Detail: "loopback"},
		{Name: "disk", Status: doctor.StatusWarn, Detail: "1 GiB free", Hint: "needs room"},
	}}

	var buf bytes.Buffer
	if err := r.WriteText(&buf, false); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	lines := strings.Split(buf.String(), "\n")

	var detail, hint string

	for _, l := range lines {
		if strings.Contains(l, "1 GiB free") {
			detail = l
		}

		if strings.Contains(l, "needs room") {
			hint = l
		}
	}

	if detail == "" || hint == "" {
		t.Fatalf("could not find both lines:\n%s", buf.String())
	}

	if strings.Index(detail, "1 GiB") != strings.Index(hint, "needs room") {
		t.Errorf("the hint is not aligned with its detail column:\n%s", buf.String())
	}
}

func TestEnvironment(t *testing.T) {
	t.Parallel()

	got := doctor.Environment(t.TempDir())

	if got.Name != "environment" {
		t.Errorf("Name = %q, want %q", got.Name, "environment")
	}

	if got.Status != doctor.StatusOK {
		t.Errorf("Status = %q, want ok for a workspace on the native filesystem", got.Status)
	}

	if !strings.Contains(got.Detail, runtime.GOOS) {
		t.Errorf("Detail = %q, want it to name the OS", got.Detail)
	}

	if doctor.UnderWSL() && !strings.Contains(got.Detail, "WSL") {
		t.Errorf("Detail = %q, want it to say WSL on a WSL kernel", got.Detail)
	}
}

// TestEnvironmentWarnsOnASlowFilesystem is the performance trap in docs/ARCHITECTURE.md#platform-support: hashing a few thousand
// images across a translated or network filesystem is slow enough to look like a hang.
//
// The filesystem comes from /proc/mounts, so this asserts against whatever the temp directory is
// really on, and states the expectation either way rather than skipping. The path-prefix version
// of this check called a 9p mount at /win/e "ok", which is how the bug was found.
func TestEnvironmentWarnsOnASlowFilesystem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fstype, _, found := doctor.Filesystem(dir)
	if !found {
		t.Skipf("no mount information for %s on this platform", dir)
	}

	got := doctor.Environment(dir)

	switch fstype {
	case "9p", "drvfs", "cifs", "smb3", "nfs", "nfs4", "fuse.sshfs":
		if got.Status != doctor.StatusWarn {
			t.Errorf("Status = %q for a workspace on %s, want warn", got.Status, fstype)
		}

		if !strings.Contains(got.Hint, "round trip") {
			t.Errorf("Hint = %q, want it to explain why the path is slow", got.Hint)
		}
	default:
		if got.Status != doctor.StatusOK {
			t.Errorf("Status = %q for a workspace on %s, want ok", got.Status, fstype)
		}
	}
}

// TestFilesystem checks the two things the mount lookup has to get right.
func TestFilesystem(t *testing.T) {
	t.Parallel()

	// The root filesystem is always a match, so an absent path still resolves rather than
	// reporting nothing - doctor runs before `init` has created anything.
	fstype, mountpoint, found := doctor.Filesystem(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	if !found {
		t.Skip("no mount information on this platform")
	}

	if fstype == "" || mountpoint == "" {
		t.Errorf("Filesystem() = (%q, %q), want both populated", fstype, mountpoint)
	}

	// The longest matching mount point wins, because mounts nest: a path under a mount
	// inside / must not be attributed to /.
	_, rootMount, ok := doctor.Filesystem("/")
	if ok && rootMount != "/" {
		t.Errorf("Filesystem(\"/\") mount = %q, want \"/\"", rootMount)
	}
}

// TestEnvironmentWithNoWorkspace covers `sicht doctor` outside an archive, where there is no path
// to report a filesystem for.
func TestEnvironmentWithNoWorkspace(t *testing.T) {
	t.Parallel()

	got := doctor.Environment("")

	if got.Status != doctor.StatusOK {
		t.Errorf("Status = %q with no workspace, want ok", got.Status)
	}

	if !strings.Contains(got.Detail, runtime.GOOS) {
		t.Errorf("Detail = %q, want it to still name the OS", got.Detail)
	}
}

func TestExiftoolMissing(t *testing.T) {
	// Not parallel: it replaces PATH for the process.
	t.Setenv("PATH", t.TempDir())

	got := doctor.Exiftool(context.Background())

	if got.Status != doctor.StatusFail {
		t.Errorf("Status = %q, want fail when exiftool is absent: docs/CONTRIBUTING.md gives metadata extraction "+
			"no fallback, so `scan` cannot run without it", got.Status)
	}

	// The hint has to carry an install command for each platform sicht targets, because this
	// is the first thing a new user sees.
	for _, want := range []string{"apt", "brew", "winget"} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("Hint = %q, want an install command for %s", got.Hint, want)
		}
	}
}

func TestExiftoolVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    doctor.Status
	}{
		{"current", "13.10", doctor.StatusOK},
		{"exactly the minimum", "12.0", doctor.StatusOK},
		{"too old", "11.99", doctor.StatusWarn},
		// exiftool prints a bare float; anything else is odd but not disqualifying, since
		// the tool is present and the flags sicht uses are ancient.
		{"unparsable", "weird-build", doctor.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeExiftool(t, dir, "echo "+tt.version)
			t.Setenv("PATH", dir)

			got := doctor.Exiftool(context.Background())

			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q (detail %q)", got.Status, tt.want, got.Detail)
			}
		})
	}
}

func TestExiftoolBroken(t *testing.T) {
	dir := t.TempDir()
	fakeExiftool(t, dir, "exit 3")
	t.Setenv("PATH", dir)

	got := doctor.Exiftool(context.Background())

	// Present but non-functional is a warn, not a fail: the diagnosis is different and so is
	// the fix, and conflating them would send someone to re-install a tool that is installed.
	if got.Status != doctor.StatusWarn {
		t.Errorf("Status = %q, want warn for an exiftool that fails to run", got.Status)
	}
}

func TestInference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info infer.ServerInfo
		want doctor.Status
	}{
		{"local", infer.ServerInfo{Name: "ollama", Local: true}, doctor.StatusOK},
		{"remote", infer.ServerInfo{Name: "openai", Local: false}, doctor.StatusWarn},
		{"unavailable", infer.ServerInfo{}, doctor.StatusSkip},
	}

	for _, tt := range tests {
		got := doctor.Inference(tt.info)
		if got.Status != tt.want {
			t.Errorf("Inference(%s) status = %q, want %q", tt.name, got.Status, tt.want)
		}
	}
}

// TestInferenceStatesLocalityOutLoud is the point of the check. docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes promise nothing
// leaves the machine, and a promise nobody can see verified is not worth much.
func TestInferenceStatesLocalityOutLoud(t *testing.T) {
	t.Parallel()

	got := doctor.Inference(infer.ServerInfo{Name: "ollama", Local: true})

	if !strings.Contains(got.Detail, "on this machine") {
		t.Errorf("Detail = %q, want it to state that inference is local", got.Detail)
	}
}

func TestOllamaExposure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want doctor.Status
	}{
		{"http://127.0.0.1:11434", doctor.StatusOK},
		{"http://localhost:11434", doctor.StatusOK},
		{"http://[::1]:11434", doctor.StatusOK},
		// A LAN or public address means Ollama's unauthenticated model-management
		// endpoints are reachable by anyone who can route to it (docs/ARCHITECTURE.md#doctor).
		{"http://192.168.1.50:11434", doctor.StatusWarn},
		{"http://0.0.0.0:11434", doctor.StatusWarn},
	}

	for _, tt := range tests {
		base, err := ollamabackend.ResolveHost(tt.host)
		if err != nil {
			t.Errorf("ResolveHost(%q) error = %v", tt.host, err)

			continue
		}

		got := doctor.OllamaExposure(ollamabackend.NewAt(base))
		if got.Status != tt.want {
			t.Errorf("OllamaExposure(%q) status = %q, want %q (detail %q)",
				tt.host, got.Status, tt.want, got.Detail)
		}
	}
}

func TestOllamaUnreachable(t *testing.T) {
	t.Parallel()

	// A port nothing is listening on. The connection is refused immediately rather than
	// hanging, so this does not need a timeout.
	base, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, info := doctor.Ollama(context.Background(), ollamabackend.NewAt(base))

	if got.Status != doctor.StatusFail {
		t.Errorf("Status = %q, want fail when the server is unreachable", got.Status)
	}

	// The WSL case is the most common cause by a wide margin, so the hint has to name it.
	if !strings.Contains(got.Hint, "mirrored") {
		t.Errorf("Hint = %q, want it to mention WSL mirrored networking", got.Hint)
	}

	// docs/ARCHITECTURE.md#inference-backend-and-model-choice: sicht never starts Ollama, and the hint says so rather than leaving the user to
	// wonder whether it tried.
	if !strings.Contains(got.Hint, "never starts it") {
		t.Errorf("Hint = %q, want it to say sicht does not start Ollama", got.Hint)
	}

	// A zero ServerInfo, so that Inference reports "unavailable" rather than claiming the
	// inference is local on the strength of a health check that never succeeded.
	if info.Name != "" || info.Local {
		t.Errorf("ServerInfo = %+v, want it zero when the server is unreachable", info)
	}

	if got := doctor.Inference(info); got.Status != doctor.StatusSkip {
		t.Errorf("Inference status = %q, want skip when the health check failed", got.Status)
	}
}

// TestOllamaLocalityComesFromTheHost is the privacy-relevant case. OLLAMA_HOST can point at
// another machine, and docs/ARCHITECTURE.md#sicht-scope and docs/ARCHITECTURE.md#privacy-notes make "does anything leave this machine" load-bearing, so the
// locality flag has to be derived rather than assumed from the backend's name.
func TestOllamaLocalityComesFromTheHost(t *testing.T) {
	t.Parallel()

	srv := stubOllama(t, "0.12.11")

	_, info := doctor.Ollama(context.Background(), srv)
	if !info.Local {
		t.Error("ServerInfo.Local = false for a loopback server, want true")
	}

	// The same server addressed by a non-loopback name is not local, whatever the backend is
	// called. Reaching it is not the point: Health reports locality from the configured host
	// before it reports anything about reachability.
	base, err := url.Parse("http://192.168.1.50:11434")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	remote := ollamabackend.NewAt(base)

	local, err := ollamabackend.HostIsLoopback(remote.Host())
	if err != nil {
		t.Fatalf("HostIsLoopback() error = %v", err)
	}

	if local {
		t.Errorf("HostIsLoopback(%q) = true, want false", remote.Host())
	}

	// And a remote backend that does answer must make Inference warn rather than reassure.
	if got := doctor.Inference(infer.ServerInfo{Name: "ollama", Local: false}); got.Status != doctor.StatusWarn {
		t.Errorf("Inference status = %q, want warn for a non-local backend", got.Status)
	}
}

func TestOllamaVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    doctor.Status
	}{
		{"current", "0.12.11", doctor.StatusOK},
		{"newer", "0.13.4", doctor.StatusOK},
		{"too old", "0.11.0", doctor.StatusWarn},
		{"unknown", "", doctor.StatusWarn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := stubOllama(t, tt.version)

			got, info := doctor.Ollama(context.Background(), srv)

			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q (detail %q)", got.Status, tt.want, got.Detail)
			}

			if tt.want == doctor.StatusWarn && tt.version != "" &&
				!strings.Contains(got.Hint, "logprobs") {
				t.Errorf("Hint = %q, want it to explain what an old server costs", got.Hint)
			}

			if tt.version != "" && info.Version != tt.version {
				t.Errorf("ServerInfo.Version = %q, want %q", info.Version, tt.version)
			}

			if tt.version != "" && !info.Local {
				t.Error("ServerInfo.Local = false for a loopback server, want true")
			}
		})
	}
}

func TestWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := doctor.Workspace(context.Background(), w, okOpener)

	if got.Status != doctor.StatusOK {
		t.Errorf("Status = %q, want ok (detail %q, hint %q)", got.Status, got.Detail, got.Hint)
	}

	if !strings.Contains(got.Detail, "schema current") {
		t.Errorf("Detail = %q, want it to report the schema state", got.Detail)
	}
}

// TestWorkspaceWithPendingMigrations is the case that tells someone to re-run init after an
// upgrade, instead of letting a query fail against an old schema later.
func TestWorkspaceWithPendingMigrations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	opener := func(context.Context, string) (doctor.Database, error) {
		return fakeDB{pending: []string{"0002_dhash.sql"}}, nil
	}

	got := doctor.Workspace(context.Background(), w, opener)

	if got.Status != doctor.StatusFail {
		t.Errorf("Status = %q, want fail with a migration outstanding", got.Status)
	}

	if !strings.Contains(got.Detail, "0002_dhash.sql") {
		t.Errorf("Detail = %q, want it to name the pending migration", got.Detail)
	}
}

func TestWorkspaceWithAnUnopenableDatabase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	opener := func(context.Context, string) (doctor.Database, error) {
		return nil, errors.New("disk I/O error")
	}

	got := doctor.Workspace(context.Background(), w, opener)

	if got.Status != doctor.StatusFail {
		t.Errorf("Status = %q, want fail when the database will not open", got.Status)
	}

	if !strings.Contains(got.Detail, "disk I/O error") {
		t.Errorf("Detail = %q, want it to carry the underlying error", got.Detail)
	}

	if got.Hint == "" || strings.Contains(got.Hint, "disk I/O error") {
		t.Errorf("Hint = %q, want advice on what to do rather than a repeat of the error", got.Hint)
	}
}

func TestDisk(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// A floor of one byte is met by any volume that can hold the test at all.
	if got := doctor.Disk(root, 1); got.Status != doctor.StatusOK {
		t.Errorf("Disk(minFree=1) status = %q, want ok (detail %q)", got.Status, got.Detail)
	}

	// An absurd floor must warn rather than fail: free space is advice, not a precondition.
	const petabyte = 1 << 50

	if got := doctor.Disk(root, petabyte); got.Status != doctor.StatusWarn {
		t.Errorf("Disk(minFree=1PiB) status = %q, want warn", got.Status)
	}

	if got := doctor.Disk(root, 0); got.Status != doctor.StatusSkip {
		t.Errorf("Disk(minFree=0) status = %q, want skip", got.Status)
	}
}

func TestDiskOnAMissingPath(t *testing.T) {
	t.Parallel()

	got := doctor.Disk(filepath.Join(t.TempDir(), "absent"), doctor.DefaultMinDiskFree)

	// Unknown is not the same as insufficient, so this skips rather than warning.
	if got.Status != doctor.StatusSkip {
		t.Errorf("Status = %q, want skip when free space cannot be determined", got.Status)
	}
}

// TestRunSkipsWhatItCannotCheck is the property that keeps the report readable: one unreachable
// server must not produce six failures that all mean the same thing.
func TestRunSkipsWhatItCannotCheck(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	base, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := config.Default()

	report := doctor.Run(context.Background(), doctor.Options{
		Workspace:    w,
		Config:       &cfg,
		OpenDatabase: okOpener,
		Backend:      ollamabackend.NewAt(base),
		MinDiskFree:  1,
	})

	byName := make(map[string]doctor.Result, len(report.Results))
	for _, r := range report.Results {
		byName[r.Name] = r
	}

	if got := byName["ollama"].Status; got != doctor.StatusFail {
		t.Errorf("ollama status = %q, want fail", got)
	}

	for _, name := range []string{"model", "vram"} {
		if got := byName[name].Status; got != doctor.StatusSkip {
			t.Errorf("%s status = %q, want skip because its precondition failed, not fail",
				name, got)
		}
	}

	if got := byName["workspace"].Status; got != doctor.StatusOK {
		t.Errorf("workspace status = %q, want ok: it does not depend on Ollama", got)
	}

	if !report.Failed() {
		t.Error("Failed() = false, want true with Ollama unreachable")
	}
}

func TestRunWithNoBackend(t *testing.T) {
	t.Parallel()

	report := doctor.Run(context.Background(), doctor.Options{
		BackendErr: errors.New("OLLAMA_HOST is not a URL"),
	})

	byName := make(map[string]doctor.Result, len(report.Results))
	for _, r := range report.Results {
		byName[r.Name] = r
	}

	if got := byName["ollama"]; got.Status != doctor.StatusFail ||
		!strings.Contains(got.Detail, "not a URL") {
		t.Errorf("ollama result = %+v, want a failure carrying the configuration error", got)
	}

	for _, name := range []string{"ollama exposure", "inference", "model", "vram"} {
		if got := byName[name].Status; got != doctor.StatusSkip {
			t.Errorf("%s status = %q, want skip with no backend", name, got)
		}
	}

	// Every check still appears, so the report's shape does not depend on the environment.
	if got := byName["workspace"].Status; got != doctor.StatusSkip {
		t.Errorf("workspace status = %q, want skip with no workspace", got)
	}
}

// TestRunWithNoWorkspace is what `sicht doctor` does outside an archive, which has to be useful
// rather than an error: the environment checks are exactly what someone wants before `init`.
func TestRunWithNoWorkspace(t *testing.T) {
	t.Parallel()

	report := doctor.Run(context.Background(), doctor.Options{})

	var workspace doctor.Result

	for _, r := range report.Results {
		if r.Name == "workspace" {
			workspace = r
		}
	}

	if workspace.Status != doctor.StatusSkip {
		t.Errorf("workspace status = %q, want skip", workspace.Status)
	}

	if !strings.Contains(workspace.Detail, "sicht init") {
		t.Errorf("workspace detail = %q, want it to point at `sicht init`", workspace.Detail)
	}
}

// TestRunOrdersLocalFactsFirst matters because the first failure a user reads should be the one
// that explains the others.
func TestRunOrdersLocalFactsFirst(t *testing.T) {
	t.Parallel()

	report := doctor.Run(context.Background(), doctor.Options{})

	if len(report.Results) < 2 {
		t.Fatalf("len(results) = %d, want the full set", len(report.Results))
	}

	if report.Results[0].Name != "environment" {
		t.Errorf("results[0] = %q, want the environment first", report.Results[0].Name)
	}

	if report.Results[1].Name != "exiftool" {
		t.Errorf("results[1] = %q, want exiftool second", report.Results[1].Name)
	}
}

// fakeExiftool writes a shell script named exiftool into dir whose body is the given snippet, so
// the version branches can be exercised without the real tool installed.
func fakeExiftool(t *testing.T, dir, body string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake relies on a shell script")
	}

	path := filepath.Join(dir, "exiftool")

	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake exiftool: %v", err)
	}
}

// stubOllama serves the two endpoints the Ollama check uses. It listens on loopback, so the
// locality assertion is meaningful rather than stubbed.
func stubOllama(t *testing.T, version string) *ollamabackend.Backend {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Ollama is running"))
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		if version == "" {
			// A server that answers its heartbeat but not /api/version, which older
			// builds do.
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}

	return ollamabackend.NewAt(base)
}

type fakeDB struct {
	pending []string
}

func (f fakeDB) PendingMigrations(context.Context) ([]string, error) { return f.pending, nil }
func (f fakeDB) Close() error                                        { return nil }

func okOpener(context.Context, string) (doctor.Database, error) { return fakeDB{}, nil }

// TestVRAMDistinguishesNotLoadedFromOnCPU is the bug this guards: a cold model reports its
// on-disk size with a VRAM footprint of zero, which looks exactly like a model running wholly on
// the CPU. Reading the first as the second made this check warn that a healthy RTX 4080 setup was
// running on the CPU, which is the opposite of useful.
func TestVRAMDistinguishesNotLoadedFromOnCPU(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		info infer.ModelInfo
		want doctor.Status
		why  string
	}{
		"not loaded at all": {
			info: infer.ModelInfo{Name: "m", Loaded: false, Size: 6 << 30},
			want: doctor.StatusSkip,
			why:  "a cold model has no footprint to measure, and saying so is not a warning",
		},
		"loaded wholly on the CPU": {
			info: infer.ModelInfo{Name: "m", Loaded: true, Size: 6 << 30, SizeVRAM: 0},
			want: doctor.StatusWarn,
			why:  "this is the real problem: twenty times slower, and silent",
		},
		"partly offloaded": {
			info: infer.ModelInfo{Name: "m", Loaded: true, Size: 6 << 30, SizeVRAM: 3 << 30},
			want: doctor.StatusWarn,
			why:  "part on the CPU is still much slower",
		},
		"fully in VRAM": {
			info: infer.ModelInfo{Name: "m", Loaded: true, Size: 6 << 30, SizeVRAM: 6 << 30},
			want: doctor.StatusOK,
			why:  "the healthy case",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := stubOllamaWithModel(t, tt.info)

			got := doctor.VRAM(context.Background(), srv, "m")
			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q (%s)\ndetail: %s", got.Status, tt.want, tt.why, got.Detail)
			}
		})
	}
}

// stubOllamaWithModel serves /api/show and /api/ps so the VRAM check can be driven without a real
// server. A model reported as not loaded is absent from /api/ps and present in /api/tags, which
// is exactly how Ollama behaves.
func stubOllamaWithModel(t *testing.T, info infer.ModelInfo) *ollamabackend.Backend {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"capabilities":["completion","vision"]}`))
	})

	entry := func() string {
		return fmt.Sprintf(`{"name":%q,"digest":"sha256:abc","size":%d,"size_vram":%d}`,
			info.Name, info.Size, info.SizeVRAM)
	}

	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !info.Loaded {
			_, _ = w.Write([]byte(`{"models":[]}`))

			return
		}

		_, _ = w.Write([]byte(`{"models":[` + entry() + `]}`))
	})

	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[` + entry() + `]}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return ollamabackend.NewAt(base)
}

// TestNoDetailContainsANewline guards the report's layout. Each check gets one detail column and
// one optional hint line beneath it; a newline inside either breaks the alignment of every row
// after it. This has now been got wrong twice, so it is asserted rather than remembered.
func TestNoDetailContainsANewline(t *testing.T) {
	t.Parallel()

	report := doctor.Run(context.Background(), doctor.Options{
		BackendErr:  errors.New("no backend"),
		MinDiskFree: doctor.DefaultMinDiskFree,
	})

	for _, r := range report.Results {
		if strings.Contains(r.Detail, "\n") {
			t.Errorf("%s: detail contains a newline: %q", r.Name, r.Detail)
		}
	}
}

// TestMemoryHeadroom is the check that would have predicted the crash that prompted it: a 5.8 GB
// model loaded on a WSL guest capped at 16 GiB, with other work in flight, exhausted the guest and
// killed the VM. Nothing reported it, because the process that would have reported it died.
func TestMemoryHeadroom(t *testing.T) {
	t.Parallel()

	mem, found := doctor.HostMemory()
	if !found {
		t.Skip("no /proc/meminfo on this platform")
	}

	if mem.Total <= 0 {
		t.Fatalf("Total = %d, want a positive figure", mem.Total)
	}

	if mem.Available <= 0 || mem.Available > mem.Total {
		t.Errorf("Available = %d, want between 0 and Total (%d)", mem.Available, mem.Total)
	}

	// A model that could not possibly fit must warn, whatever this machine has.
	huge := mem.Total * 4

	got := doctor.MemoryHeadroom("enormous:1t", huge)
	if got.Status != doctor.StatusWarn {
		t.Errorf("Status = %q for a model four times the host's memory, want warn", got.Status)
	}

	// The hint has to name the levers, since the user's next question is what to do.
	for _, want := range []string{"num_ctx", "OLLAMA_MAX_LOADED_MODELS", ".wslconfig"} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("Hint = %q, want it to mention %q", got.Hint, want)
		}
	}

	// A trivially small model must not warn.
	if got := doctor.MemoryHeadroom("tiny:1m", 1<<20); got.Status != doctor.StatusOK {
		t.Errorf("Status = %q for a 1 MiB model, want ok (detail %q)", got.Status, got.Detail)
	}

	// With no model size there is nothing to judge, so it reports the memory and no verdict.
	if got := doctor.MemoryHeadroom("", 0); got.Status != doctor.StatusOK {
		t.Errorf("Status = %q with no model size, want ok", got.Status)
	}
}

// TestMemoryHeadroomNamesTheWSLCeiling matters because the number looks surprisingly small on a
// capped guest, and someone reading "16 GiB total" on a 64 GiB machine needs to know why.
func TestMemoryHeadroomNamesTheWSLCeiling(t *testing.T) {
	t.Parallel()

	if !doctor.UnderWSL() {
		t.Skip("only meaningful under WSL")
	}

	got := doctor.MemoryHeadroom("m", 1<<20)

	if !strings.Contains(got.Detail, "WSL ceiling") {
		t.Errorf("Detail = %q, want it to say the total is a configured ceiling", got.Detail)
	}
}

// TestMemoryHeadroomIsInTheReport covers every branch, because a check that is only wired into the
// happy path is absent exactly when the machine is in trouble.
func TestMemoryHeadroomIsInTheReport(t *testing.T) {
	t.Parallel()

	reports := map[string]doctor.Report{
		"no backend": doctor.Run(context.Background(), doctor.Options{
			BackendErr: errors.New("no backend"),
		}),
	}

	base, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	reports["ollama unreachable"] = doctor.Run(context.Background(), doctor.Options{
		Backend: ollamabackend.NewAt(base),
	})

	for name, report := range reports {
		var found bool

		for _, r := range report.Results {
			if r.Name == "memory" {
				found = true
			}
		}

		if !found {
			t.Errorf("%s: the memory check is missing from the report", name)
		}
	}
}

// TestModelWarnsAboutAThinkingModel exists because sicht shipped with one as its default.
// qwen3-vl's plain tags are its thinking variants, and on the 2017 benchmark they returned no
// answer for 22 and 25 of 87 files, having reasoned until the context was full.
func TestModelWarnsAboutAThinkingModel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		capabilities string
		want         doctor.Status
	}{
		"an instruct vision model is fine": {`["completion","vision"]`, doctor.StatusOK},
		"a thinking vision model warns":    {`["completion","vision","thinking"]`, doctor.StatusWarn},
		"a text-only model still fails":    {`["completion","thinking"]`, doctor.StatusFail},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"capabilities":` + tt.capabilities + `}`))
			})

			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			base, _ := url.Parse(srv.URL)

			got := doctor.Model(context.Background(), ollamabackend.NewAt(base), "m")
			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q\ndetail: %s\nhint: %s", got.Status, tt.want, got.Detail, got.Hint)
			}

			if tt.want == doctor.StatusWarn && !strings.Contains(got.Hint, "-instruct") {
				t.Errorf("hint = %q, want it to name the -instruct variant", got.Hint)
			}
		})
	}
}
