package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/store"
)

// DefaultAICostLimit is the cost limit the form suggests, in USD.
const DefaultAICostLimit = "5"

type aiData struct {
	Enabled bool // an OpenRouter key is set
	Stats   []store.LangStats
	Users   []*store.User // AI users only
	Runs    []*store.AIRun
	Lang    string
	UserID  int64
	Input   string
	Suggest bool // save everything as suggestions
	MaxCost string
	Error   string
}

func (srv *Server) renderAI(w http.ResponseWriter, r *http.Request, u *store.User, status int, d aiData) error {
	var err error
	d.Enabled = srv.AI != nil
	if d.Stats, err = srv.store.Stats(r.Context()); err != nil {
		return err
	}
	users, err := srv.store.Users(r.Context())
	if err != nil {
		return err
	}
	for _, au := range users {
		if au.IsAI() && !au.Disabled {
			d.Users = append(d.Users, au)
		}
	}
	if d.Runs, err = srv.store.AIRuns(r.Context(), 50); err != nil {
		return err
	}
	if d.Input == "" {
		d.Input = store.AIInputAll
	}
	if d.MaxCost == "" {
		d.MaxCost = DefaultAICostLimit
	}
	return srv.render(w, r, status, "ai", u, "AI translation", d)
}

func (srv *Server) aiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderAI(w, r, u, http.StatusOK, aiData{Suggest: true})
}

// aiTranslate queues a run.
func (srv *Server) aiTranslate(w http.ResponseWriter, r *http.Request, u *store.User) error {
	d := aiData{Lang: r.PostFormValue("lang"), Input: r.PostFormValue("input"), Suggest: r.PostFormValue("suggest") == "1",
		MaxCost: strings.TrimSpace(r.PostFormValue("max_cost"))}
	d.UserID, _ = strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	fail := func(msg string) error {
		d.Error = msg
		return srv.renderAI(w, r, u, http.StatusUnprocessableEntity, d)
	}
	if srv.AI == nil {
		return fail("AI translation needs OPENROUTER_API_KEY; see the README.")
	}
	if !slices.Contains([]string{store.AIInputMissing, store.AIInputAll}, d.Input) {
		return fail("Choose which strings to translate.")
	}
	maxCost, err := strconv.ParseFloat(strings.TrimPrefix(d.MaxCost, "$"), 64)
	if err != nil || maxCost <= 0 || maxCost > 1000 {
		return fail("The cost limit must be an amount in dollars, more than 0 and at most 1000.")
	}
	stats, err := srv.store.Stats(r.Context())
	if err != nil {
		return err
	}
	i := slices.IndexFunc(stats, func(s store.LangStats) bool { return s.Lang == d.Lang })
	if i < 0 {
		return fail("Choose a language.")
	}
	ai, err := srv.store.User(r.Context(), d.UserID)
	if err != nil || !ai.IsAI() || ai.Disabled {
		return fail("Choose an AI user.")
	}
	pending, err := srv.store.AIPending(r.Context(), d.Lang, ai.ID, d.Input)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		if d.Input == store.AIInputAll {
			return fail(fmt.Sprintf("%s has done everything in %s that isn't approved.", ai.Name, langName(d.Lang)))
		}
		return fail(fmt.Sprintf("%s has no missing strings left for %s.", langName(d.Lang), ai.Name))
	}

	run, err := srv.queueAIRun(r.Context(), u, store.AIRun{Lang: d.Lang, UserID: ai.ID, Input: d.Input, Suggest: d.Suggest, MaxCost: maxCost}, pending)
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/admin/ai/runs/%d", run.ID), "")
}

// queueAIRun creates a run for the pending strings, started by u, and wakes
// the runner.
func (srv *Server) queueAIRun(ctx context.Context, u *store.User, run store.AIRun, pending []store.AIString) (*store.AIRun, error) {
	run.StartedBy = u.ID
	created, err := srv.store.CreateAIRun(ctx, run)
	if err != nil {
		return nil, err
	}
	if err := srv.store.AddAIRunEvent(ctx, created.ID, fmt.Sprintf("queued by %s: %s", u.Name, aiPlan(created, pending))); err != nil {
		return nil, err
	}
	srv.AI.Wake()
	return created, nil
}

// aiPlan says what a run will do with the pending strings.
func aiPlan(run *store.AIRun, pending []store.AIString) string {
	var missing, existing int
	for _, p := range pending {
		if p.HasCurrent {
			existing++
		} else {
			missing++
		}
	}
	n := missing + existing
	plan := fmt.Sprintf("%d %s into %s", n, plural(n, "string", "strings"), langName(run.Lang))
	switch {
	case run.Suggest:
		return plan + ", all as suggestions"
	case existing > 0 && missing > 0:
		return plan + fmt.Sprintf(": %d missing as translations, %d already translated as suggestions", missing, existing)
	case existing > 0:
		return plan + ", all already translated, as suggestions"
	}
	return plan + ", as translations"
}

type aiFillData struct {
	Enabled bool          // an OpenRouter key is set
	Users   []*store.User // AI users only
	User    *store.User   // the chosen one
	Fill    []store.AIFill
	Active  map[string]*store.AIRun // the chosen user's queued or running run per language
	MaxCost string
}

// aiFillPage shows per language how many missing strings the chosen AI
// user, and all AI users together, haven't suggested anything for yet.
func (srv *Server) aiFillPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	users, err := srv.store.Users(r.Context())
	if err != nil {
		return err
	}
	d := aiFillData{Enabled: srv.AI != nil, Active: map[string]*store.AIRun{}, MaxCost: DefaultAICostLimit}
	id, _ := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	for _, au := range users {
		if au.IsAI() && !au.Disabled {
			d.Users = append(d.Users, au)
			if d.User == nil || au.ID == id {
				d.User = au
			}
		}
	}
	if d.User != nil {
		if d.Fill, err = srv.store.AIFillStats(r.Context(), d.User.ID); err != nil {
			return err
		}
		if d.Active, err = srv.activeAIRuns(r.Context(), d.User.ID); err != nil {
			return err
		}
	}
	return srv.render(w, r, http.StatusOK, "aifill", u, "AI fill", d)
}

// activeAIRuns returns an AI user's queued or running run per language.
func (srv *Server) activeAIRuns(ctx context.Context, userID int64) (map[string]*store.AIRun, error) {
	runs, err := srv.store.AIRuns(ctx, 50)
	if err != nil {
		return nil, err
	}
	active := map[string]*store.AIRun{}
	for _, run := range runs {
		if run.UserID == userID && run.Active() && active[run.Lang] == nil {
			active[run.Lang] = run
		}
	}
	return active, nil
}

// aiFill queues a run per chosen language for the AI user's missing strings,
// saved as suggestions. Languages it's already working on are left out.
func (srv *Server) aiFill(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	back := fmt.Sprintf("/admin/ai/fill?user=%d", id)
	if srv.AI == nil {
		return redirect(w, r, back, "AI translation needs OPENROUTER_API_KEY; see the README.")
	}
	ai, err := srv.store.User(r.Context(), id)
	if err != nil || !ai.IsAI() || ai.Disabled {
		return badRequest("Choose an AI user.")
	}
	stats, err := srv.store.Stats(r.Context())
	if err != nil {
		return err
	}
	active, err := srv.activeAIRuns(r.Context(), ai.ID)
	if err != nil {
		return err
	}
	maxCost, _ := strconv.ParseFloat(DefaultAICostLimit, 64)
	var queued []string
	for _, lang := range r.PostForm["lang"] {
		if active[lang] != nil || !slices.ContainsFunc(stats, func(s store.LangStats) bool { return s.Lang == lang }) {
			continue
		}
		pending, err := srv.store.AIPending(r.Context(), lang, ai.ID, store.AIInputMissing)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			continue
		}
		run, err := srv.queueAIRun(r.Context(), u, store.AIRun{Lang: lang, UserID: ai.ID, Input: store.AIInputMissing, Suggest: true, MaxCost: maxCost}, pending)
		if err != nil {
			return err
		}
		queued = append(queued, fmt.Sprintf("#%d %s", run.ID, langName(lang)))
	}
	if len(queued) == 0 {
		return redirect(w, r, back, "Choose one or more languages with strings to fill.")
	}
	return redirect(w, r, back, fmt.Sprintf("%d fill %s queued: %s.", len(queued), plural(len(queued), "job", "jobs"), strings.Join(queued, ", ")))
}

// aiPromote makes the AI user's suggestions the translations of the strings
// missing or outdated in the chosen languages, unapproved, as if each was used
// in the editor.
func (srv *Server) aiPromote(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	back := fmt.Sprintf("/admin/ai/fill?user=%d", id)
	ai, err := srv.store.User(r.Context(), id)
	if err != nil || !ai.IsAI() {
		return badRequest("Choose an AI user.")
	}
	stats, err := srv.store.Stats(r.Context())
	if err != nil {
		return err
	}
	var total int
	var done []string
	for _, lang := range r.PostForm["lang"] {
		if !slices.ContainsFunc(stats, func(s store.LangStats) bool { return s.Lang == lang }) {
			continue
		}
		n, err := srv.store.PromoteSuggestions(r.Context(), lang, ai.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			total += n
			done = append(done, fmt.Sprintf("%s %d", langName(lang), n))
		}
	}
	if total == 0 {
		return redirect(w, r, back, "Choose one or more languages with suggestions to promote.")
	}
	return redirect(w, r, back, fmt.Sprintf("Promoted %d %s from %s to unapproved translations: %s.",
		total, plural(total, "suggestion", "suggestions"), ai.Name, strings.Join(done, ", ")))
}

type aiRunData struct {
	Run    *store.AIRun
	Events []store.AIRunEvent
}

func (srv *Server) aiRun(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := srv.store.AIRun(r.Context(), id)
	if err != nil {
		return err
	}
	ev, err := srv.store.AIRunEvents(r.Context(), id)
	if err != nil {
		return err
	}
	return srv.render(w, r, http.StatusOK, "airun", u, fmt.Sprintf("AI run %d", id), aiRunData{Run: run, Events: ev})
}

func (srv *Server) aiCancel(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if srv.AI == nil {
		return badRequest("AI translation is off.")
	}
	if err := srv.AI.Cancel(r.Context(), id); err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/admin/ai/runs/%d", id), "The run stops after its current step.")
}

func (srv *Server) aiRestart(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if srv.AI == nil {
		return badRequest("AI translation is off.")
	}
	if err := srv.AI.Restart(r.Context(), id); err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/admin/ai/runs/%d", id), "")
}

type aiPromptData struct {
	Prompt  string
	Default bool // the built-in prompt is used
	Vars    [][2]string
	Error   string
}

func (srv *Server) renderAIPrompt(w http.ResponseWriter, r *http.Request, u *store.User, status int, d aiPromptData) error {
	d.Vars = ai.PromptVars
	return srv.render(w, r, status, "aiprompt", u, "AI prompt", d)
}

func (srv *Server) aiPromptPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	d := aiPromptData{Prompt: set["ai_prompt"]}
	if d.Prompt == "" {
		d.Prompt, d.Default = ai.DefaultPrompt, true
	}
	return srv.renderAIPrompt(w, r, u, http.StatusOK, d)
}

// aiSavePrompt saves the prompt for the next runs. The built-in prompt is
// stored as empty, so later versions of it apply.
func (srv *Server) aiSavePrompt(w http.ResponseWriter, r *http.Request, u *store.User) error {
	prompt := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("prompt"), "\r\n", "\n")) + "\n"
	msg := "Prompt saved. It applies to runs started from now on."
	if r.PostFormValue("action") == "reset" || prompt == ai.DefaultPrompt {
		prompt, msg = "", "Using the built-in prompt for runs started from now on."
	} else if err := ai.CheckPrompt(prompt); err != nil {
		return srv.renderAIPrompt(w, r, u, http.StatusUnprocessableEntity, aiPromptData{Prompt: prompt, Error: err.Error() + "."})
	}
	if err := srv.store.SetSettings(r.Context(), map[string]string{"ai_prompt": prompt}); err != nil {
		return err
	}
	return redirect(w, r, "/admin/ai/prompt", msg)
}
