package apply

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adnoctem/pertry/internal/sicht/core"
	"github.com/adnoctem/pertry/internal/sicht/meta"
)

// extensionPreference decides which indexed file claims a shared companion (docs/ARCHITECTURE.md#apply-manifests-and-undo).
//
// Four stems in the reference archive are indexed twice over - IMG_0007 exists as both .heic and
// .jpeg - and without a tie-break both would try to move the same .mov. The order is fixed and
// the fallback is the filename, so the choice never depends on directory iteration order.
//
// HEIC first because it is the original where Google produced both; the JPEG beside it is the
// transcode.
var extensionPreference = []string{"heic", "heif", "jpg", "jpeg", "png", "webp", "gif"}

// preferenceOf returns an extension's position, with unknown extensions sorting last.
func preferenceOf(ext string) int {
	if i := slices.Index(extensionPreference, strings.ToLower(ext)); i >= 0 {
		return i
	}

	return len(extensionPreference)
}

// Companion is a file that belongs to an indexed file but was never indexed itself.
type Companion struct {
	// Path is workspace-relative, forward-slashed.
	Path string

	// Owner is the workspace-relative path of the indexed file it belongs to.
	Owner string

	// Kind describes what it is, for the report: "sidecar" or "motion".
	Kind string
}

// companionIndex resolves companions for a set of indexed files.
type companionIndex struct {
	// byOwner maps an indexed file's path to the companions it claims.
	byOwner map[string][]Companion

	// orphans are companions whose owner is not indexed, so nothing will move them.
	orphans []Companion
}

// indexedPaths is the set of workspace-relative paths sicht has indexed.
type indexedPaths map[string]struct{}

// buildCompanionIndex finds every companion of every indexed file.
//
// Directories are listed once each and cached, because on a translated filesystem a directory
// read is expensive and an archive has few directories relative to its files.
func buildCompanionIndex(ws *core.Workspace, indexed indexedPaths) (*companionIndex, error) {
	idx := &companionIndex{byOwner: map[string][]Companion{}}

	// Group indexed files by directory and stem, so that a shared companion's claimant can be
	// chosen deterministically.
	type stemKey struct{ dir, stem string }

	claimants := map[stemKey][]string{}
	dirs := map[string]struct{}{}

	for relPath := range indexed {
		dir, file := path.Split(relPath)
		dir = strings.TrimSuffix(dir, "/")
		stem := strings.TrimSuffix(file, path.Ext(file))

		claimants[stemKey{dir, stem}] = append(claimants[stemKey{dir, stem}], relPath)
		dirs[dir] = struct{}{}
	}

	// One listing per directory.
	listings := map[string][]string{}

	for dir := range dirs {
		entries, err := os.ReadDir(ws.Abs(dir))
		if err != nil {
			// A directory that cannot be read means its companions cannot be found. That
			// is worth reporting and not worth failing the batch over, since the indexed
			// files themselves are still movable.
			continue
		}

		names := make([]string, 0, len(entries))

		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			names = append(names, e.Name())
		}

		slices.Sort(names)

		listings[dir] = names
	}

	claimed := map[string]struct{}{}

	// Sorted keys, so that claiming is deterministic across runs.
	keys := make([]stemKey, 0, len(claimants))
	for k := range claimants {
		keys = append(keys, k)
	}

	slices.SortFunc(keys, func(a, b stemKey) int {
		if c := strings.Compare(a.dir, b.dir); c != 0 {
			return c
		}

		return strings.Compare(a.stem, b.stem)
	})

	for _, key := range keys {
		owners := claimants[key]

		// The claimant: first by extension preference, then by filename.
		slices.SortFunc(owners, func(a, b string) int {
			pa := preferenceOf(strings.TrimPrefix(path.Ext(a), "."))
			pb := preferenceOf(strings.TrimPrefix(path.Ext(b), "."))

			if pa != pb {
				return pa - pb
			}

			return strings.Compare(a, b)
		})

		owner := owners[0]
		ownerFile := path.Base(owner)

		for _, name := range listings[key.dir] {
			candidate := joinRel(key.dir, name)

			// An indexed file is moved by its own plan entry, never as somebody's
			// companion.
			if _, isIndexed := indexed[candidate]; isIndexed {
				continue
			}

			if _, taken := claimed[candidate]; taken {
				continue
			}

			kind, ok := companionKind(ownerFile, name)
			if !ok {
				continue
			}

			claimed[candidate] = struct{}{}

			idx.byOwner[owner] = append(idx.byOwner[owner], Companion{
				Path: candidate, Owner: owner, Kind: kind,
			})
		}
	}

	// Anything that looks like a companion but whose owner was never indexed. Reported, not
	// moved: guessing at an owner is how files get lost.
	for dir, names := range listings {
		for _, name := range names {
			candidate := joinRel(dir, name)

			if _, isIndexed := indexed[candidate]; isIndexed {
				continue
			}

			if _, taken := claimed[candidate]; taken {
				continue
			}

			if strings.EqualFold(path.Ext(name), ".json") {
				idx.orphans = append(idx.orphans, Companion{Path: candidate, Kind: "sidecar"})
			}
		}
	}

	slices.SortFunc(idx.orphans, func(a, b Companion) int {
		return strings.Compare(a.Path, b.Path)
	})

	return idx, nil
}

// companionKind reports whether name is a companion of ownerFile, and what sort.
func companionKind(ownerFile, name string) (string, bool) {
	// A Takeout sidecar, matched by the measured rule rather than by guesswork (docs/ARCHITECTURE.md#scan).
	for _, candidate := range meta.SidecarNames(ownerFile) {
		if name == candidate {
			return "sidecar", true
		}
	}

	// A Live Photo motion file: the same stem, a different extension, and not something
	// sicht indexes. 201 stills in the reference archive have one.
	ownerStem := strings.TrimSuffix(ownerFile, path.Ext(ownerFile))
	nameStem := strings.TrimSuffix(name, path.Ext(name))

	if ownerStem != "" && ownerStem == nameStem && name != ownerFile {
		return "motion", true
	}

	return "", false
}

// joinRel joins a workspace-relative directory and a filename with forward slashes.
func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}

	return dir + "/" + name
}

// companionTarget returns where a companion goes when its owner moves.
//
// It follows its owner into the same directory and keeps its own filename, which is the only
// choice that preserves the pairing: a Takeout sidecar is found by its name relative to the media
// file, and a Live Photo motion file by its stem.
func companionTarget(ownerTarget, companionPath string) string {
	return path.Join(path.Dir(ownerTarget), path.Base(companionPath))
}

// localPath converts a workspace-relative path to an absolute one for the current platform.
func localPath(ws *core.Workspace, rel string) string {
	return filepath.Join(ws.Root(), filepath.FromSlash(rel))
}
