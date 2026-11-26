package dispatch

import (
	"slices"
	"strings"
	"testing"
)

func TestPassEnvKeepsTheNamedVariablesAndThePrefixesWithAStar(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/home/k", "LC_ALL=C", "LC_TIME=x", "LANG=C", "AWS_SECRET_ACCESS_KEY=k", "PATHX=1", "TZ=UTC", "NOEQUALS"}
	got := PassEnv(environ, DefaultPassEnv)
	want := []string{"PATH=/bin", "HOME=/home/k", "LC_ALL=C", "LC_TIME=x", "LANG=C", "TZ=UTC"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := PassEnv(environ, []string{"*"}); !slices.Equal(got, environ) {
		t.Fatalf("* kept %q", got)
	}
	if got := PassEnv(environ, nil); len(got) != 0 {
		t.Fatalf("an empty list kept %q", got)
	}
	if got := PassEnv(environ, []string{"AWS_*"}); !slices.Equal(got, []string{"AWS_SECRET_ACCESS_KEY=k"}) {
		t.Fatalf("AWS_* kept %q", got)
	}
}

func TestPassEnvListsAreNamesOrPrefixesWithAStar(t *testing.T) {
	for _, c := range [][2]string{
		{"PATH,HOME", "PATH HOME"},
		{" PATH , LC_* ,", "PATH LC_*"},
		{"*", "*"},
		{"", ""},
		{"PATH,*", "PATH *"},
	} {
		got, err := ParsePassEnv(c[0])
		if err != nil || strings.Join(got, " ") != c[1] {
			t.Errorf("%q: %q %v, want %q", c[0], got, err, c[1])
		}
	}
	for _, bad := range []string{"PA TH", "*PATH", "LC_*X", "A=B", "1X", "LC**"} {
		if _, err := ParsePassEnv(bad); err == nil || !strings.Contains(err.Error(), "--pass-env") {
			t.Errorf("%q taken: %v", bad, err)
		}
	}
}
