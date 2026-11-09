package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { must(t, s.Close()) })
	return s
}

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildNumbersCountPerJob(t *testing.T) {
	s := open(t)
	a1 := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	a2 := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	b1 := &Build{Job: "b", Status: Queued, Trigger: "cron"}
	for _, b := range []*Build{a1, a2, b1} {
		if err := s.CreateBuild(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if a1.Number != 1 || a2.Number != 2 || b1.Number != 1 || a1.ID == 0 || a1.QueuedAt.IsZero() {
		t.Fatalf("%+v %+v %+v", a1, a2, b1)
	}
	got, err := s.BuildByNumber(ctx, "a", 2)
	if err != nil || got.ID != a2.ID {
		t.Fatal(err, got)
	}
	if _, err := s.Build(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestStatusTransitionsStampTimes(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Queued, Trigger: "cli", Params: map[string]string{"TIER": "full"}}
	must(t, s.CreateBuild(ctx, b))
	t1 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	must(t, s.SetBuildStatus(ctx, b.ID, Running, t1))
	must(t, s.SetBuildStatus(ctx, b.ID, Success, t1.Add(time.Minute)))
	got, err := s.Build(ctx, b.ID)
	must(t, err)
	if got.Status != Success || got.StartedAt == nil || !got.StartedAt.Equal(t1) || got.FinishedAt == nil || got.Params["TIER"] != "full" {
		t.Fatalf("%+v", got)
	}
}

func TestMarkRunningLostLeavesQueuedBuildsQueued(t *testing.T) {
	s := open(t)
	r := &Build{Job: "a", Status: Running, Trigger: "cli"}
	q := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, r))
	must(t, s.CreateBuild(ctx, q))
	n, stages, err := s.MarkRunningLost(ctx, time.Now())
	if err != nil || n != 1 || len(stages) != 0 {
		t.Fatal(err, n, stages)
	}
	queued, err := s.BuildsWithStatus(ctx, Queued)
	must(t, err)
	if len(queued) != 1 || queued[0].ID != q.ID {
		t.Fatalf("%+v", queued)
	}
	lost, err := s.Build(ctx, r.ID)
	must(t, err)
	if lost.Status != Lost || lost.FinishedAt == nil {
		t.Fatalf("%+v", lost)
	}
}

func TestStagesComeBackInTheOrderPut(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"lint", "unit", "smoke"}, nil))
	code := 1
	must(t, s.SetStage(ctx, b.ID, "unit", StageFailed, &code, time.Now()))
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	if len(st) != 3 || st[0].Name != "lint" || st[1].Status != StageFailed || *st[1].ExitCode != 1 || st[2].Status != StagePending {
		t.Fatalf("%+v", st)
	}
}

func TestAStageKeepsTheNeedsItsBuildRanWith(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"lint", "unit", "smoke"}, map[string][]string{"smoke": {"lint", "unit"}, "unit": {}}))
	must(t, s.SetStage(ctx, b.ID, "smoke", StageRunning, nil, time.Now()))
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	if len(st) != 3 || !slices.Equal(st[2].Needs, []string{"lint", "unit"}) {
		t.Fatalf("smoke needs %v", st[2].Needs)
	}
	for _, root := range st[:2] {
		if root.Needs == nil || len(root.Needs) != 0 {
			t.Fatalf("%s needs %#v, want an empty list", root.Name, root.Needs)
		}
	}
}

func TestAGrantReplacesAndTokensAndSessionsResolveUntilDeletedOrExpired(t *testing.T) {
	s := open(t)
	u, err := s.CreateUser(ctx, "agent", "h", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "agent", "h", false); err == nil {
		t.Fatal("duplicate user")
	}
	must(t, s.Grant(ctx, u.ID, "site-smoke", []string{"view", "build", "cancel"}))
	must(t, s.Grant(ctx, u.ID, "site-smoke", []string{"view"})) // replaces
	must(t, s.Grant(ctx, u.ID, "other", []string{"view", "build"}))
	must(t, s.Grant(ctx, u.ID, "other", nil))
	perms, err := s.Permissions(ctx, u.ID)
	must(t, err)
	if p := perms["site-smoke"]; !p.View || p.Build || len(perms) != 1 {
		t.Fatalf("%+v", perms)
	}
	tok, err := s.CreateToken(ctx, u.ID, "ci", "tokenhash")
	must(t, err)
	got, err := s.UserByTokenHash(ctx, "tokenhash")
	if err != nil || got.ID != u.ID {
		t.Fatal(err)
	}
	toks, err := s.Tokens(ctx, u.ID)
	must(t, err)
	if len(toks) != 1 || toks[0].ID != tok.ID || toks[0].LastUsedAt == nil {
		t.Fatalf("%+v", toks)
	}
	must(t, s.DeleteToken(ctx, tok.ID))
	if _, err := s.UserByTokenHash(ctx, "tokenhash"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	must(t, s.CreateSession(ctx, &Session{ID: "sid", UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}))
	must(t, s.CreateSession(ctx, &Session{ID: "old", UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(-time.Hour)}))
	if sess, err := s.Session(ctx, "sid"); err != nil || sess.UserID != u.ID {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session returned")
	}
}

func TestSecretsTestsAndActivityComeBackAsStored(t *testing.T) {
	s := open(t)
	must(t, s.PutSecret(ctx, "TOKEN", []byte{1, 2}))
	must(t, s.PutSecret(ctx, "TOKEN", []byte{3}))
	must(t, s.PutSecret(ctx, "ALPHA", []byte{4}))
	names, err := s.SecretNames(ctx)
	must(t, err)
	v, err := s.Secret(ctx, "TOKEN")
	must(t, err)
	if len(names) != 2 || names[0] != "ALPHA" || string(v) != "\x03" {
		t.Fatal(names, v)
	}
	must(t, s.DeleteSecret(ctx, "ALPHA"))
	if _, err := s.Secret(ctx, "ALPHA"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	b := &Build{Job: "a", Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutTests(ctx, b.ID, []Test{{Stage: "smoke", Suite: "parser", Name: "parses", Status: "passed", DurationMs: 12}, {Stage: "smoke", Suite: "parser", Name: "rejects", Status: "failed", Message: "no"}}))
	tests, err := s.Tests(ctx, b.ID)
	must(t, err)
	hist, err := s.TestHistory(ctx, "a", 10)
	must(t, err)
	if len(tests) != 2 || len(hist) != 2 || hist[0].BuildNumber != 1 {
		t.Fatalf("%+v %+v", tests, hist)
	}
	must(t, s.AddActivity(ctx, "a", "dropped a cron fire", time.Now()))
	act, err := s.Activity(ctx, "a", 5)
	must(t, err)
	if len(act) != 1 || act[0].Line != "dropped a cron fire" {
		t.Fatalf("%+v", act)
	}
}

func TestOldBuildsByCountAndByAge(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 5 {
		b := &Build{Job: "a", Status: Success, Trigger: "cli", QueuedAt: now.AddDate(0, 0, -10+i)}
		must(t, s.CreateBuild(ctx, b))
		ids = append(ids, b.ID)
	}
	byCount, err := s.OldBuilds(ctx, "a", 2, 0, now)
	must(t, err)
	if len(byCount) != 3 || byCount[0] != ids[0] {
		t.Fatalf("%v", byCount)
	}
	byAge, err := s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(byAge) != 2 {
		t.Fatalf("%v", byAge)
	}
	running := &Build{Job: "a", Status: Running, Trigger: "cli", QueuedAt: now.AddDate(0, 0, -30)}
	must(t, s.CreateBuild(ctx, running))
	byAge2, err := s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(byAge2) != 2 {
		t.Fatal("a running build was offered for deletion")
	}
	must(t, s.DeleteBuild(ctx, ids[0]))
	if _, err := s.Build(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestTimesCompareAtSubSecondWidth(t *testing.T) {
	s := open(t)
	u, err := s.CreateUser(ctx, "u", "h", true)
	must(t, err)
	base := time.Now().Truncate(time.Second).Add(time.Hour)
	must(t, s.CreateSession(ctx, &Session{ID: "half", UserID: u.ID, CSRF: "c", ExpiresAt: base.Add(500 * time.Millisecond)}))
	must(t, s.CreateSession(ctx, &Session{ID: "whole", UserID: u.ID, CSRF: "c", ExpiresAt: base}))
	got, err := s.Session(ctx, "half")
	must(t, err)
	if !got.ExpiresAt.Equal(base.Add(500 * time.Millisecond)) {
		t.Fatalf("expires %v", got.ExpiresAt)
	}
	must(t, s.DeleteExpiredSessions(ctx, base)) // whole second: "whole" is due, "half" is not
	if _, err := s.Session(ctx, "half"); err != nil {
		t.Fatalf("half-second session swept early: %v", err)
	}
	must(t, s.DeleteExpiredSessions(ctx, base.Add(510*time.Millisecond)))
	var n int
	must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d sessions left after sweep past .51s", n)
	}

	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -8)
	b := &Build{Job: "a", Status: Success, Trigger: "cli", QueuedAt: cutoff.Add(-500 * time.Millisecond)}
	must(t, s.CreateBuild(ctx, b))
	old, err := s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(old) != 1 {
		t.Fatalf("build queued half a second before the cutoff not offered: %v", old)
	}
	b2 := &Build{Job: "a", Status: Success, Trigger: "cli", QueuedAt: cutoff.Add(500 * time.Millisecond)}
	must(t, s.CreateBuild(ctx, b2))
	old, err = s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(old) != 1 {
		t.Fatalf("build queued half a second after the cutoff offered: %v", old)
	}
}

func TestMarkRunningLostCancelsRunningStages(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"one", "two", "three"}, nil))
	must(t, s.SetStage(ctx, b.ID, "one", StageSuccess, nil, time.Now()))
	must(t, s.SetStage(ctx, b.ID, "two", StageRunning, nil, time.Now()))
	n, _, err := s.MarkRunningLost(ctx, time.Now())
	must(t, err)
	if n != 1 {
		t.Fatalf("lost %d", n)
	}
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	if st[0].Status != StageSuccess || st[1].Status != StageCancelled || st[1].FinishedAt == nil || st[2].Status != StagePending {
		t.Fatalf("%+v %+v %+v", st[0], st[1], st[2])
	}
}

func TestBuildNumbersAreNeverReused(t *testing.T) {
	s := open(t)
	var last *Build
	for range 3 {
		last = &Build{Job: "a", Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, last))
	}
	must(t, s.DeleteBuild(ctx, last.ID))
	next := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, next))
	if next.Number != 4 {
		t.Fatalf("number %d reused", next.Number)
	}
}

func TestUnknownRowsAreNotFound(t *testing.T) {
	s := open(t)
	if err := s.SetBuildStatus(ctx, 999, Running, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("build: %v", err)
	}
	b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	if err := s.SetStage(ctx, b.ID, "nope", StageRunning, nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stage: %v", err)
	}
}

func TestCorruptTimestampIsAnError(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	_, err := s.db.ExecContext(ctx, `UPDATE builds SET started_at = 'garbage' WHERE id = ?`, b.ID)
	must(t, err)
	if _, err := s.Build(ctx, b.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestUsersAdminGrantValidationAndCascade(t *testing.T) {
	s := open(t)
	u, err := s.CreateUser(ctx, "root", "h", true)
	must(t, err)
	got, err := s.UserByName(ctx, "root")
	must(t, err)
	if !got.Admin || got.ID != u.ID {
		t.Fatalf("%+v", got)
	}
	must(t, s.Grant(ctx, u.ID, "a", []string{"view"}))
	if err := s.Grant(ctx, u.ID, "a", []string{"root"}); err == nil {
		t.Fatal("invalid perm accepted")
	}
	perms, err := s.Permissions(ctx, u.ID)
	must(t, err)
	if !perms["a"].View {
		t.Fatalf("a refused grant dropped the old set: %+v", perms)
	}
	_, err = s.CreateToken(ctx, u.ID, "l", "th")
	must(t, err)
	must(t, s.CreateSession(ctx, &Session{ID: "s", UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}))
	_, err = s.CreateUser(ctx, "other", "h", true) // root is not the last admin
	must(t, err)
	must(t, s.DeleteUser(ctx, u.ID))
	var n int
	must(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM permissions) + (SELECT COUNT(*) FROM tokens) + (SELECT COUNT(*) FROM sessions)`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d rows survived the user", n)
	}
}

func TestDeleteBuildCascades(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"x"}, nil))
	must(t, s.PutTests(ctx, b.ID, []Test{{Stage: "x", Suite: "s", Name: "n", Status: "passed"}}))
	must(t, s.DeleteBuild(ctx, b.ID))
	var n int
	must(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM stages) + (SELECT COUNT(*) FROM tests)`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d rows survived the build", n)
	}
}

func TestBuildsOrderAndLastBuilds(t *testing.T) {
	s := open(t)
	var a2, b1 *Build
	for _, j := range []string{"a", "a", "b"} {
		b := &Build{Job: j, Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		if j == "a" {
			a2 = b
		} else {
			b1 = b
		}
	}
	list, err := s.Builds(ctx, "a", 10)
	must(t, err)
	if len(list) != 2 || list[0].Number != 2 || list[1].Number != 1 {
		t.Fatalf("not newest first: %+v", list)
	}
	last, err := s.LastBuilds(ctx)
	must(t, err)
	if len(last) != 2 || last["a"].ID != a2.ID || last["b"].ID != b1.ID {
		t.Fatalf("%+v", last)
	}
}

func TestRecentFinishedGivesAtMostNFinishedBuildsOfEachJobNewestFirst(t *testing.T) {
	s := open(t)
	for _, st := range []Status{Failed, Success, Success, Unstable, Cancelled, Failed, Success, Running, Queued} {
		b := &Build{Job: "a", Status: st, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
	}
	b1 := &Build{Job: "b", Status: Timeout, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b1))
	must(t, s.CreateBuild(ctx, &Build{Job: "c", Status: Running, Trigger: "cli"}))
	got, err := s.RecentFinished(ctx, 5)
	must(t, err)
	if len(got) != 2 || got["c"] != nil {
		t.Fatalf("a job with nothing finished has no entry, got %+v", got)
	}
	var numbers []int
	for _, b := range got["a"] {
		numbers = append(numbers, b.Number)
	}
	if !slices.Equal(numbers, []int{7, 6, 5, 4, 3}) {
		t.Fatalf("not the five newest finished, newest first: %v", numbers)
	}
	if len(got["b"]) != 1 || got["b"][0].ID != b1.ID {
		t.Fatalf("b: %+v", got["b"])
	}
}

func TestRecentColouredSkipsCancelledRunningAndQueuedAndKeepsNBuildsNewestFirst(t *testing.T) {
	s := open(t)
	for _, st := range []Status{Failed, Cancelled, Success, Unstable, Lost, Timeout, Running, Queued} {
		must(t, s.CreateBuild(ctx, &Build{Job: "a", Status: st, Trigger: "cli"}))
	}
	must(t, s.CreateBuild(ctx, &Build{Job: "b", Status: Cancelled, Trigger: "cli"}))
	got, err := s.RecentColoured(ctx, 4)
	must(t, err)
	if len(got) != 1 || got["b"] != nil {
		t.Fatalf("a job with no coloured build has no entry, got %+v", got)
	}
	var numbers []int
	for _, b := range got["a"] {
		numbers = append(numbers, b.Number)
	}
	if !slices.Equal(numbers, []int{6, 5, 4, 3}) {
		t.Fatalf("not the four newest coloured, newest first: %v", numbers)
	}
}

func TestGreenIsSuccessOrUnstableAndRedIsFailedTimeoutOrLost(t *testing.T) {
	green := map[Status]bool{Success: true, Unstable: true}
	red := map[Status]bool{Failed: true, Timeout: true, Lost: true}
	for _, st := range []Status{Queued, Running, Success, Unstable, Failed, Cancelled, Lost, Timeout} {
		if st.Green() != green[st] || st.Red() != red[st] {
			t.Errorf("%s: green %v red %v", st, st.Green(), st.Red())
		}
	}
}

func TestMarkRunningLostNamesTheStagesThatWereRunningAndTheirProcessGroups(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"done", "recorded", "unrecorded", "waiting"}, nil))
	must(t, s.SetStage(ctx, b.ID, "done", StageRunning, nil, time.Now()))
	must(t, s.SetStageGroup(ctx, b.ID, "done", 41, "then"))
	must(t, s.SetStage(ctx, b.ID, "done", StageSuccess, nil, time.Now()))
	must(t, s.SetStage(ctx, b.ID, "recorded", StageRunning, nil, time.Now()))
	must(t, s.SetStageGroup(ctx, b.ID, "recorded", 4242, "1760000000.000123"))
	must(t, s.SetStage(ctx, b.ID, "unrecorded", StageRunning, nil, time.Now()))
	if err := s.SetStageGroup(ctx, b.ID, "no-such-stage", 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stage that is not there: %v", err)
	}
	_, stages, err := s.MarkRunningLost(ctx, time.Now())
	must(t, err)
	want := []LostStage{
		{Job: "a", Number: b.Number, Name: "recorded", Pgid: 4242, Born: "1760000000.000123"},
		{Job: "a", Number: b.Number, Name: "unrecorded"},
	}
	if !slices.Equal(stages, want) {
		t.Fatalf("got %+v, want %+v", stages, want)
	}
	if _, again, err := s.MarkRunningLost(ctx, time.Now()); err != nil || len(again) != 0 {
		t.Fatalf("a second pass: %+v, %v", again, err)
	}
}

// Every request with a token would otherwise be a write to the database.
func TestATokensLastUseIsWrittenAtMostOnceAMinute(t *testing.T) {
	s := open(t)
	u, err := s.CreateUser(ctx, "agent", "h", false)
	must(t, err)
	tok, err := s.CreateToken(ctx, u.ID, "ci", "hash")
	must(t, err)
	lastUsed := func() time.Time {
		t.Helper()
		tokens, err := s.Tokens(ctx, u.ID)
		must(t, err)
		if len(tokens) != 1 || tokens[0].ID != tok.ID || tokens[0].LastUsedAt == nil {
			t.Fatalf("%+v", tokens)
		}
		return *tokens[0].LastUsedAt
	}
	use := func() {
		t.Helper()
		got, err := s.UserByTokenHash(ctx, "hash")
		if err != nil || got.ID != u.ID {
			t.Fatal(got, err)
		}
	}
	use()
	first := lastUsed()
	if time.Since(first) > time.Minute {
		t.Fatalf("the first use was recorded as %v", first)
	}
	use()
	if got := lastUsed(); !got.Equal(first) {
		t.Fatalf("a second use within the minute was written: %v after %v", got, first)
	}
	// As if the last use were two minutes ago.
	old := time.Now().Add(-2 * time.Minute)
	_, err = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, ts(old), tok.ID)
	must(t, err)
	use()
	if got := lastUsed(); !got.After(first) {
		t.Fatalf("a use a minute later was not written: %v", got)
	}
	if _, err := s.UserByTokenHash(ctx, "no such hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token nobody holds: %v", err)
	}
}

func TestAnUnstableBuildAndStageComeBackAsStoredAndAreOver(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"lint", "unit"}, nil))
	t1 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	must(t, s.SetBuildStatus(ctx, b.ID, Running, t1))
	four := 4
	must(t, s.SetStage(ctx, b.ID, "lint", StageRunning, nil, t1))
	must(t, s.SetStage(ctx, b.ID, "lint", StageUnstable, &four, t1.Add(time.Second)))
	must(t, s.SetBuildStatus(ctx, b.ID, Unstable, t1.Add(time.Minute)))

	var word string
	must(t, s.db.QueryRowContext(ctx, `SELECT b.status || ' ' || s.status FROM builds b JOIN stages s ON s.build_id = b.id WHERE s.name = 'lint'`).Scan(&word))
	if word != "unstable unstable" {
		t.Fatalf("stored as %q: the word is what the API and the pages show", word)
	}
	got, err := s.Build(ctx, b.ID)
	must(t, err)
	if got.Status != Unstable || got.FinishedAt == nil || !got.FinishedAt.Equal(t1.Add(time.Minute)) {
		t.Fatalf("%+v", got)
	}
	stages, err := s.Stages(ctx, b.ID)
	must(t, err)
	if st := stages[0]; st.Status != StageUnstable || st.ExitCode == nil || *st.ExitCode != 4 || st.FinishedAt == nil {
		t.Fatalf("%+v", st)
	}
	if byStatus, err := s.BuildsWithStatus(ctx, Unstable); err != nil || len(byStatus) != 1 {
		t.Fatalf("%v %v", byStatus, err)
	}
	if active, err := s.BuildsWithStatus(ctx, Queued, Running); err != nil || len(active) != 0 {
		t.Fatalf("an unstable build counts as queued or running: %v %v", active, err)
	}
	if last, err := s.LastBuilds(ctx); err != nil || last["a"].Status != Unstable {
		t.Fatalf("%v %v", last, err)
	}
}

func TestAnUnstableBuildIsNeitherLostAtStartNorKeptByRetention(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	b := &Build{Job: "a", Status: Unstable, Trigger: "cli", QueuedAt: now.AddDate(0, 0, -10)}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"lint"}, nil))
	must(t, s.SetStage(ctx, b.ID, "lint", StageUnstable, nil, now))
	n, lost, err := s.MarkRunningLost(ctx, now)
	if err != nil || n != 0 || len(lost) != 0 {
		t.Fatalf("lost %d builds, stages %v, err %v", n, lost, err)
	}
	stages, err := s.Stages(ctx, b.ID)
	must(t, err)
	if stages[0].Status != StageUnstable {
		t.Fatalf("the stage is now %s", stages[0].Status)
	}
	old, err := s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if !slices.Equal(old, []int64{b.ID}) {
		t.Fatalf("an unstable build past its keep rule is not offered for deletion: %v", old)
	}
}

// Every status is listed, so that a new one cannot take a side unnoticed.
func TestEachStatusSaysWhetherItIsTerminalAndWhetherItSucceeded(t *testing.T) {
	cases := []struct {
		status              Status
		terminal, succeeded bool
	}{
		{Queued, false, false},
		{Running, false, false},
		{Success, true, true},
		{Unstable, true, true},
		{Failed, true, false},
		{Cancelled, true, false},
		{Lost, true, false},
		{Timeout, true, false},
	}
	for _, c := range cases {
		if c.status.Terminal() != c.terminal || c.status.Succeeded() != c.succeeded {
			t.Errorf("%s: terminal %v succeeded %v, want %v %v", c.status, c.status.Terminal(), c.status.Succeeded(), c.terminal, c.succeeded)
		}
	}
}

func TestBuildsBeforeANumberAreTheOlderOnesNewestFirst(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for range 5 {
		if err := s.CreateBuild(ctx, &Build{Job: "a", Status: Success, Trigger: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateBuild(ctx, &Build{Job: "b", Status: Success, Trigger: "cli"}); err != nil {
		t.Fatal(err)
	}
	numbers := func(before, limit int) []int {
		t.Helper()
		builds, err := s.BuildsBefore(ctx, "a", before, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, b := range builds {
			out = append(out, b.Number)
		}
		return out
	}
	if got := numbers(4, 2); !slices.Equal(got, []int{3, 2}) {
		t.Fatalf("before 4, two at most: %v", got)
	}
	if got := numbers(1, 2); len(got) != 0 {
		t.Fatalf("before the first: %v", got)
	}
}

func TestABuildKeepsTheTagItWasPushedFor(t *testing.T) {
	s := open(t)
	b := &Build{Job: "rel", Status: Queued, Trigger: "webhook", TriggerDetail: "abc", TriggerTag: "v1.2.0"}
	if err := s.CreateBuild(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Build(ctx, b.ID)
	if err != nil || got.TriggerTag != "v1.2.0" || got.TriggerBranch != "" {
		t.Fatalf("%+v, %v", got, err)
	}
}
