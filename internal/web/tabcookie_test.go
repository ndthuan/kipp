package web

import "testing"

func TestTheRememberedTabIsASecureCookieForTheWholeSite(t *testing.T) {
	c := tabCookie("unstable")
	if !c.Secure || !c.HttpOnly || c.Value != "unstable" || c.Path != "/" {
		t.Fatalf("%+v", c)
	}
}
