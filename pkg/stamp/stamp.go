// Package stamp carries build metadata injected at link time. Every value defaults to "redacted"
// so an unstamped build - `go run`, `go test`, an editor's build - still produces well-formed
// output instead of empty strings.
//
// The values are populated by Bazel through go_library x_defs, which read the keys emitted by
// scripts/bazel-create-status.sh via --workspace_status_command. Adding a stamp means adding the
// variable here, the x_defs entry in BUILD.bazel, and the key in the status script; miss one and
// the value silently stays "redacted" rather than failing the build.
package stamp

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
)

// Redacted is what an unstamped build reports. Exported so a test can assert on it rather than
// repeating the literal, and so a caller can tell "not stamped" from a real value.
const Redacted = "redacted"

var (
	Version   = Redacted
	BuildDate = Redacted
	CommitSHA = Redacted
	Branch    = Redacted
	Platform  = Redacted
	GoVersion = Redacted
)

// Stamps is a snapshot of the link-time values, suitable for marshalling into `<app> version`
// output.
type Stamps struct {
	Application string `json:"application"`
	Version     string `json:"version"`
	BuildDate   string `json:"build_date"`
	CommitSHA   string `json:"commit_sha"`
	Branch      string `json:"branch"`
	Platform    string `json:"platform"`
	GoVersion   string `json:"go_version"`

	// Runtime is the Go runtime actually executing, which is not necessarily the toolchain
	// the binary was compiled with - GoVersion comes from MODULE.bazel and this comes from
	// the running process. They differ for an unstamped build, and that difference is the
	// quickest way to tell one.
	Runtime string `json:"runtime"`

	// OSArch is the platform the binary was built for.
	OSArch string `json:"os_arch"`
}

// New returns a snapshot of the current link-time values for one application.
func New(application string) *Stamps {
	return &Stamps{
		Application: application,
		Version:     Version,
		BuildDate:   BuildDate,
		CommitSHA:   CommitSHA,
		Branch:      Branch,
		Platform:    Platform,
		GoVersion:   GoVersion,
		Runtime:     runtime.Version(),
		OSArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Stamped reports whether this binary carries real build metadata.
//
// Useful in a bug report: an unstamped binary is one somebody built themselves, and the version it
// reports says nothing about which release it corresponds to.
func (s *Stamps) Stamped() bool { return s.Version != Redacted && s.Version != "" }

// WriteText renders the stamps for a person, one field per line.
func (s *Stamps) WriteText(w io.Writer) error {
	fields := [][2]string{
		{"application", s.Application},
		{"version", s.Version},
		{"commit", s.CommitSHA},
		{"branch", s.Branch},
		{"built", s.BuildDate},
		{"built on", s.Platform},
		{"built for", s.OSArch},
		{"go (toolchain)", s.GoVersion},
		{"go (runtime)", s.Runtime},
	}

	for _, f := range fields {
		if _, err := fmt.Fprintf(w, "%-16s %s\n", f[0], f[1]); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
	}

	if !s.Stamped() {
		if _, err := fmt.Fprintf(w,
			"\nThis binary is not stamped, so the version above is a placeholder.\n"+
				"A stamped build comes from `bazel build --stamp` or a release artifact.\n"); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
	}

	return nil
}

// WriteJSON renders the stamps for a machine.
func (s *Stamps) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(s); err != nil {
		return fmt.Errorf("encode version: %w", err)
	}

	return nil
}
