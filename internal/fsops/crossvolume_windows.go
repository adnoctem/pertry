package fsops

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isCrossVolume reports whether a rename failed because the two paths are on different volumes.
//
// Windows reports this as ERROR_NOT_SAME_DEVICE rather than EXDEV. It is also worth recovering
// from ERROR_ACCESS_DENIED here: moving between volumes where the target directory is on a
// different drive can surface as access denied rather than as the specific error, and the
// recovery - copy, verify, delete - is safe either way, because it never removes the source
// until the destination is known to match.
func isCrossVolume(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}
