//go:build !darwin && !linux

package diskfree

// Free cannot tell the free space here.
func Free(string) (uint64, bool) { return 0, false }

// Space cannot tell the free space or the size here.
func Space(string) (free, total uint64, ok bool) { return 0, 0, false }

// SameDisk cannot tell here, and says so cautiously.
func SameDisk(_, _ string) bool { return true }
