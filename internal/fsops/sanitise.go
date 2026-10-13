package fsops

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// reservedNames are the DOS device names Windows still refuses as filenames, with or without an
// extension: "CON.txt" is as invalid as "CON".
//
// A facet value becomes a directory name, and a model asked to name a platform could return any
// of these. On Linux they are ordinary names, so a path built on Linux would be unopenable on
// Windows - which matters because docs/ARCHITECTURE.md#workspace-and-identity requires a workspace to work from both.
var reservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// invalidChars are the characters Windows rejects in a filename. The path separators are included
// because a facet value must never introduce a directory level of its own.
const invalidChars = `<>:"/\|?*`

// maxSegmentBytes bounds one path segment.
//
// 255 is the limit on ext4, APFS and NTFS alike. Applied in bytes because that is what the
// filesystem counts, and truncation is rune-aware so it cannot split a character in half.
const maxSegmentBytes = 255

// SanitiseSegment makes one path segment safe on every platform sicht targets.
//
// Returns an error rather than a silently mangled name when nothing usable survives, because a
// file filed under a wrong-but-plausible directory is worse than a run that stops and says why.
func SanitiseSegment(s string) (string, error) {
	original := s

	var b strings.Builder

	for _, r := range s {
		switch {
		case strings.ContainsRune(invalidChars, r):
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			// Control characters are legal on Linux and rejected by Windows, and are
			// never what anyone meant.
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}

	s = b.String()

	// Windows silently strips trailing dots and spaces, so a directory created as "ios ."
	// would be opened as "ios" - two names for one directory, which is exactly the kind of
	// mismatch that makes an undo fail.
	s = strings.TrimRight(s, ". ")

	// Leading whitespace is legal and invisible, which makes it a trap in a manifest.
	s = strings.TrimLeft(s, " \t")

	if s == "" {
		return "", fmt.Errorf("path segment %q contains nothing usable as a filename", original)
	}

	if _, reserved := reservedNames[strings.ToUpper(stem(s))]; reserved {
		// Suffixed rather than rejected: "nul" is a plausible facet value, and a run that
		// fails on it would be more annoying than one that files it under "nul_".
		s += "_"
	}

	if len(s) > maxSegmentBytes {
		s = truncateBytes(s, maxSegmentBytes)

		// Truncation can re-expose a trailing dot or space.
		s = strings.TrimRight(s, ". ")

		if s == "" {
			return "", fmt.Errorf("path segment %q leaves nothing usable after truncation", original)
		}
	}

	return s, nil
}

// stem returns the part of a name before its final dot, which is what Windows compares against
// the reserved device names.
func stem(s string) string {
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		return s[:i]
	}

	return s
}

// truncateBytes shortens a string to at most n bytes without splitting a rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8Start(s[n]) {
		n--
	}

	return s[:n]
}

// utf8Start reports whether a byte begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// SanitiseRelative sanitises every segment of a forward-slashed relative path.
//
// Rejects anything that would escape the workspace: an absolute path, a "." or ".." segment, or an
// empty one. A target template is user- and model-supplied, so this is the boundary that keeps a
// facet value from writing outside the archive.
func SanitiseRelative(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty target path")
	}

	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", fmt.Errorf("target path %q is absolute; targets are relative to the workspace", p)
	}

	// A Windows drive letter would otherwise survive as a plausible-looking segment.
	if len(p) > 1 && p[1] == ':' && unicode.IsLetter(rune(p[0])) {
		return "", fmt.Errorf("target path %q names a drive; targets are relative to the workspace", p)
	}

	// Backslashes are normalised so that a template written with them still produces one
	// level per segment rather than one long filename.
	p = strings.ReplaceAll(p, `\`, "/")

	parts := strings.Split(p, "/")

	out := make([]string, 0, len(parts))

	for _, part := range parts {
		switch part {
		case "":
			// A doubled slash is a typo in a template, not a directory.
			continue
		case ".":
			continue
		case "..":
			return "", fmt.Errorf("target path %q contains \"..\", which would write outside the workspace", p)
		}

		clean, err := SanitiseSegment(part)
		if err != nil {
			return "", err
		}

		out = append(out, clean)
	}

	if len(out) == 0 {
		return "", fmt.Errorf("target path %q has no usable segments", p)
	}

	return strings.Join(out, "/"), nil
}
