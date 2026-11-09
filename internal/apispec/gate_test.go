package apispec_test

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/apispec"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/route"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/webhook"
)

const specDir = "../../api"

func TestTheCommittedBundleIsWhatTheSourcesBundleTo(t *testing.T) {
	want, err := apispec.Bundle(specDir)
	if err != nil {
		t.Fatalf("the sources do not bundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(specDir, apispec.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/openapi.gen.yaml is not what api/openapi.yaml and the files beside it bundle to; run `make openapi` and commit the result")
	}
}

func TestTheBundleIsOpenAPI31(t *testing.T) {
	got, err := os.ReadFile(filepath.Join(specDir, apispec.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := apispec.Parse(got)
	if err != nil {
		t.Fatalf("the bundle is not valid OpenAPI 3.1: %v", err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "/api/v1" {
		t.Fatalf("servers = %v, want the one /api/v1", doc.Servers)
	}
}

// A file under schemas, responses or parameters is the component of its
// base name, and a file under paths is one path: none is left out of the
// bundle, and the bundle holds nothing the files do not.
func TestEverySourceFileIsInTheBundleUnderItsBaseName(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(specDir, apispec.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := apispec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	components := map[string][]string{
		"schemas":    slices.Collect(maps.Keys(doc.Components.Schemas)),
		"responses":  slices.Collect(maps.Keys(doc.Components.Responses)),
		"parameters": slices.Collect(maps.Keys(doc.Components.Parameters)),
	}
	for dir, names := range components {
		for _, problem := range unmatched(t, filepath.Join(specDir, dir), names) {
			t.Errorf("%s: %s", dir, problem)
		}
	}
	files, err := filepath.Glob(filepath.Join(specDir, "paths", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(doc.Paths.Map()); got != len(files) {
		t.Errorf("the bundle has %d paths and api/paths %d files: a file holds exactly one path, and each is referenced", got, len(files))
	}
}

// unmatched names the files of dir that are no component of the bundle,
// and the components that have no file.
func unmatched(t *testing.T, dir string, components []string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if !slices.Contains(components, name) {
			out = append(out, f+" is no component of the bundle: nothing refers to it")
		}
	}
	for _, name := range components {
		if !slices.Contains(files, filepath.Join(dir, name+".yaml")) {
			out = append(out, "component "+name+" has no file of that name")
		}
	}
	return out
}

func TestInfoVersionIsTheNewestReleaseOfTheChangelog(t *testing.T) {
	log, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "0.0.0"
	if m := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+[^\]]*)\]`).FindSubmatch(log); m != nil {
		want = string(m[1])
	}
	data, err := os.ReadFile(filepath.Join(specDir, apispec.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := apispec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != want {
		t.Fatalf("info.version = %q, want %q: 0.0.0 until CHANGELOG.md has a `## [X.Y.Z]` heading, then the newest one's version (AGENTS.md, Releasing)", doc.Info.Version, want)
	}
}

// The four packages that serve under /api/v1 export their route tables,
// and the spec describes exactly those: a route nobody described and an
// operation nobody serves both fail.
func TestTheSpecDescribesEveryV1RouteAndNothingElse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(specDir, apispec.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := apispec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	ops := apispec.Operations(doc)
	var apiRoutes []string
	for _, r := range api.New(api.Deps{}).Routes() {
		apiRoutes = append(apiRoutes, r.Method+" "+r.Path)
	}
	tables := map[string][]string{
		"api":       apiRoutes,
		"webhook":   v1(webhook.New(nil, webhook.Options{}).Routes()),
		"runnerhub": v1(runnerhub.New(runnerhub.Options{}).Routes()),
		"cachesrv":  v1(cachesrv.New(cachesrv.Options{}).Routes()),
	}
	for pkg, routes := range tables {
		if len(routes) == 0 {
			t.Fatalf("the %s package exports no routes", pkg)
		}
		for _, problem := range apispec.Mismatches(pkg, routes, ops) {
			t.Error(problem)
		}
	}
}

// v1 is the rows served under /api/v1, as "METHOD /path" without the prefix.
func v1(rows []route.Route) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Method+" "+strings.TrimPrefix(r.Path, api.PrefixV1))
	}
	return out
}

// The checker needs kin-openapi; the server must not carry it.
func TestTheServerDoesNotLinkTheSpecTooling(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/kipp")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, banned := range []string{"github.com/getkin/kin-openapi", "github.com/ndthuan/kipp/internal/apispec"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("cmd/kipp depends on %s, which would put the spec tooling into bin/kipp", banned)
		}
	}
}

// Each check, shown to fail once on purpose, on a spec of two lines.
func TestMismatchesNamesARouteTheSpecLacksAndAnOperationNobodyServes(t *testing.T) {
	ops := []apispec.Op{{Method: "GET", Path: "/a", Package: "api"}, {Method: "GET", Path: "/gone", Package: "api"}, {Method: "GET", Path: "/b", Package: "other"}}
	got := strings.Join(apispec.Mismatches("api", []string{"GET /a", "GET /new", "GET /files/{path...}"}, ops), "\n")
	for _, want := range []string{
		"serves GET /new, which the spec does not describe",
		"serves GET /files/{path}, which the spec does not describe",
		"describes GET /gone for the api package, which serves no such route",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/b") || strings.Contains(got, "GET /a ") {
		t.Errorf("a route in both, or an operation of another package, was named:\n%s", got)
	}
	none := apispec.Mismatches("api", nil, []apispec.Op{{Method: "GET", Path: "/x"}})
	if len(none) != 1 || !strings.Contains(none[0], "has no x-kipp-package extension") {
		t.Errorf("an operation without a package: %v", none)
	}
}
