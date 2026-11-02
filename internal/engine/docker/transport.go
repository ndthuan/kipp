// Package docker speaks the Docker Engine API, the few calls kipp needs to
// run a build's container, over a unix socket, TCP with TLS, or ssh to the
// engine's socket on another machine. No Docker client library and no
// docker CLI is needed on either end.
package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// APIVersion is the Docker Engine API kipp speaks; every engine since
// Docker 27 answers it.
const APIVersion = "v1.47"

// DefaultSocket is the engine's socket on a host reached over ssh, unless
// the URL's socket says otherwise.
const DefaultSocket = "/var/run/docker.sock"

// Options are how an engine is reached beyond its URL.
type Options struct {
	// TLS material of a tcp:// engine, as file paths; all three are needed.
	TLSCA, TLSCert, TLSKey string
	// SSHKey is the private key for an ssh:// engine; empty tries the
	// usual identities under ~/.ssh.
	SSHKey string
	// KnownHosts is the file the host's key must be in; empty is
	// ~/.ssh/known_hosts. An unknown host is refused, never trusted.
	KnownHosts string
	// Timeout bounds the connection to the engine; default 15 s.
	Timeout time.Duration
}

// transport is how requests reach an engine, and what to close after.
type transport struct {
	base  string // the URL requests are made against
	rt    *http.Transport
	close func() error
}

// CheckURL refuses an engine URL kipp would not dial, without dialling
// it: one of another scheme, a password in it, an ssh URL with no user, a
// tcp one without the three TLS files.
func CheckURL(raw string, o Options) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("engine URL: %w", err)
	}
	if _, ok := u.User.Password(); ok {
		return nil, errors.New("an engine URL names no password: the ssh key does")
	}
	switch {
	case u.Scheme == "unix" && u.Path != "":
	case u.Scheme == "tcp" && (o.TLSCA == "" || o.TLSCert == "" || o.TLSKey == ""):
		return nil, errors.New("a tcp engine needs --tls-ca, --tls-cert and --tls-key")
	case u.Scheme == "tcp" && u.Host != "":
	case u.Scheme == "ssh" && u.User.Username() == "":
		return nil, errors.New("an ssh engine URL names its user: ssh://user@host")
	case u.Scheme == "ssh" && u.Host != "":
	default:
		return nil, fmt.Errorf("engine URL %q: unix:///path/to/docker.sock, tcp://host:port with TLS, or ssh://user@host[:port][?socket=/path]", raw)
	}
	return u, nil
}

// dial makes the transport for an engine's URL.
func dial(ctx context.Context, raw string, o Options) (*transport, error) {
	u, err := CheckURL(raw, o)
	if err != nil {
		return nil, err
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	switch u.Scheme {
	case "unix":
		return unixTransport(u.Path, o), nil
	case "tcp":
		return tcpTransport(u, o)
	}
	return sshTransport(ctx, u, o)
}

func unixTransport(socket string, o Options) *transport {
	d := net.Dialer{Timeout: o.Timeout}
	rt := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", socket)
	}}
	return &transport{base: "http://docker", rt: rt, close: func() error { rt.CloseIdleConnections(); return nil }}
}

// tcpTransport is TLS with a client certificate: the Docker API is root
// on the engine's machine, so a plain tcp:// is refused.
func tcpTransport(u *url.URL, o Options) (*transport, error) {
	ca, err := os.ReadFile(o.TLSCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no PEM certificate", o.TLSCA)
	}
	cert, err := tls.LoadX509KeyPair(o.TLSCert, o.TLSKey)
	if err != nil {
		return nil, err
	}
	rt := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		DialContext:     (&net.Dialer{Timeout: o.Timeout}).DialContext,
	}
	return &transport{base: "https://" + u.Host, rt: rt, close: func() error { rt.CloseIdleConnections(); return nil }}, nil
}

// sshTransport speaks HTTP through direct-streamlocal channels of one ssh
// connection to the engine's unix socket on that host. The connection is
// made again when it breaks: a laptop that slept comes back.
func sshTransport(ctx context.Context, u *url.URL, o Options) (*transport, error) {
	cfg, err := sshConfig(u, o)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "22")
	}
	socket := u.Query().Get("socket")
	if socket == "" {
		socket = DefaultSocket
	}
	s := &sshDialer{addr: addr, cfg: cfg, timeout: o.Timeout}
	if _, err := s.client(ctx); err != nil {
		return nil, err
	}
	rt := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return s.dialSocket(ctx, socket)
	}}
	return &transport{base: "http://docker", rt: rt, close: func() error { rt.CloseIdleConnections(); return s.close() }}, nil
}

func sshConfig(u *url.URL, o Options) (*ssh.ClientConfig, error) {
	user := u.User.Username()
	signers, err := sshSigners(o.SSHKey)
	if err != nil {
		return nil, err
	}
	hosts := o.KnownHosts
	if hosts == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		hosts = filepath.Join(home, ".ssh", "known_hosts")
	}
	check, err := knownhosts.New(hosts)
	if err != nil {
		return nil, fmt.Errorf("known hosts: %w", err)
	}
	return &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: knownOnly(check, hosts),
		Timeout:         o.Timeout,
	}, nil
}

// knownOnly is the known hosts check, saying for a host that is not in
// the file the fingerprint to compare before adding it.
func knownOnly(check ssh.HostKeyCallback, file string) ssh.HostKeyCallback {
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := check(host, remote, key)
		if ke, ok := errors.AsType[*knownhosts.KeyError](err); ok && len(ke.Want) == 0 {
			return fmt.Errorf("the host key of %s (%s) is not in %s: add it there once it is known to be the host's", host, ssh.FingerprintSHA256(key), file)
		}
		return err
	}
}

// sshSigners reads the key named, or the usual identities under ~/.ssh.
func sshSigners(key string) ([]ssh.Signer, error) {
	paths := []string{key}
	if key == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = nil
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			paths = append(paths, filepath.Join(home, ".ssh", name))
		}
	}
	var signers []ssh.Signer
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) && key == "" {
			continue
		}
		if err != nil {
			return nil, err
		}
		s, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (a key with a passphrase is not read)", p, err)
		}
		signers = append(signers, s)
	}
	if len(signers) == 0 {
		return nil, errors.New("no ssh key: name one with --ssh-key, or put one under ~/.ssh")
	}
	return signers, nil
}

// sshDialer holds the one ssh connection of an engine.
type sshDialer struct {
	addr    string
	cfg     *ssh.ClientConfig
	timeout time.Duration

	mu sync.Mutex
	c  *ssh.Client
}

func (s *sshDialer) client(ctx context.Context) (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		return s.c, nil
	}
	d := net.Dialer{Timeout: s.timeout}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	// The handshake ignores ctx: a host that takes the connection and
	// never speaks would hold the caller, and every probe after it.
	if s.timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	sc, chans, reqs, err := ssh.NewClientConn(conn, s.addr, s.cfg)
	if !stop() && err == nil {
		_ = sc.Close()
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	s.c = ssh.NewClient(sc, chans, reqs)
	go func(c *ssh.Client) {
		_ = c.Wait()
		s.mu.Lock()
		if s.c == c {
			s.c = nil
		}
		s.mu.Unlock()
	}(s.c)
	return s.c, nil
}

func (s *sshDialer) dialSocket(ctx context.Context, socket string) (net.Conn, error) {
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := c.DialContext(ctx, "unix", socket)
	if err != nil && strings.Contains(err.Error(), "administratively prohibited") {
		return nil, fmt.Errorf("the ssh server of %s refused to forward to %s: it needs AllowStreamLocalForwarding: %w", s.addr, socket, err)
	}
	return conn, err
}

func (s *sshDialer) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return nil
	}
	err := s.c.Close()
	s.c = nil
	return err
}
