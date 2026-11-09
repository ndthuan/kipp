package cli

import (
	"io"

	"github.com/ndthuan/kipp/internal/config"
)

func cmdCheck(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("check <jobs-dir>", stderr)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	snap, err := config.Load(positional[0])
	if err != nil {
		say(stderr, "%v", err)
		return 1
	}
	for _, w := range snap.Warnings {
		say(stderr, "warning: %s", w)
	}
	say(stdout, "ok: %s", jobCount(len(snap.Names)))
	return 0
}
