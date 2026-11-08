// Command fakekipp stands in for kipp in the launcher's tests. It is built
// once per version, with -X main.ver; as "serve" it runs the launcher and
// then reports who ended up serving.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/ndthuan/kipp/internal/upgrade/launch"
)

var ver = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("kipp " + ver)
		return
	}
	launch.Run(slog.New(slog.NewTextHandler(os.Stderr, nil)), os.Getenv("FAKE_DATA"), ver, os.Args[2:])
	exe, _ := os.Executable()
	fmt.Printf("RAN %s pid=%d exe=%s launched=%s bundled_exe=%s bundled_version=%s args=%v\n",
		ver, os.Getpid(), exe, os.Getenv(launch.EnvLaunched), os.Getenv(launch.EnvBundledExe),
		os.Getenv(launch.EnvBundledVersion), os.Args[2:])
}
