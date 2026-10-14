package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jag2011/strings/internal/arb"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func parse(t *testing.T, src string) *arb.File {
	t.Helper()
	f, err := arb.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func entry(t *testing.T, s *Store, lang, key string) *Entry {
	t.Helper()
	e, err := s.Entry(context.Background(), lang, key)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSourceAndOutdated(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, err := s.ImportSource(ctx, parse(t, `{"a": "Apple", "b": "Banana {n}", "c": "Cherry"}`), nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.ImportTranslations(ctx, "de", parse(t, `{"a": "Apfel", "b": "Banane", "x": "?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || len(res.Unknown) != 1 || len(res.Warned) != 1 {
		t.Fatalf("import: %+v", res)
	}

	// Change a, remove c.
	src, err := s.ImportSource(ctx, parse(t, `{"b": "Banana {n}", "a": "Apples"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if src != (SourceResult{Changed: 1, Removed: 1, Unchanged: 1, Outdated: 1, Revision: 2}) {
		t.Fatalf("source: %+v", src)
	}
	if !entry(t, s, "de", "a").Outdated {
		t.Error("a should be outdated")
	}
	stats, _ := s.Stats(ctx)
	if len(stats) != 1 || stats[0].Total != 2 || stats[0].Done != 1 || stats[0].Outdated != 1 || stats[0].Missing != 0 {
		t.Errorf("stats: %+v", stats)
	}

	// Export follows the new source order and leaves out removed keys.
	f, _ := s.Export(ctx, "de")
	if got := string(arb.Write(f)); got != "{\n  \"b\": \"Banane\",\n  \"a\": \"Apfel\"\n}" {
		t.Errorf("export: %q", got)
	}

	// Saving the unchanged text marks it up to date.
	e := entry(t, s, "de", "a")
	saved, err := s.Save(ctx, "de", "a", "Apfel", 0, e.HistoryID)
	if err != nil || !saved {
		t.Fatal(saved, err)
	}
	if entry(t, s, "de", "a").Outdated {
		t.Error("a should be up to date")
	}
	// Saving the same text again does nothing.
	if saved, _ := s.Save(ctx, "de", "a", "Apfel", 0, -1); saved {
		t.Error("expected no-op")
	}
}

func TestSaveConflictAndRevert(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.ImportSource(ctx, parse(t, `{"a": "Apple"}`), nil)
	s.AddLanguage(ctx, "de")
	u1, _ := s.CreateUser(ctx, "one@example.com", "One", "translator")
	u2, _ := s.CreateUser(ctx, "two@example.com", "Two", "translator")

	if _, err := s.Save(ctx, "de", "a", "Apfel", u1.ID, 0); err != nil {
		t.Fatal(err)
	}
	first := entry(t, s, "de", "a").HistoryID
	// u2 opened the editor before u1 saved.
	if _, err := s.Save(ctx, "de", "a", "Äpfel", u2.ID, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if _, err := s.Save(ctx, "de", "a", "Äpfel", u2.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Revert(ctx, "de", "a", first, u2.ID); err != nil {
		t.Fatal(err)
	}
	e := entry(t, s, "de", "a")
	if e.Text != "Apfel" || len(e.History) != 3 || !e.History[0].Current || e.UserName != "Two" {
		t.Fatalf("after revert: %+v", e)
	}

	// Imports never overwrite edits made here.
	res, _ := s.ImportTranslations(ctx, "de", parse(t, `{"a": "Apfel!"}`))
	if len(res.Skipped) != 1 || entry(t, s, "de", "a").Text != "Apfel" {
		t.Fatalf("import overwrote an edit: %+v", res)
	}
	c, _ := s.Contributors(ctx)
	if len(c["de"]) != 1 || c["de"][0].Name != "Two" {
		t.Fatalf("contributors: %+v", c)
	}
}

func TestListAndNext(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.ImportSource(ctx, parse(t, `{"a": "1", "b": "2", "c": "3 100%", "d": "4"}`), nil)
	s.ImportTranslations(ctx, "de", parse(t, `{"b": "zwei"}`))

	rows, n, _ := s.List(ctx, "de", FilterTodo, "", 0, 10)
	if n != 3 || len(rows) != 3 || rows[0].Key != "a" {
		t.Fatalf("todo: %d %+v", n, rows)
	}
	if _, n, _ := s.List(ctx, "de", FilterAll, "%", 0, 10); n != 1 {
		t.Errorf("search for %% should match only c literally, got %d", n)
	}
	if _, n, _ := s.List(ctx, "de", FilterDone, "zwei", 0, 10); n != 1 {
		t.Errorf("search in translations: %d", n)
	}
	next, _ := s.Next(ctx, "de", FilterTodo, "", "c", 2)
	if next != "d" {
		t.Errorf("next after c: %q", next)
	}
	next, _ = s.Next(ctx, "de", FilterTodo, "", "d", 3)
	if next != "a" {
		t.Errorf("next wraps: %q", next)
	}
}

func TestDrafts(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.ImportSource(ctx, parse(t, `{"a": "Apple", "b": "b placeholder", "@b": {"x-draft": true}, "c": "Cherry", "@c": {"x-draft": false}}`), nil)
	s.AddLanguage(ctx, "de")

	if _, n, _ := s.List(ctx, "de", FilterAll, "", 0, 10); n != 2 {
		t.Errorf("list should hide the draft, got %d", n)
	}
	if next, _ := s.Next(ctx, "de", FilterTodo, "", "a", 0); next != "c" {
		t.Errorf("next should skip the draft: %q", next)
	}
	if stats, _ := s.Stats(ctx); stats[0].Total != 2 || stats[0].Missing != 2 {
		t.Errorf("stats: %+v", stats)
	}
	if _, err := s.Entry(ctx, "de", "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("entry of draft: %v", err)
	}
	if _, err := s.Save(ctx, "de", "b", "x", 0, -1); !errors.Is(err, ErrNotFound) {
		t.Errorf("save of draft: %v", err)
	}

	// The source export keeps the draft and its metadata.
	f, _ := s.Export(ctx, "en")
	if len(f.Messages) != 3 || !arb.Draft(f.Messages[1].Meta) {
		t.Errorf("source export: %+v", f.Messages)
	}

	// Dropping the tag makes it translatable.
	s.ImportSource(ctx, parse(t, `{"a": "Apple", "b": "Banana", "c": "Cherry"}`), nil)
	if _, n, _ := s.List(ctx, "de", FilterAll, "", 0, 10); n != 3 {
		t.Errorf("list after finishing the draft: %d", n)
	}
}

func TestSourceDecisions(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.ImportSource(ctx, parse(t, `{"a": "Teh cat", "b": "Dog", "c": "Bird", "d": "Fish"}`), nil)
	s.ImportTranslations(ctx, "de", parse(t, `{"a": "Die Katze", "b": "Hund", "c": "Vogel"}`))

	f := parse(t, `{"a": "The cat", "b": "Wolf", "c": "Eagle", "d": "Fish", "e": "New"}`)
	d, err := s.PreviewSource(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != 1 || !d.Modified || len(d.Added) != 1 || len(d.Changed) != 3 || d.Unchanged != 1 || d.Changed[0].Translations != 1 {
		t.Fatalf("preview: %+v", d)
	}
	if e := entry(t, s, "de", "a"); e.Source != "Teh cat" {
		t.Fatal("preview must not change anything")
	}

	res, err := s.ImportSource(ctx, f, map[string]string{"a": KeepTranslations, "c": RemoveTranslations})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Outdated != 1 || res.Cleared != 1 || res.Revision != 2 {
		t.Errorf("result: %+v", res)
	}
	if e := entry(t, s, "de", "a"); e.Outdated || e.Text != "Die Katze" {
		t.Errorf("a kept: %+v", e.Row)
	}
	if e := entry(t, s, "de", "b"); !e.Outdated {
		t.Errorf("b should be outdated")
	}
	if e := entry(t, s, "de", "c"); e.Has || len(e.History) != 1 {
		t.Errorf("c removed, history kept: %+v", e)
	}

	// The same file again changes nothing and keeps the revision.
	if d, _ := s.PreviewSource(ctx, f); d.Modified || d.Revision != 2 {
		t.Errorf("reimport: %+v", d)
	}
	// A metadata change alone is a new revision.
	res, _ = s.ImportSource(ctx, parse(t, `{"a": "The cat", "@a": {"description": "Pet"}, "b": "Wolf", "c": "Eagle", "d": "Fish", "e": "New"}`), nil)
	if res.Revision != 3 || res.Changed != 0 {
		t.Errorf("metadata change: %+v", res)
	}
}

func TestUsersAndTokens(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, err := s.CreateUser(ctx, " Admin@Example.com ", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "admin@example.com", "Again", "admin"); err == nil {
		t.Error("duplicate email accepted")
	}
	token, _ := s.CreateUserToken(ctx, u.ID)
	if _, err := s.UseUserToken(ctx, token, "short"); err == nil {
		t.Error("short password accepted")
	}
	if _, err := s.UseUserToken(ctx, token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseUserToken(ctx, token, "correct horse battery"); !errors.Is(err, ErrNotFound) {
		t.Error("token reusable")
	}
	if _, err := s.Login(ctx, "ADMIN@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "admin@example.com", "wrong"); !errors.Is(err, ErrNotFound) {
		t.Error("wrong password accepted")
	}

	sess, _ := s.CreateSession(ctx, u.ID)
	if got, err := s.SessionUser(ctx, sess); err != nil || got.ID != u.ID {
		t.Fatal(err)
	}
	// The last admin can't be disabled or demoted.
	if err := s.UpdateUser(ctx, u.ID, "Admin", "translator", false); err == nil {
		t.Error("demoted the last admin")
	}
	tr, _ := s.CreateUser(ctx, "t@example.com", "T", "translator")
	ts, _ := s.CreateSession(ctx, tr.ID)
	if err := s.UpdateUser(ctx, tr.ID, "T", "translator", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, ts); !errors.Is(err, ErrNotFound) {
		t.Error("disabled user keeps session")
	}

	api, _ := s.CreateAPIToken(ctx, "ci")
	if !s.CheckAPIToken(ctx, api) || s.CheckAPIToken(ctx, api+"x") || s.CheckAPIToken(ctx, "") {
		t.Error("api token check")
	}
}
