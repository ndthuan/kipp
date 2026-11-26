// Command openapibundle writes api/openapi.gen.yaml, the single file the
// split sources under api/ make. Run it as `make openapi` from the
// repository root; the gate fails when the committed file differs.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ndthuan/kipp/internal/apispec"
)

func main() {
	dir := flag.String("dir", "api", "the directory holding openapi.yaml")
	flag.Parse()
	if err := apispec.Write(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "openapibundle:", err)
		os.Exit(1)
	}
}
