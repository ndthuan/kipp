package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ndthuan/kipp/internal/engine/docker/dockertest"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// sshd is an ssh server that lets one key in and forwards its
// direct-streamlocal channels to the unix socket they name, as OpenSSH
// does; forwarded counts them.
type sshd struct {
	addr      string
	hostKey   ssh.PublicKey
	forwarded atomic.Int32
}

func newSSHD(t *testing.T, client ssh.PublicKey) *sshd {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) == string(client.Marshal()) {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &sshd{addr: ln.Addr().String(), hostKey: host.PublicKey()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, cfg)
		}
	}()
	return s
}

func (s *sshd) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "direct-streamlocal@openssh.com" {
			_ = nc.Reject(ssh.UnknownChannelType, "only streamlocal")
			continue
		}
		var target struct {
			Path      string
			Reserved0 string
			Reserved1 uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &target); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		up, err := net.Dial("unix", target.Path)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			_ = up.Close()
			continue
		}
		s.forwarded.Add(1)
		go ssh.DiscardRequests(creqs)
		go func() { _, _ = io.Copy(ch, up); _ = ch.CloseWrite() }()
		go func() { _, _ = io.Copy(up, ch); _ = up.Close() }()
	}
}

// clientKey is a private key file for the client, and its public half.
func clientKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, sp
}

func knownHostsFile(t *testing.T, addr string, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnEngineOverSSHIsReachedThroughItsSocketOnThatHost(t *testing.T) {
	e := dockertest.New(t)
	keyFile, pub := clientKey(t)
	srv := newSSHD(t, pub)
	known := knownHostsFile(t, srv.addr, srv.hostKey)
	c, err := Dial(context.Background(), "ssh://builder@"+srv.addr+"?socket="+e.Socket, Options{SSHKey: keyFile, KnownHosts: known})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	v, err := c.Version(context.Background())
	if err != nil || v.Os != "linux" {
		t.Fatal(v, err)
	}
	if srv.forwarded.Load() == 0 {
		t.Fatal("the call did not go through the ssh server")
	}
}

func TestAnSSHHostNotInKnownHostsIsRefusedWithItsFingerprint(t *testing.T) {
	keyFile, pub := clientKey(t)
	srv := newSSHD(t, pub)
	_, wrongPub := clientKey(t)
	known := knownHostsFile(t, "127.0.0.1:1", wrongPub)
	_, err := Dial(context.Background(), "ssh://builder@"+srv.addr, Options{SSHKey: keyFile, KnownHosts: known})
	if err == nil || !strings.Contains(err.Error(), ssh.FingerprintSHA256(srv.hostKey)) || !strings.Contains(err.Error(), "is not in "+known) {
		t.Fatalf("%v", err)
	}
}

func TestAnSSHHostWithAnotherKeyIsRefused(t *testing.T) {
	keyFile, pub := clientKey(t)
	srv := newSSHD(t, pub)
	_, wrongPub := clientKey(t)
	known := knownHostsFile(t, srv.addr, wrongPub)
	if _, err := Dial(context.Background(), "ssh://builder@"+srv.addr, Options{SSHKey: keyFile, KnownHosts: known}); err == nil {
		t.Fatal("a host whose key changed was let in")
	}
}

func TestAKeyTheSSHServerDoesNotKnowIsRefused(t *testing.T) {
	_, pub := clientKey(t)
	srv := newSSHD(t, pub)
	otherKey, _ := clientKey(t)
	known := knownHostsFile(t, srv.addr, srv.hostKey)
	if _, err := Dial(context.Background(), "ssh://builder@"+srv.addr, Options{SSHKey: otherKey, KnownHosts: known}); err == nil {
		t.Fatal("a key the server does not know was let in")
	}
}

// A host that takes the connection and never speaks ssh is given up on
// when the call's context ends, rather than holding the caller forever.
func TestAnSSHHostThatNeverAnswersIsGivenUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	keyFile, pub := clientKey(t)
	known := knownHostsFile(t, ln.Addr().String(), pub)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Dial(ctx, "ssh://builder@"+ln.Addr().String(), Options{SSHKey: keyFile, KnownHosts: known})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a host that never answered was reached")
		}
	case <-time.After(waitfor.Safety):
		t.Fatal("the ssh handshake held the caller after its context ended")
	}
}
