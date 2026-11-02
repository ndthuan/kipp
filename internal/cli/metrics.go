package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/store"
)

const metricsUsage = `metrics check <glob>
       kipp metrics put <store> --file <report.json> [--job <name>] --data <dir>
       kipp metrics forget <store> [--metric <name> [--item <item>]] --data <dir>
       kipp metrics ls [--store <name>] [--jobs <dir>] --data <dir>`

// putJob is the job a value put by hand is remembered as from, when
// --job names none.
const putJob = "cli"

type metricsFlags struct {
	file, job, metric, item, store, jobs string
}

func cmdMetrics(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "check" {
		// Inside a stage: no data directory, no server, no token.
		return metrics.CheckCommand(args[1:], os.Getenv, stdout)
	}
	fs := newFlags(metricsUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	mf := &metricsFlags{}
	fs.StringVar(&mf.file, "file", "", "put: the report `file`, in the format a stage's metrics.file is")
	fs.StringVar(&mf.job, "job", putJob, "put: the `job` the values are remembered as from, with build number 0")
	fs.StringVar(&mf.metric, "metric", "", "forget: only this `metric`; none is every metric of the store")
	fs.StringVar(&mf.item, "item", "", "forget: only this `item` of --metric")
	fs.StringVar(&mf.store, "store", "", "ls: only this `store`")
	fs.StringVar(&mf.jobs, "jobs", "", "ls: the server's jobs `directory`, to mark the stores no job names")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	return onStore("metrics", *data, fs.Usage, stderr, metricsAction(positional, mf, stdout))
}

func metricsAction(args []string, mf *metricsFlags, stdout io.Writer) action {
	switch {
	case len(args) == 2 && args[0] == "put":
		return func(ctx context.Context, st *store.Store) error { return metricsPut(ctx, st, args[1], mf, stdout) }
	case len(args) == 2 && args[0] == "forget":
		return func(ctx context.Context, st *store.Store) error { return metricsForget(ctx, st, args[1], mf, stdout) }
	case len(args) == 1 && args[0] == "ls":
		return func(ctx context.Context, st *store.Store) error { return metricsLs(ctx, st, mf, stdout) }
	}
	return nil
}

func checkStoreName(name string) error {
	if !config.ValidName(name) {
		return refused(fmt.Sprintf("store %q: lower-case letters, digits, dot, dash and underscore only", name))
	}
	return nil
}

// metricsPut remembers a report as a build of --job would have. A report
// with anything wrong in it is refused whole: what is put by hand is
// meant as it is written.
func metricsPut(ctx context.Context, st *store.Store, name string, mf *metricsFlags, stdout io.Writer) error {
	if err := checkStoreName(name); err != nil {
		return err
	}
	if mf.file == "" {
		return refused("put needs --file")
	}
	if !config.ValidName(mf.job) {
		return refused(fmt.Sprintf("--job %q: a job's name", mf.job))
	}
	ms, errs := metrics.ParseGlob(filepath.Dir(mf.file), filepath.Base(mf.file))
	if len(errs) > 0 {
		lines := make([]string, len(errs))
		for i, err := range errs {
			lines[i] = err.Error()
		}
		return fmt.Errorf("%s; nothing was remembered", strings.Join(lines, "; "))
	}
	rows := make([]store.Metric, len(ms))
	for i, m := range ms {
		rows[i] = store.Metric(m)
	}
	if err := st.Remember(ctx, name, store.Origin{Job: mf.job}, time.Now(), rows); err != nil {
		return err
	}
	items, names := 0, make([]string, len(ms))
	for i, m := range ms {
		items += len(m.Values)
		names[i] = m.Name
	}
	what := fmt.Sprintf("%s of %s", itemCount(items), strings.Join(names, ", "))
	say(stdout, "%s: %s remembered as from %s", name, what, mf.job)
	return record(ctx, st, "metrics.put", name, what+" as from "+mf.job)
}

func metricsForget(ctx context.Context, st *store.Store, name string, mf *metricsFlags, stdout io.Writer) error {
	if err := checkStoreName(name); err != nil {
		return err
	}
	if mf.item != "" && mf.metric == "" {
		return refused("--item needs --metric: an item is one metric's")
	}
	n, err := st.Forget(ctx, name, mf.metric, mf.item)
	if err != nil {
		return err
	}
	say(stdout, "%s: %s forgotten", name, itemCount(n))
	if n == 0 {
		return nil // nothing changed, and nothing is written of it
	}
	what := "everything"
	switch {
	case mf.item != "":
		what = fmt.Sprintf("metric %s, item %s", mf.metric, mf.item)
	case mf.metric != "":
		what = "metric " + mf.metric
	}
	return record(ctx, st, "metrics.forget", name, what+": "+itemCount(n))
}

// metricsLs prints a line per metric of each store: its name, the
// metric, how many items it remembers, and with --jobs whether no job
// there names the store.
func metricsLs(ctx context.Context, st *store.Store, mf *metricsFlags, stdout io.Writer) error {
	named, err := namedStores(mf.jobs)
	if err != nil {
		return err
	}
	stores, err := st.MetricStores(ctx)
	if err != nil {
		return err
	}
	for _, s := range stores {
		if mf.store != "" && s.Store != mf.store {
			continue
		}
		mark := ""
		if named != nil && !named[s.Store] {
			mark = "\torphaned: no job names it"
		}
		for _, m := range s.Metrics {
			say(stdout, "%s\t%s\t%s%s", s.Store, m.Name, itemCount(m.Items), mark)
		}
	}
	return nil
}

// namedStores are the stores the job files of dir name, nil with no dir.
func namedStores(dir string) (map[string]bool, error) {
	if dir == "" {
		return nil, nil
	}
	snap, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, j := range snap.Jobs {
		named[j.StoreName()] = true
	}
	return named, nil
}

func itemCount(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}
