// kipp-runner is the runner kipp puts into a container it starts for a
// build: internal/runner alone, without the server's database or web, and
// built static for Linux so that any image with a shell runs it.
package main

import (
	"os"

	"github.com/ndthuan/kipp/internal/runnerd"
)

func main() {
	os.Exit(runnerd.Command(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
