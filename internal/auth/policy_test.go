package auth

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func refuses(t *testing.T, err error, says string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want refused with %q", says)
	}
	if !strings.Contains(err.Error(), says) {
		t.Fatalf("refused with %q; want %q in it", err, says)
	}
}

func TestAPasswordIsFifteenCharactersOrMoreCountedAsCharactersNotBytes(t *testing.T) {
	refuses(t, CheckPassword("fourteen chars"), "at least 15 characters")
	refuses(t, CheckPassword("ééééééééééééé1"), "at least 15 characters")
	for _, pw := range []string{"fifteen chars!!", "éééééééééééééé1"} {
		if err := CheckPassword(pw); err != nil {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

func TestAPasswordIsAtMost256Characters(t *testing.T) {
	if err := CheckPassword(strings.Repeat("ab cd", 51) + "e"); err != nil {
		t.Errorf("256 characters: %v", err)
	}
	refuses(t, CheckPassword(strings.Repeat("ab cd", 51)+"ef"), "at most 256 characters")
}

func TestAPasswordNeedsNoParticularKindOfCharacter(t *testing.T) {
	for _, pw := range []string{"all lower case words", "ALLUPPERCASEONLY", "4729183650274618", "nospacesatallinthis"} {
		if err := CheckPassword(pw); err != nil {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

func TestAPasswordOnTheBlocklistIsRefusedInAnyCase(t *testing.T) {
	for _, pw := range []string{"1q2w3e4r5t6y7u8i", "1Q2W3E4R5T6Y7U8I"} {
		refuses(t, CheckPassword(pw), "common or leaked")
	}
}

func TestAPasswordMayNotEqualOrContainAContextWordInAnyCase(t *testing.T) {
	refuses(t, CheckPassword("deploy-bot-2026!", "deploy-bot", "kipp"), `"deploy-bot"`)
	refuses(t, CheckPassword("x123DEPLOY-BOT321", "deploy-bot", "kipp"), `"deploy-bot"`)
	refuses(t, CheckPassword("my Kipp server pw", "deploy-bot", "kipp"), `"kipp"`)
	refuses(t, CheckPassword("someone-long-name", "someone-long-name"), `"someone-long-name"`)
	if err := CheckPassword("a password of mine", "", "kipp"); err != nil {
		t.Errorf("an empty context word is no rule: %v", err)
	}
}

func TestTheBlocklistIsSortedLowerCaseUniqueAndFifteenCharactersOrMore(t *testing.T) {
	list := blocklist()
	if len(list) < 10000 {
		t.Fatalf("%d entries; the list did not load", len(list))
	}
	if !slices.IsSorted(list) || len(slices.Compact(slices.Clone(list))) != len(list) {
		t.Fatal("the list is not sorted and unique, so a lookup would miss")
	}
	for _, e := range list {
		if e != strings.ToLower(e) || utf8.RuneCountInString(e) < minPassword {
			t.Fatalf("%q can never match a password that passes the length rule", e)
		}
	}
}

func TestAUserNameIsAJobNameOfOneTo64Characters(t *testing.T) {
	for name, want := range map[string]bool{
		"admin":                 true,
		"ci-runner":             true,
		"a.b_c-9":               true,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
		"":                      false,
		"Admin":                 false,
		"a b":                   false,
		"-a":                    false,
		"a/b":                   false,
		"<script>":              false,
	} {
		if got := ValidUserName(name); got != want {
			t.Errorf("ValidUserName(%q): got %v, want %v", name, got, want)
		}
	}
}

func TestTheBlocklistAddsLessThanOneMegabyteToTheBinary(t *testing.T) {
	if len(blocklistFile) >= 1<<20 {
		t.Fatalf("%d bytes; cut the source lists shorter", len(blocklistFile))
	}
}

func TestTheRefusalOfAUserNameSaysWhatANameIs(t *testing.T) {
	if err := CheckUserName("alice"); err != nil {
		t.Fatal(err)
	}
	err := CheckUserName("Alice")
	if err == nil || !strings.Contains(err.Error(), `"Alice" is not a name a user can have: 1 to 64 lower-case letters`) {
		t.Fatalf("got %v", err)
	}
}
