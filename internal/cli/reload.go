package cli

import (
	"io"
	"net/http"
)

func cmdReload(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("reload [--url <url>] [--token <token>]", stderr)
	newClient := clientFlags(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 0 {
		fs.Usage()
		return 2
	}
	c, err := newClient()
	if err == nil {
		err = c.call(http.MethodPost, "/admin/reload", nil, nil)
	}
	if err != nil {
		return fail(stderr, "reload", err)
	}
	say(stdout, "reloaded")
	return 0
}
