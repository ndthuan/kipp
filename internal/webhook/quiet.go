package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	routeJob = "job"
	routeApp = "app"
)

const lateAfter = 2*time.Hour + 10*time.Minute

func quiet(w http.ResponseWriter) {
	w.Header().Set("Retry-After", quietRetry)
	problem.Write(w, problem.Quiet, "kipp is quiet: restarting once running builds end")
}

func deliveryIDs(p store.PendingDelivery) (job string, ids []string) {
	job = p.Name
	if p.Route == routeApp {
		job = "app:" + p.Name
	}
	return job, []string{job + "/" + p.ID, bodyID(job, p.Body)}
}

func (h *hook) kept(w http.ResponseWriter, r *http.Request, p store.PendingDelivery) bool {
	if !h.d.Quiet() {
		return false
	}
	if h.o.Keep == nil {
		quiet(w)
		return true
	}
	if p.ID == "" {
		problem.Write(w, problem.BadInput, "X-GitHub-Delivery is missing")
		return true
	}
	job, ids := deliveryIDs(p)
	err := h.keep(w, r, p, ids, job)
	switch {
	case errors.Is(err, store.ErrSeen):
		problem.Write(w, problem.DuplicateDelivery, "this delivery was received before")
	case err != nil:
		problem.Write(w, problem.Internal, "internal error")
	}
	return true
}

func (h *hook) keep(w http.ResponseWriter, r *http.Request, p store.PendingDelivery, ids []string, job string) error {
	p.ReceivedAt = time.Now()
	err := h.o.Keep(r.Context(), p, ids, job)
	switch {
	case errors.Is(err, store.ErrSeen):
		return err
	case err != nil:
		slog.Error("webhook: keeping a delivery while quiet failed", "route", p.Route, "name", p.Name, "err", err)
		return err
	}
	slog.Info("webhook: delivery kept while quiet", "route", p.Route, "name", p.Name, "event", p.Event, "delivery", p.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"kept": "the server is quiet; the delivery is handled when quiet ends"})
	if !h.d.Quiet() {
		h.replay(context.WithoutCancel(r.Context()))
	}
	return nil
}

func (h *hook) handle(w http.ResponseWriter, r *http.Request, p store.PendingDelivery) {
	switch p.Route {
	case routeApp:
		h.handleApp(w, r, p.Name, p.Event, p.Body)
	default:
		h.handleJob(w, r, p.Name, p.Event, p.Body)
	}
}

func (h *hook) settle(w http.ResponseWriter, r *http.Request, p store.PendingDelivery) {
	a := &answer{header: http.Header{}, code: http.StatusOK}
	h.handle(a, r, p)
	job, _ := deliveryIDs(p)
	if a.code == http.StatusServiceUnavailable && h.o.Keep != nil && h.keep(w, r, p, nil, job) == nil {
		return
	}
	if a.code >= http.StatusInternalServerError {
		h.forget(r.Context(), p)
	}
	a.sendTo(w)
}

func (a *answer) sendTo(w http.ResponseWriter) {
	if a.body.Len() > 0 && !json.Valid(a.body.Bytes()) {
		slog.Error("webhook: a settled answer is not JSON", "status", a.code)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		problem.Write(w, problem.Internal, "internal error")
		return
	}
	maps.Copy(w.Header(), a.header)
	if a.header.Get("Content-Type") == problem.MediaType {
		w.Header().Set("Content-Type", "application/problem+json")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(a.code)
	if a.body.Len() > 0 {
		_ = json.NewEncoder(w).Encode(json.RawMessage(a.body.Bytes()))
	}
}

func (h *hook) forget(ctx context.Context, p store.PendingDelivery) {
	if h.o.Forget == nil {
		return
	}
	_, ids := deliveryIDs(p)
	if err := h.o.Forget(context.WithoutCancel(ctx), ids); err != nil {
		slog.Error("webhook: forgetting a delivery that was not handled failed; sent again it is refused as a duplicate", "route", p.Route, "name", p.Name, "delivery", p.ID, "err", err)
	}
}

// Replay handles the kept deliveries as if they had just arrived, oldest
// first, and forgets each once handled. The server calls it when quiet ends
// and at start. A delivery whose signature was checked when it was kept is
// not checked again, and its ids are recorded already, so one handled twice
// is not built twice. One that ends in a server error is no longer kept,
// and its ids are forgotten so that it can be sent again.
func (s *Hooks) Replay(ctx context.Context) { s.h.replay(ctx) }

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
		if code == http.StatusServiceUnavailable {
			slog.Info("webhook: quiet again, kept deliveries left for when it ends")
			return
		}
		if code >= http.StatusInternalServerError {
			h.forget(ctx, p)
		}
		slog.Info("webhook: delivery replayed after quiet", "route", p.Route, "name", p.Name, "event", p.Event, "delivery", p.ID, "age", age.String(), "status", code)
		if err := h.o.Done(ctx, p.Seq); err != nil {
			slog.Error("webhook: forgetting a replayed delivery failed", "delivery", p.ID, "err", err)
			return
		}
	}
}

func (h *hook) handleKept(ctx context.Context, p store.PendingDelivery) int {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(p.Body))
	if err != nil {
		return http.StatusInternalServerError
	}
	r.Header.Set("X-GitHub-Event", p.Event)
	r.Header.Set("X-GitHub-Delivery", p.ID)
	w := &answer{header: http.Header{}, code: http.StatusOK}
	h.handle(w, r, p)
	return w.code
}

type answer struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

// Header is the answer's headers.
func (a *answer) Header() http.Header { return a.header }

// Write keeps the body.
func (a *answer) Write(b []byte) (int, error) { return a.body.Write(b) }

// WriteHeader keeps the status.
func (a *answer) WriteHeader(code int) { a.code = code }
