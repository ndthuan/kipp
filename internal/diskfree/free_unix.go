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
	free, _, ok := Space(dir)
	return free, ok
}

// Space is Free and the size of the disk; false when they cannot be told.
func Space(dir string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil || st.Bsize <= 0 {
		return 0, 0, false
	}
	return mul(st.Bavail, uint64(st.Bsize)), mul(st.Blocks, uint64(st.Bsize)), true
}

func mul(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}

// SameDisk reports whether a and b are on the same file system; true when
// that cannot be told, which is the cautious answer for a caller adding up
// the room both need.
func SameDisk(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return true
	}
	return sa.Dev == sb.Dev
}
