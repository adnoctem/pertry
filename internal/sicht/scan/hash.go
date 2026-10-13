// Package scan indexes the files under a workspace root: content hashes, metadata, Takeout
// sidecars and a perceptual hash (docs/ARCHITECTURE.md#scan).
package scan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"

	// Decoders registered for their side effects. HEIC is deliberately absent: it does not
	// decode in pure Go, and docs/ARCHITECTURE.md#sicht-known-limitations settles that a HEIC gets no thumbnail and no dHash rather
	// than pulling in a cgo decoder. Nothing is lost for classification, because docs/ARCHITECTURE.md#rules's
	// photo rule decides every HEIC from its EXIF.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// maxInMemory is the largest file read into memory to be hashed and decoded in one pass.
//
// Hashing needs every byte and so does decoding, so reading once and serving both from the buffer
// halves the I/O - which matters a great deal when the archive is on a translated filesystem,
// where a round trip per read is the dominant cost. Above this a file is hashed by streaming and
// gets no perceptual hash, because holding an arbitrarily large file in memory across a worker
// pool is the worse failure.
const maxInMemory = 48 << 20 // 48 MiB

// content is a file read for hashing, with its bytes retained when they fit.
type content struct {
	// Hash is the SHA-256 of the content, lowercase hex.
	Hash string

	// Size is the number of bytes hashed.
	Size int64

	// Bytes is the file's contents, or nil when the file was too large to retain.
	Bytes []byte
}

// readContent hashes a file, keeping its bytes when they fit in maxInMemory.
func readContent(path string, size int64) (content, error) {
	file, err := os.Open(path)
	if err != nil {
		return content{}, fmt.Errorf("open %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	digest := sha256.New()

	var (
		buf    *bytes.Buffer
		writer io.Writer = digest
	)

	if size >= 0 && size <= maxInMemory {
		buf = bytes.NewBuffer(make([]byte, 0, size))
		writer = io.MultiWriter(digest, buf)
	}

	written, err := io.Copy(writer, file)
	if err != nil {
		return content{}, fmt.Errorf("read %s: %w", path, err)
	}

	out := content{
		Hash: hex.EncodeToString(digest.Sum(nil)),
		Size: written,
	}

	if buf != nil {
		out.Bytes = buf.Bytes()
	}

	return out, nil
}

// errUndecodable marks a format that cannot be decoded in a pure-Go build, which is expected
// rather than exceptional - every HEIC in an iPhone archive lands here.
var errUndecodable = errors.New("format cannot be decoded")

// dimensions reads an image's pixel dimensions from its header alone.
//
// Used only as the fallback docs/ARCHITECTURE.md#scan step 4 calls for; exiftool is preferred because it answers for
// HEIC too, which no registered Go decoder does.
func dimensions(raw []byte) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w", errUndecodable, err)
	}

	return cfg.Width, cfg.Height, nil
}

// dhashWidth and dhashHeight are the sample grid. One extra column, because a difference hash
// compares each pixel with its right-hand neighbour, giving 8x8 = 64 comparisons.
const (
	dhashWidth  = 9
	dhashHeight = 8
)

// dhash computes a 64-bit difference hash.
//
// Chosen over an average hash because it responds to structure rather than overall brightness: two
// different screenshots of the same app are far more similar in average brightness than in edge
// layout. docs/ARCHITECTURE.md#scan step 5 stores it without acting on it in M1; docs/ARCHITECTURE.md#apply-manifests-and-undo is where it earns its place.
//
// The grid is sampled by averaging each source block rather than by point-sampling, so that a
// single stray pixel cannot move a bit. Bit i is set when the sampled cell is brighter than the
// cell to its right, reading left to right and top to bottom.
func dhash(raw []byte) (uint64, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errUndecodable, err)
	}

	bounds := img.Bounds()
	if bounds.Dx() < dhashWidth || bounds.Dy() < dhashHeight {
		// Too small to sample a 9x8 grid from without aliasing into nonsense. A thumbnail
		// that small is not a near-duplicate candidate worth reasoning about.
		return 0, fmt.Errorf("%w: %dx%d is smaller than the %dx%d sample grid",
			errUndecodable, bounds.Dx(), bounds.Dy(), dhashWidth, dhashHeight)
	}

	var grid [dhashHeight][dhashWidth]float64

	for gy := range dhashHeight {
		// Source rows covered by this grid row.
		y0 := bounds.Min.Y + bounds.Dy()*gy/dhashHeight
		y1 := bounds.Min.Y + bounds.Dy()*(gy+1)/dhashHeight

		for gx := range dhashWidth {
			x0 := bounds.Min.X + bounds.Dx()*gx/dhashWidth
			x1 := bounds.Min.X + bounds.Dx()*(gx+1)/dhashWidth

			var sum, n float64

			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, _ := img.At(x, y).RGBA()

					// Rec. 601 luma on the 16-bit values RGBA() returns.
					sum += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
					n++
				}
			}

			if n > 0 {
				grid[gy][gx] = sum / n
			}
		}
	}

	var out uint64

	bit := 0

	for gy := range dhashHeight {
		for gx := range dhashWidth - 1 {
			if grid[gy][gx] > grid[gy][gx+1] {
				out |= 1 << bit
			}

			bit++
		}
	}

	return out, nil
}

// Distance returns the Hamming distance between two perceptual hashes: how many of the 64
// comparisons the two images disagree about. Exported because docs/ARCHITECTURE.md#apply-manifests-and-undo's near-duplicate grouping is
// defined in terms of it.
func Distance(a, b uint64) int {
	diff := a ^ b

	count := 0

	for diff != 0 {
		diff &= diff - 1
		count++
	}

	return count
}
