package webhook

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

type readers struct {
	all     chan struct{}
	each    int
	wait    time.Duration
	client  func(*http.Request) string
	mu      sync.Mutex
	clients map[string]*clientReaders
}

type clientReaders struct {
	slots chan struct{}
	users int
}

func newReaders(o Options) *readers {
	client := o.Client
	if client == nil {
		client = peerHost
	}
	return &readers{
		all: make(chan struct{}, o.Readers), each: max(1, o.Readers/2), wait: o.Wait,
		client: client, clients: map[string]*clientReaders{},
	}
}

func peerHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *readers) enter(who string) *clientReaders {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[who]
	if c == nil {
		c = &clientReaders{slots: make(chan struct{}, s.each)}
		s.clients[who] = c
	}
	c.users++
	return c
}

func (s *readers) leave(who string, c *clientReaders) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.users--; c.users == 0 {
		delete(s.clients, who)
	}
}

func (s *readers) acquire(r *http.Request) (release func(), ok bool) {
	who := s.client(r)
	c := s.enter(who)
	ctx, cancel := context.WithTimeout(r.Context(), s.wait)
	defer cancel()
	if !take(ctx, c.slots) {
		s.leave(who, c)
		return nil, false
	}
	if !take(ctx, s.all) {
		<-c.slots
		s.leave(who, c)
		return nil, false
	}
	return func() {
		<-s.all
		<-c.slots
		s.leave(who, c)
	}, true
}

func take(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
