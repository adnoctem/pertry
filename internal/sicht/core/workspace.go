// Package core is sicht's domain layer: the workspace, and later the pipeline orchestration and
// stage runner.
//
// It must not import internal/sicht/web or internal/sicht/cmd. The CLI and the web UI are both
// thin frontends over this package and never call each other (docs/ARCHITECTURE.md#repository-layout). There is
// no global state: a *Workspace is passed explicitly.
package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the directory, relative to the workspace root, holding all of sicht's own state.
const Dir = ".sicht"

// Names of the things inside Dir (docs/ARCHITECTURE.md#workspace-and-identity).
const (
	DatabaseName = "sicht.db"
	ConfigName   = "sicht.yaml"
	ThumbsDir    = "thumbs"
	ManifestsDir = "manifests"
	LogsDir      = "logs"
	PromptsDir   = "prompts"
)

// ErrNotFound is returned by Discover when no workspace exists at or above the starting point.
var ErrNotFound = errors.New("no sicht workspace found")

// Workspace is a root directory the user has pointed sicht at. All state lives under
// <root>/.sicht/, alongside the user's images in whatever structure they already have.
type Workspace struct {
	// root is absolute and cleaned. Kept private so that every path a caller gets back comes
	// from a method that cannot forget to join it.
	root string
}

// Open returns the workspace rooted at dir, which must already have been initialised.
func Open(dir string) (*Workspace, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", dir, err)
	}

	w := &Workspace{root: root}

	info, err := os.Stat(w.Dir())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w at %s\nhint: run `sicht init` there first", ErrNotFound, root)
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", w.Dir(), err)
	case !info.IsDir():
		return nil, fmt.Errorf("%s exists but is not a directory", w.Dir())
	}

	return w, nil
}

// Create initialises a workspace at dir, creating every directory sicht needs.
//
// Idempotent: running it again on an existing workspace creates what is missing and leaves the
// rest alone. It never writes the config or the database - those belong to the callers that own
// their formats - so a caller must not assume Create left a usable sicht.yaml behind.
func Create(dir string) (*Workspace, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", dir, err)
	}

	if info, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("the workspace root %s must exist: %w", root, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("the workspace root %s is not a directory", root)
	}

	w := &Workspace{root: root}

	for _, dir := range []string{
		w.Dir(),
		w.ThumbsPath(),
		w.ManifestsPath(),
		w.LogsPath(),
		w.PromptsPath(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	return w, nil
}

// Discover finds the workspace containing start by walking up the directory tree, the way git
// finds its repository root (docs/ARCHITECTURE.md#workspace-and-identity).
//
// Walking up rather than requiring --workspace everywhere is what makes the tool usable from
// inside a subdirectory of a large archive, which is where someone sorting images actually is.
func Discover(start string) (*Workspace, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", start, err)
	}

	for {
		if info, err := os.Stat(filepath.Join(dir, Dir)); err == nil && info.IsDir() {
			return &Workspace{root: dir}, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding one.
			return nil, fmt.Errorf("%w at or above %s\nhint: run `sicht init` in the directory "+
				"holding your images, or pass --workspace", ErrNotFound, start)
		}

		dir = parent
	}
}

// Root is the absolute workspace root.
func (w *Workspace) Root() string { return w.root }

// Dir is the absolute path of the .sicht directory.
func (w *Workspace) Dir() string { return filepath.Join(w.root, Dir) }

// DatabasePath, ConfigPath and the rest are the absolute paths of the things in docs/ARCHITECTURE.md#workspace-and-identity.
func (w *Workspace) DatabasePath() string  { return filepath.Join(w.Dir(), DatabaseName) }
func (w *Workspace) ConfigPath() string    { return filepath.Join(w.Dir(), ConfigName) }
func (w *Workspace) ThumbsPath() string    { return filepath.Join(w.Dir(), ThumbsDir) }
func (w *Workspace) ManifestsPath() string { return filepath.Join(w.Dir(), ManifestsDir) }
func (w *Workspace) LogsPath() string      { return filepath.Join(w.Dir(), LogsDir) }
func (w *Workspace) PromptsPath() string   { return filepath.Join(w.Dir(), PromptsDir) }

// Rel converts an absolute path into the workspace-relative, forward-slash form used in the
// database and in manifests (docs/ARCHITECTURE.md#workspace-and-identity).
//
// Forward slashes on every platform, deliberately: a manifest written on Windows has to be
// readable on WSL against the same files, and a backslash in a stored path would make that fail in
// a way nobody would notice until an undo refused to run.
func (w *Workspace) Rel(absolute string) (string, error) {
	rel, err := filepath.Rel(w.root, absolute)
	if err != nil {
		return "", fmt.Errorf("make %q relative to %s: %w", absolute, w.root, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the workspace %s", absolute, w.root)
	}

	return filepath.ToSlash(rel), nil
}

// Abs converts a stored workspace-relative path back to an OS-native absolute one. Paths are
// converted at the filesystem boundary and nowhere else.
func (w *Workspace) Abs(relative string) string {
	return filepath.Join(w.root, filepath.FromSlash(relative))
}

// IsInternal reports whether a workspace-relative path is inside .sicht, which is always excluded
// from scanning (docs/ARCHITECTURE.md#workspace-and-identity).
func IsInternal(relative string) bool {
	relative = filepath.ToSlash(relative)

	return relative == Dir || strings.HasPrefix(relative, Dir+"/")
}
