package imageprep_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/imageprep"
)

// gradient builds an image with horizontal structure, so a downscale has something to preserve.
func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			v := uint8((x*7 + y*3) % 256)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	return img
}

func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()

	path := filepath.Join(dir, name)

	var buf bytes.Buffer

	if err := png.Encode(&buf, gradient(w, h)); err != nil {
		t.Fatalf("encode: %v", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

func writeJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()

	path := filepath.Join(dir, name)

	var buf bytes.Buffer

	if err := jpeg.Encode(&buf, gradient(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

// decode reads back a prepared image so its real dimensions can be checked.
func decode(t *testing.T, data []byte) image.Image {
	t.Helper()

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode prepared image: %v", err)
	}

	return img
}

func TestDownscalesToMaxEdge(t *testing.T) {
	t.Parallel()

	path := writeJPEG(t, t.TempDir(), "photo.jpg", 4032, 3024)

	result, err := imageprep.Prepare(path, imageprep.Options{MaxEdge: 1280})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if result.Width != 4032 || result.Height != 3024 {
		t.Errorf("source dimensions = %dx%d, want the original", result.Width, result.Height)
	}

	if len(result.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(result.Images))
	}

	bounds := decode(t, result.Images[0].Data).Bounds()

	if bounds.Dx() != 1280 {
		t.Errorf("prepared width = %d, want 1280", bounds.Dx())
	}

	// The aspect ratio is kept: 3024 * 1280/4032 = 960.
	if bounds.Dy() != 960 {
		t.Errorf("prepared height = %d, want 960 - the aspect ratio must be preserved", bounds.Dy())
	}
}

// TestDoesNotUpscale: enlarging adds no information and costs the vision encoder tokens.
func TestDoesNotUpscale(t *testing.T) {
	t.Parallel()

	path := writePNG(t, t.TempDir(), "small.png", 320, 240)

	result, err := imageprep.Prepare(path, imageprep.Options{MaxEdge: 1280})
	if err != nil {
		t.Fatalf("Prepare(): %v", err)
	}

	bounds := decode(t, result.Images[0].Data).Bounds()

	if bounds.Dx() != 320 || bounds.Dy() != 240 {
		t.Errorf("prepared = %dx%d, want the original 320x240", bounds.Dx(), bounds.Dy())
	}
}

// TestScreenshotsAreEncodedAsPNG is the one encoding decision that affects accuracy: JPEG's chroma
// subsampling destroys exactly the crisp text edges a screenshot is read for.
func TestScreenshotsAreEncodedAsPNG(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := map[string]struct {
		path string
		want string
	}{
		"a PNG source stays PNG": {writePNG(t, dir, "shot.png", 1242, 2688), "image/png"},
		"a photograph is JPEG":   {writeJPEG(t, dir, "photo.jpg", 4032, 3024), "image/jpeg"},
	}

	for name, tt := range tests {
		result, err := imageprep.Prepare(tt.path, imageprep.Options{MaxEdge: 1280})
		if err != nil {
			t.Fatalf("%s: Prepare(): %v", name, err)
		}

		if result.Images[0].MediaType != tt.want {
			t.Errorf("%s: MediaType = %q, want %q", name, result.Images[0].MediaType, tt.want)
		}
	}
}

// TestTallScreenshotStrategies covers docs/ARCHITECTURE.md#inference-backend-and-model-choice's tall-image question. The threshold is 1:3.
func TestTallScreenshotStrategies(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// 750 x 4000 is a long scrolling capture: well past 1:3.
	path := writePNG(t, dir, "long.png", 750, 4000)

	t.Run("crop keeps the top", func(t *testing.T) {
		t.Parallel()

		result, err := imageprep.Prepare(path, imageprep.Options{
			MaxEdge: 1280, Tall: imageprep.TallCrop,
		})
		if err != nil {
			t.Fatalf("Prepare(): %v", err)
		}

		if !result.Tall {
			t.Error("Tall = false for a 750x4000 image")
		}

		if len(result.Images) != 1 {
			t.Fatalf("got %d images, want 1 for a crop", len(result.Images))
		}

		bounds := decode(t, result.Images[0].Data).Bounds()

		// The crop is at most twice as tall as it is wide, so the aspect ratio stays
		// within what a vision encoder handles well.
		ratio := float64(bounds.Dy()) / float64(bounds.Dx())
		if ratio > 2.1 {
			t.Errorf("cropped ratio = %.2f, want about 2", ratio)
		}
	})

	t.Run("tile produces overlapping bands", func(t *testing.T) {
		t.Parallel()

		result, err := imageprep.Prepare(path, imageprep.Options{
			MaxEdge: 1280, Tall: imageprep.TallTile, MaxTiles: 3,
		})
		if err != nil {
			t.Fatalf("Prepare(): %v", err)
		}

		if len(result.Images) < 2 {
			t.Fatalf("got %d images, want several tiles", len(result.Images))
		}

		if len(result.Images) > 3 {
			t.Errorf("got %d tiles, want at most the 3 asked for", len(result.Images))
		}

		if result.Strategy != imageprep.TallTile {
			t.Errorf("Strategy = %q, want tile", result.Strategy)
		}
	})

	t.Run("scale keeps the whole frame", func(t *testing.T) {
		t.Parallel()

		result, err := imageprep.Prepare(path, imageprep.Options{
			MaxEdge: 1280, Tall: imageprep.TallScale,
		})
		if err != nil {
			t.Fatalf("Prepare(): %v", err)
		}

		if len(result.Images) != 1 {
			t.Fatalf("got %d images, want 1", len(result.Images))
		}

		bounds := decode(t, result.Images[0].Data).Bounds()

		// The full 1:5.3 ratio survives, which is the point and also why the text is
		// illegible.
		ratio := float64(bounds.Dy()) / float64(bounds.Dx())
		if ratio < 5.0 {
			t.Errorf("scaled ratio = %.2f, want the original aspect preserved", ratio)
		}
	})
}

// TestOrdinaryAspectRatiosAreNotTall checks the threshold does not catch a normal phone
// screenshot, which is about 1:2.2 and must be sent whole.
func TestOrdinaryAspectRatiosAreNotTall(t *testing.T) {
	t.Parallel()

	path := writePNG(t, t.TempDir(), "shot.png", 1242, 2688)

	result, err := imageprep.Prepare(path, imageprep.Options{MaxEdge: 1280})
	if err != nil {
		t.Fatalf("Prepare(): %v", err)
	}

	if result.Tall {
		t.Error("a 1242x2688 phone screenshot was treated as tall; it must be sent whole")
	}

	if len(result.Images) != 1 {
		t.Errorf("got %d images, want 1", len(result.Images))
	}
}

// TestHEICIsRefusedBeforeDecoding is docs/ARCHITECTURE.md#sicht-known-limitations: a real HEIC has no pure-Go decoder, and the error
// should name that rather than say "unknown format".
func TestHEICIsRefusedBeforeDecoding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "IMG_0007.HEIC")

	// A minimal HEIC container header: the ftyp box with the heic brand.
	header := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	header = append(header, make([]byte, 16)...)

	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := imageprep.Prepare(path, imageprep.Options{})
	if !errors.Is(err, imageprep.ErrUndecodable) {
		t.Fatalf("error = %v, want ErrUndecodable", err)
	}

	if !containsAny(err.Error(), "HEIC", "pure-Go") {
		t.Errorf("error = %q, want it to name the real reason", err)
	}
}

// TestAJPEGNamedHEICIsPrepared is docs/ARCHITECTURE.md#scan's finding applied here: 16 files named .HEIC in the
// reference archive are really JPEGs, and refusing them by extension would skip images that
// decode perfectly.
func TestAJPEGNamedHEICIsPrepared(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// A real JPEG, misleadingly named.
	path := writeJPEG(t, dir, "IMG_2060.HEIC", 4032, 3024)

	result, err := imageprep.Prepare(path, imageprep.Options{MaxEdge: 1280})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want a JPEG to be prepared whatever it is called", err)
	}

	if len(result.Images) != 1 {
		t.Errorf("got %d images, want 1", len(result.Images))
	}
}

func TestUndecodableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "not-an-image.png")

	if err := os.WriteFile(path, []byte("this is not a PNG"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := imageprep.Prepare(path, imageprep.Options{}); !errors.Is(err, imageprep.ErrUndecodable) {
		t.Errorf("error = %v, want ErrUndecodable", err)
	}
}

func TestMissingFile(t *testing.T) {
	t.Parallel()

	_, err := imageprep.Prepare(filepath.Join(t.TempDir(), "absent.png"), imageprep.Options{})
	if err == nil {
		t.Error("Prepare() error = nil for a missing file")
	}
}

func TestUnknownStrategy(t *testing.T) {
	t.Parallel()

	path := writePNG(t, t.TempDir(), "long.png", 750, 4000)

	if _, err := imageprep.Prepare(path, imageprep.Options{Tall: "nonsense"}); err == nil {
		t.Error("Prepare() error = nil for an unknown tall strategy")
	}
}

// TestPreparedPayloadIsSmaller is the reason any of this exists: inference should not be dominated
// by pixels.
func TestPreparedPayloadIsSmaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeJPEG(t, dir, "photo.jpg", 4032, 3024)

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	result, err := imageprep.Prepare(path, imageprep.Options{MaxEdge: 1280})
	if err != nil {
		t.Fatalf("Prepare(): %v", err)
	}

	if result.TotalBytes() >= len(original) {
		t.Errorf("prepared %d bytes from a %d-byte source, want a reduction",
			result.TotalBytes(), len(original))
	}
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				return true
			}
		}
	}

	return false
}
