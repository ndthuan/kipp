// kipp is a job-first CI and scheduler in one binary. Everything is in internal/.
package main

import (
	"os"
	// The zone names a job file or --timezone gives are read on any host,
	// with a zoneinfo database of its own or not.
	_ "time/tzdata"

	"github.com/ndthuan/kipp/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
