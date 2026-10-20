package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// aiSetup: a is missing, b is approved, c is a person's translation, d is a
// missing plural.
func aiSetup(t *testing.T) (*Store, *User, *User) {
	t.Helper()
	s, ctx := open(t), context.Background()
	if _, err := s.ImportSource(ctx, parse(t, `{"a": "Apple", "b": "Banana {n}", "c": "Cherry", "d": "{n, plural, one{# day} other{# days}}"}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportTranslations(ctx, "de", parse(t, `{"b": "Banane {n}"}`)); err != nil {
		t.Fatal(err)
	}
	admin, _ := s.CreateUser(ctx, "admin@example.com", "Admin", "admin")
	if _, err := s.Save(ctx, "de", "b", "Banane {n}", admin.ID, -1, Approve); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateUser(ctx, "p@example.com", "P", "translator")
	if _, err := s.Save(ctx, "de", "c", "Kirsche", p.ID, -1, KeepApproval); err != nil {
		t.Fatal(err)
	}
	ai, err := s.CreateAIUser(ctx, "Robo", "anthropic/claude-opus-5.5")
	if err != nil {
		t.Fatal(err)
	}
	return s, ai, p
}

func pendingKeys(t *testing.T, s *Store, aiID int64, input string) []string {
	t.Helper()
	ps, err := s.AIPending(context.Background(), "de", aiID, input)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, p := range ps {
		keys = append(keys, p.Key)
	}
	return keys
}

func TestAIPending(t *testing.T) {
	s, ai, _ := aiSetup(t)
	if got := pendingKeys(t, s, ai.ID, AIInputMissing); !slices.Equal(got, []string{"a", "d"}) {
		t.Errorf("missing: %v", got)
	}
	if got := pendingKeys(t, s, ai.ID, AIInputAll); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Errorf("all, approved b left out: %v", got)
	}
	ps, _ := s.AIPending(context.Background(), "de", ai.ID, AIInputAll)
	if c := ps[1]; !c.HasCurrent || c.Current != "Kirsche" {
		t.Errorf("c: %+v", c)
	}
	if d := ps[2]; d.HasCurrent || d.Source != "{n, plural, one{# day} other{# days}}" {
		t.Errorf("d: %+v", d)
	}
}

func TestAIFillStats(t *testing.T) {
	s, ai, p := aiSetup(t)
	ctx := context.Background()
	other, _ := s.CreateAIUser(ctx, "Other", "openai/gpt-6")
	s.Suggest(ctx, "de", "a", "Apfel", other.ID)
	s.Suggest(ctx, "de", "d", "Tage", p.ID)     // a person's suggestion doesn't count
	s.Suggest(ctx, "de", "c", "Kirsche", ai.ID) // c isn't missing
	s.AddLanguage(ctx, "fr")
	fill, err := s.AIFillStats(ctx, ai.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []AIFill{{Lang: "de", Missing: 2, NotByAI: 2, NotByAny: 1}, {Lang: "fr", Missing: 4, NotByAI: 4, NotByAny: 4}}
	if !slices.Equal(fill, want) {
		t.Errorf("before: %+v", fill)
	}
	s.Suggest(ctx, "de", "d", "{n, plural, one{# Tag} other{# Tage}}", ai.ID)
	fill, _ = s.AIFillStats(ctx, ai.ID)
	if f := fill[0]; f.NotByAI != 1 || f.NotByAny != 0 {
		t.Errorf("after: %+v", f)
	}
	if fill, _ = s.AIFillStats(ctx, other.ID); fill[0].NotByAI != 1 {
		t.Errorf("other: %+v", fill[0])
	}
}

func TestAISaveRules(t *testing.T) {
	s, ai, p := aiSetup(t)
	ctx := context.Background()
	missing := AISaveJob{Lang: "de", AIUserID: ai.ID, Input: AIInputMissing}
	res, err := s.AISave(ctx, missing, []AIText{
		{"a", "Apfel"},
		{"b", "Bananen {n}"},
		{"c", "Kirsche!"},
		{"d", "{n, plural, one{# Tag} other{# Tage}}"},
		{"zz", "?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{AITranslated, AIError, AIError, AITranslated, AIError}
	for i, r := range res {
		if r.Result != want[i] {
			t.Errorf("%s: %s %v, want %s", r.Key, r.Result, r.Errors, want[i])
		}
	}
	if a := entry(t, s, "de", "a"); a.Text != "Apfel" || a.Origin != "machine" || a.UserID != ai.ID || a.Approved {
		t.Errorf("a credited wrong: %+v", a.Row)
	}
	if b := entry(t, s, "de", "b"); b.Text != "Banane {n}" || !b.Approved {
		t.Errorf("approved b touched: %+v", b.Row)
	}
	if c := entry(t, s, "de", "c"); c.Text != "Kirsche" || len(c.Suggestions) != 0 {
		t.Errorf("c changed in a missing-only job: %+v", c.Row)
	}
	if got := pendingKeys(t, s, ai.ID, AIInputMissing); len(got) != 0 {
		t.Errorf("still pending: %v", got)
	}

	// With "all", existing translations get suggestions, never new text.
	all := AISaveJob{Lang: "de", AIUserID: ai.ID, Input: AIInputAll}
	res, _ = s.AISave(ctx, all, []AIText{{"c", "Kirsche!"}, {"b", "Bananen {n}"}, {"a", "Apfel"}})
	if res[0].Result != AISuggested || res[1].Result != AIError || res[2].Result != AISame {
		t.Errorf("all: %+v", res)
	}
	c := entry(t, s, "de", "c")
	if c.Text != "Kirsche" || c.UserID != p.ID || len(c.Suggestions) != 1 || c.Suggestions[0].UserID != ai.ID {
		t.Errorf("c: %+v, suggestions %+v", c.Row, c.Suggestions)
	}
	if got := pendingKeys(t, s, ai.ID, AIInputAll); len(got) != 0 {
		t.Errorf("all still pending: %v", got)
	}

	// Suggestions only: a missing string stays missing.
	s.ImportSource(ctx, parse(t, `{"a": "Apple", "b": "Banana {n}", "c": "Cherry", "d": "{n, plural, one{# day} other{# days}}", "e": "Elder {x}"}`), nil)
	res, _ = s.AISave(ctx, AISaveJob{Lang: "de", AIUserID: ai.ID, Input: AIInputMissing, Suggest: true}, []AIText{{"e", "Holunder {y}"}, {"e", "Holunder"}})
	if res[0].Result != AIError || res[1].Result != AISuggested || len(res[1].Warnings) == 0 {
		t.Errorf("suggest: %+v", res)
	}
	if e := entry(t, s, "de", "e"); e.Has || len(e.Suggestions) != 1 {
		t.Errorf("e: %+v", e)
	}

	// Only AI users.
	if _, err := s.AISave(ctx, AISaveJob{Lang: "de", AIUserID: p.ID, Input: AIInputMissing}, []AIText{{"e", "Holunder {x}"}}); err == nil {
		t.Error("a person saved as an AI user")
	}
}

func TestAILookup(t *testing.T) {
	s, ai, _ := aiSetup(t)
	ctx := context.Background()
	s.AISave(ctx, AISaveJob{Lang: "de", AIUserID: ai.ID, Input: AIInputMissing}, []AIText{{"a", "Bananenapfel"}})
	ms, err := s.AILookup(ctx, "de", "banan", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Key != "b" || !ms[0].Approved || !ms[0].Human || ms[1].Key != "a" || ms[1].Human {
		t.Errorf("matches: %+v", ms)
	}
}

func TestAIRunStates(t *testing.T) {
	s, ai, p := aiSetup(t)
	ctx := context.Background()
	if _, err := s.CreateAIRun(ctx, AIRun{Lang: "de", UserID: p.ID, Input: AIInputMissing, MaxCost: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("run for a person: %v", err)
	}
	r, err := s.CreateAIRun(ctx, AIRun{Lang: "de", UserID: ai.ID, StartedBy: p.ID, Input: AIInputAll, Suggest: true, Notes: " du ", MaxCost: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != AIQueued || r.Model != "anthropic/claude-opus-5.5" || r.Notes != "du" || r.StartedByName != "P" || !r.Suggest {
		t.Errorf("created: %+v", r)
	}
	if _, err := s.StartNextAIRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartNextAIRun(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("second start: %v", err)
	}
	s.AddAIRunUsage(ctx, r.ID, AIUsage{Steps: 1, InputTokens: 100, CachedTokens: 80, OutputTokens: 10, Cost: 0.25})
	s.AddAIRunUsage(ctx, r.ID, AIUsage{Steps: 1, InputTokens: 100, Cost: 0.5})
	s.AddAIRunCounts(ctx, r.ID, 2, 1, 1)

	// A restarted server finds the run running and marks it interrupted.
	if n, err := s.InterruptAIRuns(ctx); n != 1 || err != nil {
		t.Fatalf("interrupt: %d %v", n, err)
	}
	if err := s.RestartAIRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = s.StartNextAIRun(ctx)
	if err := s.FinishAIRun(ctx, r.ID, AIDone, "All done.", ""); err != nil {
		t.Fatal(err)
	}
	r, _ = s.AIRun(ctx, r.ID)
	if r.Status != AIDone || r.Steps != 2 || r.Cost != 0.75 || r.CachedTokens != 80 || r.Translated != 2 || r.Failed != 1 || r.Summary != "All done." {
		t.Errorf("finished: %+v", r)
	}
	if err := s.RestartAIRun(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restarted a done run: %v", err)
	}

	q, _ := s.CreateAIRun(ctx, AIRun{Lang: "de", UserID: ai.ID, Input: AIInputMissing, MaxCost: 5})
	if ok, err := s.CancelQueuedAIRun(ctx, q.ID); !ok || err != nil {
		t.Errorf("cancel queued: %v %v", ok, err)
	}
	s.AddAIRunEvent(ctx, q.ID, "one")
	s.AddAIRunEvent(ctx, q.ID, "two")
	if ev, _ := s.AIRunEvents(ctx, q.ID); len(ev) != 2 || ev[1].Text != "two" {
		t.Errorf("events: %+v", ev)
	}
	if runs, _ := s.AIRuns(ctx, 10); len(runs) != 2 || runs[0].ID != q.ID || runs[0].Status != AICancelled {
		t.Errorf("runs: %+v", runs)
	}
}
