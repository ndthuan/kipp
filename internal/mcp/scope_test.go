package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/jobstore"
)

func toolCall(name string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{"job":"smoke"}}}`
}

func TestAnAdminsJobToolCallWithoutTheJobsScopeIsA403ThatAsksForItAndKeepsTheRest(t *testing.T) {
	verify, au := atCeiling(Ceiling{View: true, Build: true, Cancel: true})
	e := newEnvAt(t, verify, metadataURL)
	*au = *e.au
	for _, tool := range jobToolNames {
		res := e.post(t, e.admin, toolCall(tool))
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d", tool, res.StatusCode)
		}
		want := `Bearer error="insufficient_scope", scope="kipp:read kipp:build kipp:jobs", resource_metadata="` + metadataURL + `"`
		if got := res.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate %q", tool, got)
		}
	}
	if res := e.post(t, e.viewer, toolCall("create_job")); res.StatusCode == http.StatusForbidden {
		t.Errorf("a viewer is asked for kipp:jobs: %s", res.Header.Get("WWW-Authenticate"))
	}
}

func TestTheScopeAskedForKeepsTheScopesTheGrantHas(t *testing.T) {
	verify, au := atCeiling(Ceiling{View: true, Jobs: true})
	e := newEnvAt(t, verify, metadataURL)
	*au = *e.au
	res := e.post(t, e.admin, toolCall("run_build"))
	want := `Bearer error="insufficient_scope", scope="kipp:read kipp:build kipp:jobs", resource_metadata="` + metadataURL + `"`
	if res.StatusCode != http.StatusForbidden || res.Header.Get("WWW-Authenticate") != want {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
}

func TestEveryToolThatIsNotReadOnlyIsKnownToTheScopeGate(t *testing.T) {
	e := newEnv(t, nil)
	res, err := e.session(t, e.admin).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := toolNames(t, e, e.viewer)
	var jobs []string
	for _, tool := range res.Tools {
		_, acts := toolActions[tool.Name]
		switch {
		case !slices.Contains(plain, tool.Name):
			jobs = append(jobs, tool.Name)
			if !jobTools[tool.Name] {
				t.Errorf("%s is served to admins only and is not among the job tools", tool.Name)
			}
		case !tool.Annotations.ReadOnlyHint && !acts:
			t.Errorf("%s changes something and the scope gate does not know it", tool.Name)
		case tool.Annotations.ReadOnlyHint && acts:
			t.Errorf("%s reads and the scope gate holds it to a scope", tool.Name)
		}
	}
	if len(jobs) != len(jobTools) {
		t.Errorf("job tools served %v, the gate knows %d", jobs, len(jobTools))
	}
}

func TestARefusalByTheCeilingNamesTheScopeAndOneByTheGrantsNamesTheGrant(t *testing.T) {
	defer func(m map[string]string) { toolActions = m }(toolActions)
	toolActions = nil
	verify, au := atCeiling(Ceiling{View: true})
	e := newEnv(t, verify)
	*au = *e.au
	refused(t, e.session(t, e.admin), "run_build", map[string]any{"job": "smoke"}, "not granted kipp:build")
	plain := newEnv(t, nil)
	refused(t, plain.session(t, plain.viewer), "run_build", map[string]any{"job": "smoke"}, "lacks the grant")

	_, err := e.mcp.replaceJob(context.Background(), &Caller{User: e.mustUser(t, "admin"), Ceiling: Ceiling{View: true}}, replaceJobIn{Job: "smoke", YAML: freshJob, ETag: "x"})
	if !errors.Is(err, api.ErrForbidden) || !strings.Contains(e.mcp.refusal(err).Error(), "not granted kipp:jobs") {
		t.Fatalf("an admin without kipp:jobs: %v", err)
	}
}

type records struct{ got []slog.Record }

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.got = append(r.got, rec)
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

func TestACallTheClientGaveUpOnIsNotLoggedAsAServerFault(t *testing.T) {
	logged := &records{}
	s := New(Config{Logger: slog.New(logged)})
	err := s.refusal(fmt.Errorf("waiting: %w", context.Canceled))
	if err == nil || !strings.Contains(err.Error(), "cancelled") || len(logged.got) != 0 {
		t.Fatalf("%v, logged %d", err, len(logged.got))
	}
	if s.refusal(errors.New("disk on fire")).Error() != "internal error" || len(logged.got) != 1 {
		t.Fatal("a real fault is no longer logged")
	}
}

func TestListJobVersionsRefusesALimitOutOfRange(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	for _, limit := range []int{-1, maxVersions + 1} {
		refused(t, s, "list_job_versions", map[string]any{"job": "smoke", "limit": limit}, fmt.Sprintf("limit is 1 to %d", maxVersions))
	}
	ok(t, s, "list_job_versions", map[string]any{"job": "smoke", "limit": maxVersions}, nil)
}

func TestTheInstructionsTellAnAdminHowToChangeAJobAndNoOneElse(t *testing.T) {
	e := newEnv(t, nil)
	if got := e.session(t, e.admin).InitializeResult().Instructions; !strings.Contains(got, "get_job_yaml") || !strings.Contains(got, "etag") {
		t.Errorf("an admin reads %q", got)
	}
	if got := e.session(t, e.viewer).InitializeResult().Instructions; strings.Contains(got, "get_job_yaml") {
		t.Errorf("a viewer reads %q", got)
	}
	ro := newEnvMode(t, nil, "", jobstore.Off)
	if got := ro.session(t, ro.admin).InitializeResult().Instructions; !strings.Contains(got, "get_job_yaml") || strings.Contains(got, "replace_job") {
		t.Errorf("with edits off an admin reads %q", got)
	}
}
