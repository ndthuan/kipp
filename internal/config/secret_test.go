package config

import (
	"slices"
	"strings"
	"testing"
)

func TestASecretIsOneLineOfEightBytesTo64KiBUnderAShellName(t *testing.T) {
	for _, c := range []struct {
		what, name, value, want string
	}{
		{what: "a fine one", name: "TOKEN", value: "an obvious test value"},
		{what: "eight bytes", name: "T", value: "12345678"},
		{what: "64 KiB", name: "T", value: strings.Repeat("x", MaxSecret)},
		{what: "a bad name", name: "NOT-A-NAME", value: "an obvious test value", want: `"NOT-A-NAME" is not a name a shell accepts`},
		{what: "seven bytes", name: "T", value: "1234567", want: "at least 8 characters"},
		{what: "over 64 KiB", name: "T", value: strings.Repeat("x", MaxSecret+1), want: "at most 65536 bytes"},
		{what: "two lines", name: "T", value: "first line\nsecond", want: "one line"},
		{what: "a carriage return", name: "T", value: "first line\rsecond", want: "one line"},
	} {
		err := CheckSecret(c.name, c.value)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.what, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.what, err, c.want)
		case err != nil && strings.Contains(err.Error(), c.value):
			t.Errorf("%s: the refusal holds the value", c.what)
		}
	}
}

func TestAFileSecretIsOneByteToAMebibyte(t *testing.T) {
	for what, c := range map[string]struct {
		n  int
		ok bool
	}{"empty": {0, false}, "one byte": {1, true}, "a mebibyte": {MaxSecretFile, true}, "one more": {MaxSecretFile + 1, false}} {
		if err := CheckSecretFile(make([]byte, c.n)); (err == nil) != c.ok {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestAUserIsOneLineOfNoColon(t *testing.T) {
	for user, ok := range map[string]bool{"deployer": true, "": false, "a:b": false, "a\nb": false, strings.Repeat("u", MaxSecretUser+1): false} {
		if err := CheckSecretUser(user); (err == nil) != ok {
			t.Errorf("%q: %v", user, err)
		}
	}
}

func TestAFileIsMaskedByItsLongLinesOnlyAndOnlyWhenItIsText(t *testing.T) {
	got := SecretMasks([]byte("-----BEGIN KEY-----\nshort\n   padded-line-here   \r\nsame-line-twice\nsame-line-twice\n"))
	want := []string{"-----BEGIN KEY-----", "padded-line-here", "same-line-twice"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := SecretMasks([]byte("\xff\xfe a line of eight or more")); got != nil {
		t.Fatalf("bytes that are not UTF-8 are masked: %q", got)
	}
}

func TestAScopeIsGlobalAGroupOrAJobPatternPathMatchCanRead(t *testing.T) {
	for _, ok := range []string{"global", "group:site", "job:site-*", "job:acme.site", "job:[ab]"} {
		if err := CheckScope(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "everyone", "group:", "group:Bad Name", "job:", "job:[", "job:a\nb"} {
		if CheckScope(bad) == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
	if CheckScopes(nil) == nil || CheckScopes([]string{"global", "x"}) == nil {
		t.Error("a secret needs one scope and every one must be valid")
	}
}

func TestScopesReachByGlobalGroupAndJobPatternAndThroughTheJobFileAJobComesFrom(t *testing.T) {
	site := &Job{Name: "site-deploy", Group: "site"}
	other := &Job{Name: "billing"}
	child := (&Job{Name: "site", Group: "web", Multibranch: &Multibranch{}}).Child(ChildRecord{Kind: KindBranch, Branch: "main"})
	repo := (&Job{Name: "acme", Owner: &Owner{}}).Node(RepoRecord{Repo: "acme/shop"})
	grandchild := repo.Child(ChildRecord{Kind: KindBranch, Branch: "main"})
	for _, c := range []struct {
		scopes []string
		job    *Job
		want   bool
	}{
		{[]string{"global"}, other, true},
		{[]string{"group:site"}, site, true},
		{[]string{"group:site"}, other, false},
		{[]string{"group:site"}, &Job{Name: "x"}, false},
		{[]string{"job:site-*"}, site, true},
		{[]string{"job:site-*"}, other, false},
		{[]string{"job:billing", "group:site"}, site, true},
		{nil, site, false},
		{[]string{"job:site"}, child, true},
		{[]string{"group:web"}, child, true},
		{[]string{"job:acme"}, repo, true},
		{[]string{"job:acme"}, grandchild, true},
		{[]string{"job:" + repo.Name}, grandchild, true},
		{[]string{"job:other"}, grandchild, false},
	} {
		if got := ScopesReach(c.scopes, c.job); got != c.want {
			t.Errorf("%q reach %s: %v, want %v", c.scopes, c.job.Name, got, c.want)
		}
	}
}
