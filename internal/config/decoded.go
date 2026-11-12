package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	locationType = reflect.TypeFor[*time.Location]()
	timeType     = reflect.TypeFor[time.Time]()
)

// DecodedHash is the hash of what a job does, not of the bytes it was
// written in: a comment, key order, quoting or blank lines leave it as it
// is, and any field of the parsed job changes it. A field left at its zero
// value is not written, so a field a later kipp adds leaves the hash of a
// job that does not use it unchanged, and an edit made over a file before
// an upgrade still matches the file after it.
func DecodedHash(j *Job) string {
	h := sha256.New()
	canon(h, reflect.ValueOf(j))
	return hex.EncodeToString(h.Sum(nil))
}

// canon writes v in one form: struct fields by name, map keys sorted.
// A kind a job cannot hold is a bug in kipp, found by the test that walks
// every field of Job.
func canon(w io.Writer, v reflect.Value) {
	if v.Type() == locationType {
		if v.IsNil() {
			_, _ = io.WriteString(w, "~")
			return
		}
		_, _ = io.WriteString(w, strconv.Quote(v.Interface().(*time.Location).String()))
		return
	}
	if v.Type() == timeType {
		_, _ = io.WriteString(w, v.Interface().(time.Time).UTC().Format(time.RFC3339Nano))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			_, _ = io.WriteString(w, "~")
			return
		}
		canon(w, v.Elem())
	case reflect.Struct:
		canonStruct(w, v)
	case reflect.Slice, reflect.Array:
		_, _ = fmt.Fprintf(w, "[%d", v.Len())
		for i := range v.Len() {
			_, _ = io.WriteString(w, ",")
			canon(w, v.Index(i))
		}
		_, _ = io.WriteString(w, "]")
	case reflect.Map:
		canonMap(w, v)
	case reflect.String:
		_, _ = io.WriteString(w, strconv.Quote(v.String()))
	case reflect.Bool:
		_, _ = io.WriteString(w, strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, _ = io.WriteString(w, strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, _ = io.WriteString(w, strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		_, _ = io.WriteString(w, strconv.FormatFloat(v.Float(), 'g', -1, 64))
	default:
		panic("config: DecodedHash cannot write a " + v.Type().String())
	}
}

func canonStruct(w io.Writer, v reflect.Value) {
	_, _ = io.WriteString(w, "{")
	t := v.Type()
	for i := range t.NumField() {
		f := v.Field(i)
		// An unexported field is where a key was in the file (a line
		// number), never what the job does.
		if !t.Field(i).IsExported() || f.IsZero() {
			continue
		}
		_, _ = io.WriteString(w, t.Field(i).Name+"=")
		canon(w, f)
		_, _ = io.WriteString(w, ";")
	}
	_, _ = io.WriteString(w, "}")
}

func canonMap(w io.Writer, v reflect.Value) {
	type entry struct{ key, value string }
	entries := make([]entry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		var k, val strings.Builder
		canon(&k, iter.Key())
		canon(&val, iter.Value())
		entries = append(entries, entry{k.String(), val.String()})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	_, _ = fmt.Fprintf(w, "<%d", len(entries))
	for _, e := range entries {
		_, _ = io.WriteString(w, ","+e.key+":"+e.value)
	}
	_, _ = io.WriteString(w, ">")
}
