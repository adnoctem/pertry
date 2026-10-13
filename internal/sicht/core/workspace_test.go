package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/adnoctem/pertry/internal/sicht/core"
)

func TestCreate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}

	for _, dir := range []string{w.Dir(), w.ThumbsPath(), w.ManifestsPath(), w.LogsPath(), w.PromptsPath()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)

			continue
		}

		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	// Create writes neither the config nor the database: those belong to the packages that
	// own their formats, and a caller must not assume init left a usable sicht.yaml behind.
	for _, path := range []string{w.ConfigPath(), w.DatabasePath()} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("Create() wrote %s, which it must leave to its owner", path)
		}
	}
}

// TestCreateIsIdempotent matters because `sicht init` is documented as safe to re-run.
func TestCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for i := range 3 {
		if _, err := core.Create(root); err != nil {
			t.Fatalf("Create() #%d error = %v, want nil", i+1, err)
		}
	}
}

func TestCreateRejectsAMissingRoot(t *testing.T) {
	t.Parallel()

	if _, err := core.Create(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Create() error = nil, want an error for a root that does not exist")
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if _, err := core.Open(root); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Open() on an uninitialised directory error = %v, want ErrNotFound", err)
	}

	if _, err := core.Create(root); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	w, err := core.Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}

	if w.Root() != root {
		t.Errorf("Root() = %q, want %q", w.Root(), root)
	}
}

// TestDiscoverWalksUp is what makes the tool usable from inside a large archive, which is where
// someone sorting images actually is (docs/ARCHITECTURE.md#workspace-and-identity).
func TestDiscoverWalksUp(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if _, err := core.Create(root); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w, err := core.Discover(deep)
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil", err)
	}

	if w.Root() != root {
		t.Errorf("Root() = %q, want the workspace root %q", w.Root(), root)
	}
}

func TestDiscoverFindsNothing(t *testing.T) {
	t.Parallel()

	// A temp directory with no workspace at or above it, short of the filesystem root.
	_, err := core.Discover(t.TempDir())
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Discover() error = %v, want ErrNotFound", err)
	}
}

// TestRelUsesForwardSlashes is load-bearing for cross-platform manifests: one written on Windows
// has to resolve under WSL against the same files, and a stored backslash would break an undo in
// a way nobody notices until they need it (docs/ARCHITECTURE.md#workspace-and-identity).
func TestRelUsesForwardSlashes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := w.Rel(filepath.Join(root, "Photos from 2023", "IMG_1234.PNG"))
	if err != nil {
		t.Fatalf("Rel() error = %v, want nil", err)
	}

	if want := "Photos from 2023/IMG_1234.PNG"; got != want {
		t.Errorf("Rel() = %q, want %q", got, want)
	}
}

// TestRelRefusesOutsideTheWorkspace stops a path escaping into the stored data, where it would
// later be joined back onto the root and point somewhere unexpected.
func TestRelRefusesOutsideTheWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Rel(filepath.Join(filepath.Dir(root), "elsewhere", "x.png")); err == nil {
		t.Error("Rel() error = nil, want an error for a path outside the workspace")
	}
}

func TestAbsRoundTrips(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := core.Create(root)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	original := filepath.Join(root, "sub dir", "image.jpg")

	rel, err := w.Rel(original)
	if err != nil {
		t.Fatalf("Rel() error = %v", err)
	}

	if got := w.Abs(rel); got != original {
		t.Errorf("Abs(Rel(%q)) = %q, want the original", original, got)
	}
}

func TestIsInternal(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		".sicht":                  true,
		".sicht/sicht.db":         true,
		".sicht/thumbs/ab/cd.jpg": true,
		"photos/img.png":          false,
		"":                        false,
		// A directory that merely starts with the same characters is not inside it.
		".sichtor/x.png": false,
	}

	for path, want := range tests {
		if got := core.IsInternal(path); got != want {
			t.Errorf("IsInternal(%q) = %v, want %v", path, got, want)
		}
	}
}
