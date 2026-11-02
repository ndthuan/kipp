package dispatch

import (
	"strconv"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// startedBy is who started the build when a person did, by the API or the
// web, which is also how a build made again by a person is started; the
// other triggers are kipp's or GitHub's own, and name nobody.
func startedBy(b *store.Build) string {
	if b.Trigger == "api" || b.Trigger == "web" {
		return b.StartedBy
	}
	return ""
}

// repoOf is the owner/name of the repo the job is about: the multibranch
// repo of a child, else the repo its webhook is for, whatever started the
// build; empty for a job with neither.
func repoOf(job *config.Job) string {
	switch {
	case job.Multibranch != nil:
		return job.Multibranch.Repo
	case job.Triggers.Webhook != nil:
		return job.Triggers.Webhook.Repo
	}
	return ""
}

// refOf is the git ref the build is for, as GitHub names one: a pull
// request's head, else the branch, else the tag. It is empty when the
// build names none, and it never reads a source job's own ref:, which may
// be a commit, and which kipp does not resolve.
func refOf(b *store.Build) string {
	switch {
	case b.PR > 0:
		return "refs/pull/" + strconv.Itoa(b.PR) + "/head"
	case b.TriggerBranch != "":
		return "refs/heads/" + b.TriggerBranch
	case b.TriggerTag != "":
		return "refs/tags/" + b.TriggerTag
	}
	return ""
}
