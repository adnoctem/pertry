package meta_test

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/adnoctem/pertry/internal/sicht/meta"
)

// TestSidecarNames uses filenames taken verbatim from the reference export, including every shape
// that broke an earlier assumption. The expected sidecar name in each case is a file that really
// exists next to the media file.
func TestSidecarNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		media string
		want  string
		why   string
	}{
		{
			name:  "ordinary",
			media: "IMG_1854.JPG",
			want:  "IMG_1854.JPG.supplemental-metadata.json",
			why:   "the common case, comfortably under the cap",
		},
		{
			name:  "duplicate counter moves to the end",
			media: "IMG_1663(1).JPG",
			want:  "IMG_1663.JPG.supplemental-metadata(1).json",
			why:   "on the media file the counter precedes the extension; on the sidecar it follows the suffix",
		},
		{
			name:  "truncated at the cap",
			media: "Corporate_Identity_PZ.jpg",
			want:  "Corporate_Identity_PZ.jpg.supplemental-metadat.json",
			why:   "51 characters exactly, so the suffix loses its last character",
		},
		{
			name:  "truncation eats most of the suffix",
			media: "00 - BSIDE_785_mixtape_2-front-large.jpg",
			want:  "00 - BSIDE_785_mixtape_2-front-large.jpg.suppl.json",
			why:   "a long media name leaves almost no room for the suffix",
		},
		{
			name:  "truncation eats the suffix and the extension",
			media: "chat-media-video-F2F83D61-3A19-478B-9A27-F97F39.mov",
			want:  "chat-media-video-F2F83D61-3A19-478B-9A27-F97F3.json",
			why:   "the media name is itself already at the cap, so nothing of the suffix survives",
		},
		{
			name:  "counter on a name that also truncates",
			media: "andrew-wulf-Tc3c8ZDIbxw-unsplash(1).jpg",
			want:  "andrew-wulf-Tc3c8ZDIbxw-unsplash.jpg.supplemen(1).json",
			why:   "the counter is appended after truncation, so the result is 54 characters - over the cap",
		},
		{
			// A verified pair from the export, and the one that proves the cap counts
			// characters: both names are 51 runes and 52 bytes, so a byte-based
			// truncation produces a candidate one rune short and finds nothing.
			name:  "unicode filename at the cap",
			media: "Unternehmensziele+Ökonomische+Ziele+Gewinnmaxim.jpg",
			want:  "Unternehmensziele+Ökonomische+Ziele+Gewinnmaxi.json",
			why:   "the cap is 51 characters, not 51 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := meta.SidecarNames(tt.media)

			if len(got) == 0 {
				t.Fatalf("SidecarNames(%q) returned nothing", tt.media)
			}

			if got[0] != tt.want {
				t.Errorf("SidecarNames(%q)[0] = %q, want %q\n(%s)", tt.media, got[0], tt.want, tt.why)
			}

			for _, n := range got {
				if r := len([]rune(n)); r > meta.NameCap && !hasCounter(n) {
					t.Errorf("candidate %q is %d characters, over the %d cap, and carries "+
						"no counter", n, r, meta.NameCap)
				}

				if !utf8.ValidString(n) {
					t.Errorf("candidate %q is not valid UTF-8, so truncation split a rune", n)
				}
			}
		})
	}
}

// hasCounter reports whether a sidecar name ends in a duplicate counter, which is the one case
// allowed to exceed the cap because Google appends it after truncating.
func hasCounter(name string) bool {
	return len(name) > 8 && name[len(name)-5:] == ".json" &&
		name[len(name)-6] == ')'
}

// TestSidecarNamesIncludesTheLegacyForm keeps one extra directory lookup rather than assuming the
// reference export is the only Takeout version that will ever be seen.
func TestSidecarNamesIncludesTheLegacyForm(t *testing.T) {
	t.Parallel()

	got := meta.SidecarNames("IMG_1854.JPG")

	var found bool

	for _, n := range got {
		if n == "IMG_1854.JPG.json" {
			found = true
		}
	}

	if !found {
		t.Errorf("SidecarNames() = %v, want it to include the legacy <name>.json form", got)
	}
}

func TestSidecarNamesAreUnique(t *testing.T) {
	t.Parallel()

	// A name long enough that several constructions collapse to the same truncation.
	got := meta.SidecarNames("chat-media-video-F2F83D61-3A19-478B-9A27-F97F39.mov")

	seen := map[string]bool{}

	for _, n := range got {
		if seen[n] {
			t.Errorf("SidecarNames() returned %q twice: %v", n, got)
		}

		seen[n] = true
	}
}

func TestParseSidecar(t *testing.T) {
	t.Parallel()

	// Verbatim shape of a real sidecar, including imageViews being a string and the localised
	// "formatted" field that must never be parsed.
	const raw = `{
	  "title": "anfahrt.png",
	  "description": "",
	  "imageViews": "0",
	  "creationTime": {"timestamp": "1539208052", "formatted": "10.10.2018, 21:47:32 UTC"},
	  "photoTakenTime": {"timestamp": "1539208044", "formatted": "10.10.2018, 21:47:24 UTC"},
	  "geoData": {"latitude": 0.0, "longitude": 0.0, "altitude": 0.0, "latitudeSpan": 0.0, "longitudeSpan": 0.0},
	  "url": "https://photos.google.com/photo/AF1Qip",
	  "googlePhotosOrigin": {"driveDesktopUploader": {"version": "BACKUP_AND_SYNC"}}
	}`

	s, err := meta.ParseSidecar([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSidecar() error = %v, want nil", err)
	}

	if s.Title != "anfahrt.png" {
		t.Errorf("Title = %q, want %q", s.Title, "anfahrt.png")
	}

	if s.ImageViews != "0" {
		t.Errorf("ImageViews = %q, want %q (it is a string in the file)", s.ImageViews, "0")
	}

	taken, ok := s.TakenAt()
	if !ok {
		t.Fatal("TakenAt() ok = false, want true")
	}

	if want := time.Unix(1539208044, 0).UTC(); !taken.Equal(want) {
		t.Errorf("TakenAt() = %v, want %v", taken, want)
	}

	if _, ok := s.Location(); ok {
		t.Error("Location() ok = true for Takeout's all-zero geoData, want false")
	}
}

// TestTakenAtPrefersPhotoTakenTime is the one that protects the chronology. creationTime is when
// the file reached Google Photos, which for a backup of an old archive is the backup date.
func TestTakenAtPrefersPhotoTakenTime(t *testing.T) {
	t.Parallel()

	const raw = `{
	  "creationTime":   {"timestamp": "1774000000"},
	  "photoTakenTime": {"timestamp": "1234567890"}
	}`

	s, err := meta.ParseSidecar([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSidecar() error = %v", err)
	}

	got, ok := s.TakenAt()
	if !ok {
		t.Fatal("TakenAt() ok = false, want true")
	}

	if want := time.Unix(1234567890, 0).UTC(); !got.Equal(want) {
		t.Errorf("TakenAt() = %v, want the photoTakenTime %v - creationTime is the backup date",
			got, want)
	}
}

func TestTakenAtFallsBackToCreationTime(t *testing.T) {
	t.Parallel()

	s, err := meta.ParseSidecar([]byte(`{"creationTime": {"timestamp": "1234567890"}}`))
	if err != nil {
		t.Fatalf("ParseSidecar() error = %v", err)
	}

	if _, ok := s.TakenAt(); !ok {
		t.Error("TakenAt() ok = false with only creationTime present, want true")
	}
}

func TestTimestampDistinguishesAbsentFromEpoch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want bool
	}{
		"absent":      {"", false},
		"unparsable":  {"not-a-number", false},
		"the epoch":   {"0", true},
		"a real time": {"1539208044", true},
	}

	for name, tt := range tests {
		s, err := meta.ParseSidecar([]byte(`{"photoTakenTime":{"timestamp":"` + tt.in + `"}}`))
		if err != nil {
			t.Fatalf("%s: ParseSidecar() error = %v", name, err)
		}

		if _, ok := s.PhotoTakenTime.Time(); ok != tt.want {
			t.Errorf("%s: Time() ok = %v, want %v", name, ok, tt.want)
		}
	}
}

func TestHasLocation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		geo  meta.GeoData
		want bool
	}{
		"Takeout's zero object": {meta.GeoData{}, false},
		"a real location":       {meta.GeoData{Latitude: 48.137, Longitude: 11.575}, true},
		"latitude only":         {meta.GeoData{Latitude: 48.137}, true},
		"altitude only":         {meta.GeoData{Altitude: 520}, false},
	}

	for name, tt := range tests {
		if got := tt.geo.HasLocation(); got != tt.want {
			t.Errorf("%s: HasLocation() = %v, want %v", name, got, tt.want)
		}
	}
}

func TestLocationPrefersExif(t *testing.T) {
	t.Parallel()

	s := &meta.Sidecar{
		GeoData:     meta.GeoData{Latitude: 1, Longitude: 1},
		GeoDataExif: meta.GeoData{Latitude: 48.137, Longitude: 11.575},
	}

	got, ok := s.Location()
	if !ok {
		t.Fatal("Location() ok = false, want true")
	}

	if got.Latitude != 48.137 {
		t.Errorf("Location() = %v, want the EXIF-derived one", got)
	}
}

// TestTitleMatches covers the case that makes Title a diagnostic rather than a key: the media
// file on disk can be shorter than the title, because Google caps both independently.
func TestTitleMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		title, media string
		want         bool
	}{
		{"IMG_1854.JPG", "IMG_1854.JPG", true},
		// The counter is on the file, not in the title.
		{"IMG_1663.JPG", "IMG_1663(1).JPG", true},
		// The real case: title longer than the file it sits beside.
		{
			"chat-media-video-F2F83D61-3A19-478B-9A27-F97F39A889D6.mov",
			"chat-media-video-F2F83D61-3A19-478B-9A27-F97F39.mov",
			true,
		},
		// An empty title tells us nothing, so it is not a mismatch.
		{"", "IMG_1854.JPG", true},
		// A genuine mismatch, which is worth logging.
		{"IMG_9999.JPG", "IMG_1854.JPG", false},
	}

	for _, tt := range tests {
		s := &meta.Sidecar{Title: tt.title}

		if got := s.TitleMatches(tt.media); got != tt.want {
			t.Errorf("Sidecar{Title:%q}.TitleMatches(%q) = %v, want %v",
				tt.title, tt.media, got, tt.want)
		}
	}
}
