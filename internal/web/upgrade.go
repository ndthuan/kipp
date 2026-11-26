package web

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

const (
	snoozeCookie = "kipp_upgrade_snooze"
	resultKept   = 7 * 24 * time.Hour
)

func (s *Site) upgradeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/server/upgrade", s.withUser(s.upgradeStart))
	mux.HandleFunc("POST /admin/server/upgrade/cancel", s.withUser(s.upgradeCancel))
	mux.HandleFunc("GET /admin/server/upgrade/rollback", s.withUser(s.confirmRollback))
	mux.HandleFunc("POST /admin/server/upgrade/rollback", s.withUser(s.rollback))
	mux.HandleFunc("GET /admin/server/upgrade/log", s.withUser(s.upgradeLog))
	mux.HandleFunc("POST /admin/server/upgrade/dry-run", s.withUser(s.upgradeDryRun))
}

type upgradeView struct {
	Info                   engine.Info
	Latest                 string
	Offer, Manual          bool
	Active                 bool
	State                  string
	Version                string
	Running, Queued, Rerun int
	Result                 string
	Reason                 string
	Log                    bool
	Snoozed                bool
	Launcher               bool
	From                   string
	ManualLines            []string
	DryRun                 *dryRunView
}

type dryRunView struct {
	engine.DryRun
	Steps []dryStepView
}

type dryStepView struct {
	engine.DryStep
	Mark, Label string
}

var dryLabels = map[string]string{
	engine.CheckRelease:  "Release",
	engine.CheckSpace:    "Disk space",
	engine.CheckDownload: "Download and signature",
	engine.CheckJobs:     "Job files",
	engine.CheckMigrate:  "Database migration",
	engine.CheckBuilds:   "Builds to wait for",
}

func newDryRunView(d engine.DryRun) *dryRunView {
	v := &dryRunView{DryRun: d}
	for _, st := range d.Steps {
		mark := "next"
		switch st.Result {
		case engine.Passed:
			mark = "done"
		case engine.Failed:
			mark = "failed"
		}
		v.Steps = append(v.Steps, dryStepView{DryStep: st, Mark: mark, Label: cmp.Or(dryLabels[st.Name], st.Name)})
	}
	return v
}

func activeState(st string) bool {
	switch st {
	case state.StatePreflight, state.StateDownloading, state.StateVerifying, state.StateWaiting, state.StateBackup, state.StateSwitching:
		return true
	}
	return false
}

func (s *Site) upgradeViewOf(r *http.Request, u *store.User) *upgradeView {
	if u == nil || !u.Admin || !update.Release(s.version) {
		return nil
	}
	info, err := s.api.Service().Upgrade(r.Context(), u)
	if err != nil {
		if r.Context().Err() == nil {
			slog.Error("web: the upgrade's state", "err", err)
		}
		return nil
	}
	return buildUpgradeView(info, snoozed(r), time.Now())
}

func snoozed(r *http.Request) string {
	if c, err := r.Cookie(snoozeCookie); err == nil {
		return c.Value
	}
	return ""
}

func buildUpgradeView(info engine.Info, snoozedFor string, now time.Time) *upgradeView {
	v := &upgradeView{Info: info, State: info.Status.State, Version: info.Status.Version,
		Running: info.Status.Running, Queued: info.Status.Queued, Rerun: info.Status.Rerun}
	v.From = info.Status.From
	v.Active = activeState(v.State)
	recent := !info.Status.UpdatedAt.IsZero() && now.Sub(info.Status.UpdatedAt) < resultKept
	switch {
	case v.State == state.StateDone && recent:
		v.Result = state.StateDone
	case v.State == state.StateFailed && recent:
		v.Result = state.StateFailed
		v.Reason = info.Error
		if f := info.Failure; f != nil {
			v.Reason, v.Log, v.Launcher, v.From = f.Reason, f.LogTail != "", true, f.From
		}
	}
	if v.Active || info.Latest == "" {
		return v
	}
	v.Offer = info.Unavailable == "" && info.Upgradable
	v.Manual = !v.Offer
	if v.Offer && snoozedFor != info.Latest {
		v.Latest = info.Latest
	}
	return v
}

// CanCancel tells whether the run has not reached its backup yet.
func (v *upgradeView) CanCancel() bool {
	switch v.State {
	case state.StatePreflight, state.StateDownloading, state.StateVerifying, state.StateWaiting:
		return true
	}
	return false
}

type upgradeStep struct{ Text, Mark string }

// Steps are the card's progress lines. The backup comes after the wait, so
// that what a rollback loses is only what is written after the switch.
func (v *upgradeView) Steps() []upgradeStep {
	groups := []struct {
		states         []string
		done, now, nxt string
	}{
		{[]string{state.StatePreflight}, "Checked that it can run", "Checking that it can run", "Check that it can run"},
		{[]string{state.StateDownloading, state.StateVerifying}, "Downloaded and verified the release", "Downloading and verifying the release", "Download and verify the release"},
		{[]string{state.StateWaiting}, "Builds ended", v.Counts(), "Wait for builds to end"},
		{[]string{state.StateBackup}, "Backed up the data", "Backing up the data", "Back up the data, just before the restart"},
		{[]string{state.StateSwitching}, "Restarted", "Restarting, this page reconnects by itself", "Restart"},
	}
	var out []upgradeStep
	for _, g := range groups {
		switch {
		case slices.Contains(g.states, v.State):
			out = append(out, upgradeStep{g.now, "now"})
		case v.Past(g.states[len(g.states)-1]):
			out = append(out, upgradeStep{g.done, "done"})
		default:
			out = append(out, upgradeStep{g.nxt, "next"})
		}
	}
	return out
}

// Label is the banner's words for the state of a run.
func (v *upgradeView) Label() string {
	switch v.State {
	case state.StatePreflight:
		return "checking that it can run"
	case state.StateDownloading:
		return "downloading"
	case state.StateVerifying:
		return "checking the download"
	case state.StateWaiting:
		return "waiting for builds (" + v.counts() + ")"
	case state.StateBackup:
		return "backing up"
	case state.StateSwitching:
		return "restarting"
	}
	return ""
}

func (v *upgradeView) counts() string {
	return dispatch.QuietState{Running: v.Running, Queued: v.Queued, Rerun: v.Rerun}.Counts()
}

// Counts is the card's waiting line.
func (v *upgradeView) Counts() string { return "Waiting: " + v.counts() }

var upgradeSteps = []string{state.StatePreflight, state.StateDownloading, state.StateVerifying, state.StateWaiting, state.StateBackup, state.StateSwitching}

// Past tells whether the run is beyond a step.
func (v *upgradeView) Past(step string) bool {
	at, in := -1, -1
	for i, st := range upgradeSteps {
		if st == step {
			at = i
		}
		if st == v.State {
			in = i
		}
	}
	return in > at
}

func manualLines(origin, latest string) []string {
	switch {
	case strings.HasPrefix(origin, "Homebrew"):
		return []string{"brew upgrade kipp", "brew services restart kipp"}
	case strings.HasPrefix(origin, "Docker"):
		return []string{"docker pull ghcr.io/ndthuan/kipp:" + latest, "docker stop kipp && docker rm kipp", "docker run ... ghcr.io/ndthuan/kipp:" + latest}
	}
	return []string{"Download the archive of " + latest + " from the release page, replace the kipp executable and restart it"}
}

func (s *Site) upgradeStart(w http.ResponseWriter, r *http.Request, u *store.User) {
	safe, lost := r.PostFormValue("safe") != "", r.PostFormValue("confirm_lost") != ""
	if !safe && !lost {
		s.confirmUpgrade(w, r, u)
		return
	}
	_, err := s.api.Service().StartUpgrade(r.Context(), u, engine.Request{Version: r.PostFormValue("version"), Safe: safe, ConfirmLost: lost})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/server", "upgrade-started")
}

func (s *Site) confirmUpgrade(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := s.api.Service().Quiet(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	line := "No build runs now, so nothing is lost, but kipp restarts at once."
	switch q.Running {
	case 0:
	case 1:
		line = "1 running build will end as lost: kipp restarts at once, without waiting for it."
	default:
		line = fmt.Sprintf("%d running builds will end as lost: kipp restarts at once, without waiting for them.", q.Running)
	}
	v := r.PostFormValue("version")
	s.confirm(w, r, "/admin/server", confirmData{
		Heading: "Upgrade " + v + " without waiting for the builds?",
		Lines:   []string{line, "A build that ends as lost is not run again by itself."},
		Button:  "Upgrade and restart now", Back: "/admin/server#upgrade",
		Hidden: []hiddenField{{"version", v}, {"confirm_lost", "1"}},
	})
}

func (s *Site) upgradeCancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	if _, err := s.api.Service().CancelUpgrade(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/server", "upgrade-cancelled")
}

func (s *Site) confirmRollback(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, err := s.api.Service().Upgrade(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if info.RollbackTo == "" {
		s.fail(w, r, fmt.Errorf("%w: there is no earlier version to go back to", api.ErrConflict))
		return
	}
	s.confirm(w, r, "/admin/server", confirmData{
		Heading: "Roll back to " + info.RollbackTo + "?",
		Lines: []string{"kipp restores the backup it made before the upgrade and restarts on " + info.RollbackTo + ". Everything written since the upgrade, builds and changes alike, is lost.",
			"Running builds are waited for first."},
		Button: "Roll back", Back: "/admin/server#upgrade",
	})
}

func (s *Site) rollback(w http.ResponseWriter, r *http.Request, u *store.User) {
	if _, err := s.api.Service().StartRollback(r.Context(), u, engine.Request{Safe: true}); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/server", "upgrade-started")
}

func (s *Site) upgradeLog(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, err := s.api.Service().Upgrade(r.Context(), u)
	if err == nil && (info.Failure == nil || info.Failure.LogTail == "") {
		err = api.ErrNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(info.Failure.LogTail))
}

func (s *Site) upgradeDryRun(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.api.Service().DryRunUpgrade(r.Context(), u, r.PostFormValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderServer(w, r, u, http.StatusOK, "", &d)
}
