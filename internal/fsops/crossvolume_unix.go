//go:build !windows

package fsops

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isCrossVolume reports whether a rename failed because the two paths are on different
// filesystems, which is the one rename failure worth recovering from with a copy.
//
// EXDEV is the documented error. Checked with errors.Is against the syscall value rather than by
// matching a message, because the message is localised on some systems.
func isCrossVolume(err error) bool {
	return errors.Is(err, unix.EXDEV)
}
