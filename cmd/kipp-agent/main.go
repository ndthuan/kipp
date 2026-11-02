// kipp-agent is the agent kipp puts into a container it starts for a
// build: internal/agent alone, without the server's database or web, and
// built static for Linux so that any image with a shell runs it.
package main

import (
	"os"

	"github.com/ndthuan/kipp/internal/agent"
)

func main() {
	os.Exit(agent.Command(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
