//go:build e2e

package linkwarden

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	bookscape "github.com/adnoctem/pertry/internal/bookscape/cmd"
	"github.com/adnoctem/pertry/internal/bookscape/netscape"
)

// Truncation limits Linkwarden applies on import, read from its importFromHTMLFile.ts. Values are
// trimmed before being cut. Recorded in docs/ARCHITECTURE.md#export-and-linkwarden.
const (
	maxLinkName    = 254
	maxDescription = 254
	maxTagName     = 49
)

// expected is what one link should look like after the round trip: the values bookscape emitted,
// with Linkwarden's own truncation applied.
//
// Expectations are derived from the emitted document rather than hand-written, so the property
// under test is "Linkwarden faithfully reproduces what bookscape emitted" and adding a link to the
// fixture needs no change here. The emitted document is itself checked against the fixture input,
// so a loss inside bookscape cannot hide in the derivation.
type expected struct {
	URL  string
	Name string

	// Description is what bookscape emitted. Linkwarden v2.16.3 discards it - see the <DD>
	// assertion in import_test.go - so this is carried only to make that failure message
	// useful if the behaviour ever changes.
	Description string

	Collection string

	// AddDate is unix seconds, or nil when the entry carried no date. A nil value must come
	// back as a null importDate rather than an epoch date.
	AddDate *int64

	Tags []string
}

// buildExport drives the real CLI over the committed fixture and returns the emitted document
// together with the expectations for it.
//
// The document posted to Linkwarden is produced by `bookscape export`, never by a committed golden
// file. The emitter is the thing under test; a stored copy of its output would drift silently,
// which is the exact failure mode this test exists to catch.
func buildExport(ctx context.Context, t *testing.T) ([]byte, []expected) {
	t.Helper()

	return buildExportAs(ctx, t, "html", "export.html")
}

// buildExportJSON is the same pipeline emitting Linkwarden's own backup format, which is
// `export`'s default and the only one of the two that carries the generated description.
func buildExportJSON(ctx context.Context, t *testing.T) ([]byte, []expected) {
	t.Helper()

	return buildExportAs(ctx, t, "json", "export.json")
}

func buildExportAs(ctx context.Context, t *testing.T, format, name string) ([]byte, []expected) {
	t.Helper()

	dir := t.TempDir()

	fixture := readFixture(t, "synthetic_export.html")
	input := filepath.Join(dir, "synthetic_export.html")
	writeFile(t, input, fixture)

	config := filepath.Join(dir, "bookscape.yaml")
	writeFile(t, config, readFixture(t, "bookscape.yaml"))

	database := filepath.Join(dir, "e2e.db")
	output := filepath.Join(dir, name)

	// Everything lives in the temp directory, so no database or export file is ever left in
	// the working tree. `check` and the model pass are deliberately not run: the first would
	// make fifty outbound requests, the second would cost money, and neither affects what is
	// exported here - a link with no liveness row ships, and the rules pass resolves all fifty.
	for _, args := range [][]string{
		{"import", input},
		{"dedupe"},
		{"classify", "--pass", "rules"},
		{"export", "--type", format, "-o", output},
	} {
		// slices.Concat, not append: appending to the loop's literal would be writing
		// through a slice this function does not own.
		full := slices.Concat(args, []string{"--config", config, "--database", database})
		if out, err := runBookscape(ctx, t, full...); err != nil {
			t.Fatalf("bookscape %v failed: %v\n%s", args, err, out)
		}
	}

	emitted, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read the emitted export: %v", err)
	}

	// The expectations are derived from the emitted *Netscape* document, which is the one
	// whose text can be cross-checked against the fixture input tag by tag. For the JSON run
	// the same pipeline over the same fixture is emitted twice - once as HTML purely to
	// derive the expectations, once as JSON to post - so both tests assert against the same
	// numbers without the JSON path having to re-implement the cross-check.
	if format == "html" {
		return emitted, expectations(t, fixture, emitted)
	}

	reference := filepath.Join(dir, "reference.html")

	full := slices.Concat([]string{"export", "--type", "html", "-o", reference},
		[]string{"--config", config, "--database", database})
	if out, err := runBookscape(ctx, t, full...); err != nil {
		t.Fatalf("bookscape export --type html (for the expectations) failed: %v\n%s", err, out)
	}

	referenceDoc, err := os.ReadFile(reference)
	if err != nil {
		t.Fatalf("read the reference export: %v", err)
	}

	return emitted, expectations(t, fixture, referenceDoc)
}

// expectations cross-checks the emitted document against the fixture input and derives the
// post-import expectation for every link.
func expectations(t *testing.T, fixture, emitted []byte) []expected {
	t.Helper()

	in, _, err := netscape.Parse(fixture)
	if err != nil {
		t.Fatalf("parse the fixture input: %v", err)
	}

	out, report, err := netscape.Parse(emitted)
	if err != nil {
		t.Fatalf("parse our own emitted document: %v", err)
	}

	// The emitter has to round-trip through the same parser the importer side uses, with
	// nothing dropped - otherwise a loss inside bookscape would be invisible here.
	if dropped := report.Dropped(len(out)); dropped != 0 {
		t.Errorf("our own emitted document drops %d entries when re-parsed", dropped)
	}

	if len(out) != len(in) {
		t.Errorf("emitted %d links from a %d-link fixture; the pipeline lost some before Linkwarden "+
			"was involved", len(out), len(in))
	}

	want := make([]expected, 0, len(out))

	for _, e := range out {
		want = append(want, expected{
			URL: e.URL,
			// Linkwarden trims, then truncates.
			Name:        truncate(e.Title, maxLinkName),
			Description: truncate(e.Description, maxDescription),
			Collection:  e.Path,
			AddDate:     e.AddDate,
			Tags:        truncateEach(e.Tags, maxTagName),
		})
	}

	return want
}

// runBookscape invokes the CLI in-process, the way internal/bookscape/cmd's own tests do, so the
// test exercises the real command wiring rather than a reimplementation of it.
func runBookscape(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	root := bookscape.NewRootCommand(&bookscape.App{Out: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)

	return out.String(), err
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}

	return body
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// truncate cuts s to at most limit runes, mirroring Linkwarden's own trim-then-slice.
//
// Runes, not bytes: Linkwarden slices JavaScript strings, so the limit counts UTF-16 code units
// rather than bytes. For the fixture's content the two agree, and the unicode link is well short
// of the limit either way - but counting bytes here would make the expectation wrong for anyone
// who later adds a long non-ASCII title.
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}

	return string(r[:limit])
}

func truncateEach(values []string, limit int) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, truncate(v, limit))
	}

	return out
}
