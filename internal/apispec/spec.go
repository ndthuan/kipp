// Package apispec reads kipp's OpenAPI description: the split sources under
// api/, the single file they bundle to, and the operations in it. It is for
// the gate and its tools; the server never imports it, so kin-openapi stays
// out of bin/kipp (a test holds that).
package apispec

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"go.yaml.in/yaml/v3"
)

// RootFile and BundleFile are the spec's root and the file it is bundled
// to, inside the api directory.
const (
	RootFile   = "openapi.yaml"
	BundleFile = "openapi.gen.yaml"
)

// PackageExt names, on an operation, the package whose tests must reach it
// (api, webhook, runnerhub or cachesrv).
const PackageExt = "x-kipp-package"

// Load reads the root file of the sources in dir and every file it refers
// to.
func Load(dir string) (*openapi3.T, error) {
	l := openapi3.NewLoader()
	l.IsExternalRefsAllowed = true
	doc, err := l.LoadFromFile(filepath.Join(dir, RootFile))
	if err != nil {
		return nil, err
	}
	return doc, validate(doc)
}

// Parse reads a bundled spec.
func Parse(data []byte) (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		return nil, err
	}
	return doc, validate(doc)
}

func validate(doc *openapi3.T) error {
	if !doc.IsOpenAPI31OrLater() {
		return fmt.Errorf("openapi is %q, want 3.1.x", doc.OpenAPI)
	}
	return doc.Validate(context.Background())
}

// Bundle is the single file the sources in dir make: every external $ref
// moved into components under its file's base name. The same sources always
// make the same bytes.
func Bundle(dir string) ([]byte, error) {
	doc, err := Load(dir)
	if err != nil {
		return nil, err
	}
	doc.InternalizeRefs(context.Background(), refName)
	data, err := doc.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return toYAML(data)
}

// refName keeps the base name of the file a component came from, so that
// schemas/Build.yaml is components/schemas/Build.
func refName(_ *openapi3.T, ref openapi3.ComponentRef) string {
	if u := ref.RefPath(); u != nil {
		if frag := strings.TrimPrefix(u.Fragment, "/"); frag != "" && u.Path == "" {
			return path.Base(frag)
		}
		return strings.TrimSuffix(path.Base(unescape(u.Path)), path.Ext(u.Path))
	}
	return path.Base(ref.RefString())
}

func unescape(p string) string {
	if s, err := url.PathUnescape(p); err == nil {
		return s
	}
	return p
}

// toYAML writes JSON as block YAML in the order it came in.
func toYAML(data []byte) ([]byte, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	block(&n)
	if len(n.Content) == 1 {
		topLevelFirst(n.Content[0])
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func block(n *yaml.Node) {
	n.Style &^= yaml.FlowStyle
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		switch {
		case strings.Contains(n.Value, "\n") && !strings.Contains(n.Value, " \n") && !strings.HasPrefix(n.Value, " "):
			n.Style = yaml.LiteralStyle
		case !needsQuotes(n):
			n.Style = 0
		}
	}
	for _, c := range n.Content {
		block(c)
	}
}

// rootOrder is how a reader meets a spec: what it is, where, then the
// operations and what they share.
var rootOrder = []string{"openapi", "info", "servers", "security", "tags", "paths", "components"}

// topLevelFirst puts the keys of the root mapping in rootOrder, the others
// after them in the order they had.
func topLevelFirst(root *yaml.Node) {
	if root.Kind != yaml.MappingNode {
		return
	}
	var out []*yaml.Node
	used := make([]bool, len(root.Content)/2)
	for _, key := range rootOrder {
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == key && !used[i/2] {
				used[i/2] = true
				out = append(out, root.Content[i], root.Content[i+1])
			}
		}
	}
	for i := 0; i < len(root.Content); i += 2 {
		if !used[i/2] {
			out = append(out, root.Content[i], root.Content[i+1])
		}
	}
	root.Content = out
}

// needsQuotes is whether a string scalar would be read as something else
// without its quotes: a number, a bool, null, or an empty string.
func needsQuotes(n *yaml.Node) bool {
	if n.Tag != "" && n.Tag != "!!str" {
		return false
	}
	var v any
	if n.Value == "" || yaml.Unmarshal([]byte(n.Value), &v) != nil {
		return true
	}
	_, isString := v.(string)
	return !isString || strings.ContainsAny(n.Value, "\n")
}

// Op is one operation of the spec.
type Op struct {
	Method, Path string
	Package      string // the x-kipp-package extension
	Operation    *openapi3.Operation
}

// Key is the operation as "GET /jobs/{name}".
func (o Op) Key() string { return o.Method + " " + o.Path }

// Operations are every operation of doc, by path and then method.
func Operations(doc *openapi3.T) []Op {
	var out []Op
	for _, p := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		item := doc.Paths.Value(p)
		for method, op := range item.Operations() {
			pkg, _ := op.Extensions[PackageExt].(string)
			out = append(out, Op{Method: method, Path: p, Package: pkg, Operation: op})
		}
	}
	slices.SortStableFunc(out, func(a, b Op) int { return strings.Compare(a.Path+" "+a.Method, b.Path+" "+b.Method) })
	return out
}

// Write bundles the sources in dir to the bundle file beside them.
func Write(dir string) error {
	data, err := Bundle(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, BundleFile), data, 0o600)
}

// Mismatches compares the routes a package serves under /api/v1, each as
// "METHOD /path" with the prefix off and {name...} written {name}, with the
// operations the spec gives that package, and names each side's extras.
func Mismatches(pkg string, routes []string, ops []Op) []string {
	var out []string
	have := map[string]bool{}
	for _, op := range ops {
		if op.Package == "" {
			out = append(out, fmt.Sprintf("operation %s has no %s extension", op.Key(), PackageExt))
		}
		if op.Package == pkg {
			have[op.Key()] = true
		}
	}
	served := map[string]bool{}
	for _, route := range routes {
		route = strings.NewReplacer("...}", "}").Replace(route)
		served[route] = true
		if !have[route] {
			out = append(out, fmt.Sprintf("the %s package serves %s, which the spec does not describe", pkg, route))
		}
	}
	for key := range have {
		if !served[key] {
			out = append(out, fmt.Sprintf("the spec describes %s for the %s package, which serves no such route", key, pkg))
		}
	}
	slices.Sort(out)
	return out
}
