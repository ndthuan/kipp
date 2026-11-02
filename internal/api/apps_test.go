package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

func TestOwnerAppsAreTheAdminsOneRowPerAppWithItsJobsSecretAndKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	good := filepath.Join(dir, "ci.pem")
	must(t, os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600))
	loose := filepath.Join(dir, "loose.pem")
	must(t, os.WriteFile(loose, []byte("THE-KEY-BODY"), 0o600))
	must(t, os.Chmod(loose, 0o644))
	var jobs []*config.Job
	for name, app := range map[string]string{
		// Two accounts sign in as one App; a third as another; a fourth with a token.
		"acme":  "app: {id: 7, key_file: " + good + "}",
		"acme2": "app: {id: 7, key_file: " + good + "}",
		"labs":  "app: {id: 9, key_file: " + loose + "}",
		"plain": "token: GH_TOKEN",
	} {
		j, err := config.ParseJob(name+".yml", []byte("name: "+name+"\nowner: {login: "+name+", "+app+", scan: 0}\n"))
		must(t, err)
		jobs = append(jobs, j)
	}
	e.api.deps.Dispatch.SetSnapshot(e.api.deps.Dispatch.Snapshot().WithJobs(jobs...))
	must(t, e.st.PutSecret(ctx, "GITHUB_APP_WEBHOOK_CI", []byte("sealed-test-bytes")))

	token(t, e.st, "dev", false, map[string][]string{"acme": {"view"}})
	_, err = e.api.svc.OwnerApps(ctx, e.user(t, "dev"))
	is(t, "a user who is not an admin", err, ErrForbidden)
	apps, err := e.api.svc.OwnerApps(ctx, e.user(t, "admin"))
	must(t, err)
	if len(apps) != 2 || apps[0].Name != "ci" || apps[1].Name != "loose" {
		t.Fatalf("not one row per App, by name: %+v", apps)
	}
	ci, lo := apps[0], apps[1]
	if ci.ID != 7 || ci.KeyFile != good || len(ci.Jobs) != 2 || ci.Jobs[0] != "acme" || ci.Jobs[1] != "acme2" ||
		ci.Secret != "GITHUB_APP_WEBHOOK_CI" || !ci.SecretSet || ci.KeyProblem != "" {
		t.Fatalf("ci: %+v", ci)
	}
	if lo.ID != 9 || len(lo.Jobs) != 1 || lo.SecretSet {
		t.Fatalf("loose: %+v", lo)
	}
	contains(t, lo.KeyProblem, "readable by group or others")
	if strings.Contains(lo.KeyProblem, "THE-KEY-BODY") {
		t.Fatal("the key's content in the problem")
	}
}
