//go:build integration

package meta_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/meta"
)

// TestExiftoolAgainstARealExport checks the flags against real files, which is the only way to
// know that -G1 groups the tags the rules in docs/ARCHITECTURE.md#rules name and that -n leaves them comparable.
func TestExiftoolAgainstARealExport(t *testing.T) {
	root := os.Getenv(archiveEnv)
	if root == "" {
		t.Skipf("set %s to a Google Takeout export directory to run this", archiveEnv)
	}

	var pngs, heics []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		switch strings.ToLower(filepath.Ext(path)) {
		case ".png":
			if len(pngs) < 60 {
				pngs = append(pngs, path)
			}
		case ".heic":
			if len(heics) < 10 {
				heics = append(heics, path)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(pngs) == 0 {
		t.Skip("no PNG files in the export")
	}

	reader := &meta.Exiftool{}

	all := append(append([]string{}, pngs...), heics...)

	tags, err := reader.Read(context.Background(), all)
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}

	if len(tags) != len(all) {
		t.Errorf("Read() returned %d results for %d files", len(tags), len(all))
	}

	// Every file must yield dimensions. docs/ARCHITECTURE.md#scan step 4 prefers exiftool for them, and a format
	// it cannot measure would mean falling back to decoding - which is impossible for HEIC.
	var noDims []string

	for path, tag := range tags {
		if _, _, ok := tag.Dimensions(); !ok {
			noDims = append(noDims, filepath.Base(path))
		}
	}

	if len(noDims) > 0 {
		t.Errorf("%d files yielded no dimensions: %v", len(noDims), noDims[:min(5, len(noDims))])
	}

	// The screenshot rule reads ExifIFD:UserComment, so -G1 must really produce that key and
	// not a bare "UserComment". Finding none at all would mean the rule can never fire.
	var screenshots int

	for _, tag := range tags {
		if v, ok := tag.String("ExifIFD:UserComment"); ok && v == "Screenshot" {
			screenshots++
		}
	}

	if screenshots == 0 {
		t.Error(`no file reported ExifIFD:UserComment == "Screenshot"; either -G1 groups it ` +
			`differently or the rule in docs/ARCHITECTURE.md#rules cannot fire`)
	}

	t.Logf("read %d files, %d tagged as screenshots", len(tags), screenshots)

	// The photo rule reads IFD0:Make and ExifIFD:FNumber. docs/ARCHITECTURE.md#apply-manifests-and-undo rests on every HEIC
	// satisfying it, so check that here rather than trusting the earlier shell measurement.
	for _, path := range heics {
		tag, ok := tags[path]
		if !ok {
			t.Errorf("no tags for %s", path)

			continue
		}

		if _, ok := tag.String("IFD0:Make"); !ok {
			t.Errorf("%s has no IFD0:Make, so the photo rule cannot decide it and a HEIC "+
				"would need a decoder after all", filepath.Base(path))
		}

		if _, ok := tag.Float("ExifIFD:FNumber"); !ok {
			t.Errorf("%s has no ExifIFD:FNumber", filepath.Base(path))
		}
	}
}

// TestExiftoolHandlesANonASCIIPath is why -charset filename=UTF8 is passed. Without it exiftool
// assumes the system code page on Windows and cannot open these at all.
func TestExiftoolHandlesANonASCIIPath(t *testing.T) {
	root := os.Getenv(archiveEnv)
	if root == "" {
		t.Skipf("set %s to a Google Takeout export directory to run this", archiveEnv)
	}

	var found []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(found) >= 3 {
			return err
		}

		name := d.Name()
		if len(name) != len([]rune(name)) && isImage(name) {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(found) == 0 {
		t.Skip("no non-ASCII image filenames in the export")
	}

	tags, err := (&meta.Exiftool{}).Read(context.Background(), found)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	for _, path := range found {
		if _, ok := tags[path]; !ok {
			t.Errorf("exiftool returned nothing for %q", path)
		}
	}

	t.Logf("read %d non-ASCII paths, e.g. %s", len(tags), filepath.Base(found[0]))
}

func isImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".heic", ".webp", ".gif":
		return true
	default:
		return false
	}
}
