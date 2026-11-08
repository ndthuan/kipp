//go:build !darwin && !linux

package diskfree

// Free cannot tell the free space here.
func Free(string) (uint64, bool) { return 0, false }
