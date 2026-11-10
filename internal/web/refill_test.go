package web

import (
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Obvious test values of the fields a secret goes by: none of them may
// come back from the password step, anywhere.
const (
	refillValue    = "an-obvious-test-secret-value"
	refillUser     = "an-obvious-test-secret-user"
	refillPassword = "an-obvious-test-secret-password"
	refillFile     = "an-obvious-test-secret-file"
)

var refillSecrets = []string{refillValue, refillUser, refillPassword, refillFile}

// secretFieldNames are the names a secret or a password goes by in a form,
// and the fields that are the session's own: none is ever carried.
var secretFieldNames = []string{"value", "file", "user", "password", "current", "new", "again", "csrf", "once"}

func TestNoFieldASecretGoesByIsEverCarriedThroughThePasswordStep(t *testing.T) {
	for route, c := range sudoCarries {
		if _, ok := sudoActions[route]; !ok {
			t.Errorf("%s carries fields but is not a change the password step knows", route)
		}
		if c.form == "" || len(c.fields) == 0 {
			t.Errorf("%s: a carry names its form and at least one field: %+v", route, c)
		}
		for _, f := range c.fields {
			if slices.Contains(secretFieldNames, f) || slices.Contains([]string{"fill", "of", "done"}, f) {
				t.Errorf("%s carries %q, a field a secret or a session goes by or one the page's address uses", route, f)
			}
		}
	}
	if _, ok := sudoCarries[grantsRoute]; ok {
		t.Error("the grants form keeps its own way through the password step")
	}
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

// hiddenOf is what the hidden fields of page send, as a browser would.
func hiddenOf(page string) url.Values {
	out := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(page, -1) {
		out.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return out
}

// lacksEverywhere holds that none of subs is in what the server wrote down:
// its log, the audit and every file of its data, the session store included.
func lacksEverywhere(t *testing.T, e *env, log string, subs ...string) {
	t.Helper()
	lacks(t, log, subs...)
	lacks(t, e.audit(t), subs...)
	data := os.DirFS(e.dir)
	must(t, fs.WalkDir(data, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(data, p)
		if err != nil {
			return err
		}
		for _, s := range subs {
			if strings.Contains(string(data), s) {
				t.Errorf("%s holds %q", p, s)
			}
		}
		return nil
	}))
}

type refillCase struct {
	user string
	// setup makes what the form needs; it may not give the session the
	// password step.
	setup func(*testing.T, *env)
	path  string
	// fields are what the form sends, its secret fields included.
	fields    url.Values
	multipart bool
	back      string
	// button opens the form on the page; of is the row it is on, if any.
	button, of string
	// carried are the fields that come back; filled is what the form
	// holds then, closed the forms that stay closed.
	carried  url.Values
	filled   []string
	unfilled []string
	closed   []string
}

var refillCases = map[string]refillCase{
	"add a user": {
		user: "admin", path: "/admin/users", back: "/admin/users", button: "Add a user",
		fields:  url.Values{"name": {"carol"}, "admin": {"on"}},
		carried: url.Values{"name": {"carol"}, "admin": {"on"}},
		filled:  []string{`name="name" maxlength="64" value="carol"`, `<input type="checkbox" name="admin" checked>`},
	},
	"make a token": {
		user: "alice", path: "/account/tokens", back: "/account", button: "Make a token",
		fields:   url.Values{"label": {"ci"}, "days": {"30"}},
		carried:  url.Values{"label": {"ci"}, "days": {"30"}},
		filled:   []string{`name="label" maxlength="100" value="ci"`, `<option value="30" selected>`},
		unfilled: []string{`<option value="90" selected>`},
		closed:   []string{"Change the password"},
	},
	"make a token for a user": {
		user: "admin", path: "/admin/users/bob/tokens", back: "/admin/users/bob", button: "Make a token",
		fields:   url.Values{"label": {"deploy"}, "days": {"365"}},
		carried:  url.Values{"label": {"deploy"}, "days": {"365"}},
		filled:   []string{`value="deploy"`, `<option value="365" selected>`},
		unfilled: []string{`<option value="90" selected>`},
	},
	"set a secret": {
		user: "admin", path: "/admin/secrets", back: "/admin/secrets", button: "Set a secret", multipart: true,
		fields: url.Values{"name": {"LOGIN"}, "kind": {"userpass"}, "scopes": {"smoke", "ask"},
			"value": {refillValue}, "user": {refillUser}, "password": {refillPassword}},
		carried:  url.Values{"name": {"LOGIN"}, "kind": {"userpass"}, "scopes": {"smoke", "ask"}},
		filled:   []string{`name="name" value="LOGIN"`, `<option value="userpass" selected>`, `<option value="smoke" selected>`, `<option value="ask" selected>`},
		unfilled: []string{`<option value="text" selected>`},
		closed:   []string{"Change"},
	},
	"change a secret's scopes": {
		user: "admin", path: "/admin/secrets/DEPLOY_KEY/scopes", back: "/admin/secrets", button: "Change", of: "DEPLOY_KEY",
		fields:  url.Values{"scopes": {"smoke"}},
		carried: url.Values{"scopes": {"smoke"}},
		filled:  []string{`<option value="smoke" selected>`},
		closed:  []string{"Set a secret"},
	},
	"make a GitHub App": {
		user: "admin", path: "/admin/apps", back: "/admin/apps", button: "Make an App",
		setup: func(t *testing.T, e *env) {
			gh, _ := githubApps(t, 201)
			e.appsOn(t, gh, "https://kipp.example")
		},
		fields:  url.Values{"name": {"my-app"}, "org": {"acme"}},
		carried: url.Values{"name": {"my-app"}, "org": {"acme"}},
		filled:  []string{`name="name" value="my-app"`, `name="org" value="acme"`},
	},
	"add an engine": {
		user: "admin", path: "/admin/runners/engines", back: "/admin/runners", button: "Add an engine",
		fields: url.Values{"name": {"box"}, "url": {"unix:///var/run/docker.sock"}, "labels": {"linux,arm64"}, "max": {"3"},
			"ssh_key": {"/home/kipp/.ssh/id_engine"}},
		carried: url.Values{"name": {"box"}, "url": {"unix:///var/run/docker.sock"}, "labels": {"linux,arm64"}, "max": {"3"},
			"ssh_key": {"/home/kipp/.ssh/id_engine"}},
		filled: []string{`name="name" value="box"`, `value="unix:///var/run/docker.sock"`, `value="linux,arm64"`, `name="max" type="number" min="1" value="3"`,
			`value="/home/kipp/.ssh/id_engine"`},
		closed: []string{"Add a standing runner"},
	},
	"add a standing runner": {
		user: "admin", path: "/admin/runners/runners", back: "/admin/runners", button: "Add a standing runner",
		fields:  url.Values{"name": {"box"}, "labels": {"linux"}, "executors": {"2"}},
		carried: url.Values{"name": {"box"}, "labels": {"linux"}, "executors": {"2"}},
		filled:  []string{`name="name" value="box"`, `name="labels" value="linux"`, `name="executors" type="number" min="1" value="2"`},
		closed:  []string{"Add an engine"},
	},
}

// A form sent without a fresh password comes back, after the password
// step, open and filled with its fields as they were sent, its secret
// fields left out on the way and nowhere written down; a wrong password
// still holds them, and Cancel leads back without them.
func TestAFormThatNeedsThePasswordComesBackFilledWithoutItsSecretFields(t *testing.T) {
	for what, c := range refillCases {
		t.Run(what, func(t *testing.T) {
			e := site(t)
			log := logged(t)
			if c.setup != nil {
				c.setup(t, e)
			}
			cl := e.login(t, c.user)
			sent := c.ask(t, e, cl)
			sent = c.wrongPassword(t, e, cl, sent)
			to := c.rightPassword(t, e, cl, sent)
			c.backFilled(t, e, cl, to)
			lacksEverywhere(t, e, log.String(), refillSecrets...)
		})
	}
}

// wantCarried holds that sent carries the case's fields, and only those.
func (c refillCase) wantCarried(t *testing.T, sent url.Values, prefix string) {
	t.Helper()
	for k, vs := range c.carried {
		eq(t, "carried "+k, strings.Join(sent[prefix+k], ","), strings.Join(vs, ","))
	}
	for _, f := range secretFieldNames {
		if sent.Has(prefix + f) {
			t.Errorf("%q is carried", f)
		}
	}
}

// ask sends the form without a fresh password and returns what the
// password form it is answered with sends.
func (c refillCase) ask(t *testing.T, e *env, cl *http.Client) url.Values {
	t.Helper()
	var res *http.Response
	var page string
	if c.multipart {
		res, page = e.uploadSecret(t, cl, c.fields, []byte(refillFile))
	} else {
		res, page = e.form(t, cl, c.path, c.fields)
	}
	eq(t, "the password is asked", res.StatusCode, http.StatusForbidden)
	contains(t, page, "Confirm your password",
		"Nothing was changed yet: once the password is right the form comes back as you filled it, without its secret fields, to send again.",
		`<a class="link" href="`+c.back+`">Cancel</a>`)
	lacks(t, page, "to send the change again", "the change is made as you ticked it")
	lacks(t, page, refillSecrets...)
	sent := hiddenOf(page)
	c.wantCarried(t, sent, carryPrefix)
	return sent
}

// wrongPassword sends sent with a wrong password, which is refused with
// the fields still carried.
func (c refillCase) wrongPassword(t *testing.T, e *env, cl *http.Client, sent url.Values) url.Values {
	t.Helper()
	sent.Set("password", "wrong")
	res, page := postBody(t, cl, e.srv.URL+"/sudo", sent)
	eq(t, "a wrong password", res.StatusCode, http.StatusUnauthorized)
	contains(t, page, "Wrong password.")
	lacks(t, page, refillSecrets...)
	again := hiddenOf(page)
	c.wantCarried(t, again, carryPrefix)
	return again
}

// rightPassword sends sent with the password and returns where it leads.
func (c refillCase) rightPassword(t *testing.T, e *env, cl *http.Client, sent url.Values) string {
	t.Helper()
	sent.Set("password", "pw")
	res, _ := postBody(t, cl, e.srv.URL+"/sudo", sent)
	eq(t, "the password is right", res.StatusCode, http.StatusSeeOther)
	to := res.Header.Get("Location")
	lacks(t, to, refillSecrets...)
	at, err := url.Parse(to)
	must(t, err)
	eq(t, "back on", at.Path, c.back)
	q := at.Query()
	if q.Get("fill") == "" {
		t.Fatalf("%s names no form to fill", to)
	}
	eq(t, "the row", q.Get("of"), c.of)
	c.wantCarried(t, q, "")
	return to
}

// backFilled holds that the page the password led to has the form open
// and filled, and the page's other forms closed.
func (c refillCase) backFilled(t *testing.T, e *env, cl *http.Client, to string) {
	t.Helper()
	code, page := get(t, cl, e.srv.URL+to)
	eq(t, "the page", code, http.StatusOK)
	body := wantPopAny(t, page, c.button, true)
	contains(t, body, c.filled...)
	lacks(t, body, c.unfilled...)
	lacks(t, page, refillSecrets...)
	for _, b := range c.closed {
		wantPopAny(t, page, b, false)
	}
}

// wantPopAny holds that the form behind button is open or closed, wherever
// the button is.
func wantPopAny(t *testing.T, page, button string, open bool) string {
	t.Helper()
	got, body := popOf(t, page, button)
	eq(t, button+" open", got, open)
	return body
}

// Nothing was changed by the password step: the form is sent again.
func TestThePasswordStepOfAFilledFormChangesNothing(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	_, page := e.form(t, admin, "/admin/users", url.Values{"name": {"carol"}, "admin": {"on"}})
	sent := hiddenOf(page)
	sent.Set("password", "pw")
	res, _ := postBody(t, admin, e.srv.URL+"/sudo", sent)
	eq(t, "the password is right", res.StatusCode, http.StatusSeeOther)
	if _, err := e.st.UserByName(t.Context(), "carol"); err == nil {
		t.Fatal("the password step added the user")
	}
}

// The address a page is filled from fills only the fields its form carries:
// a secret's value put in it by hand is not shown.
func TestAFilledAddressFillsOnlyTheFieldsItsFormCarries(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	q := url.Values{"fill": {"secret"}, "name": {"LOGIN"}, "value": {refillValue}, "user": {refillUser}, "password": {refillPassword}}
	_, page := get(t, admin, e.srv.URL+"/admin/secrets?"+q.Encode())
	body := wantPopAny(t, page, "Set a secret", true)
	contains(t, body, `name="name" value="LOGIN"`)
	lacks(t, page, refillSecrets...)

	// Another form's name fills nothing here.
	_, page = get(t, admin, e.srv.URL+"/admin/users?"+url.Values{"fill": {"runner"}, "name": {"carol"}}.Encode())
	wantPopAny(t, page, "Add a user", false)
	lacks(t, page, `value="carol"`)
}

// A link that picks a secret's name to set opens the form with it, as it
// did before forms were filled from the address.
func TestASetLinkStillOpensTheSecretFormWithItsName(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	_, page := get(t, admin, e.srv.URL+"/admin/secrets?name=CHAT_TOKEN")
	body := wantPopAny(t, page, "Set a secret", true)
	contains(t, body, `name="name" value="CHAT_TOKEN"`, `<option value="text" selected>`)
}

// A route that carries nothing leads back as before: its form is sent
// again from an empty page.
func TestAChangeWithNoFieldsOfItsOwnLeadsBackAsBefore(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	res, page := e.form(t, admin, "/admin/users/bob/password", nil)
	eq(t, "the password is asked", res.StatusCode, http.StatusForbidden)
	contains(t, page, "to send the change again")
	lacks(t, page, `name="carry.`)
	sent := hiddenOf(page)
	sent.Set("password", "pw")
	res, _ = postBody(t, admin, e.srv.URL+"/sudo", sent)
	eq(t, "the password is right", res.StatusCode, http.StatusSeeOther)
	eq(t, "led back", res.Header.Get("Location"), "/admin/users/bob/password")
}
