// Package webhook turns a GitHub push into a trigger, and a multibranch
// job's pushes and pull requests into its index's events.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/route"
	"github.com/ndthuan/kipp/internal/store"
)

const maxBody = 25 << 20

// SecretName is the secret a job's webhook is signed with.
func SecretName(job string) string {
	return "GITHUB_WEBHOOK_" + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(job))
}

// AppSecretName is the secret a GitHub App's webhook is signed with, named
// from the App's name (which holds only lower-case letters, digits and -).
func AppSecretName(app string) string {
	return "GITHUB_APP_WEBHOOK_" + strings.ReplaceAll(strings.ToUpper(app), "-", "_")
}

type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	HeadCommit *struct {
		ID string `json:"id"`
	} `json:"head_commit"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

// Options are what the webhook needs of the server.
type Options struct {
	Secret      func(ctx context.Context, name string) (string, error)
	Record      func(ctx context.Context, id, job string) error
	Keep        func(ctx context.Context, p store.PendingDelivery, ids []string, job string) error
	Pending     func(ctx context.Context) ([]store.PendingDelivery, error)
	Done        func(ctx context.Context, seq int64) error
	Forget      func(ctx context.Context, ids []string) error
	Readers     int
	Wait        time.Duration
	Client      func(r *http.Request) string
	ReadTimeout time.Duration
	Index       *index.Index
	Rescan      func(job string) error
}

const (
	defaultReaders     = 4
	defaultWait        = 3 * time.Second
	defaultReadTimeout = 30 * time.Second
	busyRetry          = "10"
	quietRetry         = "60"
)

// GitHub handles POST /api/v1/hooks/github/{job}. Whoever posts is unknown until
// the signature is checked, so what an unsigned request may cost is
// bounded: no signature header is refused before the body, at most
// Readers bodies are read at once and half of them for one client address
// (Client, else the peer's), each within ReadTimeout and maxBody. A delivery
// that finds no reader free waits for one for Wait before it is refused.
// Every name is answered by the same work, a lookup and an HMAC, so the
// time an answer takes does not tell which names have a webhook.
func GitHub(d *dispatch.Dispatcher, o Options) http.Handler { return New(d, o).Job() }

// New is the webhooks of one server: the job route and the App route
// share the readers they bound and the check that a commit is not built
// twice.
func New(d *dispatch.Dispatcher, o Options) *Hooks {
	if o.Readers < 1 {
		o.Readers = defaultReaders
	}
	if o.Wait <= 0 {
		o.Wait = defaultWait
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = defaultReadTimeout
	}
	h := &hook{d: d, o: o, readers: newReaders(o)}
	if d != nil && o.Pending != nil {
		d.OnQuietEnd(func() { h.replay(context.Background()) })
	}
	return &Hooks{h: h}
}

// Hooks are the routes GitHub delivers to.
type Hooks struct{ h *hook }

// Job handles POST /api/v1/hooks/github/{job}.
func (s *Hooks) Job() http.Handler { return s.h }

// App handles POST /api/v1/hooks/github-app/{app}.
func (s *Hooks) App() http.Handler { return http.HandlerFunc(s.h.serveApp) }

// The paths GitHub delivers to.
const (
	PathJob = "/api/v1/hooks/github/{job}"
	PathApp = "/api/v1/hooks/github-app/{app}"
)

// JobURL is where GitHub delivers a job's events, under the server's public
// address base.
func JobURL(base, job string) string {
	return trimmed(base) + "/api/v1/hooks/github/" + url.PathEscape(job)
}

// AppURL is where an App's events are delivered, under the same base.
func AppURL(base, app string) string {
	return trimmed(base) + "/api/v1/hooks/github-app/" + url.PathEscape(app)
}

func trimmed(base string) string { return strings.TrimRight(base, "/") }

// Routes are the deliveries. They are served apart from the JSON API, so
// its login, CSRF and body limit never wrap them.
func (s *Hooks) Routes() []route.Route {
	return []route.Route{
		{Method: http.MethodPost, Path: PathJob, Handler: s.Job()},
		{Method: http.MethodPost, Path: PathApp, Handler: s.App()},
	}
}

// Prefix is the tree of the versioned deliveries, which Handler serves.
const Prefix = "/api/v1/hooks/"

// Handler serves the versioned deliveries at Prefix, and answers a path or
// a method it has none for as a problem.
func (s *Hooks) Handler() http.Handler {
	mux := http.NewServeMux()
	route.Mount(mux, s.Routes())
	mux.Handle(Prefix, problem.Fallback(mux, Prefix))
	return mux
}

type hook struct {
	d         *dispatch.Dispatcher
	o         Options
	readers   *readers
	queueing  sync.Mutex
	replaying sync.Mutex
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name := r.PathValue("job")
	body, sig, ok := h.read(w, r, name)
	if !ok {
		return
	}
	job := h.d.Snapshot().Jobs[name]
	if !h.verified(w, r, job, name, body, sig) {
		return
	}
	event, id := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	p := store.PendingDelivery{Route: routeJob, Name: name, Event: event, ID: id, Body: body}
	if h.kept(w, r, p) || !recorded(w, r, h.o.Record, id, name, body) {
		return
	}
	h.settle(w, r, p)
}

func (h *hook) handleJob(w http.ResponseWriter, r *http.Request, name, event string, body []byte) {
	job := h.d.Snapshot().Jobs[name]
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "job " + name + " is gone"})
		return
	}
	if job.Disabled {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": dispatch.ErrDisabled.Error()})
		return
	}
	if job.IsParent() {
		h.multibranch(w, r, name, event, body)
		return
	}
	if job.IsOwner() {
		h.owner(w, r, name, event, body)
		return
	}
	p, why, bad := parse(job.Triggers.Webhook, event, body)
	switch {
	case bad != "":
		problem.Write(w, problem.BadInput, bad)
	case why != "":
		writeJSON(w, http.StatusOK, map[string]string{"ignored": why})
	default:
		trigger(w, r, h.d, dispatch.TriggerRequest{Job: name, Kind: "webhook", Detail: p.commit, Branch: p.branch, Tag: p.tag, DefaultBranch: p.defaultBranch, By: "github"})
	}
}

func (h *hook) read(w http.ResponseWriter, r *http.Request, name string) (body, sig []byte, ok bool) {
	sig, ok = signature(r.Header.Get("X-Hub-Signature-256"))
	if !ok {
		refuse(w, name, "missing or bad signature")
		return nil, nil, false
	}
	release, ok := h.readers.acquire(r)
	if !ok {
		w.Header().Set("Retry-After", busyRetry)
		problem.Write(w, problem.Busy, "busy with other deliveries; send it again")
		return nil, nil, false
	}
	body, code := readBody(w, r, h.o.ReadTimeout)
	release()
	if code != 0 {
		problem.Write(w, readFailure(code), http.StatusText(code))
		return nil, nil, false
	}
	return body, sig, true
}

func (h *hook) verified(w http.ResponseWriter, r *http.Request, job *config.Job, name string, body, sig []byte) bool {
	key, reason, err := lookup(r.Context(), job, name, h.o.Secret)
	if err != nil {
		slog.Error("webhook: secret lookup failed", "job", name, "err", err)
		unexpected(w)
		return false
	}
	if !signed(key, body, sig) && reason == "" {
		reason = "missing or bad signature"
	}
	if reason != "" {
		refuse(w, name, reason)
		return false
	}
	return true
}

func refuse(w http.ResponseWriter, job, reason string) {
	slog.Warn("webhook: refused", "job", job, "reason", reason)
	problem.Write(w, problem.Unauthorized, "unauthorized")
}

func unexpected(w http.ResponseWriter) {
	problem.Write(w, problem.Internal, "internal error")
}

func readFailure(status int) problem.Code {
	switch status {
	case http.StatusRequestEntityTooLarge:
		return problem.TooLarge
	case http.StatusRequestTimeout:
		return problem.RequestTimeout
	}
	return problem.BadInput
}

func readBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) ([]byte, int) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(timeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return body, 0
	case errors.As(err, &tooBig):
		return nil, http.StatusRequestEntityTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, http.StatusRequestTimeout
	}
	return nil, http.StatusBadRequest
}

func recorded(w http.ResponseWriter, r *http.Request, record func(context.Context, string, string) error, id, job string, body []byte) bool {
	if id == "" {
		problem.Write(w, problem.BadInput, "X-GitHub-Delivery is missing")
		return false
	}
	ctx := r.Context()
	err := record(ctx, job+"/"+id, job)
	if err == nil {
		err = record(ctx, bodyID(job, body), job)
	}
	switch {
	case errors.Is(err, store.ErrSeen):
		problem.Write(w, problem.DuplicateDelivery, "this delivery was received before")
	case err != nil:
		slog.Error("webhook: recording the delivery failed", "job", job, "err", err)
		problem.Write(w, problem.Internal, "internal error")
	default:
		return true
	}
	return false
}

func bodyID(job string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(job + "\x00"))
	h.Write(body)
	return "body:" + hex.EncodeToString(h.Sum(nil))
}

func trigger(w http.ResponseWriter, r *http.Request, d *dispatch.Dispatcher, req dispatch.TriggerRequest) {
	b, err := d.Trigger(r.Context(), req)
	switch {
	case errors.Is(err, dispatch.ErrDropped):
		problem.Write(w, problem.Dropped, err.Error())
	case errors.Is(err, dispatch.ErrQuiet):
		quiet(w)
	case err != nil:
		slog.Error("webhook: queueing a build", "job", req.Job, "err", err)
		problem.Write(w, problem.Internal, "could not queue the build")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": b.ID, "number": b.Number})
	}
}

const dummyKey = "kipp-webhook-no-such-secret"

func lookup(ctx context.Context, job *config.Job, name string, secret func(context.Context, string) (string, error)) (key, reason string, err error) {
	key, err = secret(ctx, SecretName(name))
	switch {
	case errors.Is(err, store.ErrNotFound):
		key, reason = dummyKey, "secret not set"
	case err != nil:
		return "", "", err
	case key == "":
		key, reason = dummyKey, "secret not set"
	}
	switch {
	case job == nil:
		return dummyKey, "unknown job", nil
	case job.Triggers.Webhook == nil && !job.IsParent() && !job.IsOwner():
		return dummyKey, "job has no webhook trigger", nil
	}
	return key, reason, nil
}

func signature(header string) (sum []byte, ok bool) {
	hexSum, found := strings.CutPrefix(header, "sha256=")
	if !found || len(hexSum) != 2*sha256.Size {
		return nil, false
	}
	sum, err := hex.DecodeString(hexSum)
	return sum, err == nil
}

func signed(key string, body, sum []byte) bool {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(body)
	return hmac.Equal(m.Sum(nil), sum)
}

type pushed struct {
	commit, branch, tag, defaultBranch string
}

func parse(hook *config.Webhook, event string, body []byte) (p pushed, ignored, invalid string) {
	if event != "push" {
		return p, "the event is not a push", ""
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return p, "", "body is not a push event"
	}
	if !strings.EqualFold(ev.Repository.FullName, hook.Repo) {
		return p, "the repository is not " + hook.Repo, ""
	}
	p.defaultBranch = ev.Repository.DefaultBranch
	what, ignored := watched(hook, ev.Ref, &p)
	if ignored != "" {
		return p, ignored, ""
	}
	if ev.Deleted || strings.Trim(ev.After, "0") == "" && ev.After != "" {
		return p, what + " was deleted", ""
	}
	field := "after"
	p.commit = ev.After
	if p.tag != "" && ev.HeadCommit != nil && ev.HeadCommit.ID != "" {
		p.commit, field = ev.HeadCommit.ID, "head_commit.id"
	}
	if !commitID(p.commit) {
		return p, "", field + " is not a full commit id"
	}
	p.commit = strings.ToLower(p.commit)
	return p, "", ""
}

func watched(hook *config.Webhook, ref string, p *pushed) (what, ignored string) {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		if !hook.Watches(branch) {
			return "", "branch " + branch + " is not watched"
		}
		p.branch = branch
		return "branch " + branch, ""
	}
	if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		if !hook.WatchesTag(tag) {
			return "", "tag " + tag + " is not watched"
		}
		p.tag = tag
		return "tag " + tag, ""
	}
	return "", ref + " is neither a branch nor a tag"
}

func commitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
