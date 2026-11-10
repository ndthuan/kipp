package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
)

type refusedCase struct {
	user  string
	setup func(*testing.T, *env)
	path  string
	// fields are what the form sends, its secret fields included.
	fields    url.Values
	multipart bool
	// code is the status the refusal had before it came back as its form.
	code int
	// button opens the form; filled is what it holds and says, closed the
	// page's other forms.
	button string
	filled []string
	closed []string
}

const refusedAlert = `<p class="notice notice-error" role="alert">`

var refusedCases = map[string]refusedCase{
	"add a user with a name outside the rule": {
		user: "admin", path: "/admin/users", code: http.StatusBadRequest, button: "Add a user",
		fields: url.Values{"name": {"Bad Name!"}, "admin": {"on"}},
		filled: []string{`value="Bad Name!"`, `<input type="checkbox" name="admin" checked>`, "is not a name a user can have"},
	},
	"add a user who is there already": {
		user: "admin", path: "/admin/users", code: http.StatusConflict, button: "Add a user",
		fields: url.Values{"name": {"bob"}},
		filled: []string{`value="bob"`, `<input type="checkbox" name="admin">`, "already exists"},
	},
	"make a token with a lifetime not offered": {
		user: "alice", path: "/account/tokens", code: http.StatusBadRequest, button: "Make a token",
		fields: url.Values{"label": {"ci"}, "days": {"7"}},
		filled: []string{`name="label" maxlength="100" value="ci"`, `<option value="90" selected>`},
		closed: []string{"Change the password"},
	},
	"make a token with no lifetime": {
		user: "alice", path: "/account/tokens", code: http.StatusBadRequest, button: "Make a token",
		fields: url.Values{"label": {"ci"}},
		filled: []string{`name="label" maxlength="100" value="ci"`, "pick how long the token lasts"},
		closed: []string{"Change the password"},
	},
	"make a token for a user with a lifetime not offered": {
		user: "admin", path: "/admin/users/bob/tokens", code: http.StatusBadRequest, button: "Make a token",
		fields: url.Values{"label": {"deploy"}, "days": {"7"}},
		filled: []string{`name="label" maxlength="100" value="deploy"`, `<option value="90" selected>`},
	},
	"set a secret with a name outside the rule": {
		user: "admin", path: "/admin/secrets", code: http.StatusBadRequest, button: "Set a secret", multipart: true,
		fields: url.Values{"name": {"9LOGIN"}, "kind": {"userpass"}, "scopes": {"global"},
			"value": {refillValue}, "user": {refillUser}, "password": {refillPassword}},
		filled: []string{`value="9LOGIN"`, `<option value="userpass" selected>`, `<option value="global" selected>`},
		closed: []string{"Change"},
	},
	"change a secret's scopes to one that is not a scope": {
		user: "admin", path: "/admin/secrets/DEPLOY_KEY/scopes", code: http.StatusBadRequest, button: "Change",
		fields: url.Values{"scopes": {"group:Bad!"}},
		filled: []string{`<option value="group:Bad!" selected>`, "a group name is"},
		closed: []string{"Set a secret"},
	},
	"make a GitHub App with a name outside the rule": {
		user: "admin", path: "/admin/apps", code: http.StatusBadRequest, button: "Make an App",
		setup: func(t *testing.T, e *env) {
			gh, _ := githubApps(t, 201)
			e.appsOn(t, gh, "https://kipp.example")
		},
		fields: url.Values{"name": {"my_app"}, "org": {"acme"}},
		filled: []string{`value="my_app"`, `name="org" value="acme"`},
	},
	"add an engine with a max that is no number": {
		user: "admin", path: "/admin/runners/engines", code: http.StatusBadRequest, button: "Add an engine",
		fields: url.Values{"name": {"box"}, "url": {"unix:///var/run/docker.sock"}, "labels": {"linux"}, "max": {"many"},
			"ssh_key": {"/home/kipp/.ssh/id_engine"}},
		filled: []string{`value="box"`, `value="unix:///var/run/docker.sock"`, `value="linux"`, `value="many"`, `value="/home/kipp/.ssh/id_engine"`, "a whole number"},
		closed: []string{"Add a standing runner"},
	},
	"add a standing runner with a name outside the rule": {
		user: "admin", path: "/admin/runners/runners", code: http.StatusBadRequest, button: "Add a standing runner",
		fields: url.Values{"name": {"Box"}, "labels": {"linux"}, "executors": {"2"}},
		filled: []string{`value="Box"`, `name="labels" value="linux"`, `value="2"`, "lower-case letters"},
		closed: []string{"Add an engine"},
	},
}

// A form refused for what it holds comes back as its own page, with the
// status the refusal has, the form open, its fields as they were sent and
// the reason at its top; its secret fields come back empty and are nowhere
// written down, and the error page is not shown.
func TestARefusedFormComesBackOpenWithItsFieldsAndWhy(t *testing.T) {
	for what, c := range refusedCases {
		t.Run(what, func(t *testing.T) {
			e := site(t)
			log := logged(t)
			if c.setup != nil {
				c.setup(t, e)
			}
			cl := e.login(t, c.user)
			e.sudo(t, cl)
			var res *http.Response
			var page string
			if c.multipart {
				res, page = e.uploadSecret(t, cl, c.fields, []byte(refillFile))
			} else {
				res, page = e.form(t, cl, c.path, c.fields)
			}
			eq(t, "the refusal's status", res.StatusCode, c.code)
			lacks(t, page, "Back to the jobs")
			body := wantPopAny(t, page, c.button, true)
			contains(t, body, c.filled...)
			_, form, _ := strings.Cut(body, "<form")
			if !strings.HasPrefix(strings.TrimSpace(afterHeading(form)), refusedAlert) {
				t.Errorf("the reason is not at the top of the form:\n%s", body)
			}
			eq(t, "reasons on the page", strings.Count(page, refusedAlert), 1)
			for _, b := range c.closed {
				wantPopAny(t, page, b, false)
			}
			lacks(t, page, refillSecrets...)
			lacksEverywhere(t, e, log.String(), refillSecrets...)
		})
	}
}

// afterHeading is a form's body from its opening tag on, after its heading
// if it has one.
func afterHeading(form string) string {
	_, rest, _ := strings.Cut(form, ">")
	for _, h := range []string{"</h2>", "</h3>"} {
		if before, after, ok := strings.Cut(rest, h); ok && !strings.Contains(before, "<input") {
			return after
		}
	}
	return rest
}

// A refusal that is not about what the form holds, such as a page the user
// may not see, is still the error page.
func TestARefusalThatIsNotTheFormsIsStillTheErrorPage(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	e.sudo(t, alice)
	res, page := e.form(t, alice, "/admin/runners/engines", url.Values{"name": {"Box"}, "labels": {"linux"}, "max": {"1"}})
	eq(t, "not an admin", res.StatusCode, http.StatusForbidden)
	contains(t, page, "You are not allowed to do that.", `<a class="link" href="/">Back to the jobs</a>`)
	lacks(t, page, `value="Box"`)
}

func referred(t *testing.T, e *env, c *http.Client, path, referer string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	must(t, err)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	res, page := do(t, c, req)
	eq(t, "the error page", res.StatusCode, http.StatusNotFound)
	return page
}

// The error page leads back to the page of this site the request came
// from, and to the jobs when it came from no page of this site.
func TestTheErrorPageLeadsBackToThePageOfThisSiteItCameFrom(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	page := referred(t, e, alice, "/jobs/nothing-here", e.srv.URL+"/admin/users/bob?fill=token")
	contains(t, page, `<a class="link" href="/admin/users/bob">Back</a>`)
	lacks(t, page, "Back to the jobs", "fill=token")
	for _, from := range []string{"", "https://elsewhere.example/admin/users", "not a url at all", e.srv.URL + "//elsewhere.example/x"} {
		page := referred(t, e, alice, "/jobs/nothing-here", from)
		contains(t, page, `<a class="link" href="/">Back to the jobs</a>`)
	}
}

// Each name rule the browser is told is the server's: its pattern, within
// its length, accepts and refuses what the Go rule does.
func TestTheNamePatternsAcceptAndRefuseWhatTheServersRulesDo(t *testing.T) {
	rules := map[string]func(string) bool{
		"job":    config.ValidName,
		"user":   auth.ValidUserName,
		"secret": config.ValidEnvName,
		"app":    func(s string) bool { return config.CheckAppName(s) == nil },
	}
	names := []string{"", "a", "0", "smoke", "web.deploy-2_x", "Smoke", "-a", ".a", "_a", "a b", "a!", "a/b", "a:b",
		"A", "LOGIN", "_LOGIN", "LOGIN_2", "9LOGIN", "LOG-IN", "log.in", "my-app", "my_app", "a-", "a\n",
		strings.Repeat("a", 64), strings.Repeat("a", 65), "été"}
	for rule, valid := range rules {
		n, ok := nameRules[rule]
		if !ok {
			t.Fatalf("no pattern for the %s rule", rule)
		}
		re := regexp.MustCompile(`^(?:` + n.Pattern + `)$`)
		for _, name := range names {
			browser := re.MatchString(name) && (n.Max == 0 || len([]rune(name)) <= n.Max)
			if browser != valid(name) {
				t.Errorf("%s %q: the browser says %v, the server %v", rule, name, browser, valid(name))
			}
		}
		if n.Title == "" {
			t.Errorf("the %s rule says nothing to a person", rule)
		}
	}
	if len(nameRules) != len(rules) {
		t.Errorf("a pattern no rule is held to: %v", nameRules)
	}
}

// The name fields carry their rule, so a browser says what is wrong before
// the form is sent.
func TestTheNameFieldsCarryTheirRule(t *testing.T) {
	e := site(t)
	gh, _ := githubApps(t, 201)
	e.appsOn(t, gh, "https://kipp.example")
	admin := e.login(t, "admin")
	for path, fields := range map[string]map[string]string{
		"/admin/users":   {"user-name": "user"},
		"/admin/secrets": {"secret-name": "secret"},
		"/admin/apps":    {"app-name": "app"},
		"/admin/runners": {"engine-name": "job", "runner-name": "job"},
	} {
		_, page := get(t, admin, e.srv.URL+path)
		for id, rule := range fields {
			n := nameRules[rule]
			_, input, ok := strings.Cut(page, `id="`+id+`"`)
			if !ok {
				t.Fatalf("%s has no %s", path, id)
			}
			input, _, _ = strings.Cut(input, ">")
			contains(t, input, `pattern="`+n.Pattern+`"`, `title="`+n.Title+`"`)
			if n.Max > 0 {
				contains(t, input, `maxlength="`+strconv.Itoa(n.Max)+`"`)
			}
		}
	}
}
