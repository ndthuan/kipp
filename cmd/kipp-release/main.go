// kipp-release is the maintainer's tool for signing releases: keygen, sign
// and verify. It is not part of a release.
package main

import (
	"os"

	"github.com/ndthuan/kipp/internal/releasetool"
	"github.com/ndthuan/kipp/internal/update"
)

func main() {
	os.Exit(releasetool.Run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, update.ReleaseKey()))
}
