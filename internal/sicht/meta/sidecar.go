// Package meta reads a file's metadata: EXIF via exiftool, and the Google Takeout sidecar JSON
// that sits beside an exported photo (docs/ARCHITECTURE.md#scan).
package meta

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NameCap is the length Google truncates a sidecar filename to, counting the ".json" extension.
//
// Measured, not guessed: in the reference export 141 sidecar names sit at exactly 51 and none of
// the 1557 exceeds it except by a duplicate counter appended afterwards (docs/ARCHITECTURE.md#scan).
//
// The unit is **characters, not bytes**. The export settles it: a German filename arrives as
// "Unternehmensziele+Ökonomische+Ziele+Gewinnmaxi.json", which is 51 runes and 52 bytes, beside a
// media file that is likewise 51 runes and 52 bytes. Truncating by byte misses that pair, and
// would also be free to cut a multi-byte rune in half and produce a name that is not valid UTF-8.
const NameCap = 51

// MetadataSuffix is what Takeout inserts between the media filename and ".json".
//
// It must never be matched as a literal, because NameCap truncates it: the reference export
// contains ".supplemental-metadat", ".supplemental", ".supplementa", ".supplemen", ".suppleme",
// ".supple", ".suppl" and ".sup" as real suffixes of real files. Construct and truncate instead.
const MetadataSuffix = ".supplemental-metadata"

// jsonExt is split out because the truncation budget depends on its length.
const jsonExt = ".json"

// counterRe matches a Google duplicate counter: "IMG_1663(1).JPG".
var counterRe = regexp.MustCompile(`^(.*?)\((\d+)\)(\.[^.]*)$`)

// SidecarNames returns the candidate sidecar filenames for a media filename, most likely first.
//
// The caller looks each one up in the media file's own directory; the first that exists wins. This
// is deliberately a short, ordered list of constructed names rather than a fuzzy search, because
// a constructed name is exact: run over the reference export it claimed all 1557 sidecars with
// none left over and none ambiguous.
//
// mediaName is a base filename, not a path.
func SidecarNames(mediaName string) []string {
	var names []string

	// The duplicate counter moves. On the media file it sits before the extension; on the
	// sidecar it sits after the metadata suffix and immediately before ".json", which can push
	// the result past NameCap - "andrew-wulf-Tc3c8ZDIbxw-unsplash.jpg.supplemen(1).json" is 54
	// characters and is a real filename.
	if m := counterRe.FindStringSubmatch(mediaName); m != nil {
		base, counter, ext := m[1], m[2], m[3]

		full := truncateName(base + ext + MetadataSuffix + jsonExt)
		names = append(names,
			strings.TrimSuffix(full, jsonExt)+"("+counter+")"+jsonExt,
			// Some exports leave the counter where it was.
			truncateName(mediaName+MetadataSuffix+jsonExt),
		)
	}

	names = append(names,
		truncateName(mediaName+MetadataSuffix+jsonExt),
		// The older form, from Takeout versions before the suffix was introduced. It does not
		// occur in the reference export, but it costs one directory lookup to tolerate.
		truncateName(mediaName+jsonExt),
	)

	return dedupe(names)
}

// truncateName applies Google's length cap, keeping the ".json" extension.
//
// Counted in runes, for the reason given on NameCap. Converting to []rune rather than slicing the
// string is what keeps a multi-byte character from being cut in half.
func truncateName(name string) string {
	runes := []rune(name)
	if len(runes) <= NameCap {
		return name
	}

	return string(runes[:NameCap-len(jsonExt)]) + jsonExt
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}

		seen[s] = struct{}{}

		out = append(out, s)
	}

	return out
}

// Sidecar is the part of a Takeout sidecar sicht uses.
//
// Only fields that actually occur are modelled. Across the reference export every sidecar has
// exactly title, description, imageViews, creationTime, photoTakenTime, geoData, url and
// googlePhotosOrigin; geoDataExif appears once. googlePhotosOrigin's own shape varies by upload
// path (driveDesktopUploader, mobileUpload and others), so it is kept as raw JSON rather than
// modelled into a union that the next Takeout version would invalidate.
type Sidecar struct {
	// Title is the original filename as Google held it. Useful as a diagnostic but **not** as
	// a key: the media file on disk may itself be truncated, so Title can legitimately be
	// longer than the file it describes. The reference export has a sidecar whose Title is
	// "chat-media-video-F2F83D61-3A19-478B-9A27-F97F39A889D6.mov" beside a file named
	// "chat-media-video-F2F83D61-3A19-478B-9A27-F97F39.mov".
	Title string `json:"title"`

	Description string `json:"description"`

	// ImageViews is a decimal string in the file, not a number.
	ImageViews string `json:"imageViews"`

	CreationTime   Timestamp `json:"creationTime"`
	PhotoTakenTime Timestamp `json:"photoTakenTime"`

	GeoData     GeoData `json:"geoData"`
	GeoDataExif GeoData `json:"geoDataExif"`

	URL string `json:"url"`

	GooglePhotosOrigin json.RawMessage `json:"googlePhotosOrigin,omitempty"`

	Favorited bool `json:"favorited,omitempty"`
}

// Timestamp is Takeout's time encoding: Unix seconds as a decimal string, plus a localised and
// therefore unparsable rendering of the same instant.
type Timestamp struct {
	Timestamp string `json:"timestamp"`

	// Formatted is kept for display only. It is localised to the account's language - the
	// reference export has "10.10.2018, 21:47:32 UTC" - so it is never parsed.
	Formatted string `json:"formatted"`
}

// Time parses the Unix seconds. The boolean is false when the field is absent or unparsable,
// which is distinguishable from the epoch.
func (t Timestamp) Time() (time.Time, bool) {
	if t.Timestamp == "" {
		return time.Time{}, false
	}

	secs, err := strconv.ParseInt(t.Timestamp, 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	return time.Unix(secs, 0).UTC(), true
}

// GeoData is a location. Takeout writes an all-zero object when it has none, which is why
// HasLocation exists rather than a nil check.
type GeoData struct {
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Altitude      float64 `json:"altitude"`
	LatitudeSpan  float64 `json:"latitudeSpan"`
	LongitudeSpan float64 `json:"longitudeSpan"`
}

// HasLocation reports whether this is a real location rather than Takeout's zero object.
//
// Treating 0,0 as absent is technically wrong for a point in the Gulf of Guinea and right for
// every photo anyone has actually taken; Takeout writes exactly 0.0 for "unknown", and the
// alternative is reporting a location nobody was at.
func (g GeoData) HasLocation() bool {
	return g.Latitude != 0 || g.Longitude != 0
}

// TakenAt returns the best capture time the sidecar offers, preferring photoTakenTime.
//
// creationTime is when the file reached Google Photos, which for a backup of an old archive is
// the backup date and not the capture date - in the reference export the two differ by years on
// older files. Preferring it would destroy exactly the chronology this project exists to recover.
func (s *Sidecar) TakenAt() (time.Time, bool) {
	if t, ok := s.PhotoTakenTime.Time(); ok {
		return t, true
	}

	return s.CreationTime.Time()
}

// Location returns the sidecar's location, preferring the EXIF-derived one when present.
func (s *Sidecar) Location() (GeoData, bool) {
	if s.GeoDataExif.HasLocation() {
		return s.GeoDataExif, true
	}

	if s.GeoData.HasLocation() {
		return s.GeoData, true
	}

	return GeoData{}, false
}

// TitleMatches reports whether the sidecar's title is consistent with the media filename it was
// found next to.
//
// Consistent means equal, or the filename being a truncation of the title: Google caps the media
// filename at the same length it caps the sidecar's, so a long original name arrives shortened on
// disk while the sidecar still records it in full. A false result is worth logging and is not
// worth failing on, since the name-based match is itself exact.
func (s *Sidecar) TitleMatches(mediaName string) bool {
	if s.Title == "" || s.Title == mediaName {
		return true
	}

	// Compare ignoring a duplicate counter, which the media file carries and the title does not.
	name := mediaName
	if m := counterRe.FindStringSubmatch(name); m != nil {
		name = m[1] + m[3]
	}

	if s.Title == name {
		return true
	}

	// A truncated media filename: same stem prefix, and no longer than the cap.
	//
	// The title is sanitised first, because Google rewrites characters in the filename that it
	// keeps in the title. The reference export has a sidecar titled
	// "https_%2F%2Fimages.genius.com%2F6975c…jpg" beside a file named
	// "https__2F_2Fimages.genius.com_2F6975c…jpg" - every "%" became "_". Without this the
	// pairing is still correct and the diagnostic cries wolf, which is worse than useless.
	if len([]rune(name)) <= NameCap {
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if stem != "" && strings.HasPrefix(sanitiseTitle(s.Title), stem) {
			return true
		}
	}

	return false
}

// titleSubstitutions are the character rewrites Google applies to a filename but not to the title.
//
// Only "%" is listed, because that is the only one the reference export demonstrates. Guessing at
// others would make the diagnostic accept mismatches it should report.
var titleSubstitutions = strings.NewReplacer("%", "_")

func sanitiseTitle(title string) string { return titleSubstitutions.Replace(title) }

// ParseSidecar decodes a sidecar's contents.
func ParseSidecar(raw []byte) (*Sidecar, error) {
	var out Sidecar

	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse Takeout sidecar: %w", err)
	}

	return &out, nil
}
