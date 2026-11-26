package webhook

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/problem"
)

type queued struct {
	Job     string `json:"job"`
	ID      int64  `json:"id,omitempty"`
	Number  int    `json:"number,omitempty"`
	Error   string `json:"error,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

const alreadyOnItsWay = "a build of this commit is already queued or running"

func (h *hook) multibranch(w http.ResponseWriter, r *http.Request, name, event string, body []byte) {
	if h.o.Index == nil {
		problem.Write(w, problem.Internal, "this server keeps no index of multibranch jobs")
		return
	}
	var plan *index.TriggerPlan
	var err error
	switch event {
	case "push":
		plan, err = h.push(r, name, body)
	case "pull_request":
		plan, err = h.pullRequest(r, name, body)
	default:
		plan = &index.TriggerPlan{Ignored: "the event is neither a push nor a pull_request"}
	}
	switch {
	case errors.Is(err, index.ErrBadEvent):
		problem.Write(w, problem.BadInput, err.Error())
	case err != nil:
		slog.Error("webhook: the index refused an event", "job", name, "err", err)
		problem.Write(w, problem.Internal, "internal error")
	case len(plan.Builds) == 0:
		writeJSON(w, http.StatusOK, map[string]string{"ignored": plan.Ignored})
	default:
		builds, refused := h.queue(r, plan)
		if refused {
			quiet(w)
			return
		}
		if allSkipped(builds) {
			writeJSON(w, http.StatusOK, map[string]string{"ignored": alreadyOnItsWay})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string][]queued{"builds": builds})
	}
}

func (h *hook) owner(w http.ResponseWriter, r *http.Request, name, event string, body []byte) {
	if h.o.Index == nil {
		problem.Write(w, problem.Internal, "this server keeps no index of multibranch jobs")
		return
	}
	var ev struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.Repository.FullName == "" {
		problem.Write(w, problem.BadInput, "body names no repository")
		return
	}
	node, ok := h.o.Index.RepoNode(name, ev.Repository.FullName)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "the repository is not one of " + name + "'s repos"})
		return
	}
	h.multibranch(w, r, node, event, body)
}

func allSkipped(builds []queued) bool {
	for _, b := range builds {
		if b.Skipped == "" {
			return false
		}
	}
	return len(builds) > 0
}

func (h *hook) queue(r *http.Request, plan *index.TriggerPlan) (builds []queued, quiet bool) {
	h.queueing.Lock()
	defer h.queueing.Unlock()
	out := make([]queued, 0, len(plan.Builds))
	for _, req := range plan.Builds {
		if req.Kind == "webhook" && req.Detail != "" && !plan.Rebuild {
			on, err := h.o.Index.OnItsWay(r.Context(), req.Job, req.Detail, req.Tag, plan.Base)
			if err != nil {
				slog.Error("webhook: looking for a build of the commit", "job", req.Job, "err", err)
				out = append(out, queued{Job: req.Job, Error: "could not queue the build"})
				continue
			}
			if on {
				out = append(out, queued{Job: req.Job, Skipped: alreadyOnItsWay})
				continue
			}
		}
		b, err := h.d.Trigger(r.Context(), req)
		switch {
		case errors.Is(err, dispatch.ErrQuiet):
			return out, true
		case errors.Is(err, dispatch.ErrDropped), errors.Is(err, dispatch.ErrDisabled):
			out = append(out, queued{Job: req.Job, Error: err.Error()})
		case err != nil:
			slog.Error("webhook: queueing a child's build", "job", req.Job, "err", err)
			out = append(out, queued{Job: req.Job, Error: "could not queue the build"})
		default:
			out = append(out, queued{Job: req.Job, ID: b.ID, Number: b.Number})
		}
	}
	return out, false
}

func (h *hook) push(r *http.Request, name string, body []byte) (*index.TriggerPlan, error) {
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, badEvent("body is not a push event")
	}
	deleted := ev.Deleted || strings.Trim(ev.After, "0") == "" && ev.After != ""
	field, commit := "after", ev.After
	if strings.HasPrefix(ev.Ref, "refs/tags/") && ev.HeadCommit != nil && ev.HeadCommit.ID != "" {
		field, commit = "head_commit.id", ev.HeadCommit.ID
	}
	if !deleted && !commitID(commit) {
		return nil, badEvent(field + " is not a full commit id")
	}
	return h.o.Index.Push(r.Context(), name, index.PushEvent{
		Repo: ev.Repository.FullName, Ref: ev.Ref, After: strings.ToLower(commit), Deleted: deleted,
		DefaultBranch: ev.Repository.DefaultBranch,
	})
}

type pullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		URL   string `json:"html_url"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Changes struct {
		Base *json.RawMessage `json:"base"`
	} `json:"changes"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

func (h *hook) pullRequest(r *http.Request, name string, body []byte) (*index.TriggerPlan, error) {
	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, badEvent("body is not a pull_request event")
	}
	p := ev.PullRequest
	switch {
	case ev.Number <= 0:
		return nil, badEvent("number is not a pull request's")
	case ev.Action != "closed" && !commitID(p.Head.SHA):
		return nil, badEvent("pull_request.head.sha is not a full commit id")
	case ev.Action != "closed" && (p.Head.Ref == "" || p.Base.Ref == ""):
		return nil, badEvent("pull_request names no head or base branch")
	}
	headRepo := ""
	if p.Head.Repo != nil {
		headRepo = p.Head.Repo.FullName
	}
	return h.o.Index.PullRequest(r.Context(), name, index.PullRequestEvent{
		Repo: ev.Repository.FullName, DefaultBranch: ev.Repository.DefaultBranch, Action: ev.Action, Number: ev.Number,
		Title: p.Title, Author: p.User.Login, URL: p.URL, HeadRef: p.Head.Ref, HeadSHA: strings.ToLower(p.Head.SHA), HeadRepo: headRepo, BaseRef: p.Base.Ref,
		BaseChanged: ev.Changes.Base != nil,
	})
}

func badEvent(why string) error { return &eventError{why} }

type eventError struct{ why string }

func (e *eventError) Error() string { return e.why }

// Is makes it an index.ErrBadEvent.
func (e *eventError) Is(target error) bool { return target == index.ErrBadEvent }
