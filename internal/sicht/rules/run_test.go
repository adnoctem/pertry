package rules_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/rules"
	"github.com/adnoctem/pertry/internal/sicht/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sicht.db"))
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	return db
}

// seed writes one scanned file with the given extension and metadata.
func seed(t *testing.T, db *store.DB, hash, path, ext, exifJSON string) {
	t.Helper()

	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	err := db.SaveScanned(context.Background(), []store.Scanned{{
		File:     store.File{Hash: hash, Size: 1024, Ext: ext, Width: 1242, Height: 2688, FirstSeen: now},
		Location: store.Location{Path: path, Hash: hash, MTime: now, SeenAt: now},
		Metadata: store.Metadata{Hash: hash, ExifJSON: exifJSON},
	}})
	if err != nil {
		t.Fatalf("SaveScanned(%s): %v", hash, err)
	}
}

func defaultConfig() *config.Config {
	cfg := config.Default()

	return &cfg
}

func TestRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.png", "png", `{"ExifIFD:UserComment":"Screenshot"}`)
	seed(t, db, "bbb", "b.jpg", "jpg", `{"IFD0:Make":"Apple","ExifIFD:FNumber":1.8}`)
	seed(t, db, "ccc", "c.jpg", "jpg", `{}`)

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: defaultConfig()})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if result.Files != 3 {
		t.Errorf("Files = %d, want 3", result.Files)
	}

	if result.Decided != 2 {
		t.Errorf("Decided = %d, want 2 - the third file has no evidence either way", result.Decided)
	}

	if result.PerValue["kind"]["screenshot"] != 1 || result.PerValue["kind"]["photo"] != 1 {
		t.Errorf("PerValue = %v, want one of each", result.PerValue)
	}

	// The undecided count is what sizes a model pass, so it is reported rather than implied.
	if result.Undecided["kind"] != 1 {
		t.Errorf("Undecided[kind] = %d, want 1", result.Undecided["kind"])
	}

	// Results are stored with the rule's index as the source, so a decision traces back to
	// the line that produced it.
	decisions, err := db.DecisionsForHash(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionsForHash(): %v", err)
	}

	if len(decisions) != 1 || decisions[0].Source != "rule:0" {
		t.Errorf("decision = %+v, want source rule:0", decisions)
	}
}

// TestRunRetractsStaleRuleResults is the reason retraction exists. An edited rule set changes what
// each index means, so a result from a rule that no longer matches - or no longer exists - must
// not survive.
func TestRunRetractsStaleRuleResults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.png", "png", `{"ExifIFD:UserComment":"Screenshot"}`)

	// A first pass with a rule that matches everything.
	wide := &config.Config{Rules: []config.Rule{
		{Facet: "kind", Value: "everything", When: `true`, Confidence: 0.5},
	}}

	if _, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: wide, Retract: true}); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	if got, _ := db.DecisionMap(ctx, "aaa"); got["kind"] != "everything" {
		t.Fatalf("first pass decided %v, want everything", got)
	}

	// Now a rule set that decides nothing at all.
	narrow := &config.Config{Rules: []config.Rule{
		{Facet: "kind", Value: "never", When: `false`, Confidence: 0.5},
	}}

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: narrow, Retract: true})
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	if result.Retracted != 1 {
		t.Errorf("Retracted = %d, want 1", result.Retracted)
	}

	got, err := db.DecisionMap(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if len(got) != 0 {
		t.Errorf("decisions = %v, want none: the rule that decided it no longer matches", got)
	}
}

// TestRunNeverTouchesAHumanDecision is docs/ARCHITECTURE.md#review's guarantee. Retracting rule output must not discard
// work from another source, and precedence is resolved when decisions are read, not by deleting.
func TestRunNeverTouchesAHumanDecision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.png", "png", `{"ExifIFD:UserComment":"Screenshot"}`)

	if err := db.SaveResults(ctx, []store.Result{{
		Hash: "aaa", Facet: "kind", Value: "scan_document",
		Source: store.SourceHuman, CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("SaveResults(): %v", err)
	}

	if _, err := rules.Run(ctx, rules.RunOptions{
		DB: db, Config: defaultConfig(), Retract: true,
	}); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	got, err := db.DecisionMap(ctx, "aaa")
	if err != nil {
		t.Fatalf("DecisionMap(): %v", err)
	}

	if got["kind"] != "scan_document" {
		t.Errorf("decision = %q, want the human's to still win", got["kind"])
	}

	// And the rule's opinion is recorded alongside it, because the review UI shows it.
	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Results != 2 {
		t.Errorf("Count().Results = %d, want both the human and the rule result", counts.Results)
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.png", "png", `{"ExifIFD:UserComment":"Screenshot"}`)

	result, err := rules.Run(ctx, rules.RunOptions{
		DB: db, Config: defaultConfig(), Retract: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Decisions != 1 {
		t.Errorf("Decisions = %d, want 1 reported", result.Decisions)
	}

	counts, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count(): %v", err)
	}

	if counts.Results != 0 {
		t.Errorf("Count().Results = %d, want 0 in a dry run", counts.Results)
	}

	// A dry run must not retract either, or it would be destructive while claiming not to be.
	if result.Retracted != 0 {
		t.Errorf("Retracted = %d, want 0 in a dry run", result.Retracted)
	}
}

func TestRunLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	for _, h := range []string{"aaa", "bbb", "ccc", "ddd"} {
		seed(t, db, h, h+".png", "png", `{"ExifIFD:UserComment":"Screenshot"}`)
	}

	result, err := rules.Run(ctx, rules.RunOptions{
		DB: db, Config: defaultConfig(), Limit: 2,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil - a limit is not a failure", err)
	}

	if result.Files != 2 {
		t.Errorf("Files = %d, want 2", result.Files)
	}
}

// TestRunUsesTheRealFormatNotTheExtension ties docs/ARCHITECTURE.md#scan to the stored data: the extension in the
// database says heic, the metadata says jpg, and a format rule must see jpg.
func TestRunUsesTheRealFormatNotTheExtension(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "IMG_2060.HEIC", "heic",
		`{"File:FileTypeExtension":"jpg","File:MIMEType":"image/jpeg"}`)

	cfg := &config.Config{Rules: []config.Rule{
		{Facet: "integrity", Value: "transcoded", When: `format == "jpg" && ext == "heic"`, Confidence: 1},
	}}

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: cfg})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Decisions != 1 {
		t.Errorf("Decisions = %d, want the format rule to fire on a mislabelled file", result.Decisions)
	}
}

// TestEnvForUsesTheSidecarsCaptureTime matters because the sidecar's photoTakenTime is the best
// claim about when a photo was taken, and the chronology is what the project exists to preserve.
func TestEnvForUsesTheSidecarsCaptureTime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	err := db.SaveScanned(ctx, []store.Scanned{{
		File:     store.File{Hash: "aaa", Size: 1, Ext: "jpg", FirstSeen: now},
		Location: store.Location{Path: "a.jpg", Hash: "aaa", MTime: now, SeenAt: now},
		Metadata: store.Metadata{
			Hash: "aaa",
			// photoTakenTime is 2018; creationTime is the backup date and must lose.
			SidecarJSON: `{"photoTakenTime":{"timestamp":"1527381327"},` +
				`"creationTime":{"timestamp":"1774000000"}}`,
		},
	}})
	if err != nil {
		t.Fatalf("SaveScanned(): %v", err)
	}

	cfg := &config.Config{Rules: []config.Rule{
		{Facet: "era", Value: "2018", When: `has_sidecar && year == 2018`, Confidence: 1},
	}}

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: cfg})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Decisions != 1 {
		t.Errorf("Decisions = %d, want the rule to see 2018 from photoTakenTime", result.Decisions)
	}
}

// TestEnvForFallsBackToExifTime covers a file with no sidecar.
func TestEnvForFallsBackToExifTime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.jpg", "jpg", `{"ExifIFD:DateTimeOriginal":"2019:08:25 02:45:28"}`)

	cfg := &config.Config{Rules: []config.Rule{
		{Facet: "era", Value: "2019", When: `year == 2019`, Confidence: 1},
	}}

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: cfg})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Decisions != 1 {
		t.Errorf("Decisions = %d, want the EXIF date used when there is no sidecar", result.Decisions)
	}
}

// TestEnvForIgnoresAnUnsetCameraClock: a camera that never had its clock set writes all zeroes,
// which must not be reported as a capture date in year 0.
func TestEnvForIgnoresAnUnsetCameraClock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDB(t)

	seed(t, db, "aaa", "a.jpg", "jpg", `{"ExifIFD:DateTimeOriginal":"0000:00:00 00:00:00"}`)

	cfg := &config.Config{Rules: []config.Rule{
		{Facet: "dated", Value: "yes", When: `has_taken`, Confidence: 1},
	}}

	result, err := rules.Run(ctx, rules.RunOptions{DB: db, Config: cfg})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if result.Decisions != 0 {
		t.Error("an all-zero EXIF date was treated as a capture time")
	}
}

func TestRunRequiresItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := rules.Run(context.Background(), rules.RunOptions{}); err == nil {
		t.Error("Run() error = nil with no database or config, want an error")
	}
}

func TestRunWithNoRules(t *testing.T) {
	t.Parallel()

	db := openDB(t)

	result, err := rules.Run(context.Background(), rules.RunOptions{
		DB: db, Config: &config.Config{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if result.Decisions != 0 {
		t.Errorf("Decisions = %d, want 0", result.Decisions)
	}
}

// TestYieldsIncludesRulesThatNeverFire is what makes a dead rule visible. In the configuration
// file a rule that matches nothing looks exactly like a correct one.
func TestYieldsIncludesRulesThatNeverFire(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()

	set, err := rules.Compile(cfg)
	if err != nil {
		t.Fatalf("Compile(): %v", err)
	}

	yields := set.Yields(map[int]int{0: 111, 2: 300})

	if len(yields) != len(cfg.Rules) {
		t.Fatalf("Yields() returned %d, want one per rule", len(yields))
	}

	if yields[1].Count != 0 {
		t.Errorf("Yields()[1].Count = %d, want 0", yields[1].Count)
	}

	// The resolution rule ships unable to fire, because screen_resolutions is empty.
	if yields[1].Facet != "kind" {
		t.Errorf("Yields()[1] = %+v, want the resolution rule", yields[1])
	}
}

func TestDescribeTruncates(t *testing.T) {
	t.Parallel()

	y := rules.RuleYield{
		Facet: "kind", Value: "screenshot",
		Source: `!has_tag(exif, "Make") && "Screenshot" in tags(exif, "UserComment")`,
	}

	got := y.Describe(30)
	if len([]rune(got)) > 60 {
		t.Errorf("Describe(30) = %q, want it shortened", got)
	}

	if !containsAll(got, "kind", "screenshot") {
		t.Errorf("Describe() = %q, want it to name the facet and value", got)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		found := false

		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				found = true

				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

func TestCompileFilter(t *testing.T) {
	t.Parallel()

	// An empty filter matches everything, so a caller need not special-case its absence.
	empty, err := rules.CompileFilter("")
	if err != nil {
		t.Fatalf("CompileFilter(\"\") error = %v", err)
	}

	ok, err := empty.Match(rules.NewEnv(rules.File{Ext: "png"}))
	if err != nil || !ok {
		t.Errorf("a nil filter matched = (%v, %v), want (true, nil)", ok, err)
	}

	filter, err := rules.CompileFilter(`!has_tag(exif, "Make")`)
	if err != nil {
		t.Fatalf("CompileFilter() error = %v", err)
	}

	for name, tt := range map[string]struct {
		exif map[string]any
		want bool
	}{
		"no camera": {nil, true},
		"a camera":  {map[string]any{"IFD0:Make": "Apple"}, false},
	} {
		got, err := filter.Match(rules.NewEnv(rules.File{Exif: tt.exif}))
		if err != nil {
			t.Fatalf("%s: Match() error = %v", name, err)
		}

		if got != tt.want {
			t.Errorf("%s: Match() = %v, want %v", name, got, tt.want)
		}
	}
}

func TestCompileFilterRejectsNonsense(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{`nonexistent_variable`, `ext`, `has_tag(`} {
		if _, err := rules.CompileFilter(bad); err == nil {
			t.Errorf("CompileFilter(%q) error = nil, want an error", bad)
		}
	}
}
