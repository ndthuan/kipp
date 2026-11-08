//go:build darwin || linux

package diskfree

import (
	"math"
	"math/bits"
	"syscall"
)

// Free is how many bytes of the disk dir is on kipp may still write;
// false when that cannot be told.
func Free(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil || st.Bsize <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(st.Bavail, uint64(st.Bsize))
	if hi != 0 {
		return math.MaxUint64, true
	}
	return lo, true
}
