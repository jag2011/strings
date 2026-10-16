package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
	Notes   string
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
		d.Input = store.AIInputMissing
	}
	if d.MaxCost == "" {
		d.MaxCost = DefaultAICostLimit
	}
	return srv.render(w, r, status, "ai", u, "AI translation", d)
}

func (srv *Server) aiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderAI(w, r, u, http.StatusOK, aiData{})
}

// aiTranslate queues a run.
func (srv *Server) aiTranslate(w http.ResponseWriter, r *http.Request, u *store.User) error {
	d := aiData{Lang: r.PostFormValue("lang"), Input: r.PostFormValue("input"), Suggest: r.PostFormValue("suggest") == "1",
		Notes: strings.TrimSpace(r.PostFormValue("notes")), MaxCost: strings.TrimSpace(r.PostFormValue("max_cost"))}
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

	run, err := srv.store.CreateAIRun(r.Context(), store.AIRun{Lang: d.Lang, UserID: ai.ID, StartedBy: u.ID, Input: d.Input,
		Suggest: d.Suggest, Notes: d.Notes, MaxCost: maxCost})
	if err != nil {
		return err
	}
	if err := srv.store.AddAIRunEvent(r.Context(), run.ID, fmt.Sprintf("queued by %s: %s", u.Name, aiPlan(run, pending))); err != nil {
		return err
	}
	srv.AI.Wake()
	return redirect(w, r, fmt.Sprintf("/admin/ai/runs/%d", run.ID), "")
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
