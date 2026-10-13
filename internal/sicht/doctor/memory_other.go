//go:build !linux

package doctor

// HostMemory has no implementation off Linux.
//
// The check it feeds exists for a memory-capped WSL guest, where the ceiling is lower than the
// physical machine's and a model load can exhaust it. On Windows proper the relevant figure is
// the committed-memory limit, which needs a different API and a different judgement about what
// counts as tight; reporting nothing is better than reporting a number that means something else.
func HostMemory() (Memory, bool) { return Memory{}, false }
