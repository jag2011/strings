package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/jag2011/strings/internal/store"
)

// What an AI run translates.
const (
	AIInputMissing = "missing" // strings without a translation
	AIInputAll     = "all"     // every string
)

type aiData struct {
	Stats   []store.LangStats
	Users   []*store.User // AI users only
	Lang    string
	UserID  int64
	Input   string
	Suggest bool   // save everything as suggestions
	Plan    string // what the submitted form would do
	Error   string
}

func (srv *Server) renderAI(w http.ResponseWriter, r *http.Request, u *store.User, status int, d aiData) error {
	var err error
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
	if d.Input == "" {
		d.Input = AIInputMissing
	}
	return srv.render(w, r, status, "ai", u, "AI translation", d)
}

func (srv *Server) aiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderAI(w, r, u, http.StatusOK, aiData{})
}

// aiTranslate checks the form and says what a run would do. Runs come next.
func (srv *Server) aiTranslate(w http.ResponseWriter, r *http.Request, u *store.User) error {
	d := aiData{Lang: r.PostFormValue("lang"), Input: r.PostFormValue("input"), Suggest: r.PostFormValue("suggest") == "1"}
	d.UserID, _ = strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	fail := func(msg string) error {
		d.Error = msg
		return srv.renderAI(w, r, u, http.StatusUnprocessableEntity, d)
	}
	if !slices.Contains([]string{AIInputMissing, AIInputAll}, d.Input) {
		return fail("Choose which strings to translate.")
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

	st := stats[i]
	missing, existing := st.Missing, 0
	if d.Input == AIInputAll {
		existing = st.Total - st.Missing
	}
	if missing+existing == 0 {
		return fail(langName(d.Lang) + " has no missing strings.")
	}
	plan := fmt.Sprintf("%s would translate %d %s into %s", ai.Name, missing+existing, plural(missing+existing, "string", "strings"), langName(d.Lang))
	switch {
	case d.Suggest:
		plan += ", all saved as suggestions."
	case existing > 0:
		plan += fmt.Sprintf(": %d missing as translations, %d already translated as suggestions.", missing, existing)
	default:
		plan += ", saved as translations."
	}
	d.Plan = plan + " Running translations isn't built yet."
	return srv.renderAI(w, r, u, http.StatusOK, d)
}
