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

// maxBody is GitHub's own cap on a webhook payload.
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
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	// HeadCommit is the commit a tag names, which for an annotated tag
	// After is not: there After is the tag object.
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
	// Secret opens a secret by its name; one that is not set is
	// store.ErrNotFound.
	Secret func(ctx context.Context, name string) (string, error)
	// Record keeps an id for the job, a delivery's or its body's sum;
	// one kept before is store.ErrSeen.
	Record func(ctx context.Context, id, job string) error
	// Readers is how many bodies are read at once (default 2), and
	// ReadTimeout how long one may take to arrive (default 30 s).
	Readers     int
	ReadTimeout time.Duration
	// Index is told the events of multibranch jobs, once they are
	// verified; without it a multibranch job's deliveries are refused.
	Index *index.Index
	// Rescan asks for a scan of an owner job, which an App's installation
	// event is a reason for: the Scanner's Rescan. Without it those events
	// are acknowledged and the next scan finds the change.
	Rescan func(job string) error
}

const (
	defaultReaders     = 2
	defaultReadTimeout = 30 * time.Second
	// busyRetry is when a delivery that found every reader taken is worth
	// sending again.
	busyRetry = "10"
	// quietRetry is the same for a delivery refused while the server is
	// quiet before a restart.
	quietRetry = "60"
)

// GitHub handles POST /api/v1/hooks/github/{job}. Whoever posts is unknown until
// the signature is checked, so what an unsigned request may cost is
// bounded: no signature header is refused before the body, at most
// Readers bodies are read at once, each within ReadTimeout and maxBody.
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
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = defaultReadTimeout
	}
	return &Hooks{h: &hook{d: d, o: o, readers: make(chan struct{}, o.Readers)}}
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
	d       *dispatch.Dispatcher
	o       Options
	readers chan struct{} // one place for each body being read
	// queueing is held from the look at what is queued to the queueing of
	// a build, so that two deliveries of one commit cannot both find none.
	queueing sync.Mutex
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name := r.PathValue("job")
	body, sig, ok := h.read(w, r, name)
	if !ok {
		return
	}
	job := h.d.Snapshot().Jobs[name]
	if !h.verified(w, r, job, name, body, sig) || h.quiet(w) || !recorded(w, r, h.o.Record, r.Header.Get("X-GitHub-Delivery"), name, body) {
		return
	}
	if job.Disabled { // said only to whoever signed: nobody else learns the job is there
		writeJSON(w, http.StatusOK, map[string]string{"ignored": dispatch.ErrDisabled.Error()})
		return
	}
	if job.IsParent() {
		h.multibranch(w, r, name, r.Header.Get("X-GitHub-Event"), body)
		return
	}
	if job.IsOwner() {
		h.owner(w, r, name, r.Header.Get("X-GitHub-Event"), body)
		return
	}
	p, why, bad := parse(job.Triggers.Webhook, r.Header.Get("X-GitHub-Event"), body)
	switch {
	case bad != "":
		problem.Write(w, problem.BadInput, bad)
	case why != "":
		writeJSON(w, http.StatusOK, map[string]string{"ignored": why})
	default:
		trigger(w, r, h.d, dispatch.TriggerRequest{Job: name, Kind: "webhook", Detail: p.commit, Branch: p.branch, Tag: p.tag, DefaultBranch: p.defaultBranch, By: "github"})
	}
}

// read takes the signature and the body, in a reader's place; it answers
// and reports false when it cannot.
// quiet answers 503 and reports true while the server is quiet. It is asked
// after the signature and before the delivery is kept, so that a delivery
// sent again once the server is back is not taken for one received before.
func (h *hook) quiet(w http.ResponseWriter) bool {
	if !h.d.Quiet() {
		return false
	}
	w.Header().Set("Retry-After", quietRetry)
	problem.Write(w, problem.Quiet, dispatch.ErrQuiet.Error())
	return true
}

func (h *hook) read(w http.ResponseWriter, r *http.Request, name string) (body, sig []byte, ok bool) {
	sig, ok = signature(r.Header.Get("X-Hub-Signature-256"))
	if !ok {
		refuse(w, name, "missing or bad signature")
		return nil, nil, false
	}
	select {
	case h.readers <- struct{}{}:
	default:
		w.Header().Set("Retry-After", busyRetry)
		problem.Write(w, problem.Busy, "busy with other deliveries; send it again")
		return nil, nil, false
	}
	body, code := readBody(w, r, h.o.ReadTimeout)
	<-h.readers
	if code != 0 {
		problem.Write(w, readFailure(code), http.StatusText(code))
		return nil, nil, false
	}
	return body, sig, true
}

// verified reports whether the body is signed with the job's secret, and
// answers when it is not.
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

// refuse answers a request that is not proven to come from whoever holds
// the job's secret, the same way whatever the reason, which goes to the log.
func refuse(w http.ResponseWriter, job, reason string) {
	slog.Warn("webhook: refused", "job", job, "reason", reason)
	problem.Write(w, problem.Unauthorized, "unauthorized")
}

// unexpected answers a fault of the server's own; the cause is in its log.
func unexpected(w http.ResponseWriter) {
	problem.Write(w, problem.Internal, "internal error")
}

// readFailure is the code of a status readBody gave for a body it could not
// read.
func readFailure(status int) problem.Code {
	switch status {
	case http.StatusRequestEntityTooLarge:
		return problem.TooLarge
	case http.StatusRequestTimeout:
		return problem.RequestTimeout
	}
	return problem.BadInput
}

// readBody reads the body within the deadline and the cap; code is the
// status to answer when it could not, 0 when it could.
func readBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) ([]byte, int) {
	// A server that cannot set one (a test's recorder) still has the cap.
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

// recorded keeps the delivery's id under its job and the sum of its job and
// body, so that a signed delivery sent again by whoever saw it on its way is
// not built twice. GitHub sends one event to each hook of a repo under the
// same id, so an id is kept per job. The signature covers the body alone: the id is the sender's
// to choose, and the body's sum is what holds a copy sent under another.
// It answers and reports false when the delivery has no id or either was
// kept before.
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

// bodyID is the id a delivery's body is kept under: the sum of the job's
// name and the body, a NUL between, which no name holds. A GitHub delivery
// id is a GUID, so it is never one of these.
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
	case err != nil:
		problem.Write(w, problem.Internal, "could not queue the build")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": b.ID, "number": b.Number})
	}
}

// dummyKey keeps the work the same when a job has no secret to sign with.
const dummyKey = "kipp-webhook-no-such-secret"

// lookup returns the key to verify with. A job that cannot be signed for
// gets the dummy key and a reason; only a failing lookup is an error. The
// secret is looked up for every name, a job's or not, so that each costs
// the same.
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

// signature is the HMAC an X-Hub-Signature-256 header holds, ok only
// for the one form GitHub sends: sha256= and 64 hex digits.
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

// pushed is what a push this job builds names: the commit, and the branch
// pushed to or the tag pushed, never both.
type pushed struct {
	commit, branch, tag, defaultBranch string
}

// parse reads a signed delivery. ignored says why it is not this job's push;
// invalid says why a push that is this job's cannot be built.
func parse(hook *config.Webhook, event string, body []byte) (p pushed, ignored, invalid string) {
	if event != "push" {
		return p, "event " + event + " is not a push", ""
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return p, "", "body is not a push event"
	}
	if !strings.EqualFold(ev.Repository.FullName, hook.Repo) {
		return p, "repository " + ev.Repository.FullName + " is not " + hook.Repo, ""
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

// watched sets the branch or the tag ref names in p, and says what it is
// ("branch main", "tag v1"), or why the hook does not watch it.
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
