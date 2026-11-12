package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func decodedHashOf(t *testing.T, text string) string {
	t.Helper()
	j, err := ParseJob("site-smoke.yml", []byte(text))
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	return DecodedHash(j)
}

func TestTheDecodedHashIgnoresCommentsKeyOrderQuotingAndBlankLines(t *testing.T) {
	base := decodedHashOf(t, smoke)
	same := `
# the smoke test of the site
group: "site"
name: 'site-smoke'

triggers:
  after: ["build"]
  cron: H 3 * * *   # nightly
params:
  TIER:
    options: [quick, full]
    type: choice
    default: quick
  DRY_RUN: {default: true, type: bool}
concurrency: drop
locks:
  - checkout
timeout: 2h
keep: {builds: 50}
env:
  FOO: "bar"
secrets: [CHAT_TOKEN]
workdir: /tmp


stages:
  lint: {run: make lint}
  unit: {run: "make test"}
  smoke:
    junit: reports/*.xml
    needs: [lint, unit]
    run: make smoke TIER=$TIER
on_failure: {run: ./notify.sh failed}
`
	if got := decodedHashOf(t, same); got != base {
		t.Fatalf("the same job written another way hashes %s, want %s", got, base)
	}
}

func TestTheDecodedHashChangesWithEveryFieldOfTheJob(t *testing.T) {
	base := decodedHashOf(t, smoke)
	for what, change := range map[string][2]string{
		"a stage's run":  {"make lint}", "make lint --fix}"},
		"the cron":       {`"H 3 * * *"`, `"H 4 * * *"`},
		"env":            {"{FOO: bar}", "{FOO: baz}"},
		"locks":          {"[checkout]", "[checkout, deploy]"},
		"params":         {"default: quick", "default: full"},
		"permissions":    {"workdir: /tmp", "workdir: /tmp\npermissions: {build: [view]}"},
		"the stage list": {"  lint: {run: make lint}\n", "  lint: {run: make lint}\n  vet: {run: go vet}\n"},
		"needs":          {"needs: [lint, unit]", "needs: [lint]"},
		"a timeout":      {"timeout: 2h", "timeout: 3h"},
		"a hook":         {"./notify.sh failed", "./notify.sh broke"},
	} {
		text := strings.Replace(smoke, change[0], change[1], 1)
		if text == smoke {
			t.Fatalf("%s: the change %q is not in the job", what, change[0])
		}
		if decodedHashOf(t, text) == base {
			t.Errorf("a change to %s leaves the decoded hash as it was", what)
		}
	}
}

// Every field of Job, however deep, is set in turn on an otherwise empty
// job: a field that DecodedHash did not write would leave the hash as it
// was, so a field added to Job later cannot be left out of it.
func TestTheDecodedHashWritesEveryFieldOfJobFoundByReflection(t *testing.T) {
	var paths []string
	walkFields(reflect.TypeFor[Job](), "Job", 0, func(path string, set func(*Job), unset func(*Job)) {
		paths = append(paths, path)
		var with, without Job
		unset(&without)
		set(&with)
		if DecodedHash(&with) == DecodedHash(&without) {
			t.Errorf("%s: setting it leaves the decoded hash as it was", path)
		}
	})
	if len(paths) < 100 {
		t.Fatalf("only %d fields were walked: %v", len(paths), paths)
	}
}

// walkFields calls visit for every field reachable from t, with a function
// that sets that field on a job and one that builds the same way down to it
// and leaves it zero.
func walkFields(t reflect.Type, path string, depth int, visit func(path string, set, unset func(*Job))) {
	// reach builds a job down to a value and hands it to k; a map's value
	// is written back once k has changed it, as it cannot be set in place.
	type reach func(j *Job, k func(reflect.Value))
	var walk func(t reflect.Type, path string, depth int, to reach)
	walk = func(t reflect.Type, path string, depth int, to reach) {
		if depth > 8 {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue // where the field was in the file, not what it says
			}
			fpath := path + "." + f.Name
			field := func(j *Job, k func(reflect.Value)) { to(j, func(v reflect.Value) { k(v.Field(i)) }) }
			visit(fpath, func(j *Job) { field(j, setNonZero) }, func(j *Job) { field(j, func(reflect.Value) {}) })
			if inner, into := innerStruct(f.Type); into != nil {
				walk(inner, fpath, depth+1, func(j *Job, k func(reflect.Value)) {
					field(j, func(v reflect.Value) { into(v, k) })
				})
			}
		}
	}
	walk(t, path, depth, func(j *Job, k func(reflect.Value)) { k(reflect.ValueOf(j).Elem()) })
}

// innerStruct is the struct a field of type t holds (itself, through a
// pointer, as the item of a slice or as a map's value), with a function
// that makes one in a field of that type and hands it to k.
func innerStruct(t reflect.Type) (reflect.Type, func(v reflect.Value, k func(reflect.Value))) {
	if t == locationType || t == timeType {
		return nil, nil
	}
	if t.Kind() == reflect.Struct {
		return t, func(v reflect.Value, k func(reflect.Value)) { k(v) }
	}
	reach := reacher(t)
	if reach == nil {
		return nil, nil
	}
	inner, into := innerStruct(t.Elem())
	if into == nil {
		return nil, nil
	}
	return inner, func(v reflect.Value, k func(reflect.Value)) {
		reach(v, func(e reflect.Value) { into(e, k) })
	}
}

// reacher is how to reach the one element of a pointer, slice or map value
// of type t, making it if there is none, and hand it to into; nil for any
// other kind.
func reacher(t reflect.Type) func(v reflect.Value, into func(reflect.Value)) {
	switch t.Kind() {
	case reflect.Pointer:
		return func(v reflect.Value, into func(reflect.Value)) {
			if v.IsNil() {
				v.Set(reflect.New(t.Elem()))
			}
			into(v.Elem())
		}
	case reflect.Slice:
		return func(v reflect.Value, into func(reflect.Value)) {
			if v.Len() == 0 {
				v.Set(reflect.MakeSlice(t, 1, 1))
			}
			into(v.Index(0))
		}
	case reflect.Map:
		return func(v reflect.Value, into func(reflect.Value)) {
			if v.IsNil() {
				v.Set(reflect.MakeMap(t))
			}
			key := reflect.ValueOf("k").Convert(t.Key())
			cur := reflect.New(t.Elem()).Elem()
			if old := v.MapIndex(key); old.IsValid() {
				cur.Set(old)
			}
			into(cur)
			v.SetMapIndex(key, cur)
		}
	}
	return nil
}

// setNonZero gives v a value other than its zero.
func setNonZero(v reflect.Value) {
	t := v.Type()
	switch {
	case t == locationType:
		v.Set(reflect.ValueOf(time.UTC))
		return
	case t == timeType:
		v.Set(reflect.ValueOf(time.Unix(7, 0)))
		return
	case t.Kind() == reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
		return
	}
	switch t.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(t, 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(t)
		m.SetMapIndex(reflect.ValueOf("k").Convert(t.Key()), reflect.Zero(t.Elem()))
		v.Set(m)
	case reflect.Struct:
		// A struct field is non-zero once any of its fields is; the walk
		// sets each of them in turn as well.
		setNonZero(v.Field(0))
	default:
		panic("setNonZero: " + t.String())
	}
}

func TestCheckRunsTheChecksOfLoadOnFilesInMemory(t *testing.T) {
	good := "name: up\nstages:\n  s: {run: echo}\n"
	snap, err := Check(Files{FileName("up"): []byte(good)})
	if err != nil || len(snap.Names) != 1 || string(snap.Texts["up"]) != good || snap.Hashes["up"] == "" {
		t.Fatalf("check: %v, %+v", err, snap)
	}
	_, err = Check(Files{FileName("up"): []byte(good), FileName("down"): []byte("name: down\ntriggers: {after: [nowhere]}\nstages:\n  s: {run: echo}\n")})
	if _, ok := errors.AsType[*Errors](err); !ok || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("a set with a missing upstream: %v", err)
	}
}
