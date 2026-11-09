// Command demo fills a data directory and a jobs directory with a made-up
// installation, so that a kipp started on them shows a full front page: some
// seventy jobs in a few groups, days of builds that overlap and queue on a
// limited number of slots, a few jobs that fail, flap or end unstable, and
// builds waiting to run. It is a tool for the one who changes kipp, for
// timing the overview on a store of a realistic size and for taking
// screenshots, and the kipp binary does not contain it.
//
//	go run ./internal/devtools/demo -dir /tmp/kipp-demo
//	go run ./cmd/kipp user add demo --admin --data /tmp/kipp-demo/data
//	go run ./cmd/kipp serve --data /tmp/kipp-demo/data --jobs /tmp/kipp-demo/jobs --builds 4
//
// The same seed and the same time make the same store; the builds end at the
// moment the tool is run, so a store of yesterday has an empty last day.
package main

import (
	"cmp"
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the driver the store uses

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// slots is the limit of builds running at once the demo is made for; serve
// it with --builds 4 for the overview to say the same.
const slots = 4

// tsLayout is how the store writes a time.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", "", "the directory to make: data/ and jobs/ go in it, and it must not exist")
	jobs := fs.Int("jobs", 70, "how many jobs")
	days := fs.Int("days", 14, "how many days of builds, ending now")
	perDay := fs.Int("per-day", 20, "builds per job per day, on average")
	seed := fs.Uint64("seed", 1, "the random seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *jobs < 1 || *days < 1 || *perDay < 1 {
		return errors.New("-dir is required, and -jobs, -days and -per-day are 1 or more")
	}
	if _, err := os.Stat(*dir); err == nil {
		return fmt.Errorf("%s exists: the demo goes into a directory of its own", *dir)
	}
	// A seeded generator on purpose: one seed makes one store.
	d := &demo{rng: rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15)), now: now.UTC()}
	d.makeJobs(*jobs)
	n := d.makeBuilds(*days, *perDay)
	d.makeChildren()
	if err := d.write(*dir); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "%d jobs and %d builds in %s\n", len(d.jobs), n, *dir)
	return err
}

// profile is how a job has been doing lately.
type profile int

const (
	steady   profile = iota // green, with a failure now and then
	flaky                   // flips colour all the time
	broken                  // the newest builds failed
	unstable                // the newest builds are unstable
	long                    // runs for a quarter of an hour, with a build waiting
)

type job struct {
	name, group string
	cron        string
	profile     profile
	typical     time.Duration // how long a build usually takes
	pass        float64       // how often a build ends green
	long        bool
}

type build struct {
	job                    *job
	status                 store.Status
	queued, started, ended time.Time
	trigger                string
	// parent is the multibranch job of a build of one of its children.
	parent string
}

type demo struct {
	rng    *rand.Rand
	now    time.Time
	jobs   []*job
	builds []*build
	// children are the branches and pull requests of the multibranch job.
	children []store.Child
}

// multibranch is the one multibranch job of the demo, in the group backend.
const multibranch = "monorepo"

// makeChildren gives the multibranch job a live branch and pull requests
// and several pull requests that are gone, some of which built before they
// went: the dashboard counts none of the gone ones, and one live pull
// request that never built.
func (d *demo) makeChildren() {
	kids := []struct {
		key, state string
		pr, builds int
	}{
		{"main", config.ChildLive, 0, 6}, {"pr-41", config.ChildLive, 41, 2}, {"pr-44", config.ChildLive, 44, 0},
		{"pr-12", config.ChildGone, 12, 3}, {"pr-19", config.ChildGone, 19, 1}, {"pr-23", config.ChildGone, 23, 0},
		{"pr-27", config.ChildGone, 27, 2}, {"pr-30", config.ChildGone, 30, 0}, {"pr-33", config.ChildGone, 33, 1},
	}
	for i, k := range kids {
		c := store.Child{Parent: multibranch, Key: k.key, Kind: config.KindBranch, Branch: k.key, State: k.state,
			HeadSHA: fmt.Sprintf("%040x", i+1), LastSeenAt: d.now}
		if k.pr > 0 {
			c.Kind, c.Branch, c.PR = config.KindPR, "feature-"+k.key, k.pr
			c.Title, c.Author = fmt.Sprintf("Change %d", k.pr), "dev"
		}
		if k.state == config.ChildGone {
			gone := d.now.Add(-time.Duration(i) * time.Hour)
			c.GoneAt = &gone
		}
		d.children = append(d.children, c)
		j := &job{name: multibranch + "." + k.key}
		for n := range k.builds {
			end := d.now.Add(-time.Duration(2*(n+i)+1) * time.Hour)
			d.builds = append(d.builds, &build{job: j, parent: multibranch, status: store.Success, trigger: "webhook",
				queued: end.Add(-3 * time.Minute), started: end.Add(-2 * time.Minute), ended: end})
		}
	}
}

var (
	groups = []struct {
		name  string
		share int
		words []string
	}{
		{"backend", 24, []string{"api", "auth", "billing", "search", "worker", "gateway", "events", "reports", "ledger", "mailer", "queue", "cache"}},
		{"frontend", 18, []string{"web", "admin", "docs", "storybook", "checkout", "landing", "dashboard", "editor", "widgets"}},
		{"infra", 14, []string{"images", "terraform", "backup", "certs", "dns", "metrics", "logs", "runners"}},
		{"mobile", 8, []string{"ios", "android", "push", "sdk", "beta"}},
		{"", 6, []string{"scratch", "sandbox", "tools", "experiments"}},
	}
	kinds = []string{"unit", "lint", "build", "e2e", "deploy", "nightly", "audit"}
	crons = []string{"*/15 * * * *", "0 * * * *", "30 2 * * *", "0 6 * * 1-5", "*/30 * * * *", "15 */4 * * *"}
)

// groupAt is the group of the i-th of n jobs: the groups' shares of the
// list, in order.
func groupAt(i, n int) (name string, words []string) {
	total := 0
	for _, g := range groups {
		total += g.share
	}
	pick := i * total / n
	for _, g := range groups {
		if pick < g.share {
			return g.name, g.words
		}
		pick -= g.share
	}
	return groups[0].name, groups[0].words
}

// makeJobs names n jobs, spread over the groups by their share, and gives
// each how it has been doing: most are steady, a few are not.
func (d *demo) makeJobs(n int) {
	used := map[string]bool{}
	for i := range n {
		group, words := groupAt(i, n)
		var name string
		for try := 0; name == "" || used[name]; try++ {
			name = words[d.rng.IntN(len(words))] + "-" + kinds[d.rng.IntN(len(kinds))]
			if try > 20 {
				name = fmt.Sprintf("%s-%d", name, i)
			}
		}
		used[name] = true
		j := &job{name: name, group: group, pass: 0.97 - 0.12*d.rng.Float64()*d.rng.Float64(),
			typical: time.Duration(5+d.rng.IntN(100)) * time.Second}
		if d.rng.IntN(3) == 0 {
			j.cron = crons[d.rng.IntN(len(crons))]
		}
		d.jobs = append(d.jobs, j)
	}
	// The profiles go to jobs by position, spread over the list, so that
	// every group and every size of demo has some.
	profiles := []profile{broken, long, unstable, flaky, long, broken, flaky, long, unstable, long, flaky, broken, long, unstable, flaky, long, broken}
	for i, p := range profiles {
		if j := d.jobs[i*len(d.jobs)/len(profiles)]; j.profile == steady {
			j.profile = p
		}
	}
	for _, j := range d.jobs {
		switch j.profile {
		case flaky:
			j.pass = 0.7
		case long:
			j.typical, j.long, j.cron = 15*time.Minute, true, ""
		}
	}
}

// hourWeight is how likely a build is to be queued in an hour of the day:
// people push in working hours.
func hourWeight(h int) float64 {
	switch {
	case h >= 9 && h < 18:
		return 1
	case h >= 7 && h < 22:
		return 0.45
	}
	return 0.12
}

// makeBuilds queues every job's builds across the days, then lets them run
// on slots slots in the order they were queued, so that they overlap and
// wait as they would. It returns how many builds there are.
func (d *demo) makeBuilds(days, perDay int) int {
	start := d.now.Add(-time.Duration(days) * 24 * time.Hour)
	var queued []*build
	for _, j := range d.jobs {
		n := days * perDay
		if j.long {
			n = days * 2
		}
		if j.cron != "" {
			n /= 2
		}
		for n > 0 {
			n--
			at := start.Add(time.Duration(d.rng.Float64() * float64(d.now.Sub(start))))
			if d.rng.Float64() > hourWeight(at.Hour()) {
				n++
				continue
			}
			queued = append(queued, &build{job: j, queued: at, trigger: []string{"webhook", "webhook", "web", "cron"}[d.rng.IntN(4)]})
		}
		if j.cron != "" {
			queued = append(queued, d.cronBuilds(j, start)...)
		}
	}
	slices.SortFunc(queued, func(a, b *build) int { return a.queued.Compare(b.queued) })
	d.run(queued)
	d.shape()
	return len(d.builds)
}

// cronBuilds are the builds a job's schedule would have made, roughly: one
// at each period of its cron, to the minute.
func (d *demo) cronBuilds(j *job, start time.Time) []*build {
	period := map[string]time.Duration{"*/15 * * * *": 15 * time.Minute, "0 * * * *": time.Hour, "*/30 * * * *": 30 * time.Minute,
		"15 */4 * * *": 4 * time.Hour, "30 2 * * *": 24 * time.Hour, "0 6 * * 1-5": 24 * time.Hour}[j.cron]
	var out []*build
	for t := start.Truncate(period); t.Before(d.now); t = t.Add(period) {
		if t.After(start) {
			out = append(out, &build{job: j, queued: t, trigger: "cron"})
		}
	}
	return out
}

// freeAt is a min-heap of the moments the slots are free.
type freeAt []time.Time

func (h freeAt) Len() int           { return len(h) }
func (h freeAt) Less(i, j int) bool { return h[i].Before(h[j]) }
func (h freeAt) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *freeAt) Push(x any)        { *h = append(*h, x.(time.Time)) }
func (h *freeAt) Pop() any          { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

// run starts each build, in the order given, when a slot is free, and ends
// it after a time of its job's size; what has not started by now waits, and
// what has not ended runs.
func (d *demo) run(queued []*build) {
	free := make(freeAt, slots)
	for i := range free {
		free[i] = queued[0].queued
	}
	heap.Init(&free)
	lastEnd := map[*job]time.Time{}
	for _, b := range queued {
		slot := heap.Pop(&free).(time.Time)
		// A job builds one at a time, as kipp's default does.
		start := slices.MaxFunc([]time.Time{b.queued, slot, lastEnd[b.job]}, time.Time.Compare)
		dur := time.Duration(float64(b.job.typical) * math.Exp(d.rng.NormFloat64()*0.45))
		end := start.Add(dur)
		heap.Push(&free, end)
		lastEnd[b.job] = end
		if end.After(d.now) {
			// What is not over by now is left for the server to run, which it
			// does for the jobs that sleep; for the others it would run for a
			// moment and end green, so it is left out.
			if b.job.long {
				b.status = store.Queued
				d.builds = append(d.builds, b)
			}
			continue
		}
		b.started, b.ended, b.status = start, end, d.outcome(b.job)
		d.builds = append(d.builds, b)
	}
	d.queueLong()
}

// queueLong makes sure each job that sleeps has a build waiting, a few
// minutes old, so that the server has more to run than it has slots.
func (d *demo) queueLong() {
	waiting := map[*job]bool{}
	for _, b := range d.builds {
		waiting[b.job] = waiting[b.job] || b.status == store.Queued
	}
	k := 0
	for _, j := range d.jobs {
		if j.long && !waiting[j] {
			k++
			d.builds = append(d.builds, &build{job: j, status: store.Queued, trigger: "web", queued: d.now.Add(-time.Duration(k) * 70 * time.Second)})
		}
	}
}

func (d *demo) outcome(j *job) store.Status {
	switch r := d.rng.Float64(); {
	case r < j.pass:
		return store.Success
	case r < j.pass+0.01:
		return store.Cancelled
	case r < j.pass+0.015:
		return store.Timeout
	case r < j.pass+0.0165:
		return store.Lost
	case r < j.pass+0.04:
		return store.Unstable
	}
	return store.Failed
}

// shape makes the newest finished builds of a job what its profile says,
// and the infra group worse in its earlier days.
func (d *demo) shape() {
	by := map[*job][]*build{}
	for _, b := range d.builds {
		if b.status.Terminal() && b.status != store.Cancelled {
			by[b.job] = append(by[b.job], b)
		}
	}
	// In the order of the jobs, so that one seed makes one store.
	for _, j := range d.jobs {
		bs := by[j]
		slices.SortFunc(bs, func(a, b *build) int { return cmp.Compare(a.ended.UnixNano(), b.ended.UnixNano()) })
		d.applyProfile(j.profile, bs)
		if j.group == "infra" {
			for _, b := range bs[:len(bs)/3] {
				if d.rng.IntN(5) == 0 {
					b.status = store.Failed
				}
			}
		}
	}
}

// applyProfile sets the newest of bs, oldest first, to what p looks like.
func (d *demo) applyProfile(p profile, bs []*build) {
	tail := func(n int, f func(i int) store.Status) {
		for i := range min(n, len(bs)) {
			bs[len(bs)-1-i].status = f(i)
		}
	}
	switch p {
	case broken:
		tail(3+d.rng.IntN(4), func(int) store.Status { return store.Failed })
		if len(bs) > 8 {
			bs[len(bs)-8].status = store.Success
		}
	case unstable:
		tail(2+d.rng.IntN(4), func(int) store.Status { return store.Unstable })
	case flaky:
		tail(12, func(i int) store.Status {
			if i%3 == 1 || i == 6 {
				return store.Failed
			}
			return store.Success
		})
	}
}

func (d *demo) write(dir string) error {
	data, jobs := filepath.Join(dir, "data"), filepath.Join(dir, "jobs")
	for _, p := range []string{data, jobs} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
	}
	file := "name: " + multibranch + "\ngroup: backend\ndescription: A made-up multibranch job of the demo.\nmultibranch: {repo: acme/" + multibranch + "}\n"
	if err := os.WriteFile(filepath.Join(jobs, multibranch+".yml"), []byte(file), 0o600); err != nil {
		return err
	}
	for _, j := range d.jobs {
		if err := os.WriteFile(filepath.Join(jobs, j.name+".yml"), []byte(j.file()), 0o600); err != nil {
			return err
		}
	}
	// The store makes the schema; the builds go in with their own times,
	// which the store's methods stamp with now.
	path := filepath.Join(data, "db.sqlite")
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	for _, c := range d.children {
		if err := st.PutChild(context.Background(), c); err != nil {
			return err
		}
	}
	if err := st.Close(); err != nil {
		return err
	}
	// kipp refuses a database that others may read.
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return d.insert(path)
}

func (j *job) file() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", j.name)
	if j.group != "" {
		fmt.Fprintf(&b, "group: %s\n", j.group)
	}
	fmt.Fprintf(&b, "description: A made-up job of the demo.\nkeep: {builds: 1000}\n")
	if j.cron != "" {
		fmt.Fprintf(&b, "triggers: {cron: '%s'}\n", j.cron)
	}
	cmd := "true"
	if j.long {
		cmd = "sleep 900"
	}
	fmt.Fprintf(&b, "stages:\n  run: {run: '%s'}\n", cmd)
	return b.String()
}

func (d *demo) insert(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	slices.SortFunc(d.builds, func(a, b *build) int { return a.queued.Compare(b.queued) })
	numbers := map[string]int{}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO builds (job, parent, number, status, trigger, queued_at, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ins.Close() }()
	for _, b := range d.builds {
		numbers[b.job.name]++
		if _, err := ins.ExecContext(ctx, b.job.name, b.parent, numbers[b.job.name], b.status, b.trigger, at(b.queued), nullAt(b.started), nullAt(b.ended)); err != nil {
			return err
		}
	}
	for name, n := range numbers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_counters (job, last_number) VALUES (?, ?)`, name, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func at(t time.Time) string { return t.UTC().Format(tsLayout) }

func nullAt(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: at(t), Valid: true}
}
