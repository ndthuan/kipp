package dispatch

import (
	"strconv"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func startedBy(b *store.Build) string {
	if b.Trigger == "api" || b.Trigger == "web" {
		return b.StartedBy
	}
	return ""
}

func repoOf(job *config.Job) string {
	switch {
	case job.Multibranch != nil:
		return job.Multibranch.Repo
	case job.Triggers.Webhook != nil:
		return job.Triggers.Webhook.Repo
	}
	return ""
}

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
