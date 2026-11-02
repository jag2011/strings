package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jag2011/strings/internal/arb"
)

// TestMigrateFiles migrates a version-10 database with data: everything
// belongs to file 1 afterwards, described by the old settings.
func TestMigrateFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := openUpTo(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO settings (key, value) VALUES ('file_prefix', 'app_'), ('source_revision', '7'), ('arb_locale', '1'),
			('arb_trailing_newline', '1'), ('name', 'Game')`,
		`INSERT INTO languages (code) VALUES ('de'), ('fr')`,
		`INSERT INTO users (id, email, name, role) VALUES (1, 'a@example.com', 'Ann', 'admin'), (2, 'b@example.com', 'Bob', 'translator')`,
		`INSERT INTO users (id, email, name, role, unlisted, ai_model) VALUES (3, 'ai@strings.invalid', 'Robo', 'translator', 1, 'x/y')`,
		`INSERT INTO source_strings (key, text, hash, meta, position, removed, draft) VALUES
			('a', 'Apple', 'h1', '{"description":"Fruit"}', 0, 0, 0), ('b', 'Banana', 'h2', NULL, 1, 0, 0),
			('c', 'Cherry', 'h3', NULL, 2, 1, 0), ('d', 'Date', 'h4', '{"x-draft":true}', 3, 0, 1)`,
		`INSERT INTO history (id, lang, key, text, source_hash, origin, user_id) VALUES
			(1, 'de', 'a', 'Apfel', 'h1', 'import', NULL), (2, 'de', 'a', 'Äpfel', 'h1', 'user', 2),
			(3, 'de', 'b', 'Banane', 'old', 'user', 2), (4, 'fr', 'c', 'Cerise', 'h3', 'machine', 3)`,
		`INSERT INTO translations (lang, key, text, source_hash, origin, user_id, history_id, approved_by, approved_at) VALUES
			('de', 'a', 'Äpfel', 'h1', 'user', 2, 2, 1, '2026-10-01 10:00:00'),
			('de', 'b', 'Banane', 'old', 'user', 2, 3, NULL, NULL),
			('fr', 'c', 'Cerise', 'h3', 'machine', 3, 4, NULL, NULL)`,
		`INSERT INTO approval_log (lang, key, approved, history_id, user_id) VALUES ('de', 'a', 1, 2, 1)`,
		`INSERT INTO suggestions (lang, key, text, source_hash, user_id) VALUES ('de', 'b', 'Bananen', 'h2', 3)`,
		`INSERT INTO ai_runs (lang, user_id, model, input, max_cost, status) VALUES ('de', 3, 'x/y', 'all', 5, 'done')`,
	} {
		if _, err := old.w.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	fs, err := s.Files(ctx)
	want := File{ID: 1, Template: "app_{locale}.arb", Name: "app", Position: 1, Revision: 7, ARBLocale: true, TrailingNewline: true}
	if err != nil || len(fs) != 1 || fs[0] != want {
		t.Fatalf("files: %+v %v", fs, err)
	}
	set, _ := s.Settings(ctx)
	for _, k := range []string{"file_prefix", "source_revision", "arb_locale", "arb_trailing_newline"} {
		if _, ok := set[k]; ok {
			t.Errorf("setting %s left", k)
		}
	}
	if set["name"] != "Game" {
		t.Errorf("settings lost: %v", set)
	}
	for table, n := range map[string]int{"source_strings": 4, "history": 4, "translations": 3, "approval_log": 1, "suggestions": 1, "ai_runs": 1} {
		var got, file1 int
		if err := s.r.QueryRow("SELECT count(*), coalesce(sum(file_id = 1), 0) FROM "+table).Scan(&got, &file1); err != nil || got != n || file1 != n {
			t.Errorf("%s: %d rows, %d in file 1, want %d (%v)", table, got, file1, n, err)
		}
	}
	e := entry(t, s, "de", "a")
	if !e.Approved || e.ApprovedBy != "Ann" || e.Text != "Äpfel" || len(e.History) != 2 || string(e.Meta) != `{"description":"Fruit"}` {
		t.Errorf("entry a: %+v", e)
	}
	if e := entry(t, s, "de", "b"); !e.Outdated || len(e.Suggestions) != 1 || e.Suggestions[0].Text != "Bananen" {
		t.Errorf("entry b: %+v", e)
	}
	if run, err := s.AIRun(ctx, 1); err != nil || run.FileID != 1 || run.FileName != "app" {
		t.Errorf("run: %+v %v", run, err)
	}
	if a, _ := s.Activity(ctx, "de", 0, 0, 10); len(a) != 3 || a[0].File != "app" || a[0].Kind != "approved" || a[2].Prev != "Apfel" {
		t.Errorf("activity: %+v", a)
	}
	// The export keeps @@locale and the trailing newline.
	f, err := s.Export(ctx, 1, "de")
	if err != nil || string(arb.Write(f)) != "{\n  \"@@locale\": \"de\",\n  \"a\": \"Äpfel\",\n  \"b\": \"Banane\"\n}\n" {
		t.Errorf("export: %q %v", arb.Write(f), err)
	}
	// Saving still works with the rebuilt tables and their foreign keys.
	if _, err := s.Save(ctx, 1, "de", "b", "Bananen", 2, -1, KeepApproval); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateEmpty(t *testing.T) {
	s, ctx := openEmpty(t), context.Background()
	if fs, err := s.Files(ctx); err != nil || len(fs) != 0 {
		t.Fatalf("a new database has no files: %+v %v", fs, err)
	}
	if _, err := s.FirstFile(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("first file: %v", err)
	}
}

// importFiles imports files and fails on any error.
func importFiles(t *testing.T, s *Store, files map[string]string) []ImportResult {
	t.Helper()
	m := map[string][]byte{}
	for k, v := range files {
		m[k] = []byte(v)
	}
	res, err := s.ImportFiles(context.Background(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Path, r.Err)
		}
	}
	return res
}

// twoFiles sets up an app file and a store listing file, both with key
// "title".
func twoFiles(t *testing.T) *Store {
	t.Helper()
	s := openEmpty(t)
	res := importFiles(t, s, map[string]string{
		"lib/l10n/intl_en.arb":          `{"title": "Quest", "start": "Start"}`,
		"lib/l10n/intl_de.arb":          `{"title": "Abenteuer", "start": "Los"}`,
		"fastlane/listing/store_en.arb": `{"title": "Quest – the game", "short": "Play now"}`,
	})
	if len(res) != 3 || !res[0].NewFile || !res[1].NewFile || res[2].Trans == nil || res[2].Trans.Added != 2 {
		t.Fatalf("import: %+v", res)
	}
	return s
}

func TestNewFilesFromPaths(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	fs, err := s.Files(ctx)
	if err != nil || len(fs) != 2 {
		t.Fatal(fs, err)
	}
	// Sources are imported in path order.
	if fs[0].Template != "fastlane/listing/store_{locale}.arb" || fs[0].Name != "store" || fs[0].Revision != 1 ||
		fs[1].Template != "lib/l10n/intl_{locale}.arb" || fs[1].Name != "intl" {
		t.Errorf("files: %+v", fs)
	}
	if fs[1].Path("pt_BR") != "lib/l10n/intl_pt_BR.arb" {
		t.Errorf("path: %s", fs[1].Path("pt_BR"))
	}
	for p, want := range map[string]string{
		"lib/l10n/intl_de.arb":       "intl de",
		"intl_zh_Hant.arb":           "intl zh_Hant", // by the file name alone
		"fastlane/listing/store.arb": "",
		"lib/intl_de.arb":            "",
		"lib/l10n/intl_xx_de.arb":    "",
	} {
		f, lang, err := s.MatchPath(ctx, p)
		if got := f.Name + " " + lang; (want == "" && err == nil) || (want != "" && got != want) {
			t.Errorf("match %s: %q %v", p, got, err)
		}
	}

	// A translation of an unknown file needs its source first.
	res, _ := s.ImportFiles(ctx, map[string][]byte{"lib/other/app_de.arb": []byte(`{"a": "b"}`)}, nil)
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "lib/other/app_en.arb first") {
		t.Errorf("unknown translation: %v", res[0].Err)
	}
	// A new file whose name is taken is refused, so a moved file doesn't
	// turn into a second one.
	res, _ = s.ImportFiles(ctx, map[string][]byte{"app/intl_en.arb": []byte(`{"a": "b"}`)}, nil)
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "name intl is taken") {
		t.Errorf("taken name: %v", res[0].Err)
	}
	// Without a prefix, a file is called app.
	importFiles(t, s, map[string]string{"res/en.arb": `{"a": "A"}`})
	if f, _, err := s.MatchPath(ctx, "res/de.arb"); err != nil || f.Name != "app" || f.Template != "res/{locale}.arb" {
		t.Errorf("app file: %+v %v", f, err)
	}
}

func TestFilesAreIndependent(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	intl, _ := s.FileByName(ctx, "intl")
	store, _ := s.FileByName(ctx, "store")

	// The same key in two files is two strings.
	if _, err := s.Save(ctx, store.ID, "de", "title", "Quest – das Spiel", 0, -1, KeepApproval); err != nil {
		t.Fatal(err)
	}
	if a, b := entryIn(t, s, intl.ID, "de", "title"), entryIn(t, s, store.ID, "de", "title"); a.Text != "Abenteuer" || b.Text != "Quest – das Spiel" ||
		len(a.History) != 1 || len(b.History) != 1 || a.File != "intl" || b.File != "store" {
		t.Errorf("entries: %+v %+v", a, b)
	}

	// Importing one source doesn't remove the keys of the other, and only
	// its own revision goes up.
	res, err := s.ImportSource(ctx, store.ID, parse(t, `{"title": "Quest – the game!", "long": "More"}`), nil)
	if err != nil || res.Removed != 1 || res.Changed != 1 || res.Added != 1 || res.Revision != 2 {
		t.Fatalf("store import: %+v %v", res, err)
	}
	if e := entryIn(t, s, intl.ID, "de", "title"); e.Outdated || e.Source != "Quest" {
		t.Errorf("intl title changed: %+v", e.Row)
	}
	if e := entryIn(t, s, store.ID, "de", "title"); !e.Outdated {
		t.Errorf("store title should be outdated")
	}
	if f, _ := s.File(ctx, intl.ID); f.Revision != 1 {
		t.Errorf("intl revision: %d", f.Revision)
	}
	if _, n, _ := s.List(ctx, intl.ID, "de", FilterAll, "", 0, 10); n != 2 {
		t.Errorf("intl strings: %d", n)
	}

	// Translations go to their file; keys of other files are unknown.
	tr, err := s.ImportTranslations(ctx, store.ID, "de", parse(t, `{"long": "Mehr", "start": "Los"}`))
	if err != nil || tr.Added != 1 || !slices.Equal(tr.Unknown, []string{"start"}) {
		t.Errorf("store translations: %+v %v", tr, err)
	}

	// Stats per file and in total.
	all, _ := s.Stats(ctx, 0)
	one, _ := s.Stats(ctx, store.ID)
	if all[0].Total != 4 || all[0].Done != 3 || all[0].Outdated != 1 || one[0].Total != 2 || one[0].Done != 1 || one[0].Outdated != 1 {
		t.Errorf("stats: %+v %+v", all, one)
	}

	// Exports per file.
	f, _ := s.Export(ctx, store.ID, "de")
	if got := string(arb.Write(f)); got != "{\n  \"title\": \"Quest – das Spiel\",\n  \"long\": \"Mehr\"\n}" {
		t.Errorf("store export: %q", got)
	}
}

func TestListAcrossFiles(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	store, _ := s.FileByName(ctx, "store")
	intl, _ := s.FileByName(ctx, "intl")

	// All files, in file order: store first, it came first.
	rows, n, err := s.List(ctx, 0, "de", FilterAll, "", 0, 10)
	var got []string
	for _, r := range rows {
		got = append(got, r.File+"/"+r.Key)
	}
	if err != nil || n != 4 || strings.Join(got, " ") != "store/title store/short intl/title intl/start" {
		t.Errorf("list: %v %d %v", got, n, err)
	}
	if counts, _ := s.FilterCounts(ctx, 0, "de", ""); counts[FilterTodo] != 2 || counts[FilterAll] != 4 {
		t.Errorf("counts: %v", counts)
	}
	if counts, _ := s.FilterCounts(ctx, store.ID, "de", ""); counts[FilterTodo] != 2 || counts[FilterAll] != 2 {
		t.Errorf("store counts: %v", counts)
	}

	// Next goes on into the next file, and stays in one when filtered.
	if next, _ := s.Next(ctx, 0, "de", FilterAll, "", Ref{FileID: store.ID, Key: "short"}); next != (Ref{intl.ID, "intl", "title"}) {
		t.Errorf("next across files: %+v", next)
	}
	if next, _ := s.Next(ctx, store.ID, "de", FilterAll, "", Ref{FileID: store.ID, Key: "short"}); next.FileID != store.ID || next.Key != "title" {
		t.Errorf("next in a file wraps: %+v", next)
	}
	if next, _ := s.Next(ctx, 0, "de", FilterTodo, "", Ref{FileID: store.ID, Key: "short"}); next.FileID != store.ID || next.Key != "title" {
		t.Errorf("next todo wraps past the translated file: %+v", next)
	}
	keys, _ := s.Keys(ctx, 0, "de", FilterTodo, "", Ref{FileID: intl.ID, Key: "start"})
	if len(keys) != 3 || keys[2].Key != "start" || keys[2].Matches || keys[0].File != "store" {
		t.Errorf("keys: %+v", keys)
	}
	if refs, _ := s.FindKey(ctx, "title"); len(refs) != 2 || refs[0].File != "store" || refs[1].File != "intl" {
		t.Errorf("find key: %+v", refs)
	}

	// Moving a file changes the order.
	if err := s.MoveFile(ctx, intl.ID, true); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := s.List(ctx, 0, "de", FilterAll, "", 0, 1); rows[0].File != "intl" {
		t.Errorf("after moving: %+v", rows[0])
	}
}

func TestFileTemplates(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	intl, _ := s.FileByName(ctx, "intl")
	store, _ := s.FileByName(ctx, "store")
	for _, c := range []struct{ path, name, err string }{
		{"lib/l10n/app_{locale}.arb", "intl", ""},
		{"lib/l10n/{locale}.arb", "intl", ""},
		{"fastlane/listing/store_{locale}.arb", "intl", "taken"},
		{"lib/l10n/intl_{locale}.arb", "store", "taken"},
		{"fastlane/listing/{locale}_store.arb", "intl", ""},
		{"fastlane/listing/{locale}.arb", "intl", ""},
		{"fastlane/{locale}/store_en.arb", "intl", ""},
		{"fastlane/listing/store_{locale}.arb.arb", "intl", ""},
		{"fastlane/listing/store_de_{locale}.arb", "intl", ""},
		{"lib/l10n/intl.arb", "intl", "{locale} once"},
		{"lib/{locale}/intl_{locale}.arb", "intl", "{locale} once"},
		{"lib/l10n/intl_{locale}.json", "intl", ".arb"},
		{"/lib/intl_{locale}.arb", "intl", "relative"},
		{"lib/../intl_{locale}.arb", "intl", ".. parts"},
		{"lib//intl_{locale}.arb", "intl", "empty"},
		{"lib/intl_{locale}.arb", "Intl", "lowercase"},
		{"lib/intl_{locale}.arb", "", "lowercase"},
	} {
		err := s.UpdateFile(ctx, intl.ID, c.path, c.name)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %s: %v, want %q", c.path, c.name, err, c.err)
		}
	}
	// Overlapping templates are refused: a path of one would fit the other.
	s.UpdateFile(ctx, intl.ID, "lib/{locale}.arb", "intl")
	if err := s.UpdateFile(ctx, store.ID, "lib/de_{locale}.arb", "store"); err != nil {
		t.Errorf("de_de isn't a locale: %v", err)
	}
	s.UpdateFile(ctx, intl.ID, "lib/{locale}/app.arb", "intl")
	if err := s.UpdateFile(ctx, store.ID, "lib/de/{locale}.arb", "store"); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("lib/de/app.arb fits both: %v", err)
	}
}

func TestLooseUploads(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	intl, _ := s.FileByName(ctx, "intl")
	// File names alone match as long as one file has the name.
	res := importFiles(t, s, map[string]string{"intl_fr.arb": `{"title": "Aventure"}`, "store_fr.arb": `{"short": "Joue"}`})
	if res[0].File.Name != "intl" || res[1].File.Name != "store" || res[0].Lang != "fr" {
		t.Errorf("loose: %+v", res)
	}
	// Two files with the same name in different folders need a zip.
	s.UpdateFile(ctx, intl.ID, "app/store_{locale}.arb", "intl")
	r, _ := s.ImportFiles(ctx, map[string][]byte{"store_fr.arb": []byte(`{"short": "Joue"}`)}, nil)
	if r[0].Err == nil || !strings.Contains(r[0].Err.Error(), "upload a zip") {
		t.Errorf("ambiguous: %v", r[0].Err)
	}
	r, _ = s.ImportFiles(ctx, map[string][]byte{"store_en.arb": []byte(`{"short": "Play"}`)}, nil)
	if r[0].Err == nil || r[0].Source != nil {
		t.Errorf("ambiguous source: %+v", r[0])
	}
	// The full path is unambiguous.
	importFiles(t, s, map[string]string{"app/store_fr.arb": `{"title": "Aventure!"}`})
}

func TestPreviewFiles(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	store, _ := s.FileByName(ctx, "store")
	ds, err := s.PreviewFiles(ctx, map[string][]byte{
		"fastlane/listing/store_en.arb": []byte(`{"title": "Quest", "short": "Play now"}`),
		"new/thing_en.arb":              []byte(`{"x": "X"}`),
		"lib/l10n/intl_fr.arb":          []byte(`{"title": "Quête"}`),
		"broken/b_en.arb":               []byte(`{`),
	})
	if err != nil || len(ds) != 2 {
		t.Fatalf("preview: %+v %v", ds, err)
	}
	if d := ds[0]; d.Path != "fastlane/listing/store_en.arb" || d.New() || d.File.ID != store.ID || len(d.Diff.Changed) != 1 || d.Diff.Revision != 1 {
		t.Errorf("store: %+v %+v", d, d.Diff)
	}
	if d := ds[1]; !d.New() || d.File.Template != "new/thing_{locale}.arb" || d.File.Name != "thing" || len(d.Diff.Added) != 1 || d.Diff.Revision != 0 {
		t.Errorf("new: %+v %+v", d, d.Diff)
	}
	if fs, _ := s.Files(ctx); len(fs) != 2 {
		t.Errorf("preview created a file")
	}

	// Decisions are per file.
	res, err := s.ImportFiles(ctx, map[string][]byte{
		"fastlane/listing/store_en.arb": []byte(`{"title": "Quest", "short": "Play now"}`),
		"lib/l10n/intl_en.arb":          []byte(`{"title": "Quest!", "start": "Start"}`),
	}, map[int64]map[string]string{store.ID: {"title": KeepTranslations}})
	if err != nil || res[0].Source.Kept != 1 || res[1].Source.Outdated != 1 {
		t.Errorf("decisions: %+v %+v %v", res[0].Source, res[1].Source, err)
	}
}

func TestDeleteFile(t *testing.T) {
	s, ctx := twoFiles(t), context.Background()
	store, _ := s.FileByName(ctx, "store")
	intl, _ := s.FileByName(ctx, "intl")
	if err := s.DeleteFile(ctx, intl.ID); !errors.Is(err, ErrFileUsed) {
		t.Errorf("intl has translations: %v", err)
	}
	fs, _ := s.FileStats(ctx)
	if fs[0].Used || fs[0].Strings != 2 || !fs[1].Used {
		t.Errorf("file stats: %+v", fs)
	}
	if err := s.DeleteFile(ctx, store.ID); err != nil {
		t.Fatal(err)
	}
	if _, n, _ := s.List(ctx, 0, "de", FilterAll, "", 0, 10); n != 2 {
		t.Errorf("strings left: %d", n)
	}
	// Its paths are free again.
	importFiles(t, s, map[string]string{"fastlane/listing/store_en.arb": `{"a": "A"}`})
}
