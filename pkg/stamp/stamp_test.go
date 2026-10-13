package stamp_test

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/pkg/stamp"
)

// TestUnstampedBuildIsWellFormed is why every variable defaults to "redacted" rather than "": an
// unstamped build is the normal case under `go test` and an editor, and empty strings would make
// the output look broken rather than unstamped.
func TestUnstampedBuildIsWellFormed(t *testing.T) {
	t.Parallel()

	s := stamp.New("sicht")

	if s.Application != "sicht" {
		t.Errorf("Application = %q, want sicht", s.Application)
	}

	for name, value := range map[string]string{
		"Version": s.Version, "BuildDate": s.BuildDate, "CommitSHA": s.CommitSHA,
		"Branch": s.Branch, "Platform": s.Platform, "GoVersion": s.GoVersion,
	} {
		if value == "" {
			t.Errorf("%s is empty; an unstamped build should report %q", name, stamp.Redacted)
		}
	}

	if s.Stamped() {
		t.Error("Stamped() = true under `go test`, which never stamps")
	}
}

// TestRuntimeIsTheRunningGo distinguishes the two Go versions, which is the quickest way to spot
// an unstamped binary: GoVersion comes from MODULE.bazel at link time and this from the process.
func TestRuntimeIsTheRunningGo(t *testing.T) {
	t.Parallel()

	s := stamp.New("bookscape")

	if s.Runtime != runtime.Version() {
		t.Errorf("Runtime = %q, want %q", s.Runtime, runtime.Version())
	}

	if want := runtime.GOOS + "/" + runtime.GOARCH; s.OSArch != want {
		t.Errorf("OSArch = %q, want %q", s.OSArch, want)
	}
}

func TestWriteText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	if err := stamp.New("sicht").WriteText(&buf); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	out := buf.String()

	for _, want := range []string{"application", "version", "commit", "built for", "go (runtime)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// An unstamped build says so, rather than reporting a placeholder as though it were a
	// release version.
	if !strings.Contains(out, "not stamped") {
		t.Errorf("an unstamped build does not say so:\n%s", out)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	if err := stamp.New("bookscape").WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	var decoded map[string]any

	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	for _, key := range []string{
		"application", "version", "build_date", "commit_sha", "branch",
		"platform", "go_version", "runtime", "os_arch",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON is missing %q: %v", key, decoded)
		}
	}
}

// TestStamped covers the distinction a bug report depends on.
func TestStamped(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		version string
		want    bool
	}{
		"a real version": {"0.3.1", true},
		"a dev describe": {"0.2.0-4-gabc1234-dirty", true},
		"unstamped":      {stamp.Redacted, false},
		"empty":          {"", false},
	}

	for name, tt := range tests {
		s := &stamp.Stamps{Version: tt.version}

		if got := s.Stamped(); got != tt.want {
			t.Errorf("%s: Stamped() = %v, want %v", name, got, tt.want)
		}
	}
}
