package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
)

// errNoUpgrades is the answer of a server that was given no engine.
var errNoUpgrades = fmt.Errorf("%w: this server does not upgrade itself", ErrConflict)

// upgradeErr turns what the engine refuses into the API's own kinds.
func upgradeErr(err error) error {
	switch {
	case errors.Is(err, engine.ErrConflict):
		return fmt.Errorf("%w: %s", ErrConflict, without(err.Error(), engine.ErrConflict))
	case errors.Is(err, engine.ErrBadRequest):
		return fmt.Errorf("%w: %s", ErrBadInput, without(err.Error(), engine.ErrBadRequest))
	}
	return err
}

// Upgrade is the server's upgrade as it stands, for an admin.
func (s *Service) Upgrade(ctx context.Context, u *store.User) (engine.Info, error) {
	if !u.Admin {
		return engine.Info{}, ErrForbidden
	}
	if s.deps.Upgrade == nil {
		return engine.Info{Unavailable: "this server does not upgrade itself"}, nil
	}
	return s.deps.Upgrade.Info(ctx)
}

// StartUpgrade begins an upgrade to the latest release. Like quiet it is an
// admin's, token or browser alike.
func (s *Service) StartUpgrade(ctx context.Context, u *store.User, req engine.Request) (engine.Info, error) {
	return s.upgradeCall(ctx, u, "upgrade", req.Version, req, s.deps.Upgrade.Start)
}

// StartRollback begins the return to the version before the last upgrade.
func (s *Service) StartRollback(ctx context.Context, u *store.User, req engine.Request) (engine.Info, error) {
	return s.upgradeCall(ctx, u, "upgrade.rollback", "", req, s.deps.Upgrade.Rollback)
}

// CancelUpgrade ends an upgrade that has not reached its backup.
func (s *Service) CancelUpgrade(ctx context.Context, u *store.User) (engine.Info, error) {
	return s.upgradeCall(ctx, u, "upgrade.cancel", "", engine.Request{}, func(ctx context.Context, _ engine.Request) (engine.Info, error) {
		return s.deps.Upgrade.Cancel(ctx)
	})
}

func (s *Service) upgradeCall(ctx context.Context, u *store.User, action, target string, req engine.Request,
	do func(context.Context, engine.Request) (engine.Info, error)) (engine.Info, error) {
	if !u.Admin {
		return engine.Info{}, ErrForbidden
	}
	if s.deps.Upgrade == nil {
		return engine.Info{}, errNoUpgrades
	}
	in, err := do(ctx, req)
	if err != nil {
		s.record(ctx, u.Name, action, target, "refused")
		return in, upgradeErr(err)
	}
	detail := "safe restart"
	if !req.Safe {
		detail = "no safe restart"
	}
	if action == "upgrade.cancel" {
		detail = ""
	}
	if target == "" {
		target = in.Status.Version
	}
	s.record(ctx, u.Name, action, target, detail)
	return in, nil
}

// upgradeJSON is the upgrade as the API answers it.
type upgradeJSON struct {
	State       string              `json:"state"`
	Version     string              `json:"version"`
	From        string              `json:"from"`
	Backup      string              `json:"backup"`
	Running     int                 `json:"running"`
	Queued      int                 `json:"queued"`
	Rerun       int                 `json:"rerun"`
	UpdatedAt   *stamp              `json:"updated_at"`
	Error       string              `json:"error"`
	Latest      string              `json:"latest"`
	Upgradable  bool                `json:"upgradable"`
	Unavailable string              `json:"unavailable"`
	RollbackTo  string              `json:"rollback_to"`
	LastFailure *upgradeFailureJSON `json:"last_failure"`
}

type upgradeFailureJSON struct {
	Version string `json:"version"`
	From    string `json:"from"`
	Reason  string `json:"reason"`
	LogTail string `json:"log_tail"`
	At      stamp  `json:"at"`
}

func toUpgradeJSON(in engine.Info) upgradeJSON {
	out := upgradeJSON{State: in.Status.State, Version: in.Status.Version, From: in.Status.From, Backup: in.Status.Backup,
		Running: in.Status.Running, Queued: in.Status.Queued, Rerun: in.Status.Rerun, Error: in.Error, Latest: in.Latest, Upgradable: in.Upgradable,
		Unavailable: in.Unavailable, RollbackTo: in.RollbackTo}
	if out.State == "" {
		out.State = "idle"
	}
	if !in.Status.UpdatedAt.IsZero() {
		out.UpdatedAt = new(stampOf(in.Status.UpdatedAt))
	}
	if f := in.Failure; f != nil {
		out.LastFailure = &upgradeFailureJSON{Version: f.Version, From: f.From, Reason: f.Reason, LogTail: f.LogTail, At: stampOf(f.At)}
	}
	return out
}

func (a *API) upgrade(w http.ResponseWriter, r *http.Request, u *store.User) {
	in, err := a.svc.Upgrade(r.Context(), u)
	a.answerUpgrade(w, r, http.StatusOK, in, err)
}

func (a *API) upgradeStart(w http.ResponseWriter, r *http.Request, u *store.User) {
	req, err := upgradeRequest(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	in, err := a.svc.StartUpgrade(r.Context(), u, req)
	a.answerUpgrade(w, r, http.StatusAccepted, in, err)
}

func (a *API) rollbackStart(w http.ResponseWriter, r *http.Request, u *store.User) {
	req, err := upgradeRequest(r)
	if err == nil && req.Version != "" {
		err = fmt.Errorf("%w: a rollback goes back to the version before and takes no version", ErrBadInput)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	in, err := a.svc.StartRollback(r.Context(), u, req)
	a.answerUpgrade(w, r, http.StatusAccepted, in, err)
}

func (a *API) upgradeCancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	in, err := a.svc.CancelUpgrade(r.Context(), u)
	a.answerUpgrade(w, r, http.StatusOK, in, err)
}

func (a *API) answerUpgrade(w http.ResponseWriter, r *http.Request, code int, in engine.Info, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, code, toUpgradeJSON(in))
}

// upgradeRequest reads the body of an upgrade; no body at all is the
// defaults, a safe restart with the usual wait.
func upgradeRequest(r *http.Request) (engine.Request, error) {
	var body struct {
		SafeRestart *bool  `json:"safe_restart"`
		ConfirmLost bool   `json:"confirm_lost"`
		Version     string `json:"version"`
		WaitTimeout string `json:"wait_timeout"`
	}
	if empty, err := optionalBody(r, &body); empty || err != nil {
		return engine.Request{Safe: true}, err
	}
	req := engine.Request{Safe: body.SafeRestart == nil || *body.SafeRestart, ConfirmLost: body.ConfirmLost, Version: body.Version}
	if body.WaitTimeout != "" {
		d, err := time.ParseDuration(body.WaitTimeout)
		if err != nil || d <= 0 {
			return engine.Request{}, fmt.Errorf("%w: wait_timeout is a duration such as 90m", ErrBadInput)
		}
		req.WaitTimeout = d
	}
	return req, nil
}

// optionalBody decodes a JSON body of one value into out, refusing a field
// out has not; empty is a request with no body at all.
func optionalBody(r *http.Request, out any) (empty bool, err error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err = dec.Decode(out)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			err = errors.New("more than one JSON value in the body")
		}
	}
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	return false, nil
}

// DryRunUpgrade makes every check of an upgrade to version (the latest
// when empty) but the restart, and changes nothing. It is an admin's, as
// the upgrade is.
func (s *Service) DryRunUpgrade(ctx context.Context, u *store.User, version string) (engine.DryRun, error) {
	if !u.Admin {
		return engine.DryRun{}, ErrForbidden
	}
	if s.deps.Upgrade == nil {
		return engine.DryRun{}, errNoUpgrades
	}
	d, err := s.deps.Upgrade.DryRunUpgrade(ctx, version)
	if err != nil {
		s.record(ctx, u.Name, "upgrade.dry-run", version, "refused")
		return d, upgradeErr(err)
	}
	detail := "passed"
	if !d.OK {
		detail = "failed"
	}
	s.record(ctx, u.Name, "upgrade.dry-run", cmp.Or(d.Version, version), detail)
	return d, nil
}

// dryRunJSON is a dry run as the API answers it.
type dryRunJSON struct {
	Version string        `json:"version"`
	From    string        `json:"from"`
	OK      bool          `json:"ok"`
	Running int           `json:"running"`
	Queued  int           `json:"queued"`
	Steps   []dryStepJSON `json:"steps"`
}

type dryStepJSON struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

func (a *API) upgradeDryRun(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Version string `json:"version"`
	}
	if _, err := optionalBody(r, &body); err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := a.svc.DryRunUpgrade(r.Context(), u, body.Version)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := dryRunJSON{Version: d.Version, From: d.From, OK: d.OK, Running: d.Running, Queued: d.Queued, Steps: []dryStepJSON{}}
	for _, s := range d.Steps {
		out.Steps = append(out.Steps, dryStepJSON(s))
	}
	writeJSON(w, http.StatusOK, out)
}
