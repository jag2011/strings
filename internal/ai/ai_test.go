package ai

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

// fakeAgent runs a script instead of a model. The script drives the
// session like a model would: a step before each tool round, then calls.
type fakeAgent struct {
	mu       sync.Mutex
	sessions []Session
	script   func(c *caller) (string, error)
}

func (f *fakeAgent) Run(ctx context.Context, s Session) (string, error) {
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	return f.script(&caller{ctx: ctx, s: s})
}

func (f *fakeAgent) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type caller struct {
	ctx  context.Context
	s    Session
	cost float64 // per step
}

// step is one model call: BeforeStep may end the session.
func (c *caller) step() error {
	if err := c.s.BeforeStep(c.ctx); err != nil {
		return err
	}
	c.s.AfterStep(c.ctx, store.AIUsage{Steps: 1, InputTokens: 1000, CachedTokens: 800, OutputTokens: 100, Cost: c.cost})
	return nil
}

// call runs a tool with JSON args, like the SDK does, and decodes the result.
func (c *caller) call(name string, args any) map[string]any {
	raw, _ := json.Marshal(args)
	for _, t := range c.s.Tools {
		if t.Name == name {
			out, err := t.Call(c.ctx, raw)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			b, _ := json.Marshal(out)
			var m map[string]any
			json.Unmarshal(b, &m)
			return m
		}
	}
	panic("no tool " + name)
}

// translateAll is a well-behaved model: it takes batches of size and
// translates each source text with tr until nothing is left.
func translateAll(size int, tr func(key, src string) string) func(c *caller) (string, error) {
	return func(c *caller) (string, error) {
		if err := c.step(); err != nil {
			return "", err
		}
		c.call("get_job", struct{}{})
		for {
			if err := c.step(); err != nil {
				return "", err
			}
			res := c.call("next_strings", map[string]int{"limit": size})
			raw, _ := json.Marshal(res["strings"])
			f, err := arb.Parse(raw)
			if err != nil {
				return "", err
			}
			if len(f.Messages) == 0 {
				if err := c.step(); err != nil {
					return "", err
				}
				return "Translated everything.", nil
			}
			var ts []map[string]string
			for _, m := range f.Messages {
				ts = append(ts, map[string]string{"key": m.Key, "text": tr(m.Key, m.Text)})
			}
			if err := c.step(); err != nil {
				return "", err
			}
			c.call("save", map[string]any{"translations": ts})
		}
	}
}

func german(_, src string) string { return "DE " + src }

type env struct {
	t   *testing.T
	st  *store.Store
	ai  *store.User
	p   int64 // a proofreader
	fa  *fakeAgent
	r   *Runner
	ctx context.Context
}

func setup(t *testing.T, script func(c *caller) (string, error)) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	f, _ := arb.Parse([]byte(`{"a": "Apple", "@a": {"description": "The fruit"}, "b": "Banana {n}", "c": "Cherry", "d": "Date", "e": "Elder"}`))
	if _, err := st.ImportSource(ctx, f, nil); err != nil {
		t.Fatal(err)
	}
	st.AddLanguage(ctx, "de")
	ai, _ := st.CreateAIUser(ctx, "Robo", "anthropic/claude-opus-5.5")
	p, _ := st.CreateUser(ctx, "p@example.com", "P", "proofreader")
	fa := &fakeAgent{script: script}
	return &env{t: t, st: st, ai: ai, p: p.ID, fa: fa, r: NewRunner(st, fa), ctx: ctx}
}

// start runs the worker until the test ends.
func (e *env) start() {
	ctx, cancel := context.WithCancel(e.ctx)
	if err := e.r.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cancel(); e.r.Wait() })
}

func (e *env) queue(run store.AIRun) *store.AIRun {
	e.t.Helper()
	run.Lang, run.UserID = "de", e.ai.ID
	if run.Input == "" {
		run.Input = store.AIInputMissing
	}
	if run.MaxCost == 0 {
		run.MaxCost = 5
	}
	r, err := e.st.CreateAIRun(e.ctx, run)
	if err != nil {
		e.t.Fatal(err)
	}
	e.r.Wake()
	return r
}

// wait returns the run once it is no longer queued or running.
func (e *env) wait(id int64) *store.AIRun {
	e.t.Helper()
	for range 500 {
		r, err := e.st.AIRun(e.ctx, id)
		if err != nil {
			e.t.Fatal(err)
		}
		if !r.Active() {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("run didn't finish")
	return nil
}

func (e *env) waitStatus(id int64, status string) {
	e.t.Helper()
	for range 500 {
		if r, _ := e.st.AIRun(e.ctx, id); r.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("run never got %s", status)
}

func (e *env) events(id int64) string {
	ev, _ := e.st.AIRunEvents(e.ctx, id)
	var lines []string
	for _, x := range ev {
		lines = append(lines, x.Text)
	}
	return strings.Join(lines, "\n")
}

func TestRunTranslatesMissing(t *testing.T) {
	e := setup(t, translateAll(2, german))
	e.st.Save(e.ctx, "de", "c", "Kirsche", e.p, -1, store.KeepApproval)
	e.start()
	r := e.wait(e.queue(store.AIRun{}).ID)
	if r.Status != store.AIDone || r.Translated != 4 || r.Suggested != 0 || r.Failed != 0 || r.Summary != "Translated everything." || r.Error != "" {
		t.Fatalf("run: %+v", r)
	}
	if r.Steps == 0 || r.InputTokens != int64(r.Steps)*1000 || r.CachedTokens != int64(r.Steps)*800 {
		t.Errorf("usage: %+v", r)
	}
	a, _ := e.st.Entry(e.ctx, "de", "a")
	if a.Text != "DE Apple" || a.UserID != e.ai.ID || a.Origin != "machine" {
		t.Errorf("a: %+v", a.Row)
	}
	if c, _ := e.st.Entry(e.ctx, "de", "c"); c.Text != "Kirsche" || len(c.Suggestions) != 0 {
		t.Errorf("c touched: %+v", c.Row)
	}
	log := e.events(r.ID)
	for _, want := range []string{"started with anthropic/claude-opus-5.5", "next_strings: 2 strings, 2 left", "saved 2: 2 ok", "done; 0 strings still to do"} {
		if !strings.Contains(log, want) {
			t.Errorf("log misses %q:\n%s", want, log)
		}
	}
	s := e.fa.sessions[0]
	if s.Model != "anthropic/claude-opus-5.5" || !strings.Contains(s.System, "from en into de (de)") || strings.Contains(s.System, "{{") {
		t.Errorf("session: %s\n%s", s.Model, s.System)
	}
	if r.Prompt != s.System {
		t.Errorf("prompt not recorded: %q", r.Prompt)
	}
}

func TestCustomPrompt(t *testing.T) {
	e := setup(t, translateAll(10, german))
	e.st.SetSettings(e.ctx, map[string]string{"ai_prompt": "Translate into {{language}} ({{ code }}), a game. {{job}} {{unknown}}"})
	e.start()
	r := e.wait(e.queue(store.AIRun{Suggest: true}).ID)
	want := "Translate into de (de), a game. Translate the strings that have no translation yet. Everything you save becomes a suggestion for a person to review. {{unknown}}"
	if s := e.fa.sessions[0].System; s != want || r.Prompt != want {
		t.Errorf("system prompt: %q", s)
	}
}

func TestCheckPrompt(t *testing.T) {
	if err := CheckPrompt(DefaultPrompt); err != nil {
		t.Errorf("default: %v", err)
	}
	if err := CheckPrompt("Into {{language}}, {{lang}} and {{ lang }} {{x}}"); err == nil || err.Error() != "unknown variables {{lang}}, {{ lang }}, {{x}}" {
		t.Errorf("unknown: %v", err)
	}
	if err := CheckPrompt(" \n"); err == nil {
		t.Error("empty prompt passed")
	}
}

func TestRunSuggestsForAll(t *testing.T) {
	e := setup(t, translateAll(10, german))
	e.st.Save(e.ctx, "de", "c", "Kirsche", e.p, -1, store.KeepApproval)
	e.st.Save(e.ctx, "de", "d", "Dattel", e.p, -1, store.Approve)
	e.start()
	r := e.wait(e.queue(store.AIRun{Input: store.AIInputAll}).ID)
	if r.Status != store.AIDone || r.Translated != 3 || r.Suggested != 1 {
		t.Fatalf("run: %+v", r)
	}
	c, _ := e.st.Entry(e.ctx, "de", "c")
	if c.Text != "Kirsche" || len(c.Suggestions) != 1 || c.Suggestions[0].Text != "DE Cherry" {
		t.Errorf("c: %+v %+v", c.Row, c.Suggestions)
	}
	if d, _ := e.st.Entry(e.ctx, "de", "d"); d.Text != "Dattel" || len(d.Suggestions) != 0 {
		t.Errorf("approved d touched: %+v", d)
	}
}

func TestNextStringsAndLookup(t *testing.T) {
	e := setup(t, nil)
	e.st.Save(e.ctx, "de", "c", "Kirsche", e.p, -1, store.Approve)
	e.st.Save(e.ctx, "de", "d", "Dattelkirsche", e.p, -1, store.KeepApproval)
	run, _ := e.st.CreateAIRun(e.ctx, store.AIRun{Lang: "de", UserID: e.ai.ID, Input: store.AIInputAll, MaxCost: 1})
	j := &job{run: run, st: e.st, langName: "German", offered: map[string]bool{}, failed: map[string]bool{}, log: func(context.Context, string, ...any) {}}
	c := &caller{ctx: e.ctx, s: Session{Tools: j.tools()}}

	if got := c.call("get_job", struct{}{}); got["strings_left"] != 4.0 || got["language"] != "German (de)" {
		t.Errorf("get_job: %v", got)
	}
	res := c.call("next_strings", map[string]int{"limit": 2})
	raw, _ := json.Marshal(res["strings"])
	f, err := arb.Parse(raw)
	if err != nil || len(f.Messages) != 2 || f.Messages[0].Key != "a" || !strings.Contains(string(f.Messages[0].Meta), "The fruit") {
		t.Errorf("first batch: %s %v", raw, err)
	}
	if res["left"] != 2.0 {
		t.Errorf("left: %v", res["left"])
	}
	res = c.call("next_strings", map[string]int{"limit": 0})
	raw, _ = json.Marshal(res["strings"])
	f, _ = arb.Parse(raw)
	var keys []string
	for _, m := range f.Messages {
		keys = append(keys, m.Key)
	}
	if !slices.Equal(keys, []string{"d", "e"}) || res["current"].(map[string]any)["d"] != "Dattelkirsche" || res["left"] != 0.0 {
		t.Errorf("second batch, approved c left out: %v %v", keys, res)
	}

	got := c.call("lookup", map[string]string{"query": "kirsche"})
	ms := got["matches"].([]any)
	if len(ms) != 2 || ms[0].(map[string]any)["key"] != "c" || ms[0].(map[string]any)["approved"] != true {
		t.Errorf("lookup: %v", got)
	}
	if got := c.call("lookup", map[string]string{"query": " "}); got["error"] == nil {
		t.Errorf("empty lookup: %v", got)
	}
}

func TestSaveErrorsCountAsFailed(t *testing.T) {
	// "b" loses its placeholder, an error the model never fixes.
	e := setup(t, translateAll(10, func(key, src string) string {
		if key == "b" {
			return "Banane {m}"
		}
		return german(key, src)
	}))
	e.start()
	r := e.wait(e.queue(store.AIRun{}).ID)
	if r.Status != store.AIDone || r.Translated != 4 || r.Failed != 1 {
		t.Fatalf("run: %+v", r)
	}
	if log := e.events(r.ID); !strings.Contains(log, "saved 5: 4 ok, 1 error: b") || !strings.Contains(log, "1 string still to do") {
		t.Errorf("log:\n%s", log)
	}
}

func TestFreshSessions(t *testing.T) {
	e := setup(t, translateAll(1, german))
	e.r.SessionSteps = 3
	e.start()
	r := e.wait(e.queue(store.AIRun{}).ID)
	if r.Status != store.AIDone || r.Translated != 5 {
		t.Fatalf("run: %+v", r)
	}
	if n := e.fa.count(); n < 3 {
		t.Errorf("%d sessions", n)
	}
	if !strings.Contains(e.fa.sessions[1].Prompt, "Continue") {
		t.Errorf("second prompt: %q", e.fa.sessions[1].Prompt)
	}
}

func TestCostLimit(t *testing.T) {
	e := setup(t, nil)
	e.fa.script = func(c *caller) (string, error) {
		c.cost = 0.4
		return translateAll(1, german)(c)
	}
	e.start()
	r := e.wait(e.queue(store.AIRun{MaxCost: 1}).ID)
	if r.Status != store.AIDone || !strings.Contains(r.Error, "cost limit of $1.00") || r.Steps != 3 || r.Translated != 1 {
		t.Fatalf("run: %+v", r)
	}
}

func TestStopsWhenModelGivesUp(t *testing.T) {
	e := setup(t, func(c *caller) (string, error) {
		if err := c.step(); err != nil {
			return "", err
		}
		return "I can't do this.", nil
	})
	e.start()
	r := e.wait(e.queue(store.AIRun{}).ID)
	if r.Status != store.AIDone || r.Summary != "I can't do this." || e.fa.count() != 1 {
		t.Fatalf("run: %+v, %d sessions", r, e.fa.count())
	}
}

func TestFailedRun(t *testing.T) {
	e := setup(t, func(c *caller) (string, error) { return "", errors.New("model not found") })
	e.start()
	r := e.wait(e.queue(store.AIRun{}).ID)
	if r.Status != store.AIFailed || r.Error != "model not found" || !r.CanRestart() {
		t.Fatalf("run: %+v", r)
	}
}

// blocking returns a script that saves one string, then waits in its next
// step until release is closed.
func blocking(started chan<- struct{}, release <-chan struct{}) func(c *caller) (string, error) {
	return func(c *caller) (string, error) {
		if err := c.step(); err != nil {
			return "", err
		}
		c.call("next_strings", map[string]int{"limit": 1})
		c.call("save", map[string]any{"translations": []map[string]string{{"key": "a", "text": "Apfel"}}})
		close(started)
		select {
		case <-release:
		case <-c.ctx.Done():
			return "", c.ctx.Err()
		}
		for {
			if err := c.step(); err != nil {
				return "", err
			}
		}
	}
}

func TestCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := setup(t, blocking(started, release))
	e.start()
	r := e.queue(store.AIRun{})
	queued := e.queue(store.AIRun{})
	<-started
	if err := e.r.Cancel(e.ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.st.AIRun(e.ctx, queued.ID); q.Status != store.AICancelled {
		t.Errorf("queued run: %s", q.Status)
	}
	if err := e.r.Cancel(e.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := e.wait(r.ID); got.Status != store.AICancelled || got.Translated != 1 {
		t.Fatalf("run: %+v", got)
	}
}

func TestInterruptedAndRestarted(t *testing.T) {
	started := make(chan struct{})
	e := setup(t, blocking(started, make(chan struct{})))
	ctx, stop := context.WithCancel(e.ctx)
	if err := e.r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := e.queue(store.AIRun{})
	<-started
	stop() // the server shuts down
	e.r.Wait()
	if got, _ := e.st.AIRun(e.ctx, r.ID); got.Status != store.AIInterrupted || got.Translated != 1 || !strings.Contains(got.Error, "server stopped") {
		t.Fatalf("after shutdown: %+v", got)
	}

	// A crash leaves the run running; the next start marks it interrupted.
	e.st.RestartAIRun(e.ctx, r.ID)
	e.st.StartNextAIRun(e.ctx)
	e.fa.script = translateAll(10, german)
	e.r = NewRunner(e.st, e.fa)
	e.start()
	e.waitStatus(r.ID, store.AIInterrupted)

	// Started again, it continues with what's left.
	if err := e.r.Restart(e.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got := e.wait(r.ID)
	if got.Status != store.AIDone || got.Translated != 5 {
		t.Fatalf("restarted: %+v", got)
	}
	if a, _ := e.st.Entry(e.ctx, "de", "a"); a.Text != "Apfel" {
		t.Errorf("a translated again: %q", a.Text)
	}
	if log := e.events(r.ID); !strings.Contains(log, "started again") || !strings.Contains(log, "next_strings: 4 strings, 0 left") {
		t.Errorf("log:\n%s", log)
	}
}
