package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/store"
)

func zipOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func unzip(t *testing.T, data string) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// twoFiles moves the first file into lib/l10n and adds a store listing
// with a zip upload, as when deploying several files.
func (e *env) twoFiles(admin *http.Client) {
	e.t.Helper()
	if res := e.post(admin, "/admin/files/1", url.Values{"path": {"lib/l10n/intl_{locale}.arb"}, "name": {"intl"}}); res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("move the first file: %d", res.StatusCode)
	}
	z := zipOf(e.t, map[string]string{
		"fastlane/listing/store_en.arb":      `{"title": "Quest – the game", "hello": "Hello from the store"}`,
		"fastlane/listing/store_de.arb":      `{"title": "Quest – das Spiel"}`,
		"lib/l10n/intl_de.arb":               `{"bye": "Tschüss"}`,
		"__MACOSX/lib/l10n/._intl_de.arb":    "junk",
		"fastlane/listing/notes.txt":         "not an ARB file",
		"fastlane/listing/.hidden_store.arb": "{}",
	})
	res := e.upload(admin, map[string]string{"strings.zip": z})
	review := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(review, "/admin/import/") {
		e.t.Fatalf("upload: %d %s", res.StatusCode, review)
	}
	_, body := e.get(admin, review)
	for _, want := range []string{"new file", "<strong>store</strong> at <code>fastlane/listing/store_{locale}.arb</code>",
		`name="state" value="new:fastlane/listing/store_{locale}.arb"`, "fastlane/listing/store_de.arb, lib/l10n/intl_de.arb"} {
		if !strings.Contains(body, want) {
			e.t.Errorf("review misses %q", want)
		}
	}
	if res := e.post(admin, review, url.Values{"state": {"new:fastlane/listing/store_{locale}.arb"}}); res.StatusCode != http.StatusOK {
		e.t.Fatalf("confirm: %d", res.StatusCode)
	}
}

func TestTwoFiles(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	admin := e.user("admin@example.com", "admin")
	tr := e.user("tr@example.com", "translator")

	// With one file, nothing is labeled.
	if _, body := e.get(tr, "/t/de"); strings.Contains(body, "file-label") || strings.Contains(body, `data-tabs="files"`) {
		t.Error("one file shows labels or chips")
	}

	e.twoFiles(admin)
	files, _ := e.store.Files(ctx)
	if len(files) != 2 || files[1].Name != "store" || files[0].Template != "lib/l10n/intl_{locale}.arb" {
		t.Fatalf("files: %+v", files)
	}
	if en, err := e.store.Entry(ctx, files[1].ID, "de", "title"); err != nil || en.Text != "Quest – das Spiel" {
		t.Errorf("store translation: %+v %v", en, err)
	}
	if en, _ := e.store.Entry(ctx, 1, "de", "bye"); en.Text != "Tschüss" {
		t.Errorf("intl translation: %+v", en.Row)
	}

	// The list shows all files with labels, and chips limit it to one.
	_, body := e.get(tr, "/t/de?filter=all")
	for _, want := range []string{`<span class="file-label">store</span>`, `href="/t/de/store/hello?filter=all"`, `href="/t/de/intl/hello?filter=all"`,
		`href="/t/de?file=store&amp;filter=all"`, `aria-current="page">All files</a>`, `id="store/title"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list misses %s", want)
		}
	}
	_, body = e.get(tr, "/t/de?file=store&filter=all")
	if strings.Contains(body, "/t/de/intl/") || strings.Contains(body, "file-label") || !strings.Contains(body, `name="file" value="store"`) {
		t.Error("list limited to store")
	}
	if !strings.Contains(body, `aria-current="page">store</a>`) || !strings.Contains(body, `href="/t/de?file=store&amp;filter=todo" >todo <span class="badge">1</span>`) {
		t.Error("store chips and counts")
	}

	// The same key in both files: two strings.
	if res := e.post(tr, "/t/de/store/hello", url.Values{"text": {"Hallo aus dem Store"}, "base": {"0"}, "action": {"stay"}}); res.StatusCode != http.StatusSeeOther ||
		res.Header.Get("Location") != "/t/de/store/hello" {
		t.Fatalf("save store hello: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if en, _ := e.store.Entry(ctx, 1, "de", "hello"); en.Has {
		t.Errorf("intl hello changed: %+v", en.Row)
	}
	_, body = e.get(tr, "/t/de/store/hello")
	for _, want := range []string{`<span class="file-label">store</span> <code>fastlane/listing/store_de.arb</code>`, "Hello from the store",
		`<li class="keys-file">intl</li>`, `<li class="keys-file">store</li>`} {
		if !strings.Contains(body, want) {
			t.Errorf("editor misses %s", want)
		}
	}

	// Save & next goes on into the next file, or stays in the chosen one.
	res := e.post(tr, "/t/de/intl/items", url.Values{"text": {"{n, plural, one{# Ding} other{# Dinge}}"}, "base": {"0"}})
	if loc := res.Header.Get("Location"); loc != "/t/de/intl/hello" {
		t.Errorf("next wraps in all files: %s", loc)
	}
	res = e.post(tr, "/t/de/intl/hello?file=intl", url.Values{"text": {"Hallo {name}!"}, "base": {"0"}, "file": {"intl"}})
	if loc := res.Header.Get("Location"); loc != "/t/de?file=intl" {
		t.Errorf("nothing left in intl: %s", loc)
	}

	// Old links find the first file with the key.
	noFollow := &http.Client{Jar: tr.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if res, _ := e.get(noFollow, "/t/de/title?filter=all"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/t/de/store/title?filter=all" {
		t.Errorf("old link: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := e.get(noFollow, "/t/de/hello"); res.Header.Get("Location") != "/t/de/intl/hello" {
		t.Errorf("old link to a key in both: %s", res.Header.Get("Location"))
	}
	if res, _ := e.get(tr, "/t/de/nope"); res.StatusCode != http.StatusNotFound {
		t.Errorf("old link to nothing: %d", res.StatusCode)
	}
	if res, _ := e.get(tr, "/t/de/nofile/hello"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown file: %d", res.StatusCode)
	}

	// Home: totals, and chips per file.
	_, body = e.get(tr, "/")
	if !strings.Contains(body, "<strong>2</strong> files") || !strings.Contains(body, `href="/?file=store"`) {
		t.Error("home misses files")
	}
	if _, body := e.get(tr, "/?file=store"); !strings.Contains(body, `href="/t/de?file=store"`) || !strings.Contains(body, "<strong>2</strong> strings in English <span class=\"muted\">in store</span>") {
		t.Error("home limited to store")
	}

	// Activity labels lines with the file.
	if _, body := e.get(tr, "/activity"); !strings.Contains(body, `<span class="file-label">store</span><a href="/t/de/store/hello">hello</a>`) {
		t.Error("activity misses the file label")
	}

	// Exports at their paths.
	res, body = e.get(admin, "/export.zip")
	got := unzip(t, body)
	if res.StatusCode != 200 || got["fastlane/listing/store_de.arb"] != "{\n  \"title\": \"Quest – das Spiel\",\n  \"hello\": \"Hallo aus dem Store\"\n}" ||
		got["lib/l10n/intl_de.arb"] == "" || len(got) != 2 {
		t.Errorf("export zip: %d %v", res.StatusCode, got)
	}
	for p, want := range map[string]int{"/export/fastlane/listing/store_de.arb": 200, "/export/store_de.arb": 200, "/export/fastlane/listing/store_en.arb": 200,
		"/export/lib/store_de.arb": 404, "/export/fastlane/listing/store_fr.arb": 404} {
		if res, _ := e.get(admin, p); res.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", p, res.StatusCode, want)
		}
	}
	if _, body := e.get(admin, "/admin"); !strings.Contains(body, `href="/export/fastlane/listing/store_de.arb"`) || !strings.Contains(body, `value="fastlane/listing/store_{locale}.arb"`) {
		t.Error("settings miss the files")
	}
}

func TestFileSettings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	admin := e.user("admin@example.com", "admin")
	e.twoFiles(admin)

	// Overlapping or broken paths are refused with the reason.
	res := e.post(admin, "/admin/files/2", url.Values{"path": {"lib/l10n/intl_{locale}.arb"}, "name": {"store"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("taken path: %d", res.StatusCode)
	}
	if res := e.post(admin, "/admin/files/2", url.Values{"path": {"store.arb"}, "name": {"store"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no {locale}: %d", res.StatusCode)
	}
	if res := e.post(admin, "/admin/files/2", url.Values{"path": {"listing/store_{locale}.arb"}, "name": {"listing"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("rename: %d", res.StatusCode)
	}
	if f, _ := e.store.File(ctx, 2); f.Name != "listing" || f.Template != "listing/store_{locale}.arb" {
		t.Errorf("renamed: %+v", f)
	}
	// Order.
	e.post(admin, "/admin/files/2/up", nil)
	if fs, _ := e.store.Files(ctx); fs[0].ID != 2 {
		t.Errorf("moved up: %+v", fs)
	}
	// Files with translations stay.
	if res := e.post(admin, "/admin/files/2/delete", nil); res.StatusCode != http.StatusConflict {
		t.Errorf("delete a used file: %d", res.StatusCode)
	}
	// A mistaken upload can go: a source alone.
	e.upload(admin, map[string]string{"oops_en.arb": `{"x": "X"}`})
	review := e.upload(admin, map[string]string{"oops_en.arb": `{"x": "X"}`}).Header.Get("Location")
	e.post(admin, review, url.Values{"state": {"new:oops_{locale}.arb"}})
	oops, err := e.store.FileByName(ctx, "oops")
	if err != nil {
		t.Fatal(err)
	}
	if _, body := e.get(admin, "/admin"); !strings.Contains(body, fmt.Sprintf(`action="/admin/files/%d/delete"`, oops.ID)) || strings.Contains(body, `action="/admin/files/1/delete"`) {
		t.Error("delete buttons")
	}
	if res := e.post(admin, fmt.Sprintf("/admin/files/%d/delete", oops.ID), nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("delete: %d", res.StatusCode)
	}
	if fs, _ := e.store.Files(ctx); len(fs) != 2 {
		t.Errorf("files after deleting: %+v", fs)
	}
}

func TestUploadAmbiguous(t *testing.T) {
	e := setup(t)
	admin := e.user("admin@example.com", "admin")
	e.twoFiles(admin)
	e.post(admin, "/admin/files/1", url.Values{"path": {"lib/l10n/store_{locale}.arb"}, "name": {"intl"}})
	// Two files are called store_…, so a single file needs a zip.
	r := e.upload(admin, map[string]string{"store_de.arb": `{"title": "Quest"}`})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	// The result is rendered on the settings page; check the import did nothing.
	if en, _ := e.store.Entry(context.Background(), 2, "de", "title"); en.Text != "Quest – das Spiel" {
		t.Errorf("ambiguous upload imported: %+v", en.Row)
	}
}

func TestFilesAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	token, _ := e.store.CreateAPIToken(ctx, "ci")
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
	decode := func(body string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return m
	}

	// A new file: previewed, then created by pushing its source.
	src := `{"title": "Quest – the game", "short": "Play"}`
	code, body := do("POST", "/api/v1/preview/fastlane/listing/store_en.arb", src)
	if m := decode(body); code != 200 || m["new_file"] != true || m["file"] != "store" || m["path"] != "fastlane/listing/store_{locale}.arb" || len(m["added"].([]any)) != 2 {
		t.Errorf("preview new: %d %s", code, body)
	}
	if fs, _ := e.store.Files(ctx); len(fs) != 1 {
		t.Fatal("preview created a file")
	}
	code, body = do("PUT", "/api/v1/files/fastlane/listing/store_en.arb", src)
	if m := decode(body); code != 200 || m["new_file"] != true || m["file"] != "store" || m["added"] != 2.0 || m["revision"] != 1.0 {
		t.Errorf("put new: %d %s", code, body)
	}
	// A translation of it.
	code, body = do("PUT", "/api/v1/files/fastlane/listing/store_de.arb", `{"title": "Quest – das Spiel", "hello": "?"}`)
	if m := decode(body); code != 200 || m["file"] != "store" || m["language"] != "de" || m["added"] != 1.0 || len(m["unknown"].([]any)) != 1 {
		t.Errorf("put translation: %d %s", code, body)
	}
	// Revisions are per file.
	code, body = do("PUT", "/api/v1/files/fastlane/listing/store_en.arb?revision=1&keep=title", `{"title": "Quest – the game!", "short": "Play"}`)
	if m := decode(body); code != 200 || m["revision"] != 2.0 || m["kept"] != 1.0 || m["new_file"] != false {
		t.Errorf("put change: %d %s", code, body)
	}
	if code, body := do("PUT", "/api/v1/files/intl_en.arb?revision=2", `{"hello": "Hello {name}!"}`); code != 409 {
		t.Errorf("intl is at revision 1: %d %s", code, body)
	}
	if code, body := do("POST", "/api/v1/preview/intl_en.arb", `{"hello": "Hello {name}!", "bye": "Bye"}`); !strings.Contains(body, `"removed": [
    "items"
  ]`) || !strings.Contains(body, `"revision": 1`) {
		t.Errorf("intl untouched: %d %s", code, body)
	}

	// Errors.
	for path, want := range map[string]int{
		"/api/v1/files/other/app_de.arb":           400, // no source yet
		"/api/v1/files/fastlane/listing/store.arb": 400,
		"/api/v1/files/app/intl_en.arb":            400, // the name intl is taken
	} {
		if code, body := do("PUT", path, `{"a": "b"}`); code != want {
			t.Errorf("PUT %s: %d %s", path, code, body)
		}
	}
	if code, body := do("PUT", "/api/v1/files/fastlane/listing/store_fr.arb", `{"@@locale": "de", "a": "b"}`); code != 400 || !strings.Contains(body, "@@locale") {
		t.Errorf("wrong @@locale: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/v1/preview/fastlane/listing/store_de.arb", `{}`); code != 400 {
		t.Errorf("preview of a translation: %d", code)
	}

	// Reading.
	code, body = do("GET", "/api/v1/files", "")
	var list []map[string]any
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list) != 2 || list[1]["name"] != "store" || list[1]["path"] != "fastlane/listing/store_{locale}.arb" || list[1]["strings"] != 2.0 || list[1]["revision"] != 2.0 {
		t.Errorf("files: %d %s", code, body)
	}
	if code, body := do("GET", "/api/v1/files/fastlane/listing/store_de.arb", ""); code != 200 || body != "{\n  \"title\": \"Quest – das Spiel\"\n}" {
		t.Errorf("get file: %d %q", code, body)
	}
	if code, _ := do("GET", "/api/v1/files/fastlane/listing/store_xx.arb", ""); code != 404 {
		t.Errorf("unknown language: %d", code)
	}
	code, body = do("GET", "/api/v1/files.zip", "")
	if got := unzip(t, body); code != 200 || len(got) != 2 || got["fastlane/listing/store_de.arb"] == "" || got["intl_de.arb"] != "{}" {
		t.Errorf("zip: %d %v", code, got)
	}
	code, body = do("GET", "/api/v1/files.zip?file=store", "")
	if got := unzip(t, body); code != 200 || len(got) != 1 {
		t.Errorf("zip of store: %d %v", code, got)
	}
	if code, _ := do("GET", "/api/v1/files.zip?file=nope", ""); code != 404 {
		t.Errorf("zip of nothing: %d", code)
	}
	code, body = do("GET", "/api/v1/stats", "")
	var stats map[string]map[string]any
	json.Unmarshal([]byte(body), &stats)
	files, _ := stats["de"]["files"].(map[string]any)
	if code != 200 || stats["de"]["total"] != 5.0 || stats["de"]["done"] != 1.0 || len(files) != 2 || files["store"].(map[string]any)["done"] != 1.0 ||
		files["intl"].(map[string]any)["missing"] != 3.0 {
		t.Errorf("stats: %d %s", code, body)
	}

	// The old endpoints refer to the first file.
	code, body = do("PUT", "/api/v1/translations/de", `{"bye": "Tschüss", "title": "?"}`)
	if m := decode(body); code != 200 || m["added"] != 1.0 || len(m["unknown"].([]any)) != 1 || m["file"] != nil {
		t.Errorf("old translations: %d %s", code, body)
	}
	if code, body := do("GET", "/api/v1/arb/store_de.arb", ""); code != 200 || !strings.Contains(body, "Quest") {
		t.Errorf("old export by name: %d %s", code, body)
	}
	code, body = do("GET", "/api/v1/arb.zip", "")
	if got := unzip(t, body); code != 200 || !slices.Contains(keys(got), "store_de.arb") || !slices.Contains(keys(got), "intl_de.arb") {
		t.Errorf("old zip is flat: %v", keys(got))
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestOldSourceAPICreatesFile: without files, the old endpoint creates the
// first one, as the import did before.
func TestOldSourceAPICreatesFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.store.DeleteFile(ctx, 1); err != nil {
		t.Fatal(err)
	}
	token, _ := e.store.CreateAPIToken(ctx, "ci")
	req, _ := http.NewRequest("PUT", e.srv.URL+"/api/v1/source", strings.NewReader(`{"a": "A"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if f, err := e.store.FirstFile(ctx); res.StatusCode != 200 || err != nil || f.Template != store.DefaultTemplate || f.Name != "intl" {
		t.Errorf("first file: %d %+v %v", res.StatusCode, f, err)
	}
}

func TestAIPerFile(t *testing.T) {
	e := setupWith(t, nil, func(s *Server) { s.AI = ai.NewRunner(s.store, stoppingAgent{}) })
	ctx := context.Background()
	admin := e.user("admin@example.com", "admin")
	e.twoFiles(admin)
	opus, _ := e.store.CreateAIUser(ctx, "Claude Opus", "anthropic/claude-opus-5.5")

	_, body := e.get(admin, "/admin/ai?file=store")
	if !strings.Contains(body, `<option value="store" selected>store · fastlane/listing/store_{locale}.arb</option>`) ||
		!strings.Contains(body, `<input type="hidden" name="file" value="store">`) || !strings.Contains(body, "German (de) · 1 missing") {
		t.Errorf("jobs page for store:\n%s", body)
	}
	res := e.post(admin, "/admin/ai", url.Values{"file": {"store"}, "lang": {"de"}, "user": {fmt.Sprint(opus.ID)}, "input": {"missing"}, "max_cost": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("queue: %d", res.StatusCode)
	}
	runs, _ := e.store.AIRuns(ctx, 10)
	if len(runs) != 1 || runs[0].FileName != "store" {
		t.Fatalf("runs: %+v", runs)
	}
	if ev, _ := e.store.AIRunEvents(ctx, runs[0].ID); len(ev) == 0 || !strings.Contains(ev[0].Text, "1 string of store into German") {
		t.Errorf("plan: %+v", ev)
	}
	if _, body := e.get(admin, "/admin/ai"); !strings.Contains(body, "<td>store</td>") {
		t.Error("runs table misses the file")
	}

	// Fill per file.
	_, body = e.get(admin, fmt.Sprintf("/admin/ai/fill?user=%d&file=intl", opus.ID))
	if !strings.Contains(body, "Missing strings have no translation in intl.") || !strings.Contains(body, `<input type="hidden" name="file" value="intl">`) {
		t.Errorf("fill page for intl:\n%s", body)
	}
	res = e.post(admin, "/admin/ai/fill", url.Values{"user": {fmt.Sprint(opus.ID)}, "file": {"intl"}, "lang": {"de"}})
	if loc := res.Header.Get("Location"); loc != fmt.Sprintf("/admin/ai/fill?user=%d&file=intl", opus.ID) {
		t.Errorf("fill: %s", loc)
	}
	runs, _ = e.store.AIRuns(ctx, 10)
	if len(runs) != 2 || runs[0].FileName != "intl" || !runs[0].Suggest {
		t.Errorf("fill run: %+v", runs[0])
	}
}
