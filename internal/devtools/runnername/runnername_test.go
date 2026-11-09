// Package runnername holds the one test that keeps the machines that run
// stages called runners: users once met them as agents, and a user must
// meet one word for one thing.
package runnername

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// otherMeanings are the phrases in which "agent" is not a machine that
// runs stages. Each is stripped from a line before the line is checked.
var otherMeanings = []string{
	"User-Agent",    // the HTTP header
	"LaunchAgents",  // the macOS directory
	"launchd agent", // the macOS kind of job
	"AGENTS.md",     // the guide for coding agents
	"coding agents", // as in that guide
	"magenta",       // a colour
	"Magenta",
	"ssh-agent",
}

// oldWordFiles are the files that must name the old word, and why.
var oldWordFiles = map[string]string{
	"CHANGELOG.md":                                    "what each release said, under the names of the time, and the entry for the rename",
	"internal/auth/blocklist.txt":                     "passwords from public lists, which are not ours to edit",
	"internal/store/migrate.go":                       "the released schema creates the table agents, and the last migration renames it",
	"internal/store/migrate_test.go":                  "the upgrade test makes a store with the table agents",
	"internal/devtools/runnername/runnername_test.go": "the word and its allowed meanings are listed here",
}

var oldWord = regexp.MustCompile(`(?i)agent`)

// strayOld is every line of text that still says agent in a sense that is
// not one of otherMeanings.
func strayOld(path string, text []byte) []string {
	if bytes.IndexByte(text, 0) >= 0 {
		return nil
	}
	var out []string
	for i, line := range strings.Split(string(text), "\n") {
		for _, ok := range otherMeanings {
			line = strings.ReplaceAll(line, ok, "")
		}
		if oldWord.MatchString(line) {
			out = append(out, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return out
}

func TestNoTrackedFileCallsARunnerAnAgent(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	listed, err := cmd.Output()
	if err != nil {
		t.Skipf("not a git checkout, so there is no list of tracked files: %v", err)
	}
	for path := range strings.SplitSeq(strings.TrimSuffix(string(listed), "\x00"), "\x00") {
		if reason, ok := oldWordFiles[path]; ok && reason != "" {
			continue
		}
		if oldWord.MatchString(path) && path != "AGENTS.md" {
			t.Errorf("%s: the file's name says agent", path)
		}
		text, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue // a tracked file that is gone from the tree is a change in flight
		}
		for _, bad := range strayOld(path, text) {
			t.Error(bad)
		}
	}
}

func TestAStrayAgentIsFoundAndTheOtherMeaningsAreNot(t *testing.T) {
	for line, stray := range map[string]bool{
		"a runner runs the stage":                 false,
		"start the agent":                         true,
		"KIPP_AGENT_TOKEN=x":                      true,
		"ranOnAgent := 1":                         true,
		"req.Header.Set(\"User-Agent\", v)":       false,
		"cp x ~/Library/LaunchAgents/":            false,
		"see AGENTS.md; people and coding agents": false,
		"the ansi-fg-magenta colour":              false,
		"a launchd agent on macOS":                false,
		"a User-Agent and then an agent":          true,
	} {
		if got := len(strayOld("f", []byte(line))) > 0; got != stray {
			t.Errorf("%q: stray = %v, want %v", line, got, stray)
		}
	}
}
