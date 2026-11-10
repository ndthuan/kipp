package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func TestTheBuildPageLinksAReusedStageToTheGreenRunItWasReusedFrom(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"gate", "ship"}, nil))
	must(t, e.st.ReuseStage(ctx, b.ID, "gate", "local", store.StageRef{Job: "ci.site.pr-12", Number: 3, Stage: "run"}, time.Now()))
	must(t, e.st.SetStage(ctx, b.ID, "ship", store.StageSuccess, new(0), time.Now()))
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke/"+strconv.Itoa(b.Number))
	contains(t, page, `>success</span><span class="sub">, reused from <a class="link" href="/jobs/ci.site.pr-12/3#log-run">ci.site.pr-12 #3</a></span>`)
	if strings.Count(page, "reused from") != 1 {
		t.Fatalf("a stage that ran says it was reused:\n%s", page)
	}
}
