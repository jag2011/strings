package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
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
	s.ImportSource(ctx, f, nil)
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

func TestApprovalFlow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.user("tr@example.com", "translator")
	pr := e.user("pr@example.com", "proofreader")
	approved := func() bool {
		en, _ := e.store.Entry(ctx, "de", "bye")
		return en.Approved
	}
	base := func() string {
		en, _ := e.store.Entry(ctx, "de", "bye")
		return fmt.Sprint(en.HistoryID)
	}

	// Translators can't approve; the checkbox is ignored.
	if res := e.post(tr, "/t/de/bye", url.Values{"text": {"Tschüss"}, "base": {"0"}, "approved": {"1"}}); res.StatusCode != http.StatusSeeOther || approved() {
		t.Fatalf("translator approve: %d %v", res.StatusCode, approved())
	}
	if _, body := e.get(tr, "/t/de/bye"); strings.Contains(body, `name="approved"`) {
		t.Error("translators get no checkbox")
	}
	// Proofreaders can, without changing the text.
	if res := e.post(pr, "/t/de/bye", url.Values{"text": {"Tschüss"}, "base": {base()}, "approved": {"1"}, "action": {"stay"}}); res.StatusCode != http.StatusSeeOther || !approved() {
		t.Fatalf("approve: %d %v", res.StatusCode, approved())
	}
	if _, body := e.get(tr, "/t/de?filter=all"); !strings.Contains(body, `aria-label="approved"`) {
		t.Error("list shows no checkmark")
	}
	if _, body := e.get(tr, "/t/de?filter=unapproved"); strings.Contains(body, `id="bye"`) {
		t.Error("approved string in the unapproved filter")
	}

	// Now translators can't edit it.
	_, body := e.get(tr, "/t/de/bye")
	if !strings.Contains(body, "readonly") || strings.Contains(body, "Save &amp; next") {
		t.Error("translator editor should be read-only")
	}
	if res := e.post(tr, "/t/de/bye", url.Values{"text": {"Ciao"}, "base": {base()}}); res.StatusCode != http.StatusConflict {
		t.Errorf("translator edit approved: %d", res.StatusCode)
	}
	// A proofreader whose editor didn't show the approval doesn't undo it.
	if res := e.post(pr, "/t/de/bye", url.Values{"text": {"Ciao"}, "base": {base()}}); res.StatusCode != http.StatusConflict || !approved() {
		t.Errorf("stale proofreader edit: %d", res.StatusCode)
	}
	// Unchecking removes the approval, and the text can change with it.
	if res := e.post(pr, "/t/de/bye", url.Values{"text": {"Ciao"}, "base": {base()}, "was_approved": {"1"}, "action": {"stay"}}); res.StatusCode != http.StatusSeeOther || approved() {
		t.Fatalf("unapprove: %d %v", res.StatusCode, approved())
	}
	if en, _ := e.store.Entry(ctx, "de", "bye"); en.Text != "Ciao" {
		t.Errorf("text %q", en.Text)
	}

	// Both show up in the activity, overall and for the language.
	for _, path := range []string{"/activity", "/activity?lang=de"} {
		res, body := e.get(tr, path)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, "✓ approved") || !strings.Contains(body, "approval removed") ||
			!strings.Contains(body, "<ins>Ciao</ins>") {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if _, body := e.get(tr, "/t/de"); !strings.Contains(body, `href="/activity?lang=de"`) {
		t.Error("no activity link in the language page")
	}
	if res, _ := e.get(tr, "/activity?lang=xx"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown language: %d", res.StatusCode)
	}

	// Admins can make proofreaders.
	ad := e.user("ad@example.com", "admin")
	if res := e.post(ad, "/admin/users", url.Values{"email": {"p2@example.com"}, "name": {"P"}, "role": {"proofreader"}}); res.StatusCode != http.StatusOK {
		t.Errorf("create proofreader: %d", res.StatusCode)
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

func (e *env) upload(c *http.Client, files map[string]string) *http.Response {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write([]byte(content))
	}
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestImportReview(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.user("admin@example.com", "admin")
	e.store.ImportTranslations(ctx, "de", mustParse(t, `{"hello": "Hallo {name}!", "bye": "Tschüss"}`))

	// Translation files alone are imported right away.
	if res := e.upload(c, map[string]string{"intl_fr.arb": `{"bye": "Salut"}`}); res.StatusCode != 200 {
		t.Fatalf("translations only: %d", res.StatusCode)
	}

	// With the source file, the changes are shown first.
	src := `{"hello": "Hello, {name}!", "bye": "Goodbye", "new": "New"}`
	res := e.upload(c, map[string]string{"intl_en.arb": src, "intl_de.arb": `{"new": "Neu"}`})
	review := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(review, "/admin/import/") {
		t.Fatalf("upload: %d %s", res.StatusCode, review)
	}
	_, body := e.get(c, review)
	for _, want := range []string{"1 → 2", "Hello<ins>,</ins> {name}!", `name="t.hello" value="keep" checked`, "intl_de.arb"} {
		if !strings.Contains(body, want) {
			t.Errorf("review misses %q", want)
		}
	}
	if en, _ := e.store.Entry(ctx, "de", "hello"); en.Source != "Hello {name}!" {
		t.Fatal("nothing is imported before confirming")
	}

	// Stale: someone imported the source in between.
	other := e.upload(c, map[string]string{"intl_en.arb": src}).Header.Get("Location")
	e.post(c, other, url.Values{"revision": {"1"}})
	if res := e.post(c, review, url.Values{"revision": {"1"}}); res.StatusCode != http.StatusConflict {
		t.Errorf("stale review: %d", res.StatusCode)
	}

	// Cancel drops the upload.
	if res := e.post(c, review+"/cancel", nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("cancel: %d", res.StatusCode)
	}
	if res, _ := e.get(c, review); res.StatusCode != 404 {
		t.Errorf("cancelled upload: %d", res.StatusCode)
	}
}

func TestImportReviewDecisions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.user("admin@example.com", "admin")
	e.store.ImportTranslations(ctx, "de", mustParse(t, `{"hello": "Hallo {name}!", "bye": "Tschüss"}`))

	review := e.upload(c, map[string]string{
		"intl_en.arb": `{"hello": "Hello, {name}!", "bye": "Goodbye", "new": "New"}`,
		"intl_de.arb": `{"new": "Neu"}`,
	}).Header.Get("Location")
	res := e.post(c, review, url.Values{"revision": {"1"}, "t.hello": {"keep"}, "t.bye": {"remove"}})
	if res.StatusCode != 200 {
		t.Fatalf("confirm: %d", res.StatusCode)
	}
	if en, _ := e.store.Entry(ctx, "de", "hello"); en.Outdated || en.Source != "Hello, {name}!" {
		t.Errorf("hello kept: %+v", en.Row)
	}
	if en, _ := e.store.Entry(ctx, "de", "bye"); en.Has {
		t.Errorf("bye removed: %+v", en.Row)
	}
	if en, _ := e.store.Entry(ctx, "de", "new"); en.Text != "Neu" {
		t.Errorf("translation file imported after the source: %+v", en.Row)
	}
	if res, _ := e.get(c, review); res.StatusCode != 404 {
		t.Errorf("upload should be gone after import: %d", res.StatusCode)
	}
}

func mustParse(t *testing.T, src string) *arb.File {
	t.Helper()
	f, err := arb.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return f
}
