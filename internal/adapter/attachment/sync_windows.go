//go:build windows

package attachment

// syncDirectory is a no-op: Windows cannot open directory handles for
// syncing, and NTFS metadata journaling owns entry durability there.
func syncDirectory(string) error { return nil }
