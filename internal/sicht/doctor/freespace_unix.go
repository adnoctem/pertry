//go:build !windows

package doctor

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeSpace returns the bytes available to an unprivileged user on the filesystem holding path.
//
// Bavail rather than Bfree: the difference is the reserve only root can use, and reporting space
// sicht cannot actually write into would make the check lie in the one direction that matters.
func FreeSpace(path string) (int64, error) {
	var stat unix.Statfs_t

	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}

	// Both conversions are deliberate and the linter's objection is platform-local: Bavail and
	// Bsize have different widths and signedness across the platforms this !windows file covers,
	// so the conversion that looks redundant on linux/amd64 is load-bearing elsewhere.
	//nolint:gosec,unconvert // non-negative in practice; widths differ by platform.
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
