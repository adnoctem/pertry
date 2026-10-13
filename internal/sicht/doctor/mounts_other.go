//go:build !linux

package doctor

// Filesystem has no implementation off Linux.
//
// The check it feeds exists to catch a translated or network filesystem being used as a workspace,
// which on Windows means a mapped network drive. Reporting that needs GetDriveType and a different
// set of judgements about which types are slow, and there is no Windows machine to verify it
// against yet, so it reports nothing rather than guessing. See docs/ARCHITECTURE.md#doctor.
func Filesystem(string) (fstype, mountpoint string, ok bool) {
	return "", "", false
}
