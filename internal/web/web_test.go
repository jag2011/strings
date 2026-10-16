package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

type env struct {
	t     *testing.T
	store *store.Store
	srv   *httptest.Server
	base  string // prefixed to paths in get and post
}

func setup(t *testing.T) *env { return setupWith(t, nil) }

func setupWith(t *testing.T, external *External, opts ...func(*Server)) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := New(s, external)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(w)
	}
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	f, _ := arb.Parse([]byte(`{"hello": "Hello {name}!", "@hello": {"description": "Greeting"}, "bye": "Bye", "items": "{n, plural, one{# item} other{# items}}"}`))
	s.ImportSource(ctx, f, nil)
	s.AddLanguage(ctx, "de")
	return &env{t: t, store: s, srv: ts, base: w.Base}
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
	req, _ := http.NewRequest("POST", e.srv.URL+e.base+path, strings.NewReader(v.Encode()))
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
	res, err := c.Get(e.srv.URL + e.base + path)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(body)
}

func TestProfile(t *testing.T) {
	e := setup(t)
	admin := e.user("admin@example.com", "admin") // id 1
	tr := e.user("tr@example.com", "translator")  // id 2
	ctx := context.Background()
	e.store.Save(ctx, "de", "hello", "Hallo {name}!", 2, -1, store.KeepApproval)

	if res, _ := e.get(tr, "/account"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/u/2" {
		t.Errorf("/account: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Own profile: email, the edit, the nav item marked current.
	_, body := e.get(tr, "/u/2")
	for _, want := range []string{"tr@example.com", "Hallo {name}!", `href="/u/2" aria-current="page"`, "<strong>1</strong> string translated"} {
		if !strings.Contains(body, want) {
			t.Errorf("own profile misses %q", want)
		}
	}
	// Someone else's: no email for translators, but for admins.
	_, body = e.get(tr, "/u/1")
	if strings.Contains(body, "admin@example.com") {
		t.Error("translator sees another user's email")
	}
	if strings.Contains(body, `href="/u/2" aria-current`) {
		t.Error("Profile marked current on someone else's profile")
	}
	if _, body := e.get(admin, "/u/2"); !strings.Contains(body, "tr@example.com") {
		t.Error("admin doesn't see the email")
	}
	if res, _ := e.get(tr, "/u/99"); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing user: %d", res.StatusCode)
	}
	// Names link to profiles.
	if _, body := e.get(tr, "/t/de/hello"); !strings.Contains(body, `<a href="/u/2">tr</a>`) {
		t.Error("editor doesn't link the translator")
	}
}

func TestPagesRender(t *testing.T) {
	e := setup(t)
	c := e.user("admin@example.com", "admin")
	for _, p := range []string{"/", "/t/de", "/t/de?filter=all&q=hel", "/t/de/hello", "/u/1", "/admin", "/admin/users", "/export/intl_de.arb"} {
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

// fakeAccounts is an external account server whose codes work once.
type fakeAccounts struct {
	mu    sync.Mutex
	codes map[string]externalAccount
}

func (f *fakeAccounts) add(code, id, email, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = externalAccount{ID: id, Email: email, Name: name}
}

func (f *fakeAccounts) has(code string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.codes[code]
	return ok
}

func setupExternal(t *testing.T) (*env, *fakeAccounts) {
	f := &fakeAccounts{codes: map[string]externalAccount{}}
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct{ Code string }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		a, ok := f.codes[req.Code]
		delete(f.codes, req.Code)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(as.Close)
	return setupWith(t, &External{Name: "Home Quest", URL: as.URL + "/w/redeem", Secret: "s3cret"}), f
}

func (e *env) signInWithCode(code string) (*http.Client, *http.Response) {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, e.post(c, "/auth/app", url.Values{"code": {code}})
}

func TestExternalSignIn(t *testing.T) {
	e, f := setupExternal(t)
	ctx := context.Background()

	_, body := e.get(http.DefaultClient, "/login")
	if !strings.Contains(body, "Have a Home Quest account?") {
		t.Error("login page doesn't mention Home Quest")
	}

	// Opening the link only shows a button, so mail scanners don't use the code.
	f.add("c1", "hq-1", "Player@Example.com", "Nick")
	res, body := e.get(http.DefaultClient, "/auth/app?code=c1")
	if res.StatusCode != 200 || !strings.Contains(body, `value="c1"`) || !f.has("c1") {
		t.Fatalf("GET: %d, code kept: %v\n%s", res.StatusCode, f.has("c1"), body)
	}

	c, res := e.signInWithCode("c1")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("sign in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := e.get(c, "/t/de"); res.StatusCode != 200 {
		t.Errorf("signed-in client: %d", res.StatusCode)
	}
	users, _ := e.store.Users(ctx)
	if len(users) != 1 || users[0].Email != "player@example.com" || users[0].Name != "Nick" || users[0].Role != "translator" || !users[0].External {
		t.Fatalf("user: %+v", users[0])
	}
	if _, res := e.signInWithCode("c1"); res.StatusCode != http.StatusNotFound {
		t.Errorf("reused code: %d", res.StatusCode)
	}

	// The name and email follow the account.
	f.add("c2", "hq-1", "new@example.com", "Nick2")
	e.signInWithCode("c2")
	users, _ = e.store.Users(ctx)
	if len(users) != 1 || users[0].Name != "Nick2" || users[0].Email != "new@example.com" {
		t.Errorf("after rename: %+v", users)
	}
	nick := users[0].ID

	// Admins can't rename external users.
	admin := e.user("admin@example.com", "admin")
	e.post(admin, fmt.Sprintf("/admin/users/%d", nick), url.Values{"name": {"Other"}, "role": {"proofreader"}})
	if u, _ := e.store.User(ctx, nick); u.Name != "Nick2" || u.Role != "proofreader" {
		t.Errorf("after admin edit: %+v", u)
	}

	// An existing user with the same email is linked and keeps their role.
	e.user("admin2@example.com", "admin")
	f.add("c3", "hq-2", "ADMIN2@example.com", "Boss")
	e.signInWithCode("c3")
	users, _ = e.store.Users(ctx)
	if len(users) != 3 {
		t.Fatalf("got %d users", len(users))
	}
	for _, u := range users {
		if u.Email == "admin2@example.com" && (u.Name != "Boss" || u.Role != "admin" || !u.External) {
			t.Errorf("linked user: %+v", u)
		}
	}

	// Disabled users stay out.
	e.store.UpdateUser(ctx, nick, "x", "translator", true, false)
	f.add("c4", "hq-1", "new@example.com", "Nick2")
	if _, res := e.signInWithCode("c4"); res.StatusCode != http.StatusForbidden {
		t.Errorf("disabled: %d", res.StatusCode)
	}
}

func TestExternalSignInOff(t *testing.T) {
	e := setup(t)
	if res, _ := e.get(http.DefaultClient, "/auth/app?code=x"); res.StatusCode != 404 {
		t.Errorf("GET without account server: %d", res.StatusCode)
	}
}

func TestMergeUser(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	admin := e.user("admin@example.com", "admin")
	old, _ := e.store.CreateUser(ctx, "old@crowdin.invalid", "Old", "translator")
	e.store.Save(ctx, "de", "bye", "Tschüss", old.ID, -1, store.KeepApproval)
	users, _ := e.store.Users(ctx)
	var me *store.User
	for _, u := range users {
		if u.Email == "admin@example.com" {
			me = u
		}
	}
	nu, _ := e.store.CreateUser(ctx, "new@example.com", "New", "translator")

	_, body := e.get(admin, "/admin/users")
	if !strings.Contains(body, fmt.Sprintf(`href="/admin/users/%d/merge"`, old.ID)) || strings.Contains(body, fmt.Sprintf(`/admin/users/%d/merge"`, me.ID)) {
		t.Error("merge offered for the wrong users")
	}
	res, body := e.get(admin, fmt.Sprintf("/admin/users/%d/merge", old.ID))
	if res.StatusCode != 200 || !strings.Contains(body, "their 1 translation and history") || strings.Contains(body, `value="`+fmt.Sprint(old.ID)+`"`) {
		t.Errorf("merge page: %d\n%s", res.StatusCode, body)
	}
	if res := e.post(admin, fmt.Sprintf("/admin/users/%d/merge", old.ID), url.Values{}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("merge without target: %d", res.StatusCode)
	}
	if res := e.post(admin, fmt.Sprintf("/admin/users/%d/merge", me.ID), url.Values{"into": {fmt.Sprint(nu.ID)}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("merge own account: %d", res.StatusCode)
	}
	res = e.post(admin, fmt.Sprintf("/admin/users/%d/merge", old.ID), url.Values{"into": {fmt.Sprint(nu.ID)}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != fmt.Sprintf("/admin/users#user-%d", nu.ID) {
		t.Fatalf("merge: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if n, _, _ := e.store.Contributions(ctx, nu.ID); n != 1 {
		t.Errorf("translations after merge: %d", n)
	}
	if res, _ := e.get(admin, fmt.Sprintf("/admin/users/%d/merge", old.ID)); res.StatusCode != 404 {
		t.Errorf("merged user's page: %d", res.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	e := setup(t)
	res, body := e.get(http.DefaultClient, "/health")
	if res.StatusCode != http.StatusOK || body != "ok\n" {
		t.Fatalf("health: %d %q", res.StatusCode, body)
	}
	e.store.Close()
	if res, _ := e.get(http.DefaultClient, "/health"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health with closed db: %d", res.StatusCode)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		trust bool
		xff   []string
		want  string
	}{
		{false, nil, "10.0.0.1"},
		{false, []string{"203.0.113.9"}, "10.0.0.1"},
		{true, nil, "10.0.0.1"},
		{true, []string{"203.0.113.9"}, "203.0.113.9"},
		// The client sent a forged entry; the proxy appended the real one.
		{true, []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		{true, []string{"1.2.3.4", "203.0.113.9"}, "203.0.113.9"},
		{true, []string{" "}, "10.0.0.1"},
	} {
		srv := &Server{TrustProxy: tc.trust}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:5000"
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := srv.clientIP(r); got != tc.want {
			t.Errorf("trust=%v xff=%q: got %s, want %s", tc.trust, tc.xff, got, tc.want)
		}
	}
}

func TestBasePath(t *testing.T) {
	e := setupWith(t, nil, func(s *Server) { s.Base = "/strings" })
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	raw := func(path string) *http.Response {
		t.Helper()
		res, err := noFollow.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	for path, want := range map[string]int{
		"/":                       http.StatusNotFound,
		"/login":                  http.StatusNotFound,
		"/stringsx/login":         http.StatusNotFound,
		"/health":                 http.StatusOK,
		"/strings/health":         http.StatusOK,
		"/strings/login":          http.StatusOK,
		"/strings/static/app.css": http.StatusOK,
	} {
		if res := raw(path); res.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, res.StatusCode, want)
		}
	}
	if loc := raw("/strings").Header.Get("Location"); loc != "/strings/" {
		t.Errorf("/strings redirects to %q", loc)
	}
	if loc := raw("/strings/t/de").Header.Get("Location"); loc != "/strings/login?next=%2Ft%2Fde" {
		t.Errorf("login redirect to %q", loc)
	}

	c := e.user("admin@example.com", "admin")
	u, _ := url.Parse(e.srv.URL + "/strings/")
	if len(c.Jar.Cookies(u)) == 0 {
		t.Error("no session cookie under /strings/")
	}
	if u, _ := url.Parse(e.srv.URL + "/"); len(c.Jar.Cookies(u)) != 0 {
		t.Error("session cookie leaks to the root")
	}
	res, body := e.get(c, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("home: %d", res.StatusCode)
	}
	for _, want := range []string{`href="/strings/t/de"`, `href="/strings/static/app.css`, `action="/strings/logout"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home misses %s", want)
		}
	}
	for _, bad := range []string{`href="/t/`, `action="/logout"`, `"/static/`} {
		if strings.Contains(body, bad) {
			t.Errorf("home has unprefixed %s", bad)
		}
	}

	res = e.post(c, "/t/de/hello", url.Values{"text": {"Hallo {name}!"}})
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/strings/t/de") {
		t.Errorf("save redirects to %q", loc)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/strings/admin/users", strings.NewReader(url.Values{"email": {"new@example.com"}, "name": {"New"}, "role": {"translator"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), e.srv.URL+"/strings/invite/") {
		t.Error("invite link misses the base path")
	}
}

func TestSuggestionsFlow(t *testing.T) {
	e := setup(t)
	admin := e.user("admin@example.com", "admin")
	tr := e.user("tr@example.com", "translator")
	ctx := context.Background()

	if res := e.post(admin, "/admin/users/ai", url.Values{"name": {"Claude Opus"}, "model": {"anthropic/claude-opus-5.5"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("add AI user: %d", res.StatusCode)
	}
	users, _ := e.store.Users(ctx)
	var ai *store.User
	for _, u := range users {
		if u.IsAI() {
			ai = u
		}
	}
	if ai == nil || ai.AIModel != "anthropic/claude-opus-5.5" {
		t.Fatalf("AI user: %+v", ai)
	}
	if res := e.post(admin, fmt.Sprintf("/admin/users/%d/link", ai.ID), nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("invite link for an AI user: %d", res.StatusCode)
	}

	e.store.Suggest(ctx, "de", "hello", "Hallo {name}!", ai.ID)
	e.store.Suggest(ctx, "de", "bye", "Tschüss {oops}", ai.ID)
	if _, body := e.get(tr, "/t/de?filter=all"); !strings.Contains(body, "1 suggestion</span>") {
		t.Error("list shows no suggestion tag")
	}
	_, body := e.get(tr, "/t/de/hello")
	if !strings.Contains(body, "Hallo {name}!") || !strings.Contains(body, ">Use</button>") || strings.Contains(body, "Use &amp; approve") {
		t.Error("editor suggestions for translators")
	}

	id := entrySuggestion(t, e, "hello")
	if res := e.post(tr, fmt.Sprintf("/t/de/hello/suggestions/%d/use", id), url.Values{"base": {"0"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("use: %d", res.StatusCode)
	}
	if en, _ := e.store.Entry(ctx, "de", "hello"); en.Text != "Hallo {name}!" || en.UserID != ai.ID || en.Origin != "machine" {
		t.Errorf("used: %+v", en.Row)
	}
	// Suggestions with errors can't be used.
	e.post(tr, fmt.Sprintf("/t/de/bye/suggestions/%d/use", entrySuggestion(t, e, "bye")), url.Values{"base": {"0"}})
	if en, _ := e.store.Entry(ctx, "de", "bye"); en.Has {
		t.Errorf("broken suggestion used: %+v", en.Row)
	}
	if res := e.post(tr, fmt.Sprintf("/t/de/bye/suggestions/%d/dismiss", entrySuggestion(t, e, "bye")), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("translator dismiss: %d", res.StatusCode)
	}
}

func entrySuggestion(t *testing.T, e *env, key string) int64 {
	t.Helper()
	en, err := e.store.Entry(context.Background(), "de", key)
	if err != nil || len(en.Suggestions) == 0 {
		t.Fatalf("no suggestion for %s: %v", key, err)
	}
	return en.Suggestions[0].ID
}

// stoppingAgent ends every session at once, as a model with nothing to do.
type stoppingAgent struct{}

func (stoppingAgent) Run(context.Context, ai.Session) (string, error) { return "", nil }

func TestAIUserModels(t *testing.T) {
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data": [{"id": "anthropic/claude-opus-5.5", "name": "Anthropic: Claude Opus 5.5",
			"architecture": {"output_modalities": ["text"]}, "pricing": {"prompt": "0.000004", "completion": "0.00002"},
			"supported_parameters": ["tools"]}]}`)
	}))
	defer models.Close()
	e := setupWith(t, nil, func(s *Server) {
		s.Models = ai.NewCatalog()
		s.Models.URL = models.URL
	})
	admin := e.user("admin@example.com", "admin")
	if _, body := e.get(admin, "/admin/users"); !strings.Contains(body, `<option value="anthropic/claude-opus-5.5">Anthropic: Claude Opus 5.5 · $4 / $20 per M tokens</option>`) {
		t.Error("model list missing")
	}
	if res := e.post(admin, "/admin/users/ai", url.Values{"name": {"Typo"}, "model": {"anthropic/claude-opus-55"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown model: %d", res.StatusCode)
	}
	if res := e.post(admin, "/admin/users/ai", url.Values{"name": {"Opus"}, "model": {"anthropic/claude-opus-5.5"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("listed model: %d", res.StatusCode)
	}
	// A model that is no longer listed stays when saving other changes.
	old, _ := e.store.CreateAIUser(context.Background(), "Old", "openai/gpt-4")
	path := fmt.Sprintf("/admin/users/%d", old.ID)
	if res := e.post(admin, path, url.Values{"name": {"Older"}, "ai_model": {"openai/gpt-4"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("unlisted, unchanged: %d", res.StatusCode)
	}
	if res := e.post(admin, path, url.Values{"name": {"Older"}, "ai_model": {"openai/gpt-4.1"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("changed to unlisted: %d", res.StatusCode)
	}
}

func TestAIPageOff(t *testing.T) {
	e := setup(t)
	admin := e.user("admin@example.com", "admin")
	tr := e.user("tr@example.com", "translator")
	if res, _ := e.get(tr, "/admin/ai"); res.StatusCode != http.StatusForbidden {
		t.Errorf("translator: %d", res.StatusCode)
	}
	if _, body := e.get(admin, "/admin/ai"); !strings.Contains(body, "There are no AI users yet") || !strings.Contains(body, "OPENROUTER_API_KEY") {
		t.Error("no hints without AI users and key")
	}
	ai, _ := e.store.CreateAIUser(context.Background(), "Claude Opus", "anthropic/claude-opus-5.5")
	if _, body := e.get(admin, "/admin/ai"); !strings.Contains(body, `<button class="primary" disabled>Translate</button>`) {
		t.Error("button enabled without key")
	}
	res := e.post(admin, "/admin/ai", url.Values{"lang": {"de"}, "user": {fmt.Sprint(ai.ID)}, "input": {"missing"}, "max_cost": {"5"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("queued without key: %d", res.StatusCode)
	}
}

func TestAIPage(t *testing.T) {
	e := setupWith(t, nil, func(s *Server) { s.AI = ai.NewRunner(s.store, stoppingAgent{}) })
	admin := e.user("admin@example.com", "admin")
	ctx := context.Background()
	opus, _ := e.store.CreateAIUser(ctx, "Claude Opus", "anthropic/claude-opus-5.5")
	e.store.Save(ctx, "de", "bye", "Tschüss", 1, -1, store.KeepApproval) // by admin
	_, body := e.get(admin, "/admin/ai")
	for _, want := range []string{"German (de) · 2 missing", "Claude Opus · anthropic/claude-opus-5.5", `value="all" checked`, `name="suggest" value="1" checked`, `name="max_cost"`, `value="5"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form misses %q", want)
		}
	}

	id := fmt.Sprint(opus.ID)
	for i, c := range []struct {
		v    url.Values
		want string
	}{
		{url.Values{"input": {"missing"}}, "queued by admin: 2 strings into German, as translations"},
		{url.Values{"input": {"all"}}, "3 strings into German: 2 missing as translations, 1 already translated as suggestions"},
		{url.Values{"input": {"all"}, "suggest": {"1"}}, "3 strings into German, all as suggestions"},
	} {
		c.v.Set("lang", "de")
		c.v.Set("user", id)
		c.v.Set("max_cost", "2.50")
		res := e.post(admin, "/admin/ai", c.v)
		run := fmt.Sprintf("/admin/ai/runs/%d", i+1)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != run {
			t.Fatalf("%v: %d %s", c.v, res.StatusCode, res.Header.Get("Location"))
		}
		if _, body := e.get(admin, run); !strings.Contains(body, c.want) || !strings.Contains(body, "data-reload") || !strings.Contains(body, "of $2.50") {
			t.Errorf("run %d misses %q", i+1, c.want)
		}
	}
	if r, _ := e.store.AIRun(ctx, 1); r.Status != store.AIQueued || r.MaxCost != 2.5 || r.Model != "anthropic/claude-opus-5.5" {
		t.Errorf("run 1: %+v", r)
	}
	for _, v := range []url.Values{
		{"lang": {"de"}, "user": {"1"}, "input": {"missing"}, "max_cost": {"5"}}, // a person
		{"lang": {"xx"}, "user": {id}, "input": {"missing"}, "max_cost": {"5"}},
		{"lang": {"de"}, "user": {id}, "input": {"missing"}, "max_cost": {"0"}},
		{"lang": {"de"}, "user": {id}, "input": {"missing"}, "max_cost": {"lots"}},
	} {
		if res := e.post(admin, "/admin/ai", v); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", v, res.StatusCode)
		}
	}

	if res := e.post(admin, "/admin/ai/runs/1/cancel", nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel: %d", res.StatusCode)
	}
	if _, body := e.get(admin, "/admin/ai/runs/1"); !strings.Contains(body, "cancelled before it started") || !strings.Contains(body, "Start again") || strings.Contains(body, "data-reload") {
		t.Error("cancelled run page")
	}
	e.post(admin, "/admin/ai/runs/1/restart", nil)
	if r, _ := e.store.AIRun(ctx, 1); r.Status != store.AIQueued {
		t.Errorf("restarted: %s", r.Status)
	}
	if _, body := e.get(admin, "/admin/ai"); !strings.Contains(body, `href="/admin/ai/runs/3">#3</a>`) || !strings.Contains(body, "$0.00 <span class=\"muted\">/ $2.50") {
		t.Error("runs table")
	}
	if res, _ := e.get(admin, "/admin/ai/runs/99"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run: %d", res.StatusCode)
	}
}

func TestAIPromptPage(t *testing.T) {
	e := setup(t)
	admin := e.user("admin@example.com", "admin")
	tr := e.user("tr@example.com", "translator")
	if res, _ := e.get(tr, "/admin/ai/prompt"); res.StatusCode != http.StatusForbidden {
		t.Errorf("translator: %d", res.StatusCode)
	}
	_, body := e.get(admin, "/admin/ai/prompt")
	for _, want := range []string{"this is the built-in prompt", "into {{language}} ({{code}})", "<code>{{job}}</code>", `aria-current="page">Prompt</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page misses %q", want)
		}
	}
	if strings.Contains(body, "Reset to built-in") {
		t.Error("reset offered for the built-in prompt")
	}
	setting := func() string {
		set, _ := e.store.Settings(context.Background())
		return set["ai_prompt"]
	}

	if res := e.post(admin, "/admin/ai/prompt", url.Values{"prompt": {"Into {{language}}.\r\nBe brief."}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", res.StatusCode)
	}
	if got := setting(); got != "Into {{language}}.\nBe brief.\n" {
		t.Errorf("saved %q", got)
	}
	if _, body := e.get(admin, "/admin/ai/prompt"); !strings.Contains(body, "Be brief.") || !strings.Contains(body, "Reset to built-in") {
		t.Error("saved prompt not shown")
	}
	if res := e.post(admin, "/admin/ai/prompt", url.Values{"prompt": {"Into {{lang}}."}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown variable: %d", res.StatusCode)
	}
	// The built-in prompt, saved unchanged or by reset, is stored as empty.
	e.post(admin, "/admin/ai/prompt", url.Values{"prompt": {ai.DefaultPrompt}})
	if got := setting(); got != "" {
		t.Errorf("default saved as %q", got)
	}
	e.post(admin, "/admin/ai/prompt", url.Values{"prompt": {"Mine {{job}}"}})
	e.post(admin, "/admin/ai/prompt", url.Values{"prompt": {"Mine {{job}}"}, "action": {"reset"}})
	if got := setting(); got != "" {
		t.Errorf("after reset %q", got)
	}
}
