//go:build linux

package doctor

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// HostMemory reads /proc/meminfo.
func HostMemory() (Memory, bool) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return Memory{}, false
	}

	defer func() { _ = file.Close() }()

	var out Memory

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}

		// Values are "  16376964 kB".
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}

		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}

		kb := n * 1024

		switch key {
		case "MemTotal":
			out.Total = kb
		case "MemAvailable":
			out.Available = kb
		case "SwapTotal":
			out.SwapTotal = kb
		}
	}

	if out.Total == 0 {
		return Memory{}, false
	}

	return out, true
}
