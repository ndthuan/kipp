package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

type appEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	} `json:"installation"`
}

func (h *hook) serveApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	app := r.PathValue("app")
	who := "app " + app
	body, sig, ok := h.read(w, r, who)
	if !ok {
		return
	}
	key, reason, err := lookupApp(r.Context(), app, h.o.Secret)
	if err != nil {
		slog.Error("webhook: secret lookup failed", "app", app, "err", err)
		unexpected(w)
		return
	}
	if ok := signed(key, body, sig); reason == "" && !ok {
		reason = "missing or bad signature"
	}
	if reason != "" {
		refuse(w, who, reason)
		return
	}
	event, id := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	p := store.PendingDelivery{Route: routeApp, Name: app, Event: event, ID: id, Body: body}
	if h.kept(w, r, p) || !recorded(w, r, h.o.Record, id, "app:"+app, body) {
		return
	}
	h.settle(w, r, p)
}

func (h *hook) handleApp(w http.ResponseWriter, r *http.Request, app, event string, body []byte) {
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "ping"})
		return
	}
	var ev appEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		problem.Write(w, problem.BadInput, "body is not an event")
		return
	}
	switch event {
	case "push", "pull_request":
		h.appRepoEvent(w, r, app, event, ev.Repository.Owner.Login, body)
	case "installation", "installation_repositories":
		h.appInstallation(w, app, event, ev)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "the event is not one the App's jobs act on"})
	}
}

func lookupApp(ctx context.Context, app string, secret func(context.Context, string) (string, error)) (key, reason string, err error) {
	key, err = secret(ctx, AppSecretName(app))
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && key == "":
		return dummyKey, "secret not set", nil
	case err != nil:
		return "", "", err
	}
	return key, "", nil
}

func (h *hook) appJobs(app, login string) []*config.Job {
	snap := h.d.Snapshot()
	var out []*config.Job
	for _, name := range snap.Names {
		j := snap.Jobs[name]
		if j.IsOwner() && j.Owner.App != nil && j.Owner.App.Name == app && strings.EqualFold(j.Owner.Login, login) {
			out = append(out, j)
		}
	}
	return out
}

func (h *hook) appRepoEvent(w http.ResponseWriter, r *http.Request, app, event, login string, body []byte) {
	jobs := h.appJobs(app, login)
	switch {
	case login == "":
		problem.Write(w, problem.BadInput, "body names no repository owner")
	case len(jobs) == 0:
		writeJSON(w, http.StatusOK, map[string]string{"ignored": fmt.Sprintf("no owner job of %s signs in as the App %s", login, app)})
	case jobs[0].Disabled:
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "job " + jobs[0].Name + " is disabled"})
	default:
		h.owner(w, r, jobs[0].Name, event, body)
	}
}

func (h *hook) appInstallation(w http.ResponseWriter, app, event string, ev appEvent) {
	login := ev.Installation.Account.Login
	jobs := h.appJobs(app, login)
	if len(jobs) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": fmt.Sprintf("no owner job of %q signs in as the App %s", login, app)})
		return
	}
	scanned := []string{}
	var errs []error
	for _, j := range jobs {
		if j.Disabled || h.o.Rescan == nil {
			continue
		}
		if err := h.o.Rescan(j.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		scanned = append(scanned, j.Name)
	}
	slices.Sort(scanned)
	if len(errs) > 0 {
		slog.Error("webhook: asking for a scan", "app", app, "event", event, "err", errors.Join(errs...))
		problem.Write(w, problem.Internal, "could not ask for a scan")
		return
	}
	if len(scanned) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "event " + event + " changes nothing a scan reads here yet"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string][]string{"scan": scanned})
}
