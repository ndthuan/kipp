package config

import (
	"strings"
	"testing"
)

func webhookTags(t *testing.T, tags string) *Webhook {
	t.Helper()
	j, err := ParseJob("rel.yml", []byte("name: rel\ntriggers:\n  webhook:\n    repo: o/r\n    tags: "+tags+"\nstages:\n  s: {run: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return j.Triggers.Webhook
}

func TestAWebhooksTagListIsItsIncludeAndTheMappingMatchesTheSame(t *testing.T) {
	list := webhookTags(t, `["v*", "release/*"]`)
	mapping := webhookTags(t, `{include: ["v*", "release/*"]}`)
	for tag, want := range map[string]bool{"v1.2.0": true, "release/2026": true, "release/a/b": false, "1.2.0": false} {
		if list.WatchesTag(tag) != want || mapping.WatchesTag(tag) != want {
			t.Errorf("tag %s: list %v, mapping %v, want %v", tag, list.WatchesTag(tag), mapping.WatchesTag(tag), want)
		}
	}
	if list.Watches("main") || mapping.Watches("main") {
		t.Error("a webhook with tags alone watches a branch")
	}
}

func TestATagBuildsWhenAnIncludeMatchesAndNoExcludeDoes(t *testing.T) {
	cases := map[string]struct {
		tags string
		want map[string]bool
	}{
		"include only": {`{include: ["v*"]}`, map[string]bool{"v1": true, "v1-rc1": true, "nightly": false}},
		"exclude only": {`{exclude: ["*-rc*"]}`, map[string]bool{"v1": true, "nightly": true, "v1-rc1": false}},
		"both, exclude wins": {`{include: ["v*"], exclude: ["*-rc*", "v0.*"]}`,
			map[string]bool{"v1": true, "v1-rc1": false, "v0.9": false, "nightly": false}},
		"every tag":  {`true`, map[string]bool{"v1": true, "anything": true}},
		"no tag":     {`false`, map[string]bool{"v1": false}},
		"empty list": {`[]`, map[string]bool{"v1": false}},
	}
	for name, c := range cases {
		w := webhookTags(t, c.tags)
		for tag, want := range c.want {
			if got := w.WatchesTag(tag); got != want {
				t.Errorf("%s: tag %s built %v, want %v", name, tag, got, want)
			}
		}
	}
}

func TestATagFilterThatCannotBeReadIsRefused(t *testing.T) {
	for _, tags := range []string{`["v["]`, `{include: ["v["]}`, `{exclude: ["["]}`, `{include: []}`, `{only: ["v*"]}`, `yes please`, `{include: "v*"}`, `[[v]]`} {
		_, err := ParseJob("rel.yml", []byte("name: rel\ntriggers:\n  webhook:\n    repo: o/r\n    tags: "+tags+"\nstages:\n  s: {run: true}\n"))
		if err == nil {
			t.Errorf("tags: %s was taken", tags)
		}
	}
	_, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site, tags: {exclude: [\"v[\"]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "multibranch.tags") || !strings.Contains(err.Error(), "v[") {
		t.Fatalf("a bad multibranch tag pattern: %v", err)
	}
	_, err = ParseJob("acme.yml", []byte("name: acme\nowner: {login: acme, token: GH, tags: {include: [\"[\"]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "owner.tags") {
		t.Fatalf("a bad owner tag pattern: %v", err)
	}
}

func TestAMultibranchOrOwnerJobBuildsEveryTagUnlessToldOtherwise(t *testing.T) {
	for file, body := range map[string]string{"site.yml": "name: site\nmultibranch: {repo: acme/site}\n", "acme.yml": "name: acme\nowner: {login: acme, token: GH}\n"} {
		j, err := ParseJob(file, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		m := j.Multibranch
		if j.Owner != nil {
			m = &j.Owner.Settings
		}
		if !m.Builds(KindTag) || !m.Tags.Matches("v1.2.0") || !m.Tags.Matches("anything") {
			t.Errorf("%s: tags %+v do not build every tag", j.Name, m.Tags)
		}
	}
	off, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site, tags: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if off.Multibranch.Builds(KindTag) || off.Multibranch.Tags.Matches("v1") {
		t.Error("tags: false still builds tags")
	}
	some, err := ParseJob("acme.yml", []byte("name: acme\nowner: {login: acme, token: GH, tags: {include: [\"v*\"], exclude: [\"*-rc*\"]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	node := some.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", State: RepoLive})
	if !node.Multibranch.Tags.Matches("v2") || node.Multibranch.Tags.Matches("v2-rc1") || node.Multibranch.Tags.Matches("nightly") {
		t.Errorf("a repo of the owner job has tags %+v", node.Multibranch.Tags)
	}
}

func TestABranchNamedTagsNeverTakesTheTagsChildsKey(t *testing.T) {
	if key := ChildKey("tags", func(string) bool { return false }); key == TagsKey {
		t.Fatalf("branch tags has key %q", key)
	}
}
