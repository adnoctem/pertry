package fsops_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/fsops"
)

func write(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

func TestMove(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	dst := filepath.Join(dir, "sorted", "photo", "a.jpg")

	write(t, src, "contents")

	out, err := fsops.Relocate(src, dst, fsops.ModeMove, fsops.ConflictFail)
	if err != nil {
		t.Fatalf("Relocate() error = %v, want nil", err)
	}

	if out.Skipped {
		t.Error("Skipped = true, want false")
	}

	if read(t, dst) != "contents" {
		t.Error("the target does not hold the source's contents")
	}

	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("the source still exists after a move")
	}
}

func TestCopyLeavesTheSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	dst := filepath.Join(dir, "copy", "a.jpg")

	write(t, src, "contents")

	out, err := fsops.Relocate(src, dst, fsops.ModeCopy, fsops.ConflictFail)
	if err != nil {
		t.Fatalf("Relocate() error = %v", err)
	}

	if !out.Verified {
		t.Error("Verified = false after a copy, want true: a copy moves bytes and must be checked")
	}

	if out.Bytes != int64(len("contents")) {
		t.Errorf("Bytes = %d, want %d", out.Bytes, len("contents"))
	}

	if read(t, src) != "contents" || read(t, dst) != "contents" {
		t.Error("a copy did not leave both files intact")
	}
}

// TestCopyPreservesModificationTime keeps an incremental scan incremental: a moved file whose
// mtime changed looks modified, and the next scan would re-read the whole archive.
func TestCopyPreservesModificationTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	dst := filepath.Join(dir, "b.jpg")

	write(t, src, "contents")

	when := time.Date(2019, 8, 25, 2, 45, 28, 0, time.UTC)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := fsops.Relocate(src, dst, fsops.ModeCopy, fsops.ConflictFail); err != nil {
		t.Fatalf("Relocate(): %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if !info.ModTime().Equal(when) {
		t.Errorf("ModTime = %v, want the source's %v", info.ModTime(), when)
	}
}

func TestConflictPolicies(t *testing.T) {
	t.Parallel()

	t.Run("suffix", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src", "a.jpg")
		dst := filepath.Join(dir, "dst", "a.jpg")

		write(t, src, "new")
		write(t, dst, "existing")

		out, err := fsops.Relocate(src, dst, fsops.ModeMove, fsops.ConflictSuffix)
		if err != nil {
			t.Fatalf("Relocate() error = %v", err)
		}

		want := filepath.Join(dir, "dst", "a (2).jpg")
		if out.To != want {
			t.Errorf("To = %q, want %q", out.To, want)
		}

		// The existing file is untouched, which is the whole point of the policy.
		if read(t, dst) != "existing" {
			t.Error("the existing file was overwritten")
		}

		if read(t, want) != "new" {
			t.Error("the moved file is not at the suffixed path")
		}
	})

	t.Run("suffix counts up", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		write(t, filepath.Join(dir, "dst", "a.jpg"), "one")
		write(t, filepath.Join(dir, "dst", "a (2).jpg"), "two")

		src := filepath.Join(dir, "src", "a.jpg")
		write(t, src, "three")

		out, err := fsops.Relocate(src, filepath.Join(dir, "dst", "a.jpg"),
			fsops.ModeMove, fsops.ConflictSuffix)
		if err != nil {
			t.Fatalf("Relocate() error = %v", err)
		}

		if filepath.Base(out.To) != "a (3).jpg" {
			t.Errorf("To = %q, want a (3).jpg", out.To)
		}
	})

	t.Run("skip", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src", "a.jpg")
		dst := filepath.Join(dir, "dst", "a.jpg")

		write(t, src, "new")
		write(t, dst, "existing")

		out, err := fsops.Relocate(src, dst, fsops.ModeMove, fsops.ConflictSkip)
		if err != nil {
			t.Fatalf("Relocate() error = %v", err)
		}

		if !out.Skipped {
			t.Error("Skipped = false, want true")
		}

		if read(t, src) != "new" {
			t.Error("the source was moved despite the skip policy")
		}
	})

	t.Run("fail", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src", "a.jpg")
		dst := filepath.Join(dir, "dst", "a.jpg")

		write(t, src, "new")
		write(t, dst, "existing")

		_, err := fsops.Relocate(src, dst, fsops.ModeMove, fsops.ConflictFail)
		if !errors.Is(err, fsops.ErrConflict) {
			t.Errorf("error = %v, want ErrConflict", err)
		}

		if read(t, src) != "new" {
			t.Error("the source was moved despite the failure")
		}
	})
}

// TestMovingOntoItselfIsANoOp matters because re-running an already-applied plan would otherwise
// fail on every file.
func TestMovingOntoItselfIsANoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")

	write(t, src, "contents")

	out, err := fsops.Relocate(src, src, fsops.ModeMove, fsops.ConflictFail)
	if err != nil {
		t.Fatalf("Relocate() error = %v, want nil", err)
	}

	if !out.Skipped {
		t.Error("Skipped = false, want true")
	}

	if read(t, src) != "contents" {
		t.Error("the file was damaged by being moved onto itself")
	}
}

func TestRelocateRefusesASymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real.jpg")
	link := filepath.Join(dir, "link.jpg")

	write(t, target, "contents")

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := fsops.Relocate(link, filepath.Join(dir, "moved.jpg"),
		fsops.ModeMove, fsops.ConflictFail)
	if err == nil {
		t.Error("Relocate() error = nil for a symlink, want a refusal rather than a guess")
	}
}

func TestRelocateRefusesADirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")

	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := fsops.Relocate(sub, filepath.Join(dir, "moved"),
		fsops.ModeMove, fsops.ConflictFail); err == nil {
		t.Error("Relocate() error = nil for a directory, want an error")
	}
}

func TestRelocateMissingSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := fsops.Relocate(filepath.Join(dir, "absent.jpg"), filepath.Join(dir, "b.jpg"),
		fsops.ModeMove, fsops.ConflictFail); err == nil {
		t.Error("Relocate() error = nil for a missing source, want an error")
	}
}

// TestCopyLeavesNoTemporaryFileBehind: an interrupted copy must not leave a half-written file at
// the destination, and a failed one must not leave litter either.
func TestCopyLeavesNoTemporaryFileBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	dst := filepath.Join(dir, "out", "a.jpg")

	write(t, src, "contents")

	if _, err := fsops.Relocate(src, dst, fsops.ModeCopy, fsops.ConflictFail); err != nil {
		t.Fatalf("Relocate(): %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sicht-copy-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}

	if len(entries) != 1 {
		t.Errorf("target directory holds %d entries, want 1", len(entries))
	}
}

func TestHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")

	write(t, path, "contents")

	got, err := fsops.Hash(path)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	// SHA-256 of "contents".
	const want = "d1b2a59fbea7e20077af9f91b27e95e865061b270be03ff539ab3b73587882e8"
	if got != want {
		t.Errorf("Hash() = %q, want %q", got, want)
	}
}

func TestSanitiseSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  string
		error bool
		why   string
	}{
		{name: "ordinary", in: "screenshot", want: "screenshot"},
		{name: "spaces are fine", in: "scan document", want: "scan document"},
		{
			name: "windows-invalid characters", in: `a<b>c:d"e|f?g*h`, want: "a_b_c_d_e_f_g_h",
			why: "these are legal on Linux and rejected by Windows, so a path built on one would not open on the other",
		},
		{
			name: "a separator cannot sneak in", in: "ios/safari", want: "ios_safari",
			why: "a facet value must never introduce a directory level of its own",
		},
		{
			name: "backslash too", in: `ios\safari`, want: "ios_safari",
		},
		{
			name: "trailing dot", in: "ios.", want: "ios",
			why: "Windows strips these silently, so the directory would have two names and undo would miss",
		},
		{name: "trailing space", in: "ios ", want: "ios"},
		{name: "leading space", in: " ios", want: "ios"},
		{name: "control characters", in: "ios\x01\x02", want: "ios__"},
		{
			name: "a reserved device name", in: "nul", want: "nul_",
			why: "Windows refuses CON, NUL and friends as filenames, with or without an extension",
		},
		{name: "reserved with an extension", in: "CON.txt", want: "CON.txt_"},
		{name: "reserved as a substring is fine", in: "console", want: "console"},
		{name: "unicode survives", in: "Fotos von 2019", want: "Fotos von 2019"},
		{name: "emoji survives", in: "🧪 tests", want: "🧪 tests"},
		{name: "empty", in: "", error: true},
		{name: "only dots", in: "...", error: true},
		{name: "only invalid", in: "", error: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := fsops.SanitiseSegment(tt.in)

			if tt.error {
				if err == nil {
					t.Errorf("SanitiseSegment(%q) = %q, want an error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("SanitiseSegment(%q) error = %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("SanitiseSegment(%q) = %q, want %q\n(%s)", tt.in, got, tt.want, tt.why)
			}
		})
	}
}

func TestSanitiseSegmentTruncates(t *testing.T) {
	t.Parallel()

	got, err := fsops.SanitiseSegment(strings.Repeat("a", 400))
	if err != nil {
		t.Fatalf("SanitiseSegment() error = %v", err)
	}

	if len(got) > 255 {
		t.Errorf("len = %d, want at most 255 bytes", len(got))
	}
}

// TestSanitiseSegmentTruncatesWithoutSplittingARune guards against producing a name that is not
// valid UTF-8, which some filesystems accept and some tools then cannot read.
func TestSanitiseSegmentTruncatesWithoutSplittingARune(t *testing.T) {
	t.Parallel()

	// Two-byte runes, so a byte-wise cut at 255 would land mid-character.
	got, err := fsops.SanitiseSegment(strings.Repeat("ü", 200))
	if err != nil {
		t.Fatalf("SanitiseSegment() error = %v", err)
	}

	if len(got) > 255 {
		t.Errorf("len = %d bytes, want at most 255", len(got))
	}

	for i, r := range got {
		if r == '\uFFFD' {
			t.Errorf("truncation split a rune at byte %d: %q", i, got)

			break
		}
	}
}

func TestSanitiseRelative(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  string
		error bool
	}{
		{name: "ordinary", in: "sorted/screenshot/reddit", want: "sorted/screenshot/reddit"},
		{name: "one level", in: "sorted", want: "sorted"},
		{name: "doubled slash", in: "sorted//screenshot", want: "sorted/screenshot"},
		{name: "dot segment", in: "sorted/./screenshot", want: "sorted/screenshot"},
		{name: "backslashes normalised", in: `sorted\screenshot`, want: "sorted/screenshot"},
		{name: "per-segment sanitisation", in: "sorted/ios:safari", want: "sorted/ios_safari"},
		{name: "parent escape", in: "sorted/../../etc", error: true},
		{name: "bare parent", in: "..", error: true},
		{name: "absolute", in: "/etc/passwd", error: true},
		{name: "windows absolute", in: `\windows\system32`, error: true},
		{name: "drive letter", in: `C:/windows`, error: true},
		{name: "empty", in: "", error: true},
		{name: "only separators", in: "///", error: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := fsops.SanitiseRelative(tt.in)

			if tt.error {
				if err == nil {
					t.Errorf("SanitiseRelative(%q) = %q, want an error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("SanitiseRelative(%q) error = %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("SanitiseRelative(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitiseRelativeRefusesEscape is the security-relevant case: a target template is user- and
// model-supplied, so this is the boundary that keeps a facet value from writing outside the
// archive.
func TestSanitiseRelativeRefusesEscape(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"../outside",
		"sorted/../../outside",
		"sorted/subdir/../../../outside",
		"/absolute",
		`C:\windows`,
	} {
		if got, err := fsops.SanitiseRelative(bad); err == nil {
			t.Errorf("SanitiseRelative(%q) = %q, want a refusal", bad, got)
		}
	}
}
