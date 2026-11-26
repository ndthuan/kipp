package cli

import (
	"context"
	"io"
	"os"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
)

func cmdCheck(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("check <jobs-dir>", stderr)
	policy := fs.String("policy", "", "`program` run as `<program> <jobs-dir>`, as serve --job-policy runs it, with KIPP_POLICY_CHANGE=check; any exit but 0 fails the check")
	policyTimeout := fs.Duration("policy-timeout", jobstore.DefaultPolicyTimeout, "how long --policy may run")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	snap, err := config.Load(positional[0])
	if err != nil {
		say(stderr, "%v", err)
		return 1
	}
	for _, w := range snap.Warnings {
		say(stderr, "warning: %s", w)
	}
	if *policy != "" {
		files, err := config.ReadDir(positional[0])
		if err == nil {
			p := &jobstore.Policy{Path: *policy, Timeout: *policyTimeout, Env: os.Environ()}
			err = p.Run(context.Background(), files, jobstore.PolicyChange{Kind: jobstore.ChangeCheck})
		}
		if err != nil {
			say(stderr, "%v", err)
			return 1
		}
	}
	say(stdout, "ok: %s", jobCount(len(snap.Names)))
	return 0
}
