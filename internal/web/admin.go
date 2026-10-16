package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

type adminData struct {
	Languages []string
	Tokens    []store.APIToken
	NewToken  string
	Imported  []store.ImportResult
	Error     string
}

func (srv *Server) renderAdmin(w http.ResponseWriter, r *http.Request, u *store.User, status int, d adminData) error {
	var err error
	if d.Languages, err = srv.store.Languages(r.Context()); err != nil {
		return err
	}
	if d.Tokens, err = srv.store.APITokens(r.Context()); err != nil {
		return err
	}
	return srv.render(w, r, status, "admin", u, "Settings", d)
}

func (srv *Server) adminPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{})
}

var (
	colorRe  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	prefixRe = regexp.MustCompile(`^[A-Za-z0-9_.-]*$`)
)

func (srv *Server) adminSettings(w http.ResponseWriter, r *http.Request, u *store.User) error {
	m := map[string]string{
		"name":        strings.TrimSpace(r.PostFormValue("name")),
		"accent":      r.PostFormValue("accent"),
		"source_lang": strings.TrimSpace(r.PostFormValue("source_lang")),
		"file_prefix": strings.TrimSpace(r.PostFormValue("file_prefix")),
	}
	var msg string
	switch {
	case m["name"] == "":
		msg = "The name can't be empty."
	case !colorRe.MatchString(m["accent"]):
		msg = "The accent color must look like #2f6fde."
	case !arb.ValidLocale(m["source_lang"]):
		msg = "The source language must be a locale like en or en_US."
	case !prefixRe.MatchString(m["file_prefix"]):
		msg = "The file prefix may only contain letters, digits, _ . and -."
	}
	if msg != "" {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: msg})
	}
	if err := srv.store.SetSettings(r.Context(), m); err != nil {
		return err
	}
	return redirect(w, r, "/admin", "Settings saved.")
}

func (srv *Server) adminAddLanguage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	code := strings.TrimSpace(r.PostFormValue("code"))
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	if !arb.ValidLocale(code) {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity,
			adminData{Error: fmt.Sprintf("%q is not a locale code. Use the code from the file name, e.g. de, pt_BR or zh_Hant.", code)})
	}
	if code == set["source_lang"] {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: "That is the source language."})
	}
	if err := srv.store.AddLanguage(r.Context(), code); err != nil {
		return err
	}
	return redirect(w, r, "/admin", "Added "+langName(code)+".")
}

func (srv *Server) adminRemoveLanguage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	code := r.PathValue("lang")
	if err := srv.store.RemoveLanguage(r.Context(), code); err != nil {
		return err
	}
	return redirect(w, r, "/admin", "Removed "+langName(code)+". Its translations are kept and come back if you add it again.")
}

const maxUpload = 32 << 20

// adminImport imports uploaded files. If they contain the source file, the
// upload is kept and the changes are shown for review first.
func (srv *Server) adminImport(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return badRequest("The upload could not be read.")
	}
	files := map[string][]byte{}
	for _, fh := range r.MultipartForm.File["files"] {
		f, err := fh.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return err
		}
		files[fh.Filename] = data
	}
	if len(files) == 0 {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: "Choose one or more .arb files."})
	}
	diff, _, err := srv.store.PreviewFiles(r.Context(), files)
	if err != nil {
		return err
	}
	if diff != nil {
		id, err := srv.store.SaveUpload(r.Context(), files)
		if err != nil {
			return err
		}
		return redirect(w, r, "/admin/import/"+id, "")
	}
	res, err := srv.store.ImportFiles(r.Context(), files, nil)
	if err != nil {
		return err
	}
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{Imported: res})
}

type reviewData struct {
	ID     string
	File   string
	Others []string // translation files imported along with the source
	Diff   *store.SourceDiff
	Error  string
}

func (srv *Server) renderReview(w http.ResponseWriter, r *http.Request, u *store.User, status int, id, msg string) error {
	files, err := srv.store.Upload(r.Context(), id)
	if err != nil {
		return err
	}
	d := reviewData{ID: id, Error: msg}
	if d.Diff, d.File, err = srv.store.PreviewFiles(r.Context(), files); err != nil {
		return err
	}
	if d.Diff == nil {
		return notFound()
	}
	for name := range files {
		if name != d.File {
			d.Others = append(d.Others, name)
		}
	}
	sort.Strings(d.Others)
	return srv.render(w, r, status, "review", u, "Review import", d)
}

// textDiff is two texts split into their common start and end and the parts
// that differ, so a typo fix stands out.
type textDiff struct{ Pre, Old, New, Suf string }

func diffText(old, new string) textDiff {
	a, b := []rune(old), []rune(new)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	n := 0
	for n < len(a)-p && n < len(b)-p && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return textDiff{string(a[:p]), string(a[p : len(a)-n]), string(b[p : len(b)-n]), string(a[len(a)-n:])}
}

func (srv *Server) adminReview(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderReview(w, r, u, http.StatusOK, r.PathValue("id"), "")
}

func (srv *Server) adminConfirmImport(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id := r.PathValue("id")
	files, err := srv.store.Upload(r.Context(), id)
	if err != nil {
		return err
	}
	diff, _, err := srv.store.PreviewFiles(r.Context(), files)
	if err != nil {
		return err
	}
	if diff == nil {
		return notFound()
	}
	// The choices were made for the source as it was when the page was shown.
	if r.PostFormValue("revision") != strconv.Itoa(diff.Revision) {
		return srv.renderReview(w, r, u, http.StatusConflict, id,
			"The source was imported by someone else in the meantime. Here are the changes against the new state.")
	}
	decide := map[string]string{}
	for _, c := range diff.Changed {
		if v := r.PostFormValue("t." + c.Key); slices.Contains(store.Decisions, v) {
			decide[c.Key] = v
		}
	}
	res, err := srv.store.ImportFiles(r.Context(), files, decide)
	if err != nil {
		return err
	}
	if err := srv.store.DeleteUpload(r.Context(), id); err != nil {
		return err
	}
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{Imported: res})
}

func (srv *Server) adminCancelImport(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := srv.store.DeleteUpload(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	return redirect(w, r, "/admin#import", "Import cancelled. Nothing was changed.")
}

func (srv *Server) adminCreateToken(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token, err := srv.store.CreateAPIToken(r.Context(), r.PostFormValue("name"))
	if err != nil {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: err.Error()})
	}
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{NewToken: token})
}

func (srv *Server) adminDeleteToken(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.DeleteAPIToken(r.Context(), id); err != nil {
		return err
	}
	return redirect(w, r, "/admin", "Token revoked.")
}

type usersData struct {
	Users  []*store.User
	Link   string // invite or reset link to pass on
	For    *store.User
	Error  string
	App    string // name of the external account server
	Models []string
}

func (srv *Server) renderUsers(w http.ResponseWriter, r *http.Request, u *store.User, status int, d usersData) error {
	var err error
	if d.Users, err = srv.store.Users(r.Context()); err != nil {
		return err
	}
	if srv.external != nil {
		d.App = srv.external.Name
	}
	d.Models = AIModels
	return srv.render(w, r, status, "users", u, "Users", d)
}

func (srv *Server) adminUsers(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{})
}

func (srv *Server) baseURL(r *http.Request) string {
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + srv.Base
}

func (srv *Server) adminCreateUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	nu, err := srv.store.CreateUser(r.Context(), r.PostFormValue("email"), r.PostFormValue("name"), r.PostFormValue("role"))
	if err != nil {
		return srv.renderUsers(w, r, u, http.StatusUnprocessableEntity, usersData{Error: err.Error()})
	}
	token, err := srv.store.CreateUserToken(r.Context(), nu.ID)
	if err != nil {
		return err
	}
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{Link: srv.baseURL(r) + "/invite/" + token, For: nu})
}

// AIModels are offered when adding an AI user; any other OpenRouter model
// name works too (openrouter.ai/models).
var AIModels = []string{"anthropic/claude-opus-5.5", "anthropic/claude-sonnet-5.5", "openai/gpt-5.5", "google/gemini-3.8-flash", "deepseek/deepseek-v4-pro"}

func (srv *Server) adminCreateAIUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if _, err := srv.store.CreateAIUser(r.Context(), r.PostFormValue("name"), r.PostFormValue("model")); err != nil {
		return srv.renderUsers(w, r, u, http.StatusUnprocessableEntity, usersData{Error: err.Error()})
	}
	return redirect(w, r, "/admin/users", "AI user added.")
}

func (srv *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	target, err := srv.store.User(r.Context(), id)
	if err != nil {
		return err
	}
	role := r.PostFormValue("role")
	if target.IsAI() {
		role = "translator"
		err = srv.store.SetAIModel(r.Context(), id, r.PostFormValue("ai_model"))
	}
	if err == nil {
		err = srv.store.UpdateUser(r.Context(), id, r.PostFormValue("name"), role,
			r.PostFormValue("disabled") == "1", r.PostFormValue("unlisted") == "1")
	}
	if err != nil {
		return srv.renderUsers(w, r, u, http.StatusUnprocessableEntity, usersData{Error: err.Error()})
	}
	return redirect(w, r, "/admin/users", "User saved.")
}

func (srv *Server) adminUserLink(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	target, err := srv.store.User(r.Context(), id)
	if err != nil {
		return err
	}
	if target.IsAI() {
		return badRequest("AI users can't sign in.")
	}
	token, err := srv.store.CreateUserToken(r.Context(), id)
	if err != nil {
		return err
	}
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{Link: srv.baseURL(r) + "/invite/" + token, For: target})
}

type mergeData struct {
	From                    *store.User
	Others                  []*store.User
	Into                    int64 // preselected after an error
	Translations, Approvals int
	App                     string
	Error                   string
}

func (srv *Server) renderMerge(w http.ResponseWriter, r *http.Request, u *store.User, status int, msg string) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d := mergeData{Error: msg}
	d.Into, _ = strconv.ParseInt(r.PostFormValue("into"), 10, 64)
	var err error
	if d.From, err = srv.store.User(r.Context(), id); err != nil {
		return err
	}
	if d.Translations, d.Approvals, err = srv.store.Contributions(r.Context(), id); err != nil {
		return err
	}
	users, err := srv.store.Users(r.Context())
	if err != nil {
		return err
	}
	for _, o := range users {
		if o.ID != id {
			d.Others = append(d.Others, o)
		}
	}
	if srv.external != nil {
		d.App = srv.external.Name
	}
	return srv.render(w, r, status, "merge", u, "Merge "+d.From.Name, d)
}

func (srv *Server) adminMergePage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderMerge(w, r, u, http.StatusOK, "")
}

func (srv *Server) adminMerge(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	into, _ := strconv.ParseInt(r.PostFormValue("into"), 10, 64)
	from, err := srv.store.User(r.Context(), id)
	if err != nil {
		return err
	}
	target, err := srv.store.User(r.Context(), into)
	if errors.Is(err, store.ErrNotFound) {
		return srv.renderMerge(w, r, u, http.StatusUnprocessableEntity, "Choose the user to merge into.")
	} else if err != nil {
		return err
	}
	// Merging deletes the user, which would sign the admin out mid-way.
	if id == u.ID {
		return srv.renderMerge(w, r, u, http.StatusUnprocessableEntity, "You can't merge your own account. Merge the other one into it instead.")
	}
	if err := srv.store.MergeUser(r.Context(), id, into); err != nil {
		return srv.renderMerge(w, r, u, http.StatusUnprocessableEntity, err.Error())
	}
	return redirect(w, r, fmt.Sprintf("/admin/users#user-%d", into), "Merged "+from.Name+" into "+target.Name+".")
}

// Export

// exportLang maps "intl_de.arb" to "de" if the prefix matches and the
// language exists (or is the source language).
func (srv *Server) exportLang(r *http.Request, file string) (string, map[string]string, error) {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return "", nil, err
	}
	lang, ok := strings.CutPrefix(strings.TrimSuffix(file, ".arb"), set["file_prefix"])
	if !ok || !strings.HasSuffix(file, ".arb") || !arb.ValidLocale(lang) {
		return "", nil, notFound()
	}
	if lang != set["source_lang"] {
		if err := srv.requireLang(r.Context(), lang); err != nil {
			return "", nil, err
		}
	}
	return lang, set, nil
}

func (srv *Server) writeARB(w http.ResponseWriter, r *http.Request, file string, attachment bool) error {
	lang, _, err := srv.exportLang(r, file)
	if err != nil {
		return err
	}
	f, err := srv.store.Export(r.Context(), lang)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if attachment {
		w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	}
	_, err = w.Write(arb.Write(f))
	return err
}

func (srv *Server) writeZip(w http.ResponseWriter, r *http.Request) error {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	langs, err := srv.store.Languages(r.Context())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, lang := range langs {
		f, err := srv.store.Export(r.Context(), lang)
		if err != nil {
			return err
		}
		fw, err := zw.Create(set["file_prefix"] + lang + ".arb")
		if err != nil {
			return err
		}
		fw.Write(arb.Write(f))
	}
	if err := zw.Close(); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportName(set["name"])+`.zip"`)
	_, err = w.Write(buf.Bytes())
	return err
}

func exportName(name string) string {
	s := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "strings"
	}
	return s
}

func (srv *Server) exportFile(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.writeARB(w, r, r.PathValue("file"), true)
}

func (srv *Server) exportZip(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.writeZip(w, r)
}

// API

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func readARB(w http.ResponseWriter, r *http.Request) (*arb.File, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, false
	}
	f, err := arb.Parse(data)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return f, true
}

func (srv *Server) apiSource(w http.ResponseWriter, r *http.Request) {
	f, ok := readARB(w, r)
	if !ok {
		return
	}
	res, err := srv.store.ImportSource(r.Context(), f, nil)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"revision": res.Revision, "added": res.Added, "changed": res.Changed, "removed": res.Removed, "unchanged": res.Unchanged})
}

func (srv *Server) apiTranslations(w http.ResponseWriter, r *http.Request) {
	lang := r.PathValue("lang")
	if !arb.ValidLocale(lang) {
		jsonError(w, http.StatusBadRequest, "invalid language code")
		return
	}
	f, ok := readARB(w, r)
	if !ok {
		return
	}
	res, err := srv.store.ImportTranslations(r.Context(), lang, f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added": res.Added, "updated": res.Updated, "unchanged": res.Unchanged,
		"skipped": nonNil(res.Skipped), "unknown": nonNil(res.Unknown), "invalid": nonNil(res.Invalid),
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (srv *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	if err := srv.writeARB(w, r, r.PathValue("file"), false); err != nil {
		jsonError(w, http.StatusNotFound, "no such file")
	}
}

func (srv *Server) apiExportZip(w http.ResponseWriter, r *http.Request) {
	if err := srv.writeZip(w, r); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

func (srv *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	stats, err := srv.store.Stats(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]map[string]int{}
	for _, s := range stats {
		out[s.Lang] = map[string]int{"total": s.Total, "done": s.Done, "outdated": s.Outdated, "missing": s.Missing, "human": s.Human, "approved": s.Approved}
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) apiContributors(w http.ResponseWriter, r *http.Request) {
	c, err := srv.store.Contributors(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}
