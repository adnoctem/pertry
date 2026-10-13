//go:build windows

package doctor

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FreeSpace returns the bytes available to the caller on the volume holding path.
//
// GetDiskFreeSpaceEx's first output is the free space available to the calling user, which is the
// one that matters: on a volume with a quota it differs from the volume's total free space.
func FreeSpace(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("convert %q: %w", path, err)
	}

	var freeToCaller, total, free uint64

	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &free); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %q: %w", path, err)
	}

	//nolint:gosec // a byte count that fits a disk fits an int64.
	return int64(freeToCaller), nil
}
