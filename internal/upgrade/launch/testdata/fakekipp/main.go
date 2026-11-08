// Command fakekipp stands in for kipp in the launcher's tests. It is built
// once per version, with -X main.ver; as "serve" it runs the launcher and
// then reports who ended up serving. Started by a launcher as the server
// of a pending version (KIPP_SUPERVISED=1) it behaves as FAKE_MODE says.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/launch"
)

var ver = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("kipp " + ver)
		return
	}
	data := os.Getenv("FAKE_DATA")
	launch.Run(slog.New(slog.NewTextHandler(os.Stderr, nil)), data, ver, os.Args[2:])
	if os.Getenv(launch.EnvSupervised) == "1" {
		supervised(data)
		return
	}
	exe, _ := os.Executable()
	fmt.Printf("RAN %s pid=%d exe=%s launched=%s bundled_exe=%s bundled_version=%s args=%v\n",
		ver, os.Getpid(), exe, os.Getenv(launch.EnvLaunched), os.Getenv(launch.EnvBundledExe),
		os.Getenv(launch.EnvBundledVersion), os.Args[2:])
}

// supervised is the server a launcher runs as a child.
//
//	exit      exits with 3 at once
//	hang      does nothing, and ends on SIGTERM
//	stubborn  does nothing, and ignores SIGTERM
//	migrate   holds the migrating file until <data>/fake-proceed exists
//	ok        answers the health check until SIGTERM, then exits with FAKE_EXIT
//
// ok and migrate also exit with 75 once <data>/fake-restart exists.
func supervised(data string) {
	fmt.Println("CHILD", ver, "supervised")
	switch os.Getenv("FAKE_MODE") {
	case "exit":
		os.Exit(3)
	case "hang":
		idle()
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		idle()
	case "migrate":
		_ = os.MkdirAll(filepath.Join(data, "upgrade"), 0o700)
		_ = os.WriteFile(filepath.Join(data, "upgrade", "migrating"), nil, 0o600)
		for !exists(filepath.Join(data, "fake-proceed")) {
			time.Sleep(10 * time.Millisecond)
		}
		_ = os.Remove(filepath.Join(data, "upgrade", "migrating"))
	}
	serveHealth(data)
}

func serveHealth(data string) {
	ln, err := net.Listen("tcp", listenArg())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakekipp:", err)
		os.Exit(4)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	go func() { _ = http.Serve(ln, mux) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for {
		select {
		case <-sig:
			code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
			os.Exit(code)
		case <-time.After(10 * time.Millisecond):
			if exists(filepath.Join(data, "fake-restart")) {
				os.Exit(launch.ExitRestart)
			}
		}
	}
}

func listenArg() string {
	for i, a := range os.Args {
		if a == "--listen" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return "127.0.0.1:8085"
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// idle never returns; a bare select would end in the runtime's deadlock
// check.
func idle() {
	for {
		time.Sleep(time.Hour)
	}
}
