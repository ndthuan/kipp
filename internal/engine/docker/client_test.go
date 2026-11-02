package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/engine/docker/dockertest"
)

func dialFake(t *testing.T, e *dockertest.Engine) *Client {
	t.Helper()
	c, err := Dial(context.Background(), e.URL(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTheEngineSaysWhatItIsOverItsSocket(t *testing.T) {
	c := dialFake(t, dockertest.New(t))
	v, err := c.Version(context.Background())
	if err != nil || v.Version != "29.0.0" || v.Os != "linux" || v.Arch != "arm64" {
		t.Fatal(v, err)
	}
}

func TestAnImageThereIsNotPulledAndOneMissingIsWithItsProgressInTheLog(t *testing.T) {
	e := dockertest.New(t, "alpine:3.22")
	c := dialFake(t, e)
	var out bytes.Buffer
	if err := c.Pull(context.Background(), "alpine:3.22", &out); err != nil || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if err := c.Pull(context.Background(), "golang", &out); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	if !strings.HasPrefix(log, "kipp: pulling golang\n") || strings.Count(log, "abc: Downloading") != 1 || !strings.Contains(log, "abc: Pull complete") {
		t.Fatalf("%q", log)
	}
	var pulled string
	for _, r := range e.Requests() {
		if r.Path == "/images/create" {
			pulled = r.Query
		}
	}
	if pulled != "fromImage=golang%3Alatest" {
		t.Fatalf("pulled %q: an image with no tag is pulled at latest, never every tag", pulled)
	}
}

func TestAnImageNamesItsTagOrDigestOrIsTakenAtLatest(t *testing.T) {
	for in, want := range map[string]string{
		"golang":                      "golang:latest",
		"golang:1.27":                 "golang:1.27",
		"registry:5000/team/app":      "registry:5000/team/app:latest",
		"registry:5000/team/app:v1":   "registry:5000/team/app:v1",
		"alpine@sha256:0123456789abc": "alpine@sha256:0123456789abc",
	} {
		if got := withTag(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A build's container is offered no published port, no mount of the
// host and no privilege: the create call carries none to begin with.
func TestAContainerIsCreatedWithNoPortsNoMountsAndNoPrivilege(t *testing.T) {
	e := dockertest.New(t)
	c := dialFake(t, e)
	id, err := c.Create(context.Background(), CreateSpec{Image: "golang:1.27", Cmd: []string{"/kipp/agent", "--url", "u"}, Env: []string{"A=1"}, Labels: map[string]string{"kipp.build": "7"}, WorkingDir: "/kipp/workspace"})
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	var body string
	for _, r := range e.Requests() {
		if r.Path == "/containers/create" {
			body = string(r.Body)
		}
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	host, _ := spec["HostConfig"].(map[string]any)
	for _, k := range []string{"Binds", "Mounts", "PortBindings", "Privileged", "CapAdd", "SecurityOpt", "Devices", "NetworkMode", "PidMode"} {
		if _, ok := host[k]; ok {
			t.Errorf("the create call sets HostConfig.%s", k)
		}
	}
	if _, ok := spec["ExposedPorts"]; ok || spec["WorkingDir"] != "/kipp/workspace" || spec["Image"] != "golang:1.27" {
		t.Fatalf("%s", body)
	}
}

func TestAFileIsPutIntoTheContainerWithItsModeAndTheDirectoriesAboveIt(t *testing.T) {
	e := dockertest.New(t)
	c := dialFake(t, e)
	id, err := c.Create(context.Background(), CreateSpec{Image: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutFile(context.Background(), id, "/kipp/agent", 0o755, []byte("binary")); err != nil {
		t.Fatal(err)
	}
	got := e.Containers()[id]
	mode, data, ok := got.File("/kipp/agent")
	if !ok || mode != 0o755 || string(data) != "binary" {
		t.Fatal(mode, string(data), ok)
	}
	if _, _, ok := got.File("/kipp"); !ok {
		t.Fatal("the directory above the file is not in the archive")
	}
}

func TestContainersAreListedByLabelAndRemovedEvenWhenGone(t *testing.T) {
	e := dockertest.New(t)
	c := dialFake(t, e)
	mine := e.Add(map[string]string{"kipp.server": "a", "kipp.build": "1"}, true)
	e.Add(map[string]string{"kipp.server": "b"}, false)
	got, err := c.List(context.Background(), "kipp.server=a")
	if err != nil || len(got) != 1 || got[0].ID != mine || !got[0].Running || got[0].Labels["kipp.build"] != "1" {
		t.Fatal(got, err)
	}
	ctx := context.Background()
	for range 2 {
		if err := c.Kill(ctx, mine); err != nil {
			t.Fatal(err)
		}
		if err := c.Remove(ctx, mine); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := e.Containers()[mine]; ok {
		t.Fatal("not removed")
	}
}

func TestAContainersOwnWordsAreReadFromItsLogs(t *testing.T) {
	e := dockertest.New(t)
	e.Logs = "kipp-agent: the server refused the agent's token\n"
	c := dialFake(t, e)
	id, _ := c.Create(context.Background(), CreateSpec{Image: "x"})
	var out bytes.Buffer
	if err := c.Logs(context.Background(), id, &out); err != nil || out.String() != e.Logs {
		t.Fatal(err, out.String())
	}
	if err := c.Kill(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if code, err := c.Wait(context.Background(), id); code != 137 || err != nil {
		t.Fatal(code, err)
	}
}

func TestAnEngineURLIsUnixTCPWithTLSOrSSH(t *testing.T) {
	for url, want := range map[string]string{
		"tcp://host:2376":            "a tcp engine needs --tls-ca, --tls-cert and --tls-key",
		"http://host:2375":           "unix:///path/to/docker.sock, tcp://host:port with TLS, or ssh://user@host",
		"unix://":                    "unix:///path/to/docker.sock",
		"ssh://host":                 "an ssh engine URL names its user",
		"ssh://me:secret@host":       "an engine URL names no password",
		"unix:///no/such/socket.ock": "",
	} {
		_, err := Dial(context.Background(), url, Options{})
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", url, err, want)
		}
	}
}

func TestAnEngineThatIsNotThereIsAnError(t *testing.T) {
	c, err := Dial(context.Background(), "unix:///no/such/docker.sock", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("a missing socket answered")
	}
}
