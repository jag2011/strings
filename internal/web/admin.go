package web

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

type adminData struct {
	Languages  []string
	Files      []store.File
	SourcePath string // of the first file, for the API example
	Tokens     []store.APIToken
	NewToken   string
	Imported   []store.ImportResult
	Error      string
}

func (srv *Server) renderAdmin(w http.ResponseWriter, r *http.Request, u *store.User, status int, d adminData) error {
	var err error
	if d.Languages, err = srv.store.Languages(r.Context()); err != nil {
		return err
	}
	if d.Files, err = srv.store.FileStats(r.Context()); err != nil {
		return err
	}
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	d.SourcePath = "lib/l10n/intl_" + set["source_lang"] + ".arb"
	if len(d.Files) > 0 {
		d.SourcePath = d.Files[0].Path(set["source_lang"])
	}
	if d.Tokens, err = srv.store.APITokens(r.Context()); err != nil {
		return err
	}
	return srv.render(w, r, status, "admin", u, "Settings", d)
}

func (srv *Server) adminPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.renderAdmin(w, r, u, http.StatusOK, adminData{})
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (srv *Server) adminSettings(w http.ResponseWriter, r *http.Request, u *store.User) error {
	m := map[string]string{
		"name":        strings.TrimSpace(r.PostFormValue("name")),
		"accent":      r.PostFormValue("accent"),
		"source_lang": strings.TrimSpace(r.PostFormValue("source_lang")),
	}
	var msg string
	switch {
	case m["name"] == "":
		msg = "The name can't be empty."
	case !colorRe.MatchString(m["accent"]):
		msg = "The accent color must look like #2f6fde."
	case !arb.ValidLocale(m["source_lang"]):
		msg = "The source language must be a locale like en or en_US."
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

// adminUpdateFile changes a file's path template and name.
func (srv *Server) adminUpdateFile(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := srv.store.UpdateFile(r.Context(), id, r.PostFormValue("path"), r.PostFormValue("name"))
	var pe *store.PathError
	if errors.As(err, &pe) {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: "Not saved: " + pe.Msg + "."})
	} else if err != nil {
		return err
	}
	return redirect(w, r, "/admin#files", "File saved.")
}

func (srv *Server) adminMoveFile(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := srv.store.MoveFile(r.Context(), id, strings.HasSuffix(r.URL.Path, "/up")); err != nil {
		return err
	}
	return redirect(w, r, "/admin#files", "")
}

// adminDeleteFile deletes a file nobody has worked on, like a mistaken upload.
func (srv *Server) adminDeleteFile(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f, err := srv.store.File(r.Context(), id)
	if err != nil {
		return err
	}
	if err := srv.store.DeleteFile(r.Context(), id); err != nil {
		return err
	}
	return redirect(w, r, "/admin#files", "Deleted the file "+f.Name+".")
}

const maxUpload = 32 << 20

// readUpload returns the uploaded files by path: .arb files by their file
// name, since browsers send no folders, and the .arb files in a zip by
// their paths in it.
func readUpload(fhs []*multipart.FileHeader) (map[string][]byte, error) {
	files := map[string][]byte{}
	var total uint64
	for _, fh := range fhs {
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		name := path.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		if !strings.HasSuffix(strings.ToLower(name), ".zip") {
			files[name] = data
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, badRequest(name + " is not a zip file.")
		}
		for _, zf := range zr.File {
			// Leaves out folders, other files and the "._" files macOS adds.
			p := zf.Name
			if zf.FileInfo().IsDir() || !strings.HasSuffix(p, ".arb") || strings.HasPrefix(p, "__MACOSX/") || strings.HasPrefix(path.Base(p), ".") {
				continue
			}
			if total += zf.UncompressedSize64; total > maxUpload {
				return nil, badRequest("The files in " + name + " are too large.")
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, badRequest(name + " could not be read: " + err.Error())
			}
			data, err := io.ReadAll(io.LimitReader(rc, maxUpload))
			rc.Close()
			if err != nil {
				return nil, badRequest(name + " could not be read: " + err.Error())
			}
			files[p] = data
		}
	}
	return files, nil
}

// adminImport imports uploaded files. If they contain source files, the
// upload is kept and the changes are shown for review first.
func (srv *Server) adminImport(w http.ResponseWriter, r *http.Request, u *store.User) error {
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return badRequest("The upload could not be read.")
	}
	files, err := readUpload(r.MultipartForm.File["files"])
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return srv.renderAdmin(w, r, u, http.StatusUnprocessableEntity, adminData{Error: "Choose one or more .arb files, or a zip with them."})
	}
	diffs, err := srv.store.PreviewFiles(r.Context(), files)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
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
	Diffs  []store.FileDiff
	Others []string // translation files imported along with the sources
	State  string   // see reviewState
	Error  string
}

// reviewState sums up the files and revisions a review shows, so that
// confirming notices a source imported in the meantime.
func reviewState(diffs []store.FileDiff) string {
	var parts []string
	for _, d := range diffs {
		if d.New() {
			parts = append(parts, "new:"+d.File.Template)
		} else {
			parts = append(parts, fmt.Sprintf("%d:%d", d.File.ID, d.Diff.Revision))
		}
	}
	return strings.Join(parts, ",")
}

func (srv *Server) renderReview(w http.ResponseWriter, r *http.Request, u *store.User, status int, id, msg string) error {
	files, err := srv.store.Upload(r.Context(), id)
	if err != nil {
		return err
	}
	d := reviewData{ID: id, Error: msg}
	if d.Diffs, err = srv.store.PreviewFiles(r.Context(), files); err != nil {
		return err
	}
	if len(d.Diffs) == 0 {
		return notFound()
	}
	d.State = reviewState(d.Diffs)
	for name := range files {
		if !slices.ContainsFunc(d.Diffs, func(x store.FileDiff) bool { return x.Path == path.Clean(name) }) {
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

// decisionField names the form field of a changed string's decision.
func decisionField(file int64, key string) string { return fmt.Sprintf("t.%d.%s", file, key) }

func (srv *Server) adminConfirmImport(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id := r.PathValue("id")
	files, err := srv.store.Upload(r.Context(), id)
	if err != nil {
		return err
	}
	diffs, err := srv.store.PreviewFiles(r.Context(), files)
	if err != nil {
		return err
	}
	if len(diffs) == 0 {
		return notFound()
	}
	// The choices were made for the sources as they were when the page was shown.
	if r.PostFormValue("state") != reviewState(diffs) {
		return srv.renderReview(w, r, u, http.StatusConflict, id,
			"A source was imported by someone else in the meantime. Here are the changes against the new state.")
	}
	decide := map[int64]map[string]string{}
	for _, d := range diffs {
		for _, c := range d.Diff.Changed {
			if v := r.PostFormValue(decisionField(d.File.ID, c.Key)); slices.Contains(store.Decisions, v) && !d.New() {
				if decide[d.File.ID] == nil {
					decide[d.File.ID] = map[string]string{}
				}
				decide[d.File.ID][c.Key] = v
			}
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
	Models []ai.Model
}

func (srv *Server) renderUsers(w http.ResponseWriter, r *http.Request, u *store.User, status int, d usersData) error {
	var err error
	if d.Users, err = srv.store.Users(r.Context()); err != nil {
		return err
	}
	if srv.external != nil {
		d.App = srv.external.Name
	}
	d.Models, _ = srv.aiModels(r.Context())
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

// AIModels are offered for AI users when OpenRouter's model list can't be
// loaded; any other OpenRouter model name works too.
var AIModels = []ai.Model{
	{ID: "anthropic/claude-opus-5.5", Name: "Anthropic: Claude Opus 5.5"},
	{ID: "anthropic/claude-sonnet-5.5", Name: "Anthropic: Claude Sonnet 5.5"},
	{ID: "openai/gpt-5.5", Name: "OpenAI: GPT-5.5"},
	{ID: "google/gemini-3.8-flash", Name: "Google: Gemini 3.8 Flash"},
	{ID: "deepseek/deepseek-v4-pro", Name: "DeepSeek: DeepSeek V4 Pro"},
}

// aiModels returns OpenRouter's models usable for AI users (live), or
// AIModels if that list isn't available. The wait is limited, since the
// first load blocks the page.
func (srv *Server) aiModels(ctx context.Context) (models []ai.Model, live bool) {
	if srv.Models != nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if ms := srv.Models.Models(ctx); ms != nil {
			return ms, true
		}
	}
	return AIModels, false
}

// checkAIModel rejects models missing from OpenRouter's list, like typos.
// Without the list, any name passes.
func (srv *Server) checkAIModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	ms, live := srv.aiModels(ctx)
	if !live || model == "" || slices.ContainsFunc(ms, func(m ai.Model) bool { return m.ID == model }) {
		return nil
	}
	return fmt.Errorf("OpenRouter has no model %q that can call tools; pick one from the list", model)
}

func (srv *Server) adminCreateAIUser(w http.ResponseWriter, r *http.Request, u *store.User) error {
	err := srv.checkAIModel(r.Context(), r.PostFormValue("model"))
	if err == nil {
		_, err = srv.store.CreateAIUser(r.Context(), r.PostFormValue("name"), r.PostFormValue("model"))
	}
	if err != nil {
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
		// A model no longer listed can stay; only a changed one is checked.
		if model := strings.TrimSpace(r.PostFormValue("ai_model")); model != target.AIModel {
			err = srv.checkAIModel(r.Context(), model)
		}
		if err == nil {
			err = srv.store.SetAIModel(r.Context(), id, r.PostFormValue("ai_model"))
		}
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

// adminDeleteUserVersions deletes a user's translations and suggestions, in
// all languages or the chosen one, from their profile page.
func (srv *Server) adminDeleteUserVersions(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := srv.store.User(r.Context(), id); err != nil {
		return err
	}
	lang, where := "", "in all languages"
	if r.PostFormValue("scope") == "lang" {
		if lang = r.PostFormValue("lang"); lang == "" {
			return badRequest("Choose a language.")
		}
		where = "in " + langName(lang)
	}
	versions, suggestions, err := srv.store.DeleteUserVersions(r.Context(), id, lang, r.PostFormValue("approved") == "1")
	if err != nil {
		return err
	}
	return redirect(w, r, "/u/"+strconv.FormatInt(id, 10), fmt.Sprintf("Deleted %d %s and %d %s %s.",
		versions, plural(versions, "version", "versions"), suggestions, plural(suggestions, "suggestion", "suggestions"), where))
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
