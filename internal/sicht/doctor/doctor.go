// Package doctor checks whether the environment sicht needs is actually present.
//
// Every check returns ok, warn or fail with a fix hint, and --json gives machine-readable output
// (docs/ARCHITECTURE.md#doctor). The distinction between warn and fail is load-bearing: fail means
// sicht cannot do its job, warn means it can but something will be slower, less accurate or less
// safe than it should be. Only a fail sets the exit code.
//
// The checks are split into those that need nothing but the local machine and those that need a
// reachable Ollama, so that a developer with no Ollama still gets useful output rather than one
// failure and five skips.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Status is a check's outcome.
type Status string

const (
	// StatusOK means the check passed.
	StatusOK Status = "ok"

	// StatusWarn means sicht will work but something is worse than it should be. Never sets
	// the exit code: a warning that blocks the tool is a failure wearing a disguise.
	StatusWarn Status = "warn"

	// StatusFail means sicht cannot do its job until this is fixed.
	StatusFail Status = "fail"

	// StatusSkip means the check could not run because something it depends on failed. It is
	// not a pass and not a failure, and saying so is more honest than guessing.
	StatusSkip Status = "skip"
)

// Result is one check's outcome.
type Result struct {
	// Name is the check's short name, stable enough to grep for.
	Name string `json:"name"`

	Status Status `json:"status"`

	// Detail is what was found.
	Detail string `json:"detail"`

	// Hint is how to fix it, present on warn and fail.
	Hint string `json:"hint,omitempty"`
}

// Report is every check's result.
type Report struct {
	Results []Result `json:"results"`
}

// Failed reports whether any check failed, which is what sets the exit code.
func (r Report) Failed() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return true
		}
	}

	return false
}

// Counts tallies the report by status.
func (r Report) Counts() map[Status]int {
	out := make(map[Status]int, 4)
	for _, res := range r.Results {
		out[res.Status]++
	}

	return out
}

// WriteJSON renders the report as JSON for `--json`.
func (r Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(r); err != nil {
		return fmt.Errorf("encode doctor report: %w", err)
	}

	return nil
}

// WriteText renders the report for a terminal.
func (r Report) WriteText(w io.Writer, color bool) error {
	width := 0
	for _, res := range r.Results {
		if len(res.Name) > width {
			width = len(res.Name)
		}
	}

	for _, res := range r.Results {
		if _, err := fmt.Fprintf(w, "%s  %-*s  %s\n",
			badge(res.Status, color), width, res.Name, res.Detail); err != nil {
			return fmt.Errorf("write doctor report: %w", err)
		}

		if res.Hint != "" {
			if _, err := fmt.Fprintf(w, "%s  %-*s  %s\n", strings.Repeat(" ", badgeWidth), width, "", res.Hint); err != nil {
				return fmt.Errorf("write doctor report: %w", err)
			}
		}
	}

	counts := r.Counts()

	if _, err := fmt.Fprintf(w, "\n%d ok, %d warn, %d fail",
		counts[StatusOK], counts[StatusWarn], counts[StatusFail]); err != nil {
		return fmt.Errorf("write doctor summary: %w", err)
	}

	if n := counts[StatusSkip]; n > 0 {
		if _, err := fmt.Fprintf(w, ", %d skipped", n); err != nil {
			return fmt.Errorf("write doctor summary: %w", err)
		}
	}

	_, err := fmt.Fprintln(w)

	return err
}

// badgeWidth is the rendered width of a badge, used to indent hints under their check.
const badgeWidth = 6

func badge(s Status, color bool) string {
	label := map[Status]string{
		StatusOK:   "  ok  ",
		StatusWarn: " warn ",
		StatusFail: " FAIL ",
		StatusSkip: " skip ",
	}[s]

	if label == "" {
		label = " ???? "
	}

	if !color {
		return label
	}

	code := map[Status]string{
		StatusOK:   "32",
		StatusWarn: "33",
		StatusFail: "31",
		StatusSkip: "90",
	}[s]

	if code == "" {
		return label
	}

	return "\x1b[" + code + "m" + label + "\x1b[0m"
}

// ok, warn, fail and skip build results.
func ok(name, detail string) Result { return Result{Name: name, Status: StatusOK, Detail: detail} }

func warn(name, detail, hint string) Result {
	return Result{Name: name, Status: StatusWarn, Detail: detail, Hint: hint}
}

func fail(name, detail, hint string) Result {
	return Result{Name: name, Status: StatusFail, Detail: detail, Hint: hint}
}

func skip(name, detail string) Result {
	return Result{Name: name, Status: StatusSkip, Detail: detail}
}

// MinExiftoolVersion is the oldest exiftool whose -json -G1 output sicht relies on. Below it the
// flags exist but the group-prefix behaviour differs enough to matter.
const MinExiftoolVersion = 12.0

// Exiftool checks that exiftool is on PATH and recent enough.
//
// A fail rather than a warn when missing: docs/CONTRIBUTING.md puts metadata extraction behind exiftool with no
// pure-Go fallback in M1, so without it `scan` cannot read the EXIF that every rule in docs/ARCHITECTURE.md#rules
// depends on.
func Exiftool(ctx context.Context) Result {
	const name = "exiftool"

	path, err := exec.LookPath("exiftool")
	if err != nil {
		return fail(name, "not found on PATH",
			"install it: `apt install libimage-exiftool-perl`, `brew install exiftool`, "+
				"or `winget install OliverBetz.ExifTool`. Metadata extraction has no fallback.")
	}

	out, err := exec.CommandContext(ctx, path, "-ver").Output()
	if err != nil {
		return warn(name, fmt.Sprintf("found at %s but `-ver` failed: %v", path, err),
			"check the install is not broken")
	}

	version := strings.TrimSpace(string(out))

	parsed, err := strconv.ParseFloat(version, 64)
	if err != nil {
		// A version string that does not parse is odd but not disqualifying.
		return ok(name, fmt.Sprintf("%s (version %q)", path, version))
	}

	if parsed < MinExiftoolVersion {
		return warn(name, fmt.Sprintf("%s version %s", path, version),
			fmt.Sprintf("version %.1f or newer is expected; older builds differ in how `-G1` "+
				"groups tags", MinExiftoolVersion))
	}

	return ok(name, fmt.Sprintf("%s version %s", path, version))
}

// wslMarker is what identifies a WSL kernel in /proc/version.
const wslMarker = "microsoft"

// slowFilesystems are filesystem types whose latency changes what is practical over an archive of
// several thousand files, mapped to how to say so.
//
// 9p and drvfs are how WSL exposes a Windows drive; the rest are network filesystems that are slow
// for the same reason - a per-file round trip that a local disk does not pay.
var slowFilesystems = map[string]string{
	"9p":         "a Windows drive mounted into WSL",
	"drvfs":      "a Windows drive mounted into WSL",
	"cifs":       "an SMB network share",
	"smb3":       "an SMB network share",
	"nfs":        "an NFS network share",
	"nfs4":       "an NFS network share",
	"fuse.sshfs": "an SSHFS mount",
}

// Environment reports the OS, whether sicht is running under WSL, and whether the workspace is on
// a filesystem slow enough to matter.
//
// The warning earns its place: docs/ARCHITECTURE.md#platform-support's ground rule is to work on the native filesystem of the system
// sicht runs on, and hashing a few thousand images across 9P is slow enough to look like a hang.
// The filesystem is read from /proc/mounts rather than guessed from the path, because WSL's
// automount root is configurable - the archive this was written against is at /win/e, not /mnt/e,
// and a "/mnt/" prefix test saw nothing wrong with it.
func Environment(root string) Result {
	const name = "environment"

	detail := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)

	if UnderWSL() {
		detail += " (WSL)"
	}

	if root == "" {
		return ok(name, detail)
	}

	fstype, mountpoint, found := Filesystem(root)
	if !found {
		return ok(name, detail)
	}

	what, slow := slowFilesystems[fstype]
	if !slow {
		return ok(name, detail)
	}

	return warn(name, fmt.Sprintf("%s, workspace on %s (%s)", detail, mountpoint, fstype),
		fmt.Sprintf("the workspace is on %s. Every file access is a round trip, which over an "+
			"archive of several thousand images is slow enough to look like a hang. Move the "+
			"archive onto a local disk, or run the build native to that filesystem against it "+
			"instead.", what))
}

// UnderWSL reports whether this is a WSL kernel.
func UnderWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	body, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}

	return strings.Contains(strings.ToLower(string(body)), wslMarker)
}

// versionRe extracts a dotted version from a string like "0.12.11" or "v0.40.3".
var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// CompareVersions reports whether got is at least want, both dotted triples. A string it cannot
// parse compares as equal, so an unrecognised version never produces a spurious warning.
func CompareVersions(got, want string) int {
	g, okG := parseVersion(got)
	w, okW := parseVersion(want)

	if !okG || !okW {
		return 0
	}

	for i := range 3 {
		if g[i] != w[i] {
			if g[i] < w[i] {
				return -1
			}

			return 1
		}
	}

	return 0
}

func parseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}

	var out [3]int

	for i := range 3 {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}

		out[i] = n
	}

	return out, true
}

// Memory is the host's memory situation, in bytes.
//
// Declared here rather than beside HostMemory because that function is per-platform and the type
// is not.
type Memory struct {
	// Total is the memory the kernel can use. Under WSL this is the ceiling from .wslconfig
	// and not the physical machine's RAM, which is exactly why it is worth reporting: the two
	// differ, and the lower one is the one that kills a process.
	Total int64

	// Available is what the kernel thinks can be allocated without swapping.
	Available int64

	// SwapTotal is the configured swap, which is what stands between an over-commitment and
	// an out-of-memory kill.
	SwapTotal int64
}
