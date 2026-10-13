package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultBatchSize is how many files are passed to one exiftool invocation.
//
// Process startup is the dominant cost for a single file and is amortised to nothing by a few
// hundred, while the memory held is one JSON document for the batch. 200 keeps that document
// small enough to decode without streaming.
const DefaultBatchSize = 200

// Tags is one file's metadata, keyed by exiftool's group-qualified tag name such as "IFD0:Make".
//
// Group prefixes are mandatory rather than cosmetic: "Make" alone is ambiguous between IFD0 and
// the QuickTime track of a HEIC, and a rule that matched the wrong one would be wrong silently.
type Tags map[string]any

// Reader reads metadata for a set of files.
//
// An interface because docs/CONTRIBUTING.md puts extraction behind one: the exiftool dependency is the only external
// binary in the critical path, and tests must not need it installed.
type Reader interface {
	// Read returns one Tags per path, keyed by the path as given. A file exiftool could not
	// read is absent from the result rather than an error, so one unreadable file does not
	// fail a batch of two hundred.
	Read(ctx context.Context, paths []string) (map[string]Tags, error)
}

// Exiftool is a Reader backed by the exiftool binary.
type Exiftool struct {
	// Binary is the executable to run. Empty means look up "exiftool" on PATH.
	Binary string

	// BatchSize overrides DefaultBatchSize.
	BatchSize int

	// TempDir is where argument files are written. Empty means the OS temp directory.
	TempDir string
}

// sourceFileKey is the key exiftool uses for the path it read.
const sourceFileKey = "SourceFile"

// Read implements Reader.
func (e *Exiftool) Read(ctx context.Context, paths []string) (map[string]Tags, error) {
	out := make(map[string]Tags, len(paths))

	if len(paths) == 0 {
		return out, nil
	}

	binary := e.Binary
	if binary == "" {
		binary = "exiftool"
	}

	size := e.BatchSize
	if size <= 0 {
		size = DefaultBatchSize
	}

	for start := 0; start < len(paths); start += size {
		end := min(start+size, len(paths))

		batch, err := e.readBatch(ctx, binary, paths[start:end])
		if err != nil {
			return out, err
		}

		for path, tags := range batch {
			out[path] = tags
		}
	}

	return out, nil
}

func (e *Exiftool) readBatch(ctx context.Context, binary string, paths []string) (map[string]Tags, error) {
	// Paths go in an argument file rather than on the command line. Windows caps a command
	// line at about 32 KiB, which a couple of hundred absolute paths can exceed, and the
	// failure mode would be a batch that works on Linux and not on Windows.
	argfile, err := e.writeArgFile(paths)
	if err != nil {
		return nil, err
	}

	defer func() { _ = os.Remove(argfile) }()

	args := []string{
		// -json for machine-readable output, -G1 for group-qualified tag names.
		"-json", "-G1",
		// -n disables exiftool's print conversion, so FNumber is 1.8 rather than "1.8"
		// and Orientation is 6 rather than "Rotate 90 CW". Rules compare values, so the
		// raw form is the only stable one.
		"-n",
		// Filenames are UTF-8 on every platform sicht targets; without this exiftool
		// assumes the system code page on Windows and cannot open a path with an umlaut
		// in it. The reference archive has several.
		"-charset", "filename=UTF8",
		// Suppress the "N image files read" trailer, which is not JSON.
		"-q",
		"-@", argfile,
	}

	cmd := exec.CommandContext(ctx, binary, args...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// exiftool exits 1 when it could not read *some* input but still writes valid JSON for
	// the rest, so the exit code alone is not a verdict. Parse first and only report the
	// error if nothing came back.
	tags, parseErr := parseExiftoolJSON(stdout.Bytes())

	switch {
	case parseErr != nil && runErr != nil:
		return nil, fmt.Errorf("exiftool: %w (stderr: %s)", runErr, trimStderr(stderr.String()))
	case parseErr != nil:
		return nil, fmt.Errorf("exiftool returned unparsable output: %w", parseErr)
	}

	return tags, nil
}

func (e *Exiftool) writeArgFile(paths []string) (string, error) {
	file, err := os.CreateTemp(e.TempDir, "sicht-exiftool-*.args")
	if err != nil {
		return "", fmt.Errorf("create exiftool argument file: %w", err)
	}

	name := file.Name()

	var buf bytes.Buffer

	for _, p := range paths {
		buf.WriteString(p)
		buf.WriteByte('\n')
	}

	if _, err := file.Write(buf.Bytes()); err != nil {
		_ = file.Close()
		_ = os.Remove(name)

		return "", fmt.Errorf("write exiftool argument file: %w", err)
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(name)

		return "", fmt.Errorf("close exiftool argument file: %w", err)
	}

	return name, nil
}

func parseExiftoolJSON(raw []byte) (map[string]Tags, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("no output")
	}

	var records []map[string]any

	if err := json.Unmarshal(trimmed, &records); err != nil {
		return nil, fmt.Errorf("decode exiftool JSON: %w", err)
	}

	out := make(map[string]Tags, len(records))

	for _, rec := range records {
		source, _ := rec[sourceFileKey].(string)
		if source == "" {
			continue
		}

		tags := make(Tags, len(rec))

		for k, v := range rec {
			if k == sourceFileKey {
				continue
			}

			tags[k] = v
		}

		out[source] = tags
	}

	return out, nil
}

// trimStderr shortens exiftool's diagnostics to something that fits an error message.
func trimStderr(s string) string {
	s = strings.TrimSpace(s)

	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}

	return strings.ReplaceAll(s, "\n", "; ")
}

// Get returns a tag by its group-qualified name.
func (t Tags) Get(name string) (any, bool) {
	v, ok := t[name]

	return v, ok
}

// String returns a tag as a string. Numbers are rendered without a trailing ".0" so that a rule
// comparing against "6" matches an Orientation exiftool reported as the number 6.
func (t Tags) String(name string) (string, bool) {
	v, ok := t[name]
	if !ok {
		return "", false
	}

	switch typed := v.(type) {
	case string:
		return typed, true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	case nil:
		return "", false
	default:
		return fmt.Sprint(typed), true
	}
}

// Float returns a tag as a number, accepting the string form too: exiftool emits a number for
// most numeric tags under -n, but a few stay strings, and a caller should not have to care which.
func (t Tags) Float(name string) (float64, bool) {
	v, ok := t[name]
	if !ok {
		return 0, false
	}

	switch typed := v.(type) {
	case float64:
		return typed, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}

		return f, true
	default:
		return 0, false
	}
}

// Int returns a tag as an integer.
func (t Tags) Int(name string) (int, bool) {
	f, ok := t.Float(name)
	if !ok {
		return 0, false
	}

	return int(f), true
}

// Dimensions returns the pixel dimensions, preferring the image's real size over the EXIF
// record of it.
//
// ImageWidth is what the file is; ExifImageWidth is what the camera wrote down, and the two differ
// on an edited file. docs/ARCHITECTURE.md#scan step 4 prefers exiftool for dimensions, and this is that preference made
// explicit about which of exiftool's several answers to take.
func (t Tags) Dimensions() (width, height int, ok bool) {
	for _, pair := range [][2]string{
		{"File:ImageWidth", "File:ImageHeight"},
		{"ImageWidth", "ImageHeight"},
		{"ExifIFD:ExifImageWidth", "ExifIFD:ExifImageHeight"},
		{"ExifImageWidth", "ExifImageHeight"},
		{"PNG:ImageWidth", "PNG:ImageHeight"},
		{"QuickTime:ImageWidth", "QuickTime:ImageHeight"},
	} {
		w, okW := t.Int(pair[0])
		h, okH := t.Int(pair[1])

		if okW && okH && w > 0 && h > 0 {
			return w, h, true
		}
	}

	return 0, 0, false
}

// Ungrouped returns a tag by its bare name, ignoring the group prefix.
//
// For a caller that knows which tag it wants but not which group this file puts it in - a HEIC
// records Make under IFD0 while some formats use a different group. Deterministic when several
// groups hold the same tag: the lexicographically first group wins, so the answer does not depend
// on map iteration order.
func (t Tags) Ungrouped(name string) (any, bool) {
	if v, ok := t[name]; ok {
		return v, true
	}

	var (
		bestKey string
		bestVal any
		found   bool
	)

	suffix := ":" + name

	for k, v := range t {
		if !strings.HasSuffix(k, suffix) {
			continue
		}

		if !found || k < bestKey {
			bestKey, bestVal, found = k, v, true
		}
	}

	return bestVal, found
}

// FileName returns the base filename exiftool reported, falling back to the key it was stored
// under. Used for sidecar pairing, where the name matters and the directory does not.
func FileName(path string, tags Tags) string {
	if name, ok := tags.String("System:FileName"); ok && name != "" {
		return name
	}

	if name, ok := tags.String("FileName"); ok && name != "" {
		return name
	}

	return filepath.Base(path)
}
