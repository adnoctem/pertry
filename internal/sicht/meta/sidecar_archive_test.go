//go:build integration

package meta_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/meta"
)

// archiveEnv names the directory to verify against. The test skips when it is unset, so it is
// meaningful on a machine that has a Takeout export and silent everywhere else.
const archiveEnv = "SICHT_TEST_ARCHIVE"

// TestSidecarNamesAgainstARealExport is the check that makes docs/ARCHITECTURE.md#scan a measurement rather than a
// claim: it pairs every media file in a real export and requires that every sidecar be claimed by
// exactly one of them.
//
// "Every sidecar claimed" is the assertion that matters. A matcher that is merely permissive scores
// well on media files and leaves sidecars unaccounted for, which is how a wrong rule hides.
func TestSidecarNamesAgainstARealExport(t *testing.T) {
	root := os.Getenv(archiveEnv)
	if root == "" {
		t.Skipf("set %s to a Google Takeout export directory to run this", archiveEnv)
	}

	mediaExts := map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true,
		".webp": true, ".gif": true, ".mov": true, ".mp4": true, ".mpeg": true,
	}

	type dirIndex struct {
		sidecars map[string]bool
		media    []string
	}

	dirs := map[string]*dirIndex{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		dir, name := filepath.Dir(path), d.Name()

		idx := dirs[dir]
		if idx == nil {
			idx = &dirIndex{sidecars: map[string]bool{}}
			dirs[dir] = idx
		}

		switch ext := strings.ToLower(filepath.Ext(name)); {
		case ext == ".json":
			idx.sidecars[name] = true
		case mediaExts[ext]:
			idx.media = append(idx.media, name)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	var (
		media, matched, unmatched int
		claimed                   = map[string]bool{}
		unmatchedSample           []string
	)

	for dir, idx := range dirs {
		for _, name := range idx.media {
			media++

			var hit string

			for _, cand := range meta.SidecarNames(name) {
				if idx.sidecars[cand] {
					hit = cand

					break
				}
			}

			if hit == "" {
				unmatched++

				if len(unmatchedSample) < 10 {
					unmatchedSample = append(unmatchedSample, filepath.Join(filepath.Base(dir), name))
				}

				continue
			}

			matched++

			key := filepath.Join(dir, hit)
			if claimed[key] {
				t.Errorf("sidecar %q claimed by more than one media file", key)
			}

			claimed[key] = true
		}
	}

	var sidecars, orphans int

	var orphanSample []string

	for dir, idx := range dirs {
		for name := range idx.sidecars {
			sidecars++

			if !claimed[filepath.Join(dir, name)] {
				orphans++

				if len(orphanSample) < 10 {
					orphanSample = append(orphanSample, filepath.Join(filepath.Base(dir), name))
				}
			}
		}
	}

	t.Logf("media %d, matched %d, unmatched %d; sidecars %d, unclaimed %d",
		media, matched, unmatched, sidecars, orphans)

	if orphans != 0 {
		t.Errorf("%d sidecars went unclaimed, so the naming rule is incomplete: %v",
			orphans, orphanSample)
	}

	// Unmatched media are expected and legitimate - a Live Photo motion file has no sidecar of
	// its own - so this is a ceiling rather than zero. It exists to catch the rule regressing
	// into matching nothing.
	if media > 0 {
		if ratio := float64(unmatched) / float64(media); ratio > 0.05 {
			t.Errorf("%d of %d media files (%.1f%%) found no sidecar, which is more than the "+
				"Live Photo motion files can account for: %v",
				unmatched, media, ratio*100, unmatchedSample)
		}
	}
}
