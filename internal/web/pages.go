package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

const pageSize = 100

type homeData struct {
	Stats      []store.LangStats
	SourceLang string
	Total      int
}

func (srv *Server) home(w http.ResponseWriter, r *http.Request, u *store.User) error {
	stats, err := srv.store.Stats(r.Context())
	if err != nil {
		return err
	}
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	d := homeData{Stats: stats, SourceLang: set["source_lang"]}
	if len(stats) > 0 {
		d.Total = stats[0].Total
	}
	return srv.render(w, r, http.StatusOK, "home", u, "", d)
}

type listData struct {
	Lang, Filter, Q string
	Filters         []string
	Rows            []store.Row
	Count           int
	Page, Pages     int
}

func (srv *Server) list(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang := r.PathValue("lang")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	q := r.URL.Query()
	d := listData{Lang: lang, Filter: q.Get("filter"), Q: strings.TrimSpace(q.Get("q")), Filters: store.Filters}
	if !slices.Contains(store.Filters, d.Filter) {
		d.Filter = store.FilterTodo
	}
	d.Page, _ = strconv.Atoi(q.Get("page"))
	d.Page = max(d.Page, 1)
	rows, count, err := srv.store.List(r.Context(), lang, d.Filter, d.Q, (d.Page-1)*pageSize, pageSize)
	if err != nil {
		return err
	}
	d.Rows, d.Count = rows, count
	d.Pages = max((count+pageSize-1)/pageSize, 1)
	return srv.render(w, r, http.StatusOK, "list", u, langName(lang), d)
}

type editorData struct {
	Lang, Filter, Q string
	Entry           *store.Entry
	Description     string
	Placeholders    []arb.Placeholder
	Text            string // textarea content
	Base            int64  // history id the form is based on
	Errors, Warns   []string
	Problems        []string // issues of the current text, shown when opening the editor
	Conflict        bool
	ListURL         string
	NextKey         string
}

func (srv *Server) editorState(r *http.Request, lang, key string) (*editorData, error) {
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return nil, err
	}
	e, err := srv.store.Entry(r.Context(), lang, key)
	if err != nil {
		return nil, err
	}
	d := &editorData{Lang: lang, Entry: e, Text: e.Text, Base: e.HistoryID}
	d.Filter, d.Q = r.FormValue("filter"), r.FormValue("q")
	if !slices.Contains(store.Filters, d.Filter) {
		d.Filter = store.FilterTodo
	}
	d.Description, d.Placeholders = arb.Placeholders(e.Meta)
	v := url.Values{}
	if d.Filter != store.FilterTodo {
		v.Set("filter", d.Filter)
	}
	if d.Q != "" {
		v.Set("q", d.Q)
	}
	d.ListURL = "/t/" + lang
	if len(v) > 0 {
		d.ListURL += "?" + v.Encode()
	}
	d.ListURL += "#" + key
	d.NextKey, err = srv.store.Next(r.Context(), lang, d.Filter, d.Q, key, e.Position)
	return d, err
}

func (srv *Server) editor(w http.ResponseWriter, r *http.Request, u *store.User) error {
	d, err := srv.editorState(r, r.PathValue("lang"), r.PathValue("key"))
	if err != nil {
		return err
	}
	if d.Entry.Has {
		errs, warns := arb.Check(d.Entry.Source, d.Entry.Text)
		d.Problems = append(errs, warns...)
	}
	return srv.render(w, r, http.StatusOK, "editor", u, d.Entry.Key+" · "+langName(d.Lang), d)
}

// editorURL keeps the list filter and search while moving between strings.
func editorURL(lang, key, filter, q string) string {
	v := url.Values{}
	if filter != "" && filter != store.FilterTodo {
		v.Set("filter", filter)
	}
	if q != "" {
		v.Set("q", q)
	}
	s := "/t/" + lang + "/" + url.PathEscape(key)
	if len(v) > 0 {
		s += "?" + v.Encode()
	}
	return s
}

func (srv *Server) save(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	d, err := srv.editorState(r, lang, key)
	if err != nil {
		return err
	}
	// Browsers send textarea line breaks as CRLF.
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	base, _ := strconv.ParseInt(r.PostFormValue("base"), 10, 64)
	d.Text, d.Base = text, base
	title := d.Entry.Key + " · " + langName(lang)

	d.Errors, d.Warns = arb.Check(d.Entry.Source, text)
	if len(d.Errors) > 0 || (len(d.Warns) > 0 && r.PostFormValue("force") == "") {
		return srv.render(w, r, http.StatusUnprocessableEntity, "editor", u, title, d)
	}
	saved, err := srv.store.Save(r.Context(), lang, key, text, u.ID, base)
	if errors.Is(err, store.ErrConflict) {
		d.Conflict, d.Base, d.Warns = true, d.Entry.HistoryID, nil
		return srv.render(w, r, http.StatusConflict, "editor", u, title, d)
	}
	if err != nil {
		return err
	}
	flash := ""
	if saved {
		flash = "Saved " + key + "."
	}
	if r.PostFormValue("action") == "stay" {
		return redirect(w, r, editorURL(lang, key, d.Filter, d.Q), flash)
	}
	// The next key is looked up after saving: with the "todo" filter the
	// string just saved no longer matches.
	next, err := srv.store.Next(r.Context(), lang, d.Filter, d.Q, key, d.Entry.Position)
	if err != nil {
		return err
	}
	if next == "" {
		return redirect(w, r, strings.TrimSuffix(d.ListURL, "#"+key), strings.TrimSpace(flash+" Nothing left in this list."))
	}
	return redirect(w, r, editorURL(lang, next, d.Filter, d.Q), flash)
}

func (srv *Server) revert(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.Revert(r.Context(), lang, key, id, u.ID); err != nil {
		return err
	}
	return redirect(w, r, editorURL(lang, key, r.PostFormValue("filter"), r.PostFormValue("q")), "Restored an earlier version.")
}

func (srv *Server) account(w http.ResponseWriter, r *http.Request, u *store.User) error {
	edits, err := srv.store.UserEdits(r.Context(), u.ID, 200)
	if err != nil {
		return err
	}
	return srv.render(w, r, http.StatusOK, "account", u, "My edits", edits)
}
