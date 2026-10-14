package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	res, err := srv.store.ImportFiles(r.Context(), files)
	if err != nil {
		return err
	}
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{Imported: res})
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
	Users []*store.User
	Link  string // invite or reset link to pass on
	For   *store.User
	Error string
}

func (srv *Server) renderUsers(w http.ResponseWriter, r *http.Request, u *store.User, status int, d usersData) error {
	var err error
	if d.Users, err = srv.store.Users(r.Context()); err != nil {
		return err
	}
	return srv.render(w, r, status, "users", u, "Users", d)
}

func (srv *Server) adminUsers(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{})
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
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
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{Link: baseURL(r) + "/invite/" + token, For: nu})
}

func (srv *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := srv.store.UpdateUser(r.Context(), id, r.PostFormValue("name"), r.PostFormValue("role"), r.PostFormValue("disabled") == "1")
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
	token, err := srv.store.CreateUserToken(r.Context(), id)
	if err != nil {
		return err
	}
	return srv.renderUsers(w, r, u, http.StatusOK, usersData{Link: baseURL(r) + "/invite/" + token, For: target})
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
	res, err := srv.store.ImportSource(r.Context(), f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": res.Added, "changed": res.Changed, "removed": res.Removed, "unchanged": res.Unchanged})
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
		out[s.Lang] = map[string]int{"total": s.Total, "done": s.Done, "outdated": s.Outdated, "missing": s.Missing, "human": s.Human}
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
