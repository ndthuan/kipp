package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ByteSize is a number of bytes, written in a job file or on a command
// line as a whole number with or without a unit: 500, 64KiB, 1.5GB, 10 GiB.
// KB, MB, GB and TB are powers of 1000, KiB, MiB, GiB and TiB of 1024, as
// a disk and a file manager count them.
type ByteSize int64

// The sizes kipp's defaults and limits are written in.
const (
	KiB ByteSize = 1 << 10
	MiB ByteSize = 1 << 20
	GiB ByteSize = 1 << 30
	GB  ByteSize = 1000 * 1000 * 1000
)

var sizeUnits = map[string]float64{
	"": 1, "B": 1,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
}

// ParseSize reads a size such as 1GiB or 50GB. A size is above zero.
func ParseSize(s string) (ByteSize, error) {
	bad := fmt.Errorf("%q: a size such as 500MB, 1GiB or 1073741824 (bytes)", s)
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	unit, ok := sizeUnits[strings.ToUpper(strings.TrimSpace(s[i:]))]
	if err != nil || !ok || n <= 0 || math.IsInf(n, 0) {
		return 0, bad
	}
	v := n * unit
	if v >= math.MaxInt64 || v < 1 {
		return 0, bad
	}
	return ByteSize(v), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a size such as 500MB or 1GiB", n.Line)
	}
	v, err := ParseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// String writes the size in the unit it is the fewest whole ones of, so
// that what kipp says can be read back: 1GiB, 50GB.
func (b ByteSize) String() string {
	out := strconv.FormatInt(int64(b), 10)
	best := b
	for _, u := range []struct {
		name string
		size ByteSize
	}{{"KiB", KiB}, {"MiB", MiB}, {"GiB", GiB}, {"TiB", 1 << 40}, {"KB", 1000}, {"MB", 1000 * 1000}, {"GB", GB}, {"TB", 1000 * GB}} {
		if b > 0 && b%u.size == 0 && b/u.size < best {
			best, out = b/u.size, strconv.FormatInt(int64(b/u.size), 10)+u.name
		}
	}
	return out
}

// Set implements flag.Value, so that a flag takes a size as a job file does.
func (b *ByteSize) Set(s string) error {
	v, err := ParseSize(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// Readable is n bytes for people to read, in powers of 1024: "512 B",
// "1.3 GiB".
func Readable(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
