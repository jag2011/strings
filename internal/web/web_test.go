package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

type env struct {
	t     *testing.T
	store *store.Store
	srv   *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	f, _ := arb.Parse([]byte(`{"hello": "Hello {name}!", "@hello": {"description": "Greeting"}, "bye": "Bye", "items": "{n, plural, one{# item} other{# items}}"}`))
	s.ImportSource(ctx, f)
	s.AddLanguage(ctx, "de")
	return &env{t: t, store: s, srv: ts}
}

// user creates a user and returns a logged-in client.
func (e *env) user(email, role string) *http.Client {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.store.CreateUser(ctx, email, strings.Split(email, "@")[0], role)
	if err != nil {
		e.t.Fatal(err)
	}
	token, _ := e.store.CreateUserToken(ctx, u.ID)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res := e.post(c, "/invite/"+token, url.Values{"password": {"correct horse battery"}, "password2": {"correct horse battery"}})
	if res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("invite: %d", res.StatusCode)
	}
	return c
}

func (e *env) post(c *http.Client, path string, v url.Values) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func (e *env) get(c *http.Client, path string) (*http.Response, string) {
	e.t.Helper()
	res, err := c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(body)
}

func TestPagesRender(t *testing.T) {
	e := setup(t)
	c := e.user("admin@example.com", "admin")
	for _, p := range []string{"/", "/t/de", "/t/de?filter=all&q=hel", "/t/de/hello", "/account", "/admin", "/admin/users", "/export/intl_de.arb"} {
		res, body := e.get(c, p)
		if res.StatusCode != 200 {
			t.Errorf("GET %s: %d\n%s", p, res.StatusCode, body)
		}
	}
	_, body := e.get(c, "/t/de/hello")
	for _, want := range []string{"Hello {name}!", "Greeting", `lang="de"`, "Save &amp; next"} {
		if !strings.Contains(body, want) {
			t.Errorf("editor misses %q", want)
		}
	}
	if res, _ := e.get(c, "/t/xx"); res.StatusCode != 404 {
		t.Errorf("unknown language: %d", res.StatusCode)
	}
}

func TestLoginRequired(t *testing.T) {
	e := setup(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ := e.get(c, "/t/de")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?next=%2Ft%2Fde" {
		t.Fatalf("got %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	e.user("someone@example.com", "translator")
	res = e.post(c, "/login", url.Values{"email": {"someone@example.com"}, "password": {"wrong"}})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", res.StatusCode)
	}
	res = e.post(c, "/login", url.Values{"email": {"someone@example.com"}, "password": {"correct horse battery"}, "next": {"//evil.example"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Errorf("login: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestTranslateFlow(t *testing.T) {
	e := setup(t)
	c := e.user("tr@example.com", "translator")

	// Broken placeholder: error, nothing saved.
	res := e.post(c, "/t/de/hello", url.Values{"text": {"Hallo {nam}!"}, "base": {"0"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d", res.StatusCode)
	}
	// Valid: saved, on to the next todo string in source order.
	res = e.post(c, "/t/de/hello", url.Values{"text": {"Hallo {name}!"}, "base": {"0"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/t/de/bye" {
		t.Fatalf("save: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// A second editor based on the old state gets a conflict.
	if res := e.post(c, "/t/de/hello", url.Values{"text": {"Hi {name}!"}, "base": {"0"}}); res.StatusCode != http.StatusConflict {
		t.Fatalf("conflict: %d", res.StatusCode)
	}
	// Warnings need confirmation. CRLF from the browser is stored as LF.
	if res := e.post(c, "/t/de/bye", url.Values{"text": {"Tschüss\r\nund weg"}, "base": {"0"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("warning: %d", res.StatusCode)
	}
	res = e.post(c, "/t/de/bye", url.Values{"text": {"Tschüss\r\nund weg"}, "base": {"0"}, "force": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("force: %d", res.StatusCode)
	}
	if en, _ := e.store.Entry(context.Background(), "de", "bye"); en.Text != "Tschüss\nund weg" {
		t.Errorf("stored %q", en.Text)
	}
	// Plural with Czech-like extra categories is fine; losing the plural is not.
	if res := e.post(c, "/t/de/items", url.Values{"text": {"{n} Dinge"}, "base": {"0"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("plural lost: %d", res.StatusCode)
	}
	res = e.post(c, "/t/de/items", url.Values{"text": {"{n, plural, one{# Ding} few{# Dinge} other{# Dinge}}"}, "base": {"0"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/t/de" {
		t.Fatalf("last todo string should go back to the list: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// Translators can't open admin pages.
	if res, _ := e.get(c, "/admin"); res.StatusCode != http.StatusForbidden {
		t.Errorf("admin as translator: %d", res.StatusCode)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	e := setup(t)
	c := e.user("tr@example.com", "translator")
	req, _ := http.NewRequest("POST", e.srv.URL+"/t/de/hello", strings.NewReader("text=x&base=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", res.StatusCode)
	}
}

func TestAPI(t *testing.T) {
	e := setup(t)
	token, _ := e.store.CreateAPIToken(context.Background(), "ci")
	do := func(method, path, body, auth string) (int, string) {
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
	if code, _ := do("GET", "/api/v1/stats", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := do("GET", "/api/v1/stats", "", "st_nope"); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	code, body := do("PUT", "/api/v1/source", `{"hello": "Hello {name}!", "bye": "Goodbye"}`, token)
	if code != 200 || !strings.Contains(body, `"changed": 1`) || !strings.Contains(body, `"removed": 1`) {
		t.Errorf("source: %d %s", code, body)
	}
	code, body = do("PUT", "/api/v1/translations/fr", `{"hello": "Bonjour {name} !", "bye": ""}`, token)
	if code != 200 || !strings.Contains(body, `"added": 1`) {
		t.Errorf("translations: %d %s", code, body)
	}
	code, body = do("GET", "/api/v1/arb/intl_fr.arb", "", token)
	if code != 200 || body != "{\n  \"hello\": \"Bonjour {name} !\"\n}" {
		t.Errorf("export: %d %q", code, body)
	}
	if code, _ := do("GET", "/api/v1/arb/intl_xx.arb", "", token); code != 404 {
		t.Errorf("unknown export: %d", code)
	}
	if code, _ := do("GET", "/api/v1/arb.zip", "", token); code != 200 {
		t.Errorf("zip: %d", code)
	}
}
