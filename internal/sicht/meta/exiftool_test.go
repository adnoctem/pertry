package meta_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/meta"
)

func TestTagsString(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{
		"ExifIFD:UserComment": "Screenshot",
		"IFD0:Orientation":    float64(6),
		"ExifIFD:FNumber":     1.8,
		"XMP:Favorited":       true,
		"IFD0:Empty":          nil,
	}

	tests := map[string]struct {
		key  string
		want string
		ok   bool
	}{
		"string":        {"ExifIFD:UserComment", "Screenshot", true},
		"whole number":  {"IFD0:Orientation", "6", true},
		"fractional":    {"ExifIFD:FNumber", "1.8", true},
		"bool":          {"XMP:Favorited", "true", true},
		"explicit null": {"IFD0:Empty", "", false},
		"absent":        {"IFD0:Nope", "", false},
	}

	for name, tt := range tests {
		got, ok := tags.String(tt.key)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: String(%q) = (%q, %v), want (%q, %v)", name, tt.key, got, ok, tt.want, tt.ok)
		}
	}
}

// TestTagsStringRendersWholeNumbersWithoutADecimal matters because a rule comparing
// Orientation == "6" would never match "6.0", and JSON has no integers.
func TestTagsStringRendersWholeNumbersWithoutADecimal(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{"n": float64(6)}

	if got, _ := tags.String("n"); got != "6" {
		t.Errorf("String() = %q, want %q", got, "6")
	}
}

func TestTagsFloat(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{
		"num":    1.8,
		"str":    "2.8",
		"padded": "  3.5 ",
		"words":  "f/1.8",
		"nil":    nil,
	}

	for name, want := range map[string]struct {
		val float64
		ok  bool
	}{
		"num":    {1.8, true},
		"str":    {2.8, true},
		"padded": {3.5, true},
		"words":  {0, false},
		"nil":    {0, false},
		"absent": {0, false},
	} {
		got, ok := tags.Float(name)
		if got != want.val || ok != want.ok {
			t.Errorf("Float(%q) = (%v, %v), want (%v, %v)", name, got, ok, want.val, want.ok)
		}
	}
}

// TestDimensionsPrefersTheRealSize is the docs/ARCHITECTURE.md#scan step 4 preference made concrete: ImageWidth is
// what the file is, ExifImageWidth is what the camera wrote down, and on an edited file they
// differ. Filing by the camera's stale record would misclassify a cropped screenshot.
func TestDimensionsPrefersTheRealSize(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{
		"File:ImageWidth":         float64(750),
		"File:ImageHeight":        float64(1150),
		"ExifIFD:ExifImageWidth":  float64(750),
		"ExifIFD:ExifImageHeight": float64(1334),
	}

	w, h, ok := tags.Dimensions()
	if !ok {
		t.Fatal("Dimensions() ok = false, want true")
	}

	if w != 750 || h != 1150 {
		t.Errorf("Dimensions() = %dx%d, want 750x1150 (the file's real size, not the EXIF record)", w, h)
	}
}

func TestDimensionsFallsBackThroughGroups(t *testing.T) {
	t.Parallel()

	tests := map[string]meta.Tags{
		"ungrouped": {"ImageWidth": float64(100), "ImageHeight": float64(200)},
		"exif only": {"ExifImageWidth": float64(100), "ExifImageHeight": float64(200)},
		"png group": {"PNG:ImageWidth": float64(100), "PNG:ImageHeight": float64(200)},
		"quicktime": {"QuickTime:ImageWidth": float64(100), "QuickTime:ImageHeight": float64(200)},
	}

	for name, tags := range tests {
		w, h, ok := tags.Dimensions()
		if !ok || w != 100 || h != 200 {
			t.Errorf("%s: Dimensions() = (%d, %d, %v), want (100, 200, true)", name, w, h, ok)
		}
	}
}

func TestDimensionsRejectsZero(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{"ImageWidth": float64(0), "ImageHeight": float64(0)}

	if _, _, ok := tags.Dimensions(); ok {
		t.Error("Dimensions() ok = true for a zero-sized image, want false")
	}
}

// TestUngroupedIsDeterministic guards against an answer that depends on map iteration order,
// which would make a rule intermittent - far worse than one that is simply wrong.
func TestUngroupedIsDeterministic(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{
		"QuickTime:Make": "QuickTime",
		"IFD0:Make":      "Apple",
		"XMP:Make":       "XMP",
	}

	for range 20 {
		got, ok := tags.Ungrouped("Make")
		if !ok {
			t.Fatal("Ungrouped() ok = false, want true")
		}

		// IFD0 sorts first of the three.
		if got != "Apple" {
			t.Fatalf("Ungrouped(\"Make\") = %v, want the lexicographically first group's value", got)
		}
	}
}

func TestUngroupedPrefersAnExactMatch(t *testing.T) {
	t.Parallel()

	tags := meta.Tags{"Make": "bare", "IFD0:Make": "grouped"}

	if got, _ := tags.Ungrouped("Make"); got != "bare" {
		t.Errorf("Ungrouped() = %v, want the exact key", got)
	}
}

func TestFileName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
		tags meta.Tags
		want string
	}{
		"from System group":  {"/a/b/x.jpg", meta.Tags{"System:FileName": "real.jpg"}, "real.jpg"},
		"ungrouped":          {"/a/b/x.jpg", meta.Tags{"FileName": "real.jpg"}, "real.jpg"},
		"falls back to path": {"/a/b/x.jpg", meta.Tags{}, "x.jpg"},
		"ignores empty":      {"/a/b/x.jpg", meta.Tags{"FileName": ""}, "x.jpg"},
	}

	for name, tt := range tests {
		if got := meta.FileName(tt.path, tt.tags); got != tt.want {
			t.Errorf("%s: FileName() = %q, want %q", name, got, tt.want)
		}
	}
}

func TestExiftoolReadEmpty(t *testing.T) {
	t.Parallel()

	got, err := (&meta.Exiftool{}).Read(context.Background(), nil)
	if err != nil {
		t.Fatalf("Read(nil) error = %v, want nil - it must not run the binary at all", err)
	}

	if len(got) != 0 {
		t.Errorf("Read(nil) = %v, want empty", got)
	}
}

// TestExiftoolBatches drives a fake binary that records how many times it was invoked, so the
// batching is observable without the real tool.
func TestExiftoolBatches(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	counter := filepath.Join(dir, "invocations")

	// Echoes one JSON record per path in the argument file, so the batching is observable.
	// POSIX sh only - the test runs it through /bin/sh, not bash.
	script := `#!/bin/sh
echo x >> ` + counter + `
argfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-@" ]; then argfile="$a"; fi
  prev="$a"
done
printf '['
first=1
while IFS= read -r line; do
  [ -z "$line" ] && continue
  [ $first -eq 0 ] && printf ','
  first=0
  printf '{"SourceFile":"%s","IFD0:Make":"Apple"}' "$line"
done < "$argfile"
printf ']'
`

	binary := filepath.Join(dir, "fake-exiftool")
	if err := writeExecutable(binary, []byte(script)); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	paths := make([]string, 25)
	for i := range paths {
		paths[i] = filepath.Join("/archive", "IMG_"+strings.Repeat("0", 3)+string(rune('a'+i))+".jpg")
	}

	reader := &meta.Exiftool{Binary: binary, BatchSize: 10, TempDir: dir}

	tags, err := reader.Read(context.Background(), paths)
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}

	if len(tags) != len(paths) {
		t.Errorf("Read() returned %d results, want %d", len(tags), len(paths))
	}

	for _, p := range paths {
		if _, ok := tags[p]; !ok {
			t.Errorf("no result for %q", p)
		}
	}

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read invocation count: %v", err)
	}

	// 25 paths at 10 per batch is three invocations.
	if got := strings.Count(string(raw), "x"); got != 3 {
		t.Errorf("binary invoked %d times, want 3 for 25 paths at a batch size of 10", got)
	}
}

// TestExiftoolSurvivesAPartialFailure is the behaviour the exit code alone would get wrong:
// exiftool exits 1 when it cannot read some input but still emits valid JSON for the rest, and
// failing the batch would lose 199 good results to one bad file.
func TestExiftoolSurvivesAPartialFailure(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()

	script := `#!/bin/sh
printf '[{"SourceFile":"/archive/good.jpg","IFD0:Make":"Apple"}]'
echo 'Error: File not found - /archive/bad.jpg' >&2
exit 1
`

	binary := filepath.Join(dir, "fake-exiftool")
	if err := writeExecutable(binary, []byte(script)); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	reader := &meta.Exiftool{Binary: binary, TempDir: dir}

	tags, err := reader.Read(context.Background(), []string{"/archive/good.jpg", "/archive/bad.jpg"})
	if err != nil {
		t.Fatalf("Read() error = %v, want the good result to survive a non-zero exit", err)
	}

	if _, ok := tags["/archive/good.jpg"]; !ok {
		t.Error("the readable file is missing from the result")
	}

	// The unreadable one is absent rather than present-and-empty, so a caller can tell.
	if _, ok := tags["/archive/bad.jpg"]; ok {
		t.Error("the unreadable file is present in the result, want it absent")
	}
}

func TestExiftoolReportsATotalFailure(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()

	binary := filepath.Join(dir, "fake-exiftool")
	if err := writeExecutable(binary, []byte("#!/bin/sh\necho 'boom' >&2\nexit 2\n")); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	_, err := (&meta.Exiftool{Binary: binary, TempDir: dir}).
		Read(context.Background(), []string{"/archive/x.jpg"})
	if err == nil {
		t.Fatal("Read() error = nil, want an error when nothing came back")
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to carry exiftool's own diagnostic", err)
	}
}

func TestExiftoolMissingBinary(t *testing.T) {
	t.Parallel()

	_, err := (&meta.Exiftool{Binary: filepath.Join(t.TempDir(), "absent")}).
		Read(context.Background(), []string{"/archive/x.jpg"})
	if err == nil {
		t.Error("Read() error = nil, want an error for a binary that does not exist")
	}
}

// writeExecutable writes a fake binary for a test to exec.
//
// Not os.WriteFile on its own. These tests run in parallel, and each of them forks exiftool. A
// fork that lands while another test still has its fake open for writing gives the child a copy of
// that descriptor until the child execs - and executing a file somebody holds open for writing
// fails with ETXTBSY, "text file busy" (golang/go#22315). It showed up under --config=race, where
// the timing widens, as an intermittent failure of TestExiftoolReportsATotalFailure.
//
// os/exec takes syscall.ForkLock for every fork, so holding it across open, write and close means
// no fork can happen while the descriptor exists.
func writeExecutable(path string, body []byte) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(path, body, 0o755)
}
