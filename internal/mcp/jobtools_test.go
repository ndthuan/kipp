package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
)

var jobToolNames = []string{"get_job_yaml", "list_job_versions", "get_job_version", "create_job", "replace_job", "delete_job", "restore_job"}

const freshJob = "name: fresh\nstages:\n  s: {run: echo fresh}\n"

func toolNames(t *testing.T, e *env, token string) []string {
	t.Helper()
	res, err := e.session(t, token).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func hasAny(names []string, wanted []string) bool {
	return slices.ContainsFunc(wanted, func(w string) bool { return slices.Contains(names, w) })
}

// atCeiling verifies every token as its user under the ceiling c.
func atCeiling(c Ceiling) (Verifier, *auth.Service) {
	au := new(auth.Service)
	return func(ctx context.Context, token string) (*Caller, error) {
		got, err := APITokens(au)(ctx, token)
		if got != nil {
			got.Ceiling = c
		}
		return got, err
	}, au
}

func TestTheJobToolsAreListedToAnAdminAtTheFullCeilingAndNoOneElse(t *testing.T) {
	e := newEnv(t, nil)
	if got := toolNames(t, e, e.admin); !slices.Contains(got, "replace_job") || !slices.Contains(got, "get_job_yaml") {
		t.Fatalf("an admin sees %v", got)
	}
	for name, token := range map[string]string{"a viewer": e.viewer, "a builder": e.builder, "a stopper": e.stopper} {
		if got := toolNames(t, e, token); hasAny(got, jobToolNames) {
			t.Errorf("%s sees job tools: %v", name, got)
		}
	}
	// A user who may view every job is still not an admin.
	all := e.token(t, "all", false, map[string][]string{"smoke": {"view", "build", "cancel"}, "other": {"view", "build", "cancel"}}, nil)
	if got := toolNames(t, e, all); hasAny(got, jobToolNames) {
		t.Errorf("a user with every grant sees job tools: %v", got)
	}
}

func TestAnAdminsLowerCeilingNeitherSeesNorCallsAJobTool(t *testing.T) {
	// A read-only grant, and a build grant: neither is the full ceiling.
	for name, c := range map[string]Ceiling{"view only": {View: true}, "no cancel": {View: true, Build: true}, "everything but jobs": {View: true, Build: true, Cancel: true}, "nothing": {}} {
		verify, au := atCeiling(c)
		e := newEnv(t, verify)
		*au = *e.au
		if got := toolNames(t, e, e.admin); hasAny(got, jobToolNames) {
			t.Errorf("%s: an admin sees %v", name, got)
		}
		res, err := e.session(t, e.admin).CallTool(context.Background(), &sdk.CallToolParams{Name: "replace_job",
			Arguments: map[string]any{"job": "smoke", "yaml": freshJob, "etag": "x"}})
		if err == nil && !res.IsError {
			t.Errorf("%s: replace_job went through", name)
		}
	}
}

func TestAnOAuthGrantWithTheJobsScopeIsServedTheJobToolsToAnAdminOnly(t *testing.T) {
	c := Ceiling{View: true, Jobs: true}
	verify, au := atCeiling(c)
	e := newEnv(t, verify)
	*au = *e.au
	if got := toolNames(t, e, e.admin); !slices.Contains(got, "replace_job") || !slices.Contains(got, "get_job_yaml") {
		t.Errorf("an admin with the jobs scope sees %v", got)
	}
	if got := toolNames(t, e, e.viewer); hasAny(got, jobToolNames) {
		t.Errorf("a non-admin with the jobs scope sees %v", got)
	}
	_, err := e.mcp.replaceJob(context.Background(), &Caller{User: e.mustUser(t, "viewer"), Ceiling: c}, replaceJobIn{Job: "smoke", YAML: freshJob, ETag: "x"})
	if !errors.Is(err, api.ErrForbidden) {
		t.Errorf("a non-admin with the jobs scope: %v", err)
	}
}

func TestTheJobToolsRefuseACallerTheCeilingForbidsEvenWhenTheyAreReached(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []*Caller{{User: e.mustUser(t, "viewer"), Ceiling: AllGrants}, {User: e.mustUser(t, "admin"), Ceiling: Ceiling{View: true}}} {
		_, err := e.mcp.replaceJob(context.Background(), c, replaceJobIn{Job: "smoke", YAML: freshJob, ETag: "x"})
		if !errors.Is(err, api.ErrForbidden) {
			t.Errorf("%s at %+v: %v", c.User.Name, c.Ceiling, err)
		}
	}
}

func TestAnAdminCreatesReadsChangesAndDeletesAJobAndItsVersionsAreKept(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)

	var made definitionOut
	ok(t, s, "create_job", map[string]any{"yaml": freshJob}, &made)
	if made.Job != "fresh" || made.YAML != freshJob || made.ETag == "" || made.Source != "store" {
		t.Fatalf("created %+v", made)
	}
	refused(t, s, "create_job", map[string]any{"yaml": freshJob}, "exists")
	refused(t, s, "create_job", map[string]any{"yaml": "stages: {}\n"}, "bad input")

	var read definitionOut
	ok(t, s, "get_job_yaml", map[string]any{"job": "fresh"}, &read)
	if read != made {
		t.Fatalf("read %+v, created %+v", read, made)
	}

	changed := strings.Replace(freshJob, "echo fresh", "echo changed", 1)
	var second definitionOut
	ok(t, s, "replace_job", map[string]any{"job": "fresh", "yaml": changed, "etag": made.ETag}, &second)
	if second.YAML != changed || second.ETag == made.ETag {
		t.Fatalf("replaced %+v", second)
	}
	refused(t, s, "replace_job", map[string]any{"job": "fresh", "yaml": changed}, "etag")

	var versions jobVersionsOut
	ok(t, s, "list_job_versions", map[string]any{"job": "fresh"}, &versions)
	if len(versions.Versions) != 2 || versions.Versions[0].How != "mcp" || versions.Versions[0].Author != "admin" {
		t.Fatalf("versions %+v", versions)
	}
	oldest := versions.Versions[1].ID
	ok(t, s, "list_job_versions", map[string]any{"job": "fresh", "limit": 1}, &versions)
	if len(versions.Versions) != 1 || versions.NextBefore != versions.Versions[0].ID {
		t.Fatalf("a page of one: %+v", versions)
	}
	var one versionOut
	ok(t, s, "get_job_version", map[string]any{"job": "fresh", "version": oldest}, &one)
	if one.YAML != freshJob {
		t.Fatalf("version %+v", one)
	}

	var restored definitionOut
	refused(t, s, "restore_job", map[string]any{"job": "fresh", "version": oldest}, "ETag")
	ok(t, s, "restore_job", map[string]any{"job": "fresh", "version": oldest, "etag": second.ETag}, &restored)
	if restored.YAML != freshJob {
		t.Fatalf("restored %+v", restored)
	}

	refused(t, s, "delete_job", map[string]any{"job": "fresh"}, "etag")
	var gone deletedOut
	ok(t, s, "delete_job", map[string]any{"job": "fresh", "etag": restored.ETag}, &gone)
	if !gone.Deleted {
		t.Fatalf("deleted %+v", gone)
	}
	refused(t, s, "get_job_yaml", map[string]any{"job": "fresh"}, "not found")
	ok(t, s, "list_job_versions", map[string]any{"job": "fresh"}, &versions)
	if len(versions.Versions) != 4 || !versions.Versions[0].Deleted {
		t.Fatalf("versions after a delete: %+v", versions)
	}
	// A deleted job comes back without an etag.
	ok(t, s, "restore_job", map[string]any{"job": "fresh", "version": versions.Versions[1].ID}, &restored)

	auditHas(t, e, "job.create", "job.update", "job.revert", "job.delete")
}

func auditHas(t *testing.T, e *env, wants ...string) {
	t.Helper()
	audit, err := e.st.AuditBefore(context.Background(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range audit {
		actions = append(actions, a.Action)
	}
	for _, want := range wants {
		if !slices.Contains(actions, want) {
			t.Errorf("no audit row %s in %v", want, actions)
		}
	}
}

func TestAStaleEtagIsRefusedWithTheCurrentTextAndEtag(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	var cur definitionOut
	ok(t, s, "get_job_yaml", map[string]any{"job": "other"}, &cur)
	refused(t, s, "replace_job", map[string]any{"job": "other", "yaml": cur.YAML, "etag": `"stale"`}, cur.ETag)
	refused(t, s, "delete_job", map[string]any{"job": "other", "etag": `"stale"`}, "echo other")
}

func TestARefusedJobSetReturnsEveryError(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	var cur definitionOut
	ok(t, s, "get_job_yaml", map[string]any{"job": "other"}, &cur)
	refused(t, s, "replace_job", map[string]any{"job": "other", "yaml": "name: other\nstages: {a: {}}\n", "etag": cur.ETag}, "stage a")

	err := e.mcp.jobFailure(context.Background(), &Caller{User: e.mustUser(t, "admin"), Ceiling: AllGrants}, "other",
		&config.Errors{Items: []error{errors.New("a.yml: first"), errors.New("b.yml: second")}})
	for _, want := range []string{"- a.yml: first", "- b.yml: second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
}

func TestInOffModeTheJobsCanBeReadAndNoToolChangesOne(t *testing.T) {
	e := newEnvMode(t, nil, "", jobstore.Off)
	got := toolNames(t, e, e.admin)
	if !slices.Contains(got, "get_job_yaml") {
		t.Fatalf("reading is gone in off mode: %v", got)
	}
	for _, w := range []string{"create_job", "replace_job", "delete_job", "restore_job"} {
		if slices.Contains(got, w) {
			t.Errorf("%s is listed in off mode", w)
		}
	}
}
