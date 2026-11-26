// kipp is a job-first CI and scheduler in one binary. Everything is in internal/.
package main

import (
	"os"
	_ "time/tzdata"

	"github.com/ndthuan/kipp/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
