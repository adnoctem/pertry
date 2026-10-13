// Package imageprep turns a file on disk into the images sent to a vision model
// (docs/ARCHITECTURE.md#inference-backend-and-model-choice).
//
// Three jobs: keep the payload small enough that inference is not dominated by pixels, never hand
// the model a format it cannot read, and deal with the screenshots that are far taller than they
// are wide.
package imageprep

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"

	_ "image/gif"  // registered for decoding
	_ "image/jpeg" // registered for decoding

	_ "golang.org/x/image/webp" // registered for decoding

	"github.com/adnoctem/pertry/pkg/infer"
)

// DefaultMaxEdge is the default long-edge limit.
//
// 1280 is docs/ARCHITECTURE.md#model-stages's default and is a real trade-off rather than a round number: a phone screenshot at
// 1242 x 2688 is already near it, so text stays legible, while a 4032 x 3024 photo is reduced by
// about ten times in pixel count and so in the tokens the vision encoder spends on it.
const DefaultMaxEdge = 1280

// TallAspectRatio is the height-to-width ratio above which an image is treated as a tall
// screenshot (docs/ARCHITECTURE.md#inference-backend-and-model-choice).
const TallAspectRatio = 3.0

// JPEGQuality is used for photographs. 85 is high enough that no artefact is visible at the sizes
// involved and low enough to keep the payload small.
const JPEGQuality = 85

// TallStrategy is how a tall screenshot is handled.
type TallStrategy string

const (
	// TallTile splits the image into overlapping tiles, sent as several images in one
	// message.
	TallTile TallStrategy = "tile"

	// TallCrop keeps the top region only, which is where a screenshot's identifying
	// furniture - status bar, address bar, app header - lives.
	TallCrop TallStrategy = "crop"

	// TallScale ignores the aspect ratio and simply scales the whole thing to fit, which
	// makes small text illegible but keeps the layout intact.
	TallScale TallStrategy = "scale"
)

// Options configures preparation.
type Options struct {
	// MaxEdge is the long-edge limit. Zero means DefaultMaxEdge.
	MaxEdge int

	// Tall is the strategy for an image taller than TallAspectRatio. Empty means TallCrop.
	Tall TallStrategy

	// TileOverlap is the fraction of a tile that overlaps its neighbour, so that a UI element
	// on a boundary appears whole in at least one tile. Zero means 0.1.
	TileOverlap float64

	// MaxTiles bounds how many tiles a very long screenshot produces. Zero means 3.
	MaxTiles int
}

// ErrUndecodable means no registered decoder could read the file.
//
// Expected rather than exceptional: a real HEIC does not decode in a pure-Go build, and docs/ARCHITECTURE.md#sicht-known-limitations
// settles that this is reported rather than worked around.
var ErrUndecodable = errors.New("image cannot be decoded in a pure-Go build")

// Result is what was prepared.
type Result struct {
	// Images are what to send, in order. Usually one; more when a tall screenshot was tiled.
	Images []infer.Image

	// Width and Height are the decoded source dimensions.
	Width  int
	Height int

	// Tall reports that the aspect ratio exceeded the threshold.
	Tall bool

	// Strategy is what was actually done to a tall image, empty when it was not tall.
	Strategy TallStrategy
}

// Prepare reads a file and returns the images to send.
func Prepare(path string, opts Options) (*Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return PrepareBytes(raw, filepath.Ext(path), opts)
}

// PrepareBytes prepares an already-read image.
func PrepareBytes(raw []byte, ext string, opts Options) (*Result, error) {
	// HEIC is refused before decoding is attempted, so the error names the real reason rather
	// than "unknown format". docs/ARCHITECTURE.md#inference-backend-and-model-choice forbids sending it to the model at all.
	if isHEIC(ext, raw) {
		return nil, fmt.Errorf("%w: HEIC has no pure-Go decoder, and docs/ARCHITECTURE.md#sicht-known-limitations settles that sicht "+
			"does not acquire one", ErrUndecodable)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUndecodable, err)
	}

	maxEdge := opts.MaxEdge
	if maxEdge <= 0 {
		maxEdge = DefaultMaxEdge
	}

	strategy := opts.Tall
	if strategy == "" {
		strategy = TallCrop
	}

	overlap := opts.TileOverlap
	if overlap <= 0 {
		overlap = 0.1
	}

	maxTiles := opts.MaxTiles
	if maxTiles <= 0 {
		maxTiles = 3
	}

	bounds := img.Bounds()

	out := &Result{Width: bounds.Dx(), Height: bounds.Dy()}

	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return nil, fmt.Errorf("%w: zero-sized image", ErrUndecodable)
	}

	ratio := float64(bounds.Dy()) / float64(bounds.Dx())
	out.Tall = ratio > TallAspectRatio

	// A screenshot is re-encoded as PNG and a photograph as JPEG. Text in a screenshot is
	// exactly what JPEG's chroma subsampling destroys, and reading the text is the whole
	// point; a photograph has no such edges and JPEG is far smaller.
	encodePNG := looksLikeScreenshot(ext, ratio)

	if !out.Tall {
		encoded, err := encode(scaleToFit(img, maxEdge), encodePNG)
		if err != nil {
			return nil, err
		}

		out.Images = []infer.Image{encoded}

		return out, nil
	}

	out.Strategy = strategy

	switch strategy {
	case TallScale:
		encoded, err := encode(scaleToFit(img, maxEdge), encodePNG)
		if err != nil {
			return nil, err
		}

		out.Images = []infer.Image{encoded}

	case TallCrop:
		// The top region, squared off. A screenshot's identifying furniture - status bar,
		// address bar, app header, the first post - is at the top; the tail is usually
		// more of the same content.
		height := min(bounds.Dx()*2, bounds.Dy())

		cropped := crop(img, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Min.Y+height))

		encoded, err := encode(scaleToFit(cropped, maxEdge), encodePNG)
		if err != nil {
			return nil, err
		}

		out.Images = []infer.Image{encoded}

	case TallTile:
		for _, region := range tileRegions(bounds, overlap, maxTiles) {
			encoded, err := encode(scaleToFit(crop(img, region), maxEdge), encodePNG)
			if err != nil {
				return nil, err
			}

			out.Images = append(out.Images, encoded)
		}

	default:
		return nil, fmt.Errorf("unknown tall-image strategy %q", strategy)
	}

	return out, nil
}

// tileRegions splits a tall image into overlapping horizontal bands.
//
// Overlapping so that a UI element straddling a boundary appears whole in at least one tile; a
// clean split is exactly how an address bar gets cut in half and becomes unreadable.
func tileRegions(bounds image.Rectangle, overlap float64, maxTiles int) []image.Rectangle {
	width := bounds.Dx()
	height := bounds.Dy()

	// Each tile is at most twice as tall as it is wide, which keeps it within the aspect
	// ratio a vision encoder handles well.
	tileHeight := width * 2

	count := (height + tileHeight - 1) / tileHeight
	count = min(max(count, 1), maxTiles)

	step := height / count
	pad := int(float64(step) * overlap)

	regions := make([]image.Rectangle, 0, count)

	for i := range count {
		top := bounds.Min.Y + i*step - pad
		bottom := bounds.Min.Y + (i+1)*step + pad

		top = max(top, bounds.Min.Y)
		bottom = min(bottom, bounds.Max.Y)

		if bottom-top <= 0 {
			continue
		}

		regions = append(regions, image.Rect(bounds.Min.X, top, bounds.Max.X, bottom))
	}

	return regions
}

func crop(img image.Image, region image.Rectangle) image.Image {
	out := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))

	draw.Draw(out, out.Bounds(), img, region.Min, draw.Src)

	return out
}

// scaleToFit reduces an image so its long edge is at most maxEdge. An image already smaller is
// returned unchanged: upscaling adds no information and costs tokens.
func scaleToFit(img image.Image, maxEdge int) image.Image {
	bounds := img.Bounds()

	longest := max(bounds.Dx(), bounds.Dy())
	if longest <= maxEdge {
		return img
	}

	scale := float64(maxEdge) / float64(longest)

	width := max(int(float64(bounds.Dx())*scale), 1)
	height := max(int(float64(bounds.Dy())*scale), 1)

	out := image.NewRGBA(image.Rect(0, 0, width, height))

	// CatmullRom rather than a nearest-neighbour sample: downscaling a screenshot with
	// nearest-neighbour aliases text into noise, and the text is what the model is being
	// asked to read.
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, bounds, xdraw.Over, nil)

	return out
}

func encode(img image.Image, asPNG bool) (infer.Image, error) {
	var buf bytes.Buffer

	if asPNG {
		if err := png.Encode(&buf, img); err != nil {
			return infer.Image{}, fmt.Errorf("encode PNG: %w", err)
		}

		return infer.Image{Data: buf.Bytes(), MediaType: "image/png"}, nil
	}

	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return infer.Image{}, fmt.Errorf("encode JPEG: %w", err)
	}

	return infer.Image{Data: buf.Bytes(), MediaType: "image/jpeg"}, nil
}

// looksLikeScreenshot decides the output encoding.
//
// A guess, and a cheap one: PNG for a PNG source or a tall image, JPEG otherwise. It only affects
// the encoding, never the classification, so being wrong costs some bytes and nothing else.
func looksLikeScreenshot(ext string, ratio float64) bool {
	if strings.EqualFold(strings.TrimPrefix(ext, "."), "png") {
		return true
	}

	return ratio > 1.8
}

// isHEIC detects HEIC from the file's bytes, falling back to the extension only when the bytes
// say nothing.
//
// The order matters and getting it wrong is the docs/ARCHITECTURE.md#scan trap in miniature. An earlier version
// checked for an `ftyp` box and, failing to find one, trusted the extension - which refused the 16
// files in the reference archive that are named `.HEIC` and are really JPEGs. A decodable magic
// number is now checked **first**, so the name can never override what the file plainly is.
func isHEIC(ext string, raw []byte) bool {
	// A format a registered decoder handles is not HEIC, whatever it is called.
	if decodableMagic(raw) {
		return false
	}

	if len(raw) >= 12 && string(raw[4:8]) == "ftyp" {
		switch string(raw[8:12]) {
		case "heic", "heix", "hevc", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
			return true
		}

		// Some other ISO-BMFF container, an MP4 say. Not HEIC, and not decodable either -
		// image.Decode will say so more precisely than this function could.
		return false
	}

	// The bytes say nothing recognisable, so the name is all there is to go on.
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "heic", "heif":
		return true
	default:
		return false
	}
}

// decodableMagic reports whether the file begins with a signature one of the registered decoders
// handles.
func decodableMagic(raw []byte) bool {
	switch {
	case len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff:
		return true // JPEG
	case len(raw) >= 8 && string(raw[:8]) == "\x89PNG\r\n\x1a\n":
		return true // PNG
	case len(raw) >= 6 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a"):
		return true // GIF
	case len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return true // WebP, including the extended form x/image/webp may still refuse
	default:
		return false
	}
}

// TotalBytes is how much image data a result will send, for logging and cost estimation.
func (r *Result) TotalBytes() int {
	total := 0
	for _, img := range r.Images {
		total += len(img.Data)
	}

	return total
}
