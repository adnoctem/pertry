package cmd_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/cmd"
	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

// seeded builds a workspace with a database holding the given scanned files and results.
func seeded(t *testing.T, scanned []store.Scanned, results []store.Result) string {
	t.Helper()

	root := t.TempDir()

	ws, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}

	db, err := store.Open(context.Background(), ws.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	if err := db.SaveScanned(context.Background(), scanned); err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	if err := db.SaveResults(context.Background(), results); err != nil {
		t.Fatalf("SaveResults(): %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	// stats reads the config, so write the default one.
	if _, _, err := run(t, "init", root); err != nil {
		t.Fatalf("init: %v", err)
	}

	return root
}

func file(hash, path string, w, h int, exif string) store.Scanned {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	return store.Scanned{
		File: store.File{
			Hash: hash, Size: 1024, Ext: strings.TrimPrefix(filepath.Ext(path), "."),
			Width: w, Height: h, FirstSeen: now,
		},
		Location: store.Location{Path: path, Hash: hash, MTime: now, SeenAt: now},
		Metadata: store.Metadata{Hash: hash, ExifJSON: exif},
	}
}

func decided(hash, facet, value, source string) store.Result {
	return store.Result{
		Hash: hash, Facet: facet, Value: value, Source: source,
		CreatedAt: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
	}
}

func TestStatsOverview(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.jpg", 4032, 3024, `{"IFD0:Make":"Apple"}`),
		},
		[]store.Result{decided("aaa", "kind", "screenshot", "rule:0")},
	)

	out, _, err := run(t, "--workspace", root, "stats")
	if err != nil {
		t.Fatalf("stats error = %v", err)
	}

	for _, want := range []string{"distinct files", "2", "png", "jpg", "kind"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output is missing %q:\n%s", want, out)
		}
	}
}

func TestStatsOnAnEmptyWorkspace(t *testing.T) {
	t.Parallel()

	root := seeded(t, nil, nil)

	out, _, err := run(t, "--workspace", root, "stats")
	if err != nil {
		t.Fatalf("stats error = %v, want nil on an empty workspace", err)
	}

	if !strings.Contains(out, "sicht scan") {
		t.Errorf("stats on an empty workspace does not say what to do:\n%s", out)
	}
}

func TestStatsFacets(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.png", 1242, 2688, `{}`),
			file("ccc", "c.jpg", 4032, 3024, `{}`),
		},
		[]store.Result{
			decided("aaa", "kind", "screenshot", "rule:0"),
			decided("bbb", "kind", "screenshot", "rule:0"),
			decided("ccc", "kind", "photo", "rule:2"),
		},
	)

	out, _, err := run(t, "--workspace", root, "stats", "facets")
	if err != nil {
		t.Fatalf("stats facets error = %v", err)
	}

	if !strings.Contains(out, "screenshot") || !strings.Contains(out, "photo") {
		t.Errorf("output is missing values:\n%s", out)
	}

	// The source class is shown, because which source decided something is as interesting
	// as what was decided.
	if !strings.Contains(out, "rule") {
		t.Errorf("output does not name the source class:\n%s", out)
	}
}

// TestStatsFacetsShowsADisagreement is the raw material for docs/ARCHITECTURE.md#confidence's accuracy numbers, and is only
// visible because the overridden row is kept rather than deleted.
func TestStatsFacetsShowsADisagreement(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{
			decided("aaa", "kind", "photo", store.ModelSource("qwen3-vl:8b")),
			decided("aaa", "kind", "screenshot", store.SourceHuman),
		},
	)

	out, _, err := run(t, "--workspace", root, "stats", "facets")
	if err != nil {
		t.Fatalf("stats facets error = %v", err)
	}

	if !strings.Contains(out, "decided differently") {
		t.Errorf("output does not report the override:\n%s", out)
	}

	// The winning decision is the human's.
	if !strings.Contains(out, "screenshot") {
		t.Errorf("output does not show the human's value:\n%s", out)
	}
}

func TestStatsFacetsAll(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{file("aaa", "a.png", 1242, 2688, `{}`)},
		[]store.Result{
			decided("aaa", "kind", "photo", store.ModelSource("m")),
			decided("aaa", "kind", "screenshot", store.SourceHuman),
		},
	)

	winners, _, err := run(t, "--workspace", root, "stats", "facets")
	if err != nil {
		t.Fatalf("stats facets: %v", err)
	}

	all, _, err := run(t, "--workspace", root, "stats", "facets", "--all")
	if err != nil {
		t.Fatalf("stats facets --all: %v", err)
	}

	// The overridden model value appears only with --all, which is the difference between
	// "what holds" and "what was ever thought".
	if strings.Count(winners, "photo") >= strings.Count(all, "photo") {
		t.Errorf("--all did not reveal more than the default view:\n--- default ---\n%s\n--- all ---\n%s",
			winners, all)
	}
}

// TestStatsDimsEvidenceIsOverAllFiles is the metric bug this was written to prevent. Computed over
// decided files alone, a size with 2 known screenshots and 8 unknowns reports 100%; over all
// files it reports 20%, which is the number that stops a bad rule being written.
func TestStatsDimsEvidenceIsOverAllFiles(t *testing.T) {
	t.Parallel()

	var (
		scanned []store.Scanned
		results []store.Result
	)

	// Ten files at one size; two are known screenshots, eight have no decision.
	for i := range 10 {
		hash := string(rune('a'+i)) + "aa"
		scanned = append(scanned, file(hash, hash+".png", 750, 1334, `{}`))

		if i < 2 {
			results = append(results, decided(hash, "kind", "screenshot", "rule:0"))
		}
	}

	root := seeded(t, scanned, results)

	out, _, err := run(t, "--workspace", root, "stats", "dims")
	if err != nil {
		t.Fatalf("stats dims error = %v", err)
	}

	if !strings.Contains(out, "20.0%") {
		t.Errorf("output does not report 20%% evidence for 2 known of 10 files:\n%s", out)
	}

	if strings.Contains(out, "100.0%") {
		t.Errorf("output reports 100%%, so evidence is being computed over decided files only:\n%s", out)
	}

	if !strings.Contains(out, "do not use") {
		t.Errorf("a 20%%-evidence size is not flagged as unusable:\n%s", out)
	}
}

func TestStatsDimsYAMLRequiresEvidenceAndSupport(t *testing.T) {
	t.Parallel()

	var (
		scanned []store.Scanned
		results []store.Result
	)

	// A well-supported, well-evidenced size: six files, all known screenshots.
	for i := range 6 {
		hash := "good" + string(rune('a'+i))
		scanned = append(scanned, file(hash, hash+".png", 1242, 2688, `{}`))
		results = append(results, decided(hash, "kind", "screenshot", "rule:0"))
	}

	// A single cropped screenshot: 100% evidence on a sample of one.
	scanned = append(scanned, file("crop", "crop.png", 750, 1217, `{}`))
	results = append(results, decided("crop", "kind", "screenshot", "rule:0"))

	// A size with a counterexample: one screenshot and one photo.
	scanned = append(scanned,
		file("mix1", "mix1.png", 800, 600, `{}`),
		file("mix2", "mix2.png", 800, 600, `{}`))
	results = append(results,
		decided("mix1", "kind", "screenshot", "rule:0"),
		decided("mix2", "kind", "photo", "rule:2"))

	root := seeded(t, scanned, results)

	out, _, err := run(t, "--workspace", root, "stats", "dims", "--yaml")
	if err != nil {
		t.Fatalf("stats dims --yaml error = %v", err)
	}

	if !strings.Contains(out, `"1242x2688"`) {
		t.Errorf("the well-supported size is missing:\n%s", out)
	}

	// A crop size occurring once must not be offered as a device resolution: a future photo
	// cropped to that size would be mislabelled silently.
	if strings.Contains(out, `- "750x1217"`) {
		t.Errorf("a single-file crop size was suggested as a device resolution:\n%s", out)
	}

	// One counterexample disqualifies a size outright.
	if strings.Contains(out, `- "800x600"`) {
		t.Errorf("a size with a known counterexample was suggested:\n%s", out)
	}

	// The output is valid YAML in shape: a key and a list of quoted strings.
	if !strings.Contains(out, "screen_resolutions:") {
		t.Errorf("output is not a pasteable block:\n%s", out)
	}
}

func TestStatsDimsWhere(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "a.png", 1242, 2688, `{}`),
			file("bbb", "b.jpg", 4032, 3024, `{"IFD0:Make":"Apple"}`),
		},
		nil,
	)

	out, _, err := run(t, "--workspace", root, "stats", "dims", "--where", `!has_tag(exif, "Make")`)
	if err != nil {
		t.Fatalf("stats dims --where error = %v", err)
	}

	if !strings.Contains(out, "1 of 2 files match") {
		t.Errorf("the filter was not applied:\n%s", out)
	}

	if strings.Contains(out, "4032x3024") {
		t.Errorf("the camera photo survived the filter:\n%s", out)
	}
}

func TestStatsDimsRejectsABadFilter(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.png", 1, 1, `{}`)}, nil)

	if got := exitCode(t, "--workspace", root, "stats", "dims", "--where", "nonsense_variable"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want a usage error for an invalid filter", got)
	}
}

func TestStatsDimsExcludesUnknownDimensions(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.heic", 0, 0, `{}`)}, nil)

	out, _, err := run(t, "--workspace", root, "stats", "dims")
	if err != nil {
		t.Fatalf("stats dims error = %v", err)
	}

	// 0x0 must never appear: it would look like a resolution somebody could match on.
	if strings.Contains(out, "0x0") {
		t.Errorf("a file with unknown dimensions was reported as 0x0:\n%s", out)
	}
}

func TestStatsDupes(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			// The same content in two places.
			file("aaa", "photos/a.jpg", 100, 100, `{}`),
			file("aaa", "backup/a.jpg", 100, 100, `{}`),
			// The same photo in two formats: different hashes, same stem.
			file("bbb", "2018/IMG_0007.heic", 4032, 3024, `{}`),
			file("ccc", "2018/IMG_0007.jpeg", 4032, 3024, `{}`),
		},
		nil,
	)

	out, _, err := run(t, "--workspace", root, "stats", "dupes")
	if err != nil {
		t.Fatalf("stats dupes error = %v", err)
	}

	if !strings.Contains(out, "photos/a.jpg") || !strings.Contains(out, "backup/a.jpg") {
		t.Errorf("byte-identical files are not reported:\n%s", out)
	}

	if !strings.Contains(out, "IMG_0007") {
		t.Errorf("the format twin is not reported:\n%s", out)
	}

	// Both are advisory, and the output has to say so rather than implying action.
	if !strings.Contains(out, "advisory") {
		t.Errorf("the format-twin section is not marked advisory:\n%s", out)
	}
}

func TestStatsDupesWithNone(t *testing.T) {
	t.Parallel()

	root := seeded(t, []store.Scanned{file("aaa", "a.jpg", 100, 100, `{}`)}, nil)

	out, _, err := run(t, "--workspace", root, "stats", "dupes")
	if err != nil {
		t.Fatalf("stats dupes error = %v", err)
	}

	if strings.Count(out, "none") != 2 {
		t.Errorf("both sections should report none:\n%s", out)
	}
}

// TestFormatTwinsIgnoresTheSameContentTwice: two names for identical bytes is an exact duplicate,
// not a format twin, and reporting it in both places would double-count it.
func TestFormatTwinsIgnoresTheSameContentTwice(t *testing.T) {
	t.Parallel()

	root := seeded(t,
		[]store.Scanned{
			file("aaa", "x/IMG_1.jpg", 100, 100, `{}`),
			file("aaa", "x/IMG_1.jpeg", 100, 100, `{}`),
		},
		nil,
	)

	out, _, err := run(t, "--workspace", root, "stats", "dupes")
	if err != nil {
		t.Fatalf("stats dupes error = %v", err)
	}

	parts := strings.SplitN(out, "advisory", 2)
	if len(parts) != 2 {
		t.Fatalf("could not split the report:\n%s", out)
	}

	if !strings.Contains(parts[1], "none") {
		t.Errorf("identical bytes under two names were reported as a format twin:\n%s", parts[1])
	}
}

func TestStatsRequiresAWorkspace(t *testing.T) {
	t.Parallel()

	absent := filepath.Join(t.TempDir(), "nope")

	if got := exitCode(t, "--workspace", absent, "stats"); got != cmd.ExitUsage {
		t.Errorf("exit code = %d, want a usage error", got)
	}
}
