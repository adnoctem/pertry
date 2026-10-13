package doctor

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Filesystem returns the filesystem type and mount point the given path sits on.
//
// Read from /proc/mounts rather than inferred from the path, because the path tells you nothing
// reliable: WSL's automount root is configurable (`automount.root` in .wslconfig), so a Windows
// drive is just as likely to be at /win/e as at /mnt/c, and checking for a "/mnt/" prefix misses
// the former entirely. Asking the kernel also generalises for free to an NFS or SMB mount on an
// ordinary Linux box, which is slow for the same reason.
func Filesystem(path string) (fstype, mountpoint string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}

	// Resolve symlinks so a link into a mounted drive is not mistaken for a local path. Best
	// effort: a path that does not exist yet is still worth reporting on.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	file, err := os.Open("/proc/mounts")
	if err != nil {
		return "", "", false
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}

		point, kind := unescapeMount(fields[1]), fields[2]

		if !under(abs, point) {
			continue
		}

		// The longest matching mount point wins, since mounts nest.
		if len(point) >= len(mountpoint) {
			mountpoint, fstype, ok = point, kind, true
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", false
	}

	return fstype, mountpoint, ok
}

// under reports whether path is at or below mountpoint, comparing whole path elements so that
// /winter is not treated as being under /win.
func under(path, mountpoint string) bool {
	if path == mountpoint {
		return true
	}

	if mountpoint == "/" {
		return true
	}

	return strings.HasPrefix(path, strings.TrimSuffix(mountpoint, "/")+"/")
}

// unescapeMount decodes the octal escapes /proc/mounts uses for characters that would otherwise
// break its field separation. A space becomes \040, which matters here: the directory this was
// written against is "/win/e/Bilder/2026.03.29 - Google Fotos Sicherung".
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))

				i += 3

				continue
			}
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
