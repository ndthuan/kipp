package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// The admin reads as JSON. A token may make them, so that a script can
// read everything a browser can; none of them changes anything.

type userJSON struct {
	Name        string              `json:"name"`
	Admin       bool                `json:"admin"`
	LastLoginAt *stamp              `json:"last_login_at"`
	Grants      map[string][]string `json:"grants"`
	GoneGrants  []string            `json:"gone_grants"`
	Tokens      []tokenJSON         `json:"tokens"`
	Sessions    int                 `json:"sessions"`
}

// tokenJSON is what is known of a token, never the token or its hash.
type tokenJSON struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	CreatedAt  stamp  `json:"created_at"`
	LastUsedAt *stamp `json:"last_used_at"`
	ExpiresAt  *stamp `json:"expires_at"`
}

type secretJSON struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	User      string   `json:"user"`
	Scopes    []string `json:"scopes"`
	Narrow    bool     `json:"narrow"`
	UpdatedAt *stamp   `json:"updated_at"`
	Jobs      []string `json:"jobs"`
	Webhook   bool     `json:"webhook"`
}

type secretsJSON struct {
	Set     []secretJSON `json:"set"`
	Missing []secretJSON `json:"missing"`
	// OutOfScope are secrets that are set but not in scope for a job that lists them.
	OutOfScope []secretJSON `json:"out_of_scope"`
}

type serverJSON struct {
	Version           string   `json:"version"`
	GoVersion         string   `json:"go_version"`
	Schema            int      `json:"schema"`
	StartedAt         stamp    `json:"started_at"`
	Listen            string   `json:"listen"`
	BaseURL           string   `json:"base_url"`
	Executors         int      `json:"executors"`
	Builds            int      `json:"builds"`
	DefaultRunsOn     []string `json:"default_runs_on"`
	AgentEnv          []string `json:"agent_env"`
	AgentURL          string   `json:"agent_url"`
	AgentListen       string   `json:"agent_listen"`
	Shell             string   `json:"shell"`
	SessionTTLSeconds int64    `json:"session_ttl_seconds"`
	TimeZone          string   `json:"timezone"`
	DataDir           string   `json:"data_dir"`
	JobsDir           string   `json:"jobs_dir"`
	// FreeDiskBytes and DBSizeBytes are null when they could not be read.
	FreeDiskBytes   *uint64 `json:"free_disk_bytes"`
	DBSizeBytes     *int64  `json:"db_size_bytes"`
	LastReloadAt    stamp   `json:"last_reload_at"`
	LastReloadError string  `json:"last_reload_error"`
	Jobs            int     `json:"jobs"`
}

type auditJSON struct {
	ID     int64  `json:"id"`
	At     stamp  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

func toUserJSON(info *UserInfo) userJSON {
	out := userJSON{Name: info.Name, Admin: info.Admin, LastLoginAt: stampPtr(info.LastLoginAt), Grants: map[string][]string{},
		GoneGrants: slices.Clone(info.GoneGrants), Tokens: []tokenJSON{}, Sessions: info.Sessions}
	if out.GoneGrants == nil {
		out.GoneGrants = []string{}
	}
	for job, p := range info.Grants {
		out.Grants[job] = append([]string{}, p.Names()...)
	}
	for _, t := range info.Tokens {
		out.Tokens = append(out.Tokens, toTokenJSON(t))
	}
	return out
}

func toTokenJSON(t *store.Token) tokenJSON {
	return tokenJSON{ID: t.ID, Label: t.Label, CreatedAt: stampOf(t.CreatedAt), LastUsedAt: stampPtr(t.LastUsedAt), ExpiresAt: stampPtr(t.ExpiresAt)}
}

func toSecretsJSON(rows []SecretRow) []secretJSON {
	out := make([]secretJSON, 0, len(rows))
	for _, r := range rows {
		jobs := append([]string{}, r.Jobs...)
		out = append(out, secretJSON{Name: r.Name, Kind: r.Kind, User: r.User, Scopes: append([]string{}, r.Scopes...), Narrow: r.Narrow, UpdatedAt: stampPtr(r.UpdatedAt), Jobs: jobs, Webhook: r.Webhook})
	}
	return out
}

func toServerJSON(info *ServerInfo) serverJSON {
	out := serverJSON{Version: info.Version, GoVersion: info.GoVersion, Schema: info.Schema, StartedAt: stampOf(info.StartedAt),
		Listen: info.Listen, BaseURL: info.BaseURL, Executors: info.Executors, Builds: info.Builds,
		DefaultRunsOn: append([]string{}, info.DefaultRunsOn...), AgentEnv: append([]string{}, info.AgentEnv...),
		AgentURL: info.AgentURL, AgentListen: info.AgentListen, Shell: info.Shell,
		SessionTTLSeconds: int64(info.SessionTTL / time.Second), TimeZone: info.TimeZone, DataDir: info.DataDir, JobsDir: info.JobsDir,
		LastReloadAt: stampOf(info.LastReloadAt), LastReloadError: info.LastReloadError, Jobs: info.Jobs}
	if info.FreeDiskKnown {
		out.FreeDiskBytes = new(info.FreeDisk)
	}
	if info.DBSize >= 0 {
		out.DBSizeBytes = new(info.DBSize)
	}
	return out
}

func (a *API) users(w http.ResponseWriter, r *http.Request, u *store.User) {
	all, err := a.svc.Users(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]userJSON, 0, len(all))
	for i := range all {
		out = append(out, toUserJSON(&all[i]))
	}
	writeList(w, out, "")
}

func (a *API) userDetail(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, err := a.svc.User(r.Context(), u, r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserJSON(info))
}

func (a *API) secrets(w http.ResponseWriter, r *http.Request, u *store.User) {
	v, err := a.svc.Secrets(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, secretsJSON{Set: toSecretsJSON(v.Set), Missing: toSecretsJSON(v.Missing), OutOfScope: toSecretsJSON(v.OutOfScope)})
}

func (a *API) server(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, err := a.svc.ServerInfo(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerJSON(info))
}

// auditQuery is how many audit rows a request asks for and the id of the row
// it asks for those older than, as buildsQuery is for builds.
func auditQuery(r *http.Request) (limit int, before int64, err error) {
	limit, at, err := pageArgs(r, "audit", 1)
	return limit, firstOf(at), err
}

func (a *API) audit(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit, before, err := auditQuery(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rows, more, err := a.svc.Audit(r.Context(), u, before, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]auditJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, auditJSON{ID: row.ID, At: stampOf(row.At), Actor: row.Actor, Action: row.Action, Target: row.Target, Detail: row.Detail})
	}
	next := ""
	if more {
		next = cursorFor("audit", rows[len(rows)-1].ID)
	}
	writeList(w, out, next)
}
