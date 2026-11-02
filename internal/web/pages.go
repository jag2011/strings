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

// strPath is the editor's path for a string.
func strPath(lang, file, key string) string {
	return "/t/" + lang + "/" + url.PathEscape(file) + "/" + url.PathEscape(key)
}

// fileFilter returns the file that ?file= limits a list to, or no file (ID
// 0) for all files. An unknown name, like a renamed file, shows all.
func (srv *Server) fileFilter(r *http.Request) (store.File, error) {
	name := r.FormValue("file")
	if name == "" {
		return store.File{}, nil
	}
	f, err := srv.store.FileByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		return store.File{}, nil
	}
	return f, err
}

// chip is a link that limits a page to one file, or shows all.
type chip struct {
	Label   string
	URL     string
	Current bool
}

// fileChips links to path once per file and once for all files, keeping
// the other query parameters but the page. With one file there are none.
func fileChips(files []store.File, current, path string, q url.Values) []chip {
	if len(files) < 2 {
		return nil
	}
	link := func(name string) string {
		v := url.Values{}
		for k, vs := range q {
			if k != "file" && k != "page" {
				v[k] = vs
			}
		}
		if name != "" {
			v.Set("file", name)
		}
		if len(v) == 0 {
			return path
		}
		return path + "?" + v.Encode()
	}
	out := []chip{{"All files", link(""), current == ""}}
	for _, f := range files {
		out = append(out, chip{f.Name, link(f.Name), f.Name == current})
	}
	return out
}

type homeData struct {
	Stats      []store.LangStats
	SourceLang string
	Total      int
	File       string // the file the stats are limited to; empty for all
	Chips      []chip

	// Project totals for the side column, summed over all languages.
	Complete int
	Sum      store.LangStats
	Top      []store.Contributor
}

func (srv *Server) home(w http.ResponseWriter, r *http.Request, u *store.User) error {
	file, err := srv.fileFilter(r)
	if err != nil {
		return err
	}
	stats, err := srv.store.Stats(r.Context(), file.ID)
	if err != nil {
		return err
	}
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	top, err := srv.store.TopContributors(r.Context(), "", 20)
	if err != nil {
		return err
	}
	files, err := srv.store.Files(r.Context())
	if err != nil {
		return err
	}
	d := homeData{Stats: stats, SourceLang: set["source_lang"], Top: top, File: file.Name,
		Chips: fileChips(files, file.Name, "/", r.URL.Query())}
	if len(stats) > 0 {
		d.Total = stats[0].Total
	}
	for _, s := range stats {
		if s.Done == s.Total {
			d.Complete++
		}
		d.Sum.Total += s.Total
		d.Sum.Done += s.Done
		d.Sum.Approved += s.Approved
		d.Sum.Outdated += s.Outdated
		d.Sum.Missing += s.Missing
	}
	return srv.render(w, r, http.StatusOK, "home", u, "", d)
}

type listData struct {
	Lang, Filter, Q string
	File            string // the file the list is limited to; empty for all
	Labels          bool   // rows show their file
	Chips           []chip
	Filters         []string
	Counts          map[string]int // per filter, with the search applied
	Rows            []store.Row
	Count           int
	Page, Pages     int
	Stats           store.LangStats
	Top             []store.Contributor
}

func (srv *Server) list(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang := r.PathValue("lang")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	file, err := srv.fileFilter(r)
	if err != nil {
		return err
	}
	files, err := srv.store.Files(r.Context())
	if err != nil {
		return err
	}
	q := r.URL.Query()
	d := listData{Lang: lang, Filter: q.Get("filter"), Q: strings.TrimSpace(q.Get("q")), Filters: store.Filters,
		File: file.Name, Labels: len(files) > 1 && file.ID == 0, Chips: fileChips(files, file.Name, "/t/"+lang, q)}
	if !slices.Contains(store.Filters, d.Filter) {
		d.Filter = store.FilterTodo
	}
	d.Page, _ = strconv.Atoi(q.Get("page"))
	d.Page = max(d.Page, 1)
	rows, count, err := srv.store.List(r.Context(), file.ID, lang, d.Filter, d.Q, (d.Page-1)*pageSize, pageSize)
	if err != nil {
		return err
	}
	if d.Counts, err = srv.store.FilterCounts(r.Context(), file.ID, lang, d.Q); err != nil {
		return err
	}
	if d.Top, err = srv.store.TopContributors(r.Context(), lang, 5); err != nil {
		return err
	}
	stats, err := srv.store.Stats(r.Context(), file.ID)
	if err != nil {
		return err
	}
	for _, st := range stats {
		if st.Lang == lang {
			d.Stats = st
		}
	}
	d.Rows, d.Count = rows, count
	d.Pages = max((count+pageSize-1)/pageSize, 1)
	return srv.render(w, r, http.StatusOK, "list", u, langName(lang), d)
}

// keyItem is a line of the editor's key list: a string, after a heading
// where a new file starts.
type keyItem struct {
	store.KeyState
	Heading string
}

type editorData struct {
	Lang, Filter, Q string
	File            store.File // the string's file
	FilePath        string     // its path in this language
	ListFile        string     // the file the list being worked through is limited to; empty for all
	Entry           *store.Entry
	Description     string
	Placeholders    []arb.Placeholder
	Text            string // textarea content
	Approved        bool   // checkbox state, for proofreaders and admins
	Base            int64  // history id the form is based on
	Errors, Warns   []string
	Problems        []string // issues of the current text, shown when opening the editor
	Conflict        bool
	ListURL         string
	Next            store.Ref
	Keys            []keyItem // the list being worked through, for the side column
	KeysMatching    int       // Keys minus the current key when it no longer matches
}

// pathFile returns the file named in the URL path.
func (srv *Server) pathFile(r *http.Request) (store.File, error) {
	return srv.store.FileByName(r.Context(), r.PathValue("file"))
}

func (srv *Server) editorState(r *http.Request, lang string, file store.File, key string) (*editorData, error) {
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return nil, err
	}
	e, err := srv.store.Entry(r.Context(), file.ID, lang, key)
	if err != nil {
		return nil, err
	}
	list, err := srv.fileFilter(r)
	if err != nil {
		return nil, err
	}
	d := &editorData{Lang: lang, File: file, FilePath: file.Path(lang), ListFile: list.Name, Entry: e, Text: e.Text, Base: e.HistoryID, Approved: e.Approved}
	d.Filter, d.Q = r.FormValue("filter"), r.FormValue("q")
	if !slices.Contains(store.Filters, d.Filter) {
		d.Filter = store.FilterTodo
	}
	d.Description, d.Placeholders = arb.Placeholders(e.Meta)
	d.ListURL = listURL(lang, d.Filter, d.Q, d.ListFile) + "#" + file.Name + "/" + key
	cur := store.Ref{FileID: file.ID, File: file.Name, Key: key}
	if d.Next, err = srv.store.Next(r.Context(), list.ID, lang, d.Filter, d.Q, cur); err != nil {
		return nil, err
	}
	keys, err := srv.store.Keys(r.Context(), list.ID, lang, d.Filter, d.Q, cur)
	if err != nil {
		return nil, err
	}
	multi := false
	for _, k := range keys {
		multi = multi || k.FileID != keys[0].FileID
	}
	for i, k := range keys {
		item := keyItem{KeyState: k}
		if multi && (i == 0 || k.FileID != keys[i-1].FileID) {
			item.Heading = k.File
		}
		d.Keys = append(d.Keys, item)
		if k.Matches {
			d.KeysMatching++
		}
	}
	return d, nil
}

func (srv *Server) editor(w http.ResponseWriter, r *http.Request, u *store.User) error {
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	d, err := srv.editorState(r, r.PathValue("lang"), file, r.PathValue("key"))
	if err != nil {
		return err
	}
	if d.Entry.Has {
		errs, warns := arb.Check(d.Entry.Source, d.Entry.Text)
		d.Problems = append(errs, warns...)
	}
	return srv.render(w, r, http.StatusOK, "editor", u, d.Entry.Key+" · "+langName(d.Lang), d)
}

// oldEditor redirects links from before there were several files to the
// first file that has the key.
func (srv *Server) oldEditor(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	refs, err := srv.store.FindKey(r.Context(), key)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return notFound()
	}
	to := strPath(lang, refs[0].File, key)
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
	return nil
}

// listURL is the list with a filter, search and file.
func listURL(lang, filter, q, file string) string {
	return "/t/" + lang + listQuery(filter, q, file)
}

func listQuery(filter, q, file string) string {
	v := url.Values{}
	if filter != "" && filter != store.FilterTodo {
		v.Set("filter", filter)
	}
	if q != "" {
		v.Set("q", q)
	}
	if file != "" {
		v.Set("file", file)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// editorURL keeps the list filter, search and file while moving between
// strings.
func editorURL(lang, file, key, filter, q, listFile string) string {
	return strPath(lang, file, key) + listQuery(filter, q, listFile)
}

// editorBack returns to the editor after a form in it, keeping the list.
func editorBack(r *http.Request, lang string, file store.File, key string) string {
	return editorURL(lang, file.Name, key, r.PostFormValue("filter"), r.PostFormValue("q"), r.PostFormValue("file"))
}

func (srv *Server) save(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	d, err := srv.editorState(r, lang, file, key)
	if err != nil {
		return err
	}
	// Browsers send textarea line breaks as CRLF.
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	base, _ := strconv.ParseInt(r.PostFormValue("base"), 10, 64)
	d.Text, d.Base = text, base
	title := d.Entry.Key + " · " + langName(lang)

	// The form says whether the string was approved when it was opened, so a
	// proofreader doesn't undo an approval made in the meantime.
	approval := store.KeepApproval
	if u.CanApprove() {
		was := r.PostFormValue("was_approved") == "1"
		d.Approved = r.PostFormValue("approved") == "1"
		switch {
		case d.Approved:
			approval = store.Approve
		case was:
			approval = store.Unapprove
		}
	}

	// Only removing an approval needs no checks.
	if approval != store.Unapprove || text != d.Entry.Text {
		d.Errors, d.Warns = arb.Check(d.Entry.Source, text)
		if len(d.Errors) > 0 || (len(d.Warns) > 0 && r.PostFormValue("force") == "") {
			return srv.render(w, r, http.StatusUnprocessableEntity, "editor", u, title, d)
		}
	}
	saved, err := srv.store.Save(r.Context(), file.ID, lang, key, text, u.ID, base, approval)
	if errors.Is(err, store.ErrConflict) {
		d.Conflict, d.Base, d.Warns = true, d.Entry.HistoryID, nil
		return srv.render(w, r, http.StatusConflict, "editor", u, title, d)
	}
	if errors.Is(err, store.ErrApproved) {
		// Approved since the editor was opened.
		if d, err = srv.editorState(r, lang, file, key); err != nil {
			return err
		}
		d.Text, d.Errors = text, []string{"This translation has been approved in the meantime. Only proofreaders and admins can change it."}
		return srv.render(w, r, http.StatusConflict, "editor", u, title, d)
	}
	if err != nil {
		return err
	}
	flash := ""
	switch {
	case !saved:
	case approval == store.Approve:
		flash = "Approved " + key + "."
	case approval == store.Unapprove:
		flash = "Removed the approval of " + key + "."
	default:
		flash = "Saved " + key + "."
	}
	if r.PostFormValue("action") == "stay" {
		return redirect(w, r, editorURL(lang, file.Name, key, d.Filter, d.Q, d.ListFile), flash)
	}
	// The next key is looked up after saving: with the "todo" filter the
	// string just saved no longer matches.
	list, err := srv.fileFilter(r)
	if err != nil {
		return err
	}
	next, err := srv.store.Next(r.Context(), list.ID, lang, d.Filter, d.Q, store.Ref{FileID: file.ID, Key: key})
	if err != nil {
		return err
	}
	if next.Key == "" {
		return redirect(w, r, listURL(lang, d.Filter, d.Q, d.ListFile), strings.TrimSpace(flash+" Nothing left in this list."))
	}
	return redirect(w, r, editorURL(lang, next.File, next.Key, d.Filter, d.Q, d.ListFile), flash)
}

func (srv *Server) revert(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.Revert(r.Context(), file.ID, lang, key, id, u.ID); err != nil {
		return err
	}
	return redirect(w, r, editorBack(r, lang, file, key), "Restored an earlier version.")
}

func (srv *Server) deleteVersion(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.DeleteVersion(r.Context(), file.ID, lang, key, id, u); err != nil {
		return err
	}
	return redirect(w, r, editorBack(r, lang, file, key), "Deleted a version.")
}

func (srv *Server) useSuggestion(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	d, err := srv.editorState(r, lang, file, key)
	if err != nil {
		return err
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	i := slices.IndexFunc(d.Entry.Suggestions, func(g store.Suggestion) bool { return g.ID == id })
	if i < 0 {
		return notFound()
	}
	text := d.Entry.Suggestions[i].Text
	back := editorURL(lang, file.Name, key, d.Filter, d.Q, d.ListFile)
	if errs, _ := arb.Check(d.Entry.Source, text); len(errs) > 0 {
		return redirect(w, r, back, "This suggestion can't be used: "+errs[0])
	}
	approve := r.PostFormValue("approve") == "1"
	base, _ := strconv.ParseInt(r.PostFormValue("base"), 10, 64)
	err = srv.store.UseSuggestion(r.Context(), file.ID, lang, key, id, u, base, approve)
	switch {
	case errors.Is(err, store.ErrConflict):
		return redirect(w, r, back, "Someone saved this string in the meantime. Check the current text before using a suggestion.")
	case errors.Is(err, store.ErrApproved):
		return redirect(w, r, back, "This translation is approved. Only proofreaders and admins can replace it.")
	case err != nil:
		return err
	}
	if approve {
		return redirect(w, r, back, "Used and approved the suggestion.")
	}
	return redirect(w, r, back, "Used the suggestion.")
}

func (srv *Server) dismissSuggestion(w http.ResponseWriter, r *http.Request, u *store.User) error {
	lang, key := r.PathValue("lang"), r.PathValue("key")
	if err := srv.requireLang(r.Context(), lang); err != nil {
		return err
	}
	file, err := srv.pathFile(r)
	if err != nil {
		return err
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.DismissSuggestion(r.Context(), file.ID, lang, key, id, u); err != nil {
		return err
	}
	return redirect(w, r, editorBack(r, lang, file, key), "Deleted a suggestion.")
}

type activityData struct {
	Path  string // the page, for the pager
	Lang  string // empty for all languages
	Mine  bool   // only the user's own activity
	Multi bool   // label lines with their file
	Items []store.Activity
	Page  int
	More  bool
}

// activity shows recent edits and approval changes. A language is a query
// parameter rather than /t/{lang}/activity, which a key could be named.
func (srv *Server) activity(w http.ResponseWriter, r *http.Request, u *store.User) error {
	d := activityData{Path: "/activity", Lang: r.URL.Query().Get("lang")}
	if d.Lang != "" {
		if err := srv.requireLang(r.Context(), d.Lang); err != nil {
			return err
		}
	}
	if err := srv.loadActivity(r, &d, 0); err != nil {
		return err
	}
	title := "Activity"
	if d.Lang != "" {
		title += " · " + langName(d.Lang)
	}
	return srv.render(w, r, http.StatusOK, "activity", u, title, d)
}

// account was the "My edits" page, now part of the user's profile.
func (srv *Server) account(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return redirect(w, r, "/u/"+strconv.FormatInt(u.ID, 10), "")
}

type profileData struct {
	Profile      *store.User
	Own          bool // the viewer's own profile
	ShowEmail    bool // to the user themselves and admins
	Translations int
	Approvals    int
	Langs        []store.LangCount
	Activity     activityData
	AllLangs     []string // for admins deleting the user's translations
}

// profile shows a user's contributions and recent edits and approvals.
func (srv *Server) profile(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := srv.store.User(r.Context(), id)
	if err != nil {
		return err
	}
	own := u != nil && p.ID == u.ID
	d := profileData{Profile: p, Own: own, ShowEmail: own || u.IsAdmin()}
	if d.Langs, err = srv.store.UserLanguages(r.Context(), p.ID); err != nil {
		return err
	}
	for _, l := range d.Langs {
		d.Translations += l.Count
	}
	if _, d.Approvals, err = srv.store.Contributions(r.Context(), p.ID); err != nil {
		return err
	}
	if u.IsAdmin() {
		if d.AllLangs, err = srv.store.Languages(r.Context()); err != nil {
			return err
		}
	}
	d.Activity = activityData{Path: "/u/" + strconv.FormatInt(p.ID, 10), Mine: true}
	if err := srv.loadActivity(r, &d.Activity, p.ID); err != nil {
		return err
	}
	return srv.render(w, r, http.StatusOK, "profile", u, p.Name, d)
}

// loadActivity fills in the requested page of activity.
func (srv *Server) loadActivity(r *http.Request, d *activityData, userID int64) error {
	d.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	d.Page = max(d.Page, 1)
	items, err := srv.store.Activity(r.Context(), d.Lang, userID, (d.Page-1)*pageSize, pageSize+1)
	if err != nil {
		return err
	}
	files, err := srv.store.Files(r.Context())
	if err != nil {
		return err
	}
	d.Multi = len(files) > 1
	d.Items, d.More = items[:min(len(items), pageSize)], len(items) > pageSize
	return nil
}
