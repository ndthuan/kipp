//go:build dockerreal

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"
)

// realEngine is the engine KIPP_TEST_DOCKER names, as `make docker` runs
// these tests: the same calls the fake answers, against a real engine.
func realEngine(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("KIPP_TEST_DOCKER")
	if url == "" {
		t.Skip("KIPP_TEST_DOCKER names no engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, url, Options{SSHKey: os.Getenv("KIPP_TEST_DOCKER_SSH_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// testImage is the image the real tests run: alpine, or the one
// KIPP_TEST_DOCKER_IMAGE names, for an engine that reaches Docker Hub only
// through a mirror.
func testImage() string {
	if im := os.Getenv("KIPP_TEST_DOCKER_IMAGE"); im != "" {
		return im
	}
	return "alpine:3.22"
}

func TestARealEngineRunsAContainerAndRemovesIt(t *testing.T) {
	c := realEngine(t)
	image := testImage()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var pulled bytes.Buffer
	if err := c.Pull(ctx, image, &pulled); err != nil {
		t.Fatal(err, pulled.String())
	}
	label := "kipp.test=" + rand.Text()
	k, v, _ := strings.Cut(label, "=")
	id, err := c.Create(ctx, CreateSpec{Image: image, Cmd: []string{"sh", "-c", "cat /kipp/hello; echo real"}, Labels: map[string]string{k: v}, WorkingDir: "/kipp/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Remove(context.Background(), id) })
	if err := c.PutFile(ctx, id, "/kipp/hello", 0o644, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if code, err := c.Wait(ctx, id); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	var out bytes.Buffer
	if err := c.Logs(ctx, id, &out); err != nil || out.String() != "hello\nreal\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	if err := c.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	left, err := c.List(ctx, label)
	if err != nil || len(left) != 0 {
		t.Fatal(left, err)
	}
}
