package webhook

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

// The routes a kept delivery came in by.
const (
	routeJob = "job"
	routeApp = "app"
)

// lateAfter is how old a kept delivery is when it is worth a warning: a
// quiet lasts at most the two hours a safe restart waits, and a little
// more for the restart itself. It is handled all the same, since a deploy
// late is better than none.
const lateAfter = 2*time.Hour + 10*time.Minute

// kept keeps a verified delivery that arrives while the server is quiet and
// answers 202, and reports true. GitHub never sends a webhook again by
// itself, so a delivery refused here would be lost: it is kept with its ids
// recorded, and handled as if it had just arrived once quiet ends (Replay).
// Without a place to keep it, it answers 503 as before.
func (h *hook) kept(w http.ResponseWriter, r *http.Request, p store.PendingDelivery) bool {
	if !h.d.Quiet() {
		return false
	}
	if h.o.Keep == nil {
		w.Header().Set("Retry-After", quietRetry)
		problem.Write(w, problem.Quiet, "kipp is quiet: restarting once running builds end")
		return true
	}
	if p.ID == "" {
		problem.Write(w, problem.BadInput, "X-GitHub-Delivery is missing")
		return true
	}
	job := p.Name
	if p.Route == routeApp {
		job = "app:" + p.Name
	}
	ids := []string{job + "/" + p.ID, bodyID(job, p.Body)}
	p.ReceivedAt = time.Now()
	err := h.o.Keep(r.Context(), p, ids, job)
	switch {
	case errors.Is(err, store.ErrSeen):
		problem.Write(w, problem.DuplicateDelivery, "this delivery was received before")
	case err != nil:
		slog.Error("webhook: keeping a delivery while quiet failed", "route", p.Route, "name", p.Name, "err", err)
		problem.Write(w, problem.Internal, "internal error")
	default:
		slog.Info("webhook: delivery kept while quiet", "route", p.Route, "name", p.Name, "event", p.Event, "delivery", p.ID)
		writeJSON(w, http.StatusAccepted, map[string]string{"kept": "the server is quiet; the delivery is handled when quiet ends"})
		// Quiet may have ended between the check and the keeping, after
		// the replay that ended it had already looked.
		if !h.d.Quiet() {
			h.replay(context.WithoutCancel(r.Context()))
		}
	}
	return true
}

// Replay handles the kept deliveries as if they had just arrived, oldest
// first, and forgets each once handled. The server calls it when quiet ends
// and at start. A delivery whose signature was checked when it was kept is
// not checked again, and its ids are recorded already, so one handled twice
// is not built twice.
func (s *Hooks) Replay(ctx context.Context) { s.h.replay(ctx) }

// replay stops at the first delivery the server is quiet again for, and
// leaves it and the ones after it kept.
func (h *hook) replay(ctx context.Context) {
	if h.o.Pending == nil || h.o.Done == nil {
		return
	}
	h.replaying.Lock()
	defer h.replaying.Unlock()
	kept, err := h.o.Pending(ctx)
	if err != nil {
		slog.Error("webhook: listing the kept deliveries failed", "err", err)
		return
	}
	for _, p := range kept {
		if h.d.Quiet() {
			slog.Info("webhook: quiet again, kept deliveries left for when it ends")
			return
		}
		age := time.Since(p.ReceivedAt).Round(time.Second)
		if age > lateAfter {
			slog.Warn("webhook: a kept delivery is older than a quiet lasts, handled all the same", "age", age.String())
		}
		code := h.handleKept(ctx, p)
		if code == http.StatusServiceUnavailable { // quiet turned on again meanwhile
			slog.Info("webhook: quiet again, kept deliveries left for when it ends")
			return
		}
		slog.Info("webhook: delivery replayed after quiet", "route", p.Route, "name", p.Name, "event", p.Event, "delivery", p.ID, "age", age.String(), "status", code)
		if err := h.o.Done(ctx, p.Seq); err != nil {
			slog.Error("webhook: forgetting a replayed delivery failed", "delivery", p.ID, "err", err)
			return
		}
	}
}

// handleKept runs a kept delivery through the handling a live one gets, and
// returns the status it would have been answered.
func (h *hook) handleKept(ctx context.Context, p store.PendingDelivery) int {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(p.Body))
	if err != nil {
		return http.StatusInternalServerError
	}
	r.Header.Set("X-GitHub-Event", p.Event)
	r.Header.Set("X-GitHub-Delivery", p.ID)
	w := &discard{header: http.Header{}, code: http.StatusOK}
	switch p.Route {
	case routeApp:
		h.handleApp(w, r, p.Name, p.Event, p.Body)
	default:
		h.handleJob(w, r, p.Name, p.Event, p.Body)
	}
	return w.code
}

// discard is the answer nobody reads: GitHub was told 202 long ago.
type discard struct {
	header http.Header
	code   int
}

// Header is the answer's headers, which nobody reads.
func (d *discard) Header() http.Header { return d.header }

// Write drops the body.
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }

// WriteHeader keeps the status, which the replay logs.
func (d *discard) WriteHeader(code int) { d.code = code }
