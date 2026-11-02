//go:build !darwin && !linux

package cli

// freeDisk cannot tell the free space here.
func freeDisk(string) (uint64, bool) { return 0, false }
