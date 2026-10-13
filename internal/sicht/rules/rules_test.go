package rules_test

import (
	"strings"
	"testing"
	"time"

	"github.com/adnoctem/pertry/internal/sicht/config"
	"github.com/adnoctem/pertry/internal/sicht/rules"
)

// compile builds a set from a rule list, failing the test if it does not compile.
func compile(t *testing.T, rs ...config.Rule) *rules.Set {
	t.Helper()

	cfg := config.Config{Rules: rs}

	set, err := rules.Compile(&cfg)
	if err != nil {
		t.Fatalf("Compile() error = %v, want nil", err)
	}

	return set
}

func TestCompileTheDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	set, err := rules.Compile(&cfg)
	if err != nil {
		t.Fatalf("the shipped default rules do not compile: %v", err)
	}

	if set.Len() != len(cfg.Rules) {
		t.Errorf("Len() = %d, want %d", set.Len(), len(cfg.Rules))
	}
}

// TestCompileRejectsAnUnknownVariable is why the environment is a typed struct. A rule naming a
// variable that does not exist is a configuration error, and it should be reported once at
// startup rather than silently evaluating as nil on every one of ten thousand files.
func TestCompileRejectsAnUnknownVariable(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Rules: []config.Rule{
		{Facet: "kind", Value: "photo", When: `camra == "Apple"`},
	}}

	_, err := rules.Compile(&cfg)
	if err == nil {
		t.Fatal("Compile() error = nil, want an error for a misspelled variable")
	}

	if !strings.Contains(err.Error(), "rules[0]") {
		t.Errorf("error = %q, want it to name the offending rule", err)
	}

	if !strings.Contains(err.Error(), "docs/ARCHITECTURE.md#rules") {
		t.Errorf("error = %q, want a hint pointing at the documented variables", err)
	}
}

// TestCompileRejectsANonBooleanExpression catches a rule that is not a condition at all, which is
// a plausible mistake: `when: 'ext'` instead of `when: 'ext == "png"'`.
func TestCompileRejectsANonBooleanExpression(t *testing.T) {
	t.Parallel()

	for _, when := range []string{`ext`, `width`, `exif`} {
		cfg := config.Config{Rules: []config.Rule{
			{Facet: "kind", Value: "photo", When: when},
		}}

		if _, err := rules.Compile(&cfg); err == nil {
			t.Errorf("Compile() error = nil for when: %q, want an expression that is not a "+
				"condition to be rejected", when)
		}
	}
}

// TestADynamicallyNonBooleanExpressionErrors covers the half the compiler cannot see. Exif values
// are typed `any`, so `exif["IFD0:Make"]` as a whole condition compiles and can only be caught
// when it runs. What matters is that it is an error rather than a silent false, because a rule
// that never fires and never complains is the worst of the three outcomes.
func TestADynamicallyNonBooleanExpressionErrors(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{Facet: "kind", Value: "photo", When: `exif["IFD0:Make"]`})

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext:  "jpg",
		Exif: map[string]any{"IFD0:Make": "Apple"},
	}))

	if err == nil {
		t.Error("Evaluate() error = nil, want a string-valued condition to be reported")
	}

	if len(got) != 0 {
		t.Errorf("Evaluate() = %v, want no decision from a rule that did not evaluate to a bool", got)
	}
}

// TestAMissingExifKeyIsFalseNotAnError is the other side of that, and is deliberate: it is what
// makes `exif["ExifIFD:UserComment"] == "Screenshot"` safe to write without a has() guard.
func TestAMissingExifKeyIsFalseNotAnError(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "screenshot",
		When: `exif["ExifIFD:UserComment"] == "Screenshot"`,
	})

	got, err := set.Evaluate(rules.NewEnv(rules.File{Ext: "png"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want a missing key to compare as false", err)
	}

	if len(got) != 0 {
		t.Errorf("Evaluate() = %v, want no decision", got)
	}
}

func TestCompileReportsTheRightIndex(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Rules: []config.Rule{
		{Facet: "kind", Value: "photo", When: `ext == "jpg"`},
		{Facet: "kind", Value: "photo", When: `ext == "png"`},
		{Facet: "kind", Value: "photo", When: `nonsense()`},
	}}

	_, err := rules.Compile(&cfg)
	if err == nil {
		t.Fatal("Compile() error = nil, want an error")
	}

	if !strings.Contains(err.Error(), "rules[2]") {
		t.Errorf("error = %q, want it to name rules[2]", err)
	}
}

// TestTheRealScreenshotRule is the rule as docs/ARCHITECTURE.md#rules settled it, against the values measured in the
// owner's export. The guard on extension is gone because one screenshot there is a JPG, and the
// comparison is by equality because UserComment is a junk drawer in that archive.
func TestTheRealScreenshotRule(t *testing.T) {
	t.Parallel()

	const when = `!has(exif, "IFD0:Make") && exif["ExifIFD:UserComment"] == "Screenshot"`

	set := compile(t, config.Rule{Facet: "kind", Value: "screenshot", When: when, Confidence: 0.99})

	tests := []struct {
		name  string
		file  rules.File
		match bool
		why   string
	}{
		{
			name: "a tagged PNG screenshot",
			file: rules.File{
				Ext:  "png",
				Exif: map[string]any{"ExifIFD:UserComment": "Screenshot"},
			},
			match: true,
		},
		{
			name: "a tagged JPG screenshot",
			file: rules.File{
				Ext:  "jpg",
				Exif: map[string]any{"ExifIFD:UserComment": "Screenshot"},
			},
			match: true,
			why:   "one JPG in the export carries the tag, which is why the ext guard was dropped",
		},
		{
			name: "a photo from a camera",
			file: rules.File{
				Ext:  "jpg",
				Exif: map[string]any{"IFD0:Make": "Apple", "ExifIFD:FNumber": 1.8},
			},
			match: false,
		},
		{
			name: "a PicsArt editor blob in UserComment",
			file: rules.File{
				Ext: "jpg",
				Exif: map[string]any{
					"ExifIFD:UserComment": `{"total_effects_actions":0,"width":960,"source":"editor"}`,
				},
			},
			match: false,
			why:   "a substring test would have matched something in this blob eventually",
		},
		{
			name: "a VSCO preset string in UserComment",
			file: rules.File{
				Ext:  "jpg",
				Exif: map[string]any{"ExifIFD:UserComment": "Processed with VSCO with c1 preset"},
			},
			match: false,
		},
		{
			name:  "no metadata at all",
			file:  rules.File{Ext: "png"},
			match: false,
			why:   "a file with no EXIF must evaluate to false, not error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := set.Evaluate(rules.NewEnv(tt.file))
			if err != nil {
				t.Fatalf("Evaluate() error = %v, want nil", err)
			}

			if matched := len(got) > 0; matched != tt.match {
				t.Errorf("matched = %v, want %v\n(%s)", matched, tt.match, tt.why)
			}

			if tt.match && got[0].Source() != "rule:0" {
				t.Errorf("Source() = %q, want %q", got[0].Source(), "rule:0")
			}
		})
	}
}

// TestTheResolutionRule uses the three verified resolutions from docs/ARCHITECTURE.md#rules and the two that were
// deliberately rejected, so that a future edit widening the list has to come through this test.
func TestTheResolutionRule(t *testing.T) {
	t.Parallel()

	verified := []string{"1242x2688", "2688x1242", "750x1334"}

	set := compile(t, config.Rule{
		Facet: "kind", Value: "screenshot", Confidence: 0.9,
		When: `!has(exif, "IFD0:Make") && dims in screen_resolutions`,
	})

	tests := []struct {
		name          string
		width, height int
		match         bool
		why           string
	}{
		{"iPhone XS Max portrait", 1242, 2688, true, "82 files in the export"},
		{"the same landscape", 2688, 1242, true, "10 files, and a separate entry because dims is literal"},
		{"iPhone 6/7/8", 750, 1334, true, "14 files"},
		{
			"a cropped screenshot", 750, 1150, false,
			"crops keep the device width and lose the height, so they fall through to the model",
		},
		{
			"the rejected Galaxy size", 1440, 3040, false,
			"all six such files were re-encoded and carry no UserComment, so there is no evidence",
		},
		{"a square download", 1080, 1080, false, "untagged PNGs in the export are downloads"},
		{"a desktop wallpaper", 1920, 1080, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := set.Evaluate(rules.NewEnv(rules.File{
				Ext: "png", Width: tt.width, Height: tt.height,
				ScreenResolutions: verified,
			}))
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}

			if matched := len(got) > 0; matched != tt.match {
				t.Errorf("%dx%d matched = %v, want %v\n(%s)",
					tt.width, tt.height, matched, tt.match, tt.why)
			}
		})
	}
}

// TestAnEmptyResolutionListMatchesNothing is the shipped state. docs/ARCHITECTURE.md#rules forbids inventing device
// dimensions, so `init` writes an empty list, and this rule must then simply never fire rather
// than erroring on every file.
func TestAnEmptyResolutionListMatchesNothing(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "screenshot",
		When: `dims in screen_resolutions`,
	})

	got, err := set.Evaluate(rules.NewEnv(rules.File{Ext: "png", Width: 1242, Height: 2688}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil with an empty resolution list", err)
	}

	if len(got) != 0 {
		t.Errorf("Evaluate() = %v, want no decision", got)
	}
}

// TestThePhotoRuleDecidesEveryHEIC is what docs/ARCHITECTURE.md#apply-manifests-and-undo rests on. If this stops holding, a HEIC
// becomes undecidable without a decoder the build cannot have.
func TestThePhotoRuleDecidesEveryHEIC(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "photo", Confidence: 0.98,
		When: `has(exif, "IFD0:Make") && has(exif, "ExifIFD:FNumber")`,
	})

	// The tags a real HEIC from the export carries.
	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext: "heic",
		Exif: map[string]any{
			"IFD0:Make": "Apple", "IFD0:Model": "iPhone 7",
			"ExifIFD:FNumber": 1.8, "ExifIFD:ISO": float64(100),
		},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 1 || got[0].Value != "photo" {
		t.Fatalf("Evaluate() = %v, want one decision of \"photo\"", got)
	}

	if got[0].Confidence != 0.98 {
		t.Errorf("Confidence = %v, want 0.98", got[0].Confidence)
	}
}

// TestFirstMatchPerFacetWins is the documented ordering guarantee, and the reason a cheap
// high-precision rule belongs first.
func TestFirstMatchPerFacetWins(t *testing.T) {
	t.Parallel()

	set := compile(t,
		config.Rule{Facet: "kind", Value: "first", When: `ext == "png"`, Confidence: 0.9},
		config.Rule{Facet: "kind", Value: "second", When: `ext == "png"`, Confidence: 0.5},
	)

	got, err := set.Evaluate(rules.NewEnv(rules.File{Ext: "png"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("Evaluate() returned %d decisions, want 1 per facet", len(got))
	}

	if got[0].Value != "first" || got[0].RuleIndex != 0 {
		t.Errorf("Evaluate() = %+v, want the first rule to win", got[0])
	}
}

func TestSeveralFacetsAreDecidedIndependently(t *testing.T) {
	t.Parallel()

	set := compile(t,
		config.Rule{Facet: "kind", Value: "photo", When: `ext == "jpg"`},
		config.Rule{Facet: "source", Value: "camera", When: `has(exif, "IFD0:Make")`},
	)

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext:  "jpg",
		Exif: map[string]any{"IFD0:Make": "Apple"},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("Evaluate() returned %d decisions, want one per facet", len(got))
	}
}

func TestEnvDerivedFields(t *testing.T) {
	t.Parallel()

	taken := time.Date(2019, 8, 25, 2, 45, 28, 0, time.UTC)

	env := rules.NewEnv(rules.File{
		Path: "Fotos von 2019/IMG_1234.JPG", Dir: "Fotos von 2019", Name: "IMG_1234.JPG",
		Ext: ".JPG", Size: 4096, Width: 4032, Height: 3024,
		Exif: map[string]any{
			"IFD0:Make": "Apple", "IFD0:Model": "iPhone 7", "IFD0:Software": "13.3",
		},
		HasSidecar: true, Taken: taken,
	})

	tests := map[string]struct{ got, want any }{
		"ext lowercased and undotted": {env.Ext, "png"},
		"dims":                        {env.Dims, "4032x3024"},
		"landscape":                   {env.Landscape, true},
		"portrait":                    {env.Portrait, false},
		"camera":                      {env.Camera, "Apple iPhone 7"},
		"software":                    {env.Software, "13.3"},
		"has_sidecar":                 {env.HasSidecar, true},
		"has_taken":                   {env.HasTaken, true},
		"year":                        {env.Year, 2019},
	}

	// ext is ".JPG" in, so the expected value above is wrong on purpose if this is ever
	// edited carelessly; assert the real one here.
	tests["ext lowercased and undotted"] = struct{ got, want any }{env.Ext, "jpg"}

	for name, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", name, tt.got, tt.want)
		}
	}

	if env.Megapixels < 12.1 || env.Megapixels > 12.2 {
		t.Errorf("megapixels = %v, want about 12.19", env.Megapixels)
	}
}

func TestEnvHandlesASquareImage(t *testing.T) {
	t.Parallel()

	env := rules.NewEnv(rules.File{Width: 1080, Height: 1080})

	if env.Portrait || env.Landscape {
		t.Errorf("portrait = %v, landscape = %v, want both false for a square image",
			env.Portrait, env.Landscape)
	}
}

// TestEnvWithNoDimensions covers a file exiftool could not measure: dims must be empty rather
// than "0x0", which would otherwise be a resolution a user could accidentally match on.
func TestEnvWithNoDimensions(t *testing.T) {
	t.Parallel()

	env := rules.NewEnv(rules.File{Ext: "jpg"})

	if env.Dims != "" {
		t.Errorf("dims = %q, want empty when the dimensions are unknown", env.Dims)
	}

	if env.Year != 0 || env.HasTaken {
		t.Errorf("year = %d, has_taken = %v, want 0 and false", env.Year, env.HasTaken)
	}
}

// TestEnvNeverLeavesANilMap is what keeps a file with no metadata from erroring instead of
// evaluating to false.
func TestEnvNeverLeavesANilMap(t *testing.T) {
	t.Parallel()

	env := rules.NewEnv(rules.File{})

	if env.Exif == nil {
		t.Error("Exif is nil; has(exif, …) would fail at run time rather than return false")
	}

	if env.ScreenResolutions == nil {
		t.Error("ScreenResolutions is nil; `dims in screen_resolutions` would fail")
	}
}

func TestCamera(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		exif map[string]any
		want string
	}{
		{"make and model", map[string]any{"IFD0:Make": "Apple", "IFD0:Model": "iPhone 7"}, "Apple iPhone 7"},
		{"model repeats the make", map[string]any{
			"IFD0:Make": "NIKON CORPORATION", "IFD0:Model": "NIKON CORPORATION D7000",
		}, "NIKON CORPORATION D7000"},
		{"make only", map[string]any{"IFD0:Make": "Apple"}, "Apple"},
		{"model only", map[string]any{"IFD0:Model": "iPhone 7"}, "iPhone 7"},
		{"neither", map[string]any{}, ""},
		{"empty strings", map[string]any{"IFD0:Make": "", "IFD0:Model": ""}, ""},
		{"whitespace trimmed", map[string]any{"IFD0:Make": "  Apple  "}, "Apple"},
		{"ungrouped fallback", map[string]any{"Make": "Apple", "Model": "iPhone 7"}, "Apple iPhone 7"},
	}

	for _, tt := range tests {
		if got := rules.NewEnv(rules.File{Exif: tt.exif}).Camera; got != tt.want {
			t.Errorf("%s: camera = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFacets(t *testing.T) {
	t.Parallel()

	set := compile(t,
		config.Rule{Facet: "kind", Value: "a", When: `true`},
		config.Rule{Facet: "kind", Value: "b", When: `true`},
		config.Rule{Facet: "platform", Value: "c", When: `true`},
	)

	got := set.Facets()

	if len(got) != 2 {
		t.Fatalf("Facets() = %v, want 2 unique facets", got)
	}
}

// TestEvaluateCollectsErrorsWithoutAbandoningOtherFacets: one rule tripping over an unexpected
// tag type must not cost the file its other decisions.
func TestEvaluateCollectsErrors(t *testing.T) {
	t.Parallel()

	set := compile(t,
		// has() rejects a non-map first argument at run time.
		config.Rule{Facet: "broken", Value: "x", When: `has(exif["IFD0:Make"], "k")`},
		config.Rule{Facet: "kind", Value: "photo", When: `ext == "jpg"`},
	)

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext:  "jpg",
		Exif: map[string]any{"IFD0:Make": "Apple"},
	}))

	if err == nil {
		t.Error("Evaluate() error = nil, want the broken rule to be reported")
	}

	if len(got) != 1 || got[0].Facet != "kind" {
		t.Errorf("Evaluate() = %v, want the healthy facet still decided", got)
	}
}

// TestFormatComesFromTheBytesNotTheName is the lesson of the first real scan: in the reference
// export 16 of 25 files named ".HEIC" are plain JPEGs that Google transcoded without renaming, and
// 3 of 23 named ".gif" are WebP. A rule keyed on `ext` silently does not fire for those.
func TestFormatComesFromTheBytesNotTheName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ext          string
		exif         map[string]any
		wantFormat   string
		wantMIME     string
		wantMismatch bool
	}{
		{
			name: "a JPEG wearing a .HEIC extension",
			ext:  "heic",
			exif: map[string]any{
				"File:FileType": "JPEG", "File:FileTypeExtension": "jpg",
				"File:MIMEType": "image/jpeg",
			},
			wantFormat: "jpg", wantMIME: "image/jpeg", wantMismatch: true,
		},
		{
			name: "a genuine HEIC",
			ext:  "heic",
			exif: map[string]any{
				"File:FileType": "HEIC", "File:FileTypeExtension": "heic",
				"File:MIMEType": "image/heic",
			},
			wantFormat: "heic", wantMIME: "image/heic", wantMismatch: false,
		},
		{
			name: "a WebP wearing a .gif extension",
			ext:  "gif",
			exif: map[string]any{
				"File:FileType": "Extended WEBP", "File:FileTypeExtension": "webp",
				"File:MIMEType": "image/webp",
			},
			wantFormat: "webp", wantMIME: "image/webp", wantMismatch: true,
		},
		{
			// jpg and jpeg name one format, so this is not a mismatch worth reporting -
			// burying the real ones in noise would defeat the point.
			name:       "jpeg against jpg",
			ext:        "jpeg",
			exif:       map[string]any{"File:FileTypeExtension": "jpg"},
			wantFormat: "jpg", wantMismatch: false,
		},
		{
			// With no metadata the extension is the only thing there is to go on, which is
			// a better answer than an empty string every comparison would fail against.
			name:       "no metadata at all",
			ext:        "png",
			exif:       nil,
			wantFormat: "png", wantMismatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := rules.NewEnv(rules.File{Ext: tt.ext, Exif: tt.exif})

			if env.Format != tt.wantFormat {
				t.Errorf("format = %q, want %q", env.Format, tt.wantFormat)
			}

			if tt.wantMIME != "" && env.MIME != tt.wantMIME {
				t.Errorf("mime = %q, want %q", env.MIME, tt.wantMIME)
			}

			if env.FormatMismatch != tt.wantMismatch {
				t.Errorf("format_mismatch = %v, want %v", env.FormatMismatch, tt.wantMismatch)
			}
		})
	}
}

// TestARuleCanUseFormat proves the variable is reachable from an expression, which is the whole
// point of adding it.
func TestARuleCanUseFormat(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "photo", Confidence: 0.9,
		When: `format == "jpg" && has(exif, "IFD0:Make")`,
	})

	// The file is called .HEIC but is really a JPEG, which is the case this exists for.
	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext: "heic",
		Exif: map[string]any{
			"File:FileTypeExtension": "jpg",
			"IFD0:Make":              "Apple",
		},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 1 {
		t.Errorf("Evaluate() = %v, want the rule to fire on the real format", got)
	}
}

// TestARuleCanFlagAMislabelledFile is the other use: surfacing the mismatch rather than being
// caught out by it.
func TestARuleCanFlagAMislabelledFile(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "integrity", Value: "extension_lies", Confidence: 1,
		When: `format_mismatch`,
	})

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext:  "gif",
		Exif: map[string]any{"File:FileTypeExtension": "webp"},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 1 || got[0].Value != "extension_lies" {
		t.Errorf("Evaluate() = %v, want the mismatch flagged", got)
	}
}

// TestGroupAgnosticTagLookup is the finding that changed the shipped rules. In the reference
// archive the screenshot marker lives in two EXIF groups with different coverage - 100 files under
// ExifIFD:UserComment, 105 under XMP-exif:UserComment, 94 in both, union 111 - so a rule naming
// one group saw 99 of 111.
func TestGroupAgnosticTagLookup(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "screenshot", Confidence: 0.99,
		When: `!has_tag(exif, "Make") && "Screenshot" in tags(exif, "UserComment")`,
	})

	tests := []struct {
		name  string
		exif  map[string]any
		match bool
	}{
		{
			name:  "marker in ExifIFD only",
			exif:  map[string]any{"ExifIFD:UserComment": "Screenshot"},
			match: true,
		},
		{
			name:  "marker in XMP only",
			exif:  map[string]any{"XMP-exif:UserComment": "Screenshot"},
			match: true,
		},
		{
			name: "marker in both",
			exif: map[string]any{
				"ExifIFD:UserComment": "Screenshot", "XMP-exif:UserComment": "Screenshot",
			},
			match: true,
		},
		{
			// The groups disagree. `in tags(...)` is exact where `tag(...) == x` would
			// depend on which group sorts first, so this still matches.
			name: "groups disagree, one says Screenshot",
			exif: map[string]any{
				"ExifIFD:UserComment":  "Processed with VSCO with c1 preset",
				"XMP-exif:UserComment": "Screenshot",
			},
			match: true,
		},
		{
			name:  "no marker anywhere",
			exif:  map[string]any{"ExifIFD:UserComment": "something else"},
			match: false,
		},
		{
			// A camera make in any group disqualifies it, including a group the draft
			// rule's `IFD0:Make` key would not have seen.
			name: "a camera make in a different group",
			exif: map[string]any{
				"QuickTime:Make": "Apple", "ExifIFD:UserComment": "Screenshot",
			},
			match: false,
		},
		{
			name:  "no metadata at all",
			exif:  nil,
			match: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := set.Evaluate(rules.NewEnv(rules.File{Ext: "png", Exif: tt.exif}))
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}

			if matched := len(got) > 0; matched != tt.match {
				t.Errorf("matched = %v, want %v", matched, tt.match)
			}
		})
	}
}

// TestTagIsDeterministic: a rule must not change answer between runs because of map ordering.
func TestTagIsDeterministic(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{
		Facet: "kind", Value: "x", When: `tag(exif, "Make") == "Apple"`,
	})

	exif := map[string]any{
		"QuickTime:Make": "QuickTime", "IFD0:Make": "Apple", "XMP:Make": "XMP",
	}

	for range 20 {
		got, err := set.Evaluate(rules.NewEnv(rules.File{Exif: exif}))
		if err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}

		// IFD0 sorts first of the three, so the answer is Apple every time.
		if len(got) != 1 {
			t.Fatalf("Evaluate() = %v, want a stable match on the first group in sorted order", got)
		}
	}
}

func TestHasTag(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{Facet: "k", Value: "v", When: `has_tag(exif, "FNumber")`})

	for name, tt := range map[string]struct {
		exif  map[string]any
		match bool
	}{
		"grouped":     {map[string]any{"ExifIFD:FNumber": 1.8}, true},
		"ungrouped":   {map[string]any{"FNumber": 1.8}, true},
		"other group": {map[string]any{"Composite:FNumber": 1.8}, true},
		"absent":      {map[string]any{"ExifIFD:ISO": 100}, false},
		"empty":       {nil, false},
	} {
		got, err := set.Evaluate(rules.NewEnv(rules.File{Exif: tt.exif}))
		if err != nil {
			t.Fatalf("%s: Evaluate() error = %v", name, err)
		}

		if matched := len(got) > 0; matched != tt.match {
			t.Errorf("%s: matched = %v, want %v", name, matched, tt.match)
		}
	}
}

// TestTagDoesNotMatchASuffixOfAnotherTag guards the obvious bug in suffix matching: "Make" must
// not be found inside "LensMake".
func TestTagDoesNotMatchASuffixOfAnotherTag(t *testing.T) {
	t.Parallel()

	set := compile(t, config.Rule{Facet: "k", Value: "v", When: `has_tag(exif, "Make")`})

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Exif: map[string]any{"ExifIFD:LensMake": "Apple"},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 0 {
		t.Error(`has_tag(exif, "Make") matched ExifIFD:LensMake, want it to match whole tag names only`)
	}
}

// TestTheShippedRulesMatchTheMeasurements runs the real default rules against the tag sets
// measured in the reference archive, so that editing the defaults has to come through a test that
// states what each clause is for.
func TestTheShippedRulesMatchTheMeasurements(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	set, err := rules.Compile(&cfg)
	if err != nil {
		t.Fatalf("the shipped rules do not compile: %v", err)
	}

	tests := []struct {
		name  string
		file  rules.File
		facet string
		value string
	}{
		{
			name:  "a tagged PNG screenshot",
			file:  rules.File{Ext: "png", Exif: map[string]any{"ExifIFD:UserComment": "Screenshot"}},
			facet: "kind", value: "screenshot",
		},
		{
			name:  "a tagged JPG screenshot - the one the ext guard used to miss",
			file:  rules.File{Ext: "jpg", Exif: map[string]any{"ExifIFD:UserComment": "Screenshot"}},
			facet: "kind", value: "screenshot",
		},
		{
			name:  "an XMP-only screenshot - the eleven the group key used to miss",
			file:  rules.File{Ext: "png", Exif: map[string]any{"XMP-exif:UserComment": "Screenshot"}},
			facet: "kind", value: "screenshot",
		},
		{
			name: "a JPEG named .HEIC, from a camera",
			file: rules.File{Ext: "heic", Exif: map[string]any{
				"File:FileTypeExtension": "jpg",
				"IFD0:Make":              "Apple",
				"ExifIFD:FNumber":        1.8,
			}},
			facet: "kind", value: "photo",
		},
		{
			name: "a real HEIC from an iPhone",
			file: rules.File{Ext: "heic", Exif: map[string]any{
				"IFD0:Make": "Apple", "IFD0:Model": "iPhone 7", "ExifIFD:FNumber": 1.8,
			}},
			facet: "kind", value: "photo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := set.Evaluate(rules.NewEnv(tt.file))
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}

			if len(got) != 1 {
				t.Fatalf("Evaluate() = %v, want exactly one decision", got)
			}

			if got[0].Facet != tt.facet || got[0].Value != tt.value {
				t.Errorf("decision = %s=%s, want %s=%s",
					got[0].Facet, got[0].Value, tt.facet, tt.value)
			}
		})
	}
}

// TestTheShippedRulesLeaveADownloadUndecided is the other half: a web image with no camera
// evidence and no screenshot marker must fall through to the model rather than being guessed at.
func TestTheShippedRulesLeaveADownloadUndecided(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	set, err := rules.Compile(&cfg)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	got, err := set.Evaluate(rules.NewEnv(rules.File{
		Ext: "jpg", Width: 1000, Height: 1000,
		Exif: map[string]any{"File:FileTypeExtension": "jpg"},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if len(got) != 0 {
		t.Errorf("Evaluate() = %v, want no decision for a file with no evidence either way", got)
	}
}
