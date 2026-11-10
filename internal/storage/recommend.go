package storage

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/config"
)

// The kinds of a Recommendation.
const (
	RecommendKeep    = "keep"    // keep fewer builds: a line for the job file
	RecommendRemove  = "remove"  // the data of a job whose file is gone
	RecommendCompact = "compact" // compact the database
	RecommendStray   = "stray"   // a file kipp did not make
)

// Recommendation is one change worth making, with its reason and what it
// saves.
type Recommendation struct {
	Kind string
	// Target is the job or the file it is about; empty for the database.
	Target string
	Reason string
	// Saving is the bytes it would give back, an estimate.
	Saving int64
	// Change is exactly what to do: the line of YAML for a keep, or what
	// to press or remove.
	Change string
	// Until is, for the data of a job whose file is gone, when retention
	// removes it without anybody doing anything.
	Until time.Time
}

// Recommend is what to change, from a report, the largest saving first.
func Recommend(r *Report) []Recommendation {
	var out []Recommendation
	if file := r.Database.PageSize * r.Database.Pages; file > 0 && float64(r.Database.FreeBytes) > FreeShare*float64(file) {
		out = append(out, Recommendation{Kind: RecommendCompact, Saving: r.Database.FreeBytes, Change: "compact the database",
			Reason: fmt.Sprintf("%s of the database's %s is free pages (%s), over the %s a compact is worth; the file keeps them until it is compacted",
				config.Readable(r.Database.FreeBytes), config.Readable(file), percent(float64(r.Database.FreeBytes)/float64(file)), percent(FreeShare))})
	}
	for _, j := range r.Jobs {
		if rec, ok := keepFewer(j, r.Database.UsedBytes); ok {
			out = append(out, rec)
		}
		if rec, ok := removeGone(j); ok {
			out = append(out, rec)
		}
	}
	for _, p := range r.Parts {
		if !p.Made {
			out = append(out, Recommendation{Kind: RecommendStray, Target: p.Name, Saving: p.Bytes, Change: "remove it by hand if nothing needs it",
				Reason: fmt.Sprintf("kipp did not make this file; it is %s, and kipp never removes a file it did not make", config.Readable(p.Bytes))})
		}
	}
	slices.SortStableFunc(out, func(a, b Recommendation) int { return cmp.Compare(b.Saving, a.Saving) })
	return out
}

func percent(f float64) string { return fmt.Sprintf("%.0f%%", 100*f) }

// keepFewer recommends a smaller keep to a job whose rows are over
// JobShare of the database's bytes in use.
func keepFewer(j Job, used int64) (Recommendation, bool) {
	if j.Gone != "" || j.Builds < 2 || used <= 0 || float64(j.DBBytes) <= JobShare*float64(used) {
		return Recommendation{}, false
	}
	n := FewerBuilds(j.DBBytes, j.Builds, used)
	line := fmt.Sprintf("keep: {builds: %d}", n)
	if j.Keep.Days > 0 {
		line = fmt.Sprintf("keep: {builds: %d, days: %d}", n, j.Keep.Days)
	}
	where := "its job file"
	if j.Parent != "" {
		where = "the job file of " + j.Parent + ", whose branches and pull requests it is one of"
	}
	perBuild := float64(j.Bytes()) / float64(j.Builds)
	return Recommendation{Kind: RecommendKeep, Target: j.Name, Change: line,
		Saving: int64(perBuild * float64(j.Builds-n)),
		Reason: fmt.Sprintf("%s's %d builds hold %d test rows and %d metric rows, about %s of the database's %s in use (%s), over the %s one job should take; with %d builds it falls under it. Put the line in %s; kipp never edits a job file",
			j.Name, j.Builds, j.Tests, j.Metrics, config.Readable(j.DBBytes), config.Readable(used),
			percent(float64(j.DBBytes)/float64(used)), percent(JobShare), n, where)}, true
}

// FewerBuilds is the most builds a job of rows bytes over builds may keep
// for its rows to stay under JobShare of a database of used bytes, at
// least 1. Keeping fewer shrinks the database too, which the bound counts:
// n*per < share * (used - rows + n*per).
func FewerBuilds(rows int64, builds int, used int64) int {
	per := float64(rows) / float64(builds)
	limit := JobShare * float64(used-rows) / (per * (1 - JobShare))
	n := int(math.Ceil(limit)) - 1
	return max(1, min(n, builds-1))
}

// removeGone offers to remove the data of a job whose file is gone.
func removeGone(j Job) (Recommendation, bool) {
	if j.Gone == "" || (j.Builds-j.Kept == 0 && j.Logs+j.Artifacts == 0) {
		return Recommendation{}, false
	}
	what := "its job file was removed"
	if j.Gone == GoneClosed {
		what = "its branch or pull request of " + j.Parent + " is gone"
	}
	reason := fmt.Sprintf("%s: %s, and its %d builds still hold %s", j.Name, what, j.Builds, config.Readable(j.Bytes()))
	if !j.Until.IsZero() {
		reason += fmt.Sprintf("; retention removes them by %s, %d days after the newest", j.Until.UTC().Format("2006-01-02"), config.RemovedKeepDays)
	}
	if j.Kept > 0 {
		reason += fmt.Sprintf("; %d kept by hand stay either way", j.Kept)
	}
	return Recommendation{Kind: RecommendRemove, Target: j.Name, Saving: j.Bytes(), Change: "remove now", Until: j.Until, Reason: reason}, true
}
