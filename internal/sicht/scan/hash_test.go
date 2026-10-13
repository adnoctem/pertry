package scan_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/scan"
)

// gradient builds an image with a horizontal brightness ramp, which gives a dHash with every
// comparison bit set in the same direction - a predictable value to reason about.
func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			v := uint8(255 * x / max(w-1, 1))
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buf bytes.Buffer

	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()

	var buf bytes.Buffer

	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}

	return buf.Bytes()
}

func TestDistance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, 0b1011, 3},
		{^uint64(0), 0, 64},
		{0x0f0f0f0f0f0f0f0f, 0x0f0f0f0f0f0f0f0f, 0},
	}

	for _, tt := range tests {
		if got := scan.Distance(tt.a, tt.b); got != tt.want {
			t.Errorf("Distance(%#x, %#x) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// colorGray is a grey of the given intensity.
func colorGray(v uint8) color.Color { return color.RGBA{R: v, G: v, B: v, A: 255} }
