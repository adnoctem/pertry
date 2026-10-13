// Package fsops moves and copies files safely across volumes (docs/ARCHITECTURE.md#apply-manifests-and-undo).
//
// Everything here is written on the assumption that the files are irreplaceable. A move verifies
// before it deletes, a cross-volume move is a copy-verify-delete rather than a rename, and nothing
// overwrites an existing file unless the caller says so explicitly.
package fsops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Mode is how a file is relocated.
type Mode string

const (
	// ModeMove relocates the file, leaving nothing behind.
	ModeMove Mode = "move"

	// ModeCopy duplicates it, leaving the source in place.
	ModeCopy Mode = "copy"
)

// Conflict is what to do when the target already exists.
type Conflict string

const (
	// ConflictSuffix appends " (2)", " (3)" and so on before the extension.
	ConflictSuffix Conflict = "suffix"

	// ConflictSkip leaves the file where it is.
	ConflictSkip Conflict = "skip"

	// ConflictFail stops the batch.
	ConflictFail Conflict = "fail"
)

// Outcome describes what happened to one file.
type Outcome struct {
	// From and To are the paths actually used. To differs from the requested target when a
	// conflict policy renamed it.
	From string
	To   string

	// Skipped is true when a conflict policy declined to act.
	Skipped bool

	// CrossVolume is true when a rename was impossible and the file was copied instead.
	// Worth reporting: it is far slower, and it is the only path where a verification
	// failure can leave the source in place.
	CrossVolume bool

	// Verified is true when the written file's content was confirmed to match the source.
	// Always true for a cross-volume move or any copy; a same-volume rename does not move
	// bytes, so there is nothing to verify and this stays false.
	Verified bool

	// Bytes is how many bytes were written, zero for a rename.
	Bytes int64
}

// ErrConflict is returned when the target exists and the policy is ConflictFail.
var ErrConflict = errors.New("target already exists")

// Relocate moves or copies src to dst according to mode and the conflict policy.
//
// The target's parent directory is created if needed. The source is never deleted until the
// destination is known to hold the same bytes.
func Relocate(src, dst string, mode Mode, conflict Conflict) (Outcome, error) {
	out := Outcome{From: src, To: dst}

	srcInfo, err := os.Lstat(src)
	if err != nil {
		return out, fmt.Errorf("stat source %s: %w", src, err)
	}

	// A symlink is not followed. Moving the link is defensible and copying what it points at
	// is defensible, and guessing between them silently is not; nothing in a Takeout export
	// produces one.
	if srcInfo.Mode()&fs.ModeSymlink != 0 {
		return out, fmt.Errorf("refusing to relocate the symbolic link %s", src)
	}

	if !srcInfo.Mode().IsRegular() {
		return out, fmt.Errorf("refusing to relocate %s: not a regular file", src)
	}

	// Moving a file onto itself is a no-op rather than an error, which matters because a plan
	// re-run over an already-applied batch would otherwise fail on every file.
	if same, err := sameFile(src, dst); err == nil && same {
		out.Skipped = true

		return out, nil
	}

	resolved, skip, err := resolveConflict(dst, conflict)
	if err != nil {
		return out, err
	}

	if skip {
		out.Skipped = true

		return out, nil
	}

	out.To = resolved

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return out, fmt.Errorf("create target directory for %s: %w", resolved, err)
	}

	if mode == ModeCopy {
		written, err := copyVerify(src, resolved, srcInfo)
		if err != nil {
			return out, err
		}

		out.Bytes, out.Verified = written, true

		return out, nil
	}

	// A rename is atomic and moves no bytes, so it is always preferred.
	if err := os.Rename(src, resolved); err == nil {
		return out, nil
	} else if !isCrossVolume(err) {
		return out, fmt.Errorf("move %s to %s: %w", src, resolved, err)
	}

	// Cross-volume: copy, verify, then delete the source. The order is the point - a crash
	// between the copy and the delete leaves two copies, which is recoverable, whereas the
	// other order can lose the file.
	out.CrossVolume = true

	written, err := copyVerify(src, resolved, srcInfo)
	if err != nil {
		return out, err
	}

	out.Bytes, out.Verified = written, true

	if err := os.Remove(src); err != nil {
		return out, fmt.Errorf("copied %s to %s but could not remove the source: %w",
			src, resolved, err)
	}

	return out, nil
}

// copyVerify copies src to dst via a temporary file in the target directory, fsyncs it, verifies
// the content hash, and only then renames it into place.
//
// Writing to a temporary name first means an interrupted copy never leaves a half-written file at
// the destination path, where a later run would mistake it for a completed one.
func copyVerify(src, dst string, srcInfo os.FileInfo) (int64, error) {
	source, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open source %s: %w", src, err)
	}

	defer func() { _ = source.Close() }()

	temp, err := os.CreateTemp(filepath.Dir(dst), ".sicht-copy-*")
	if err != nil {
		return 0, fmt.Errorf("create temporary file beside %s: %w", dst, err)
	}

	tempName := temp.Name()

	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}

	digest := sha256.New()

	written, err := io.Copy(io.MultiWriter(temp, digest), source)
	if err != nil {
		cleanup()

		return 0, fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}

	// fsync before the rename: without it the rename can be durable while the contents are
	// not, which on a crash yields a file of the right name and the wrong length.
	if err := temp.Sync(); err != nil {
		cleanup()

		return 0, fmt.Errorf("flush %s: %w", tempName, err)
	}

	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)

		return 0, fmt.Errorf("close %s: %w", tempName, err)
	}

	if written != srcInfo.Size() {
		_ = os.Remove(tempName)

		return 0, fmt.Errorf("copy %s: wrote %d bytes, source is %d", src, written, srcInfo.Size())
	}

	// Verify against the source rather than trusting the write. The copy's own hash was
	// computed from what was handed to the writer, so it is re-read from disk here; anything
	// less would not be a verification.
	wantHash, err := hashFile(src)
	if err != nil {
		_ = os.Remove(tempName)

		return 0, err
	}

	gotHash, err := hashFile(tempName)
	if err != nil {
		_ = os.Remove(tempName)

		return 0, err
	}

	if gotHash != wantHash {
		_ = os.Remove(tempName)

		return 0, fmt.Errorf("copy %s to %s: content hash differs after writing", src, dst)
	}

	if err := os.Chmod(tempName, srcInfo.Mode().Perm()); err != nil {
		// Not fatal: the bytes are right, which is what matters. A mode that did not carry
		// over is worth neither failing nor losing the copy over.
		_ = err
	}

	if err := os.Rename(tempName, dst); err != nil {
		_ = os.Remove(tempName)

		return 0, fmt.Errorf("move the verified copy into place at %s: %w", dst, err)
	}

	// Preserve the modification time, so that an incremental scan does not see every moved
	// file as changed and re-read the whole archive.
	if err := os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		_ = err
	}

	return written, nil
}

// Hash returns a file's SHA-256 as lowercase hex. Exported because `undo` verifies a file is
// unchanged before reversing an operation on it.
func Hash(path string) (string, error) { return hashFile(path) }

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for hashing: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	digest := sha256.New()

	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// resolveConflict applies the conflict policy, returning the path to use.
func resolveConflict(dst string, conflict Conflict) (path string, skip bool, err error) {
	if _, err := os.Lstat(dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return dst, false, nil
		}

		return "", false, fmt.Errorf("stat target %s: %w", dst, err)
	}

	switch conflict {
	case ConflictSkip:
		return "", true, nil

	case ConflictFail:
		return "", false, fmt.Errorf("%w: %s", ErrConflict, dst)

	case ConflictSuffix:
		suffixed, err := nextFreeName(dst)
		if err != nil {
			return "", false, err
		}

		return suffixed, false, nil

	default:
		return "", false, fmt.Errorf("unknown conflict policy %q", conflict)
	}
}

// maxSuffixAttempts bounds the search for a free name. A directory with ten thousand collisions
// for one name is a bug somewhere else, and looping forever would hide it.
const maxSuffixAttempts = 10000

// nextFreeName finds "name (2).ext", "name (3).ext" and so on.
func nextFreeName(dst string) (string, error) {
	dir := filepath.Dir(dst)
	base := filepath.Base(dst)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	for i := 2; i < maxSuffixAttempts; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))

		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not find a free name for %s after %d attempts", dst, maxSuffixAttempts)
}

// sameFile reports whether two paths refer to the same file on disk.
func sameFile(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, err
	}

	infoB, err := os.Stat(b)
	if err != nil {
		return false, err
	}

	return os.SameFile(infoA, infoB), nil
}
