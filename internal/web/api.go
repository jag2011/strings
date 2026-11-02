package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

// Export

// exportTarget returns the file and language of a path, or of a file name
// alone (see store.MatchPath). The source language is included: the settings
// page offers the source files for download.
func (srv *Server) exportTarget(r *http.Request, p string) (store.File, string, error) {
	f, lang, err := srv.store.MatchPath(r.Context(), p)
	var pe *store.PathError
	switch {
	case errors.As(err, &pe):
		return f, "", badRequest(pe.Msg)
	case errors.Is(err, store.ErrNotFound):
		return f, "", notFound()
	case err != nil:
		return f, "", err
	}
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return f, "", err
	}
	if lang != set["source_lang"] {
		if err := srv.requireLang(r.Context(), lang); err != nil {
			return f, "", err
		}
	}
	return f, lang, nil
}

func (srv *Server) writeARB(w http.ResponseWriter, r *http.Request, p string, attachment bool) error {
	file, lang, err := srv.exportTarget(r, p)
	if err != nil {
		return err
	}
	f, err := srv.store.Export(r.Context(), file.ID, lang)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if attachment {
		w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(file.Path(lang))+`"`)
	}
	_, err = w.Write(arb.Write(f))
	return err
}

// writeZip sends the translated files of all languages, of one file or (0)
// of all, at their paths, so the zip unpacks at the repository root. Flat
// leaves out the folders, as the API did before there were several files;
// a name that two files share keeps its path.
func (srv *Server) writeZip(w http.ResponseWriter, r *http.Request, file int64, flat bool) error {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	langs, err := srv.store.Languages(r.Context())
	if err != nil {
		return err
	}
	files, err := srv.store.Files(r.Context())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	seen := map[string]bool{}
	for _, fl := range files {
		if file != 0 && fl.ID != file {
			continue
		}
		for _, lang := range langs {
			f, err := srv.store.Export(r.Context(), fl.ID, lang)
			if err != nil {
				return err
			}
			name := fl.Path(lang)
			if flat && !seen[path.Base(name)] {
				name = path.Base(name)
			}
			seen[name] = true
			fw, err := zw.Create(name)
			if err != nil {
				return err
			}
			fw.Write(arb.Write(f))
		}
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
	return srv.writeARB(w, r, r.PathValue("path"), true)
}

func (srv *Server) exportZip(w http.ResponseWriter, r *http.Request, u *store.User) error {
	return srv.writeZip(w, r, 0, false)
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

// apiTarget resolves a path and checks the file's @@locale against it.
func (srv *Server) apiTarget(w http.ResponseWriter, r *http.Request, p string, f *arb.File) (store.Target, bool) {
	t, err := srv.store.Resolve(r.Context(), p)
	var pe *store.PathError
	switch {
	case errors.As(err, &pe):
		jsonError(w, http.StatusBadRequest, p+": "+pe.Msg)
		return t, false
	case err != nil:
		jsonError(w, http.StatusInternalServerError, err.Error())
		return t, false
	case f.Locale != "" && f.Locale != t.Lang:
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("@@locale %q does not match the path", f.Locale))
		return t, false
	}
	return t, true
}

// firstSource is the source path of the first file, which the old
// endpoints refer to. Without files, the source creates one at
// store.DefaultTemplate.
func (srv *Server) firstSource(r *http.Request) (string, error) {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return "", err
	}
	f, err := srv.store.FirstFile(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		f, err = store.File{Template: store.DefaultTemplate}, nil
	}
	return f.Path(set["source_lang"]), err
}

func diffJSON(d *store.SourceDiff) map[string]any {
	changed := []map[string]any{}
	for _, c := range d.Changed {
		changed = append(changed, map[string]any{
			"key": c.Key, "old": c.Old, "new": c.New, "translations": c.Translations, "was_draft": c.WasDraft,
		})
	}
	return map[string]any{
		"revision": d.Revision, "modified": d.Modified, "added": nonNil(d.Added), "removed": nonNil(d.Removed),
		"changed": changed, "unchanged": d.Unchanged,
	}
}

// preview answers what importing a source path would change, without
// changing it.
func (srv *Server) preview(w http.ResponseWriter, r *http.Request, p string, withFile bool) {
	f, ok := readARB(w, r)
	if !ok {
		return
	}
	t, ok := srv.apiTarget(w, r, p, f)
	if !ok {
		return
	}
	if !t.Source {
		jsonError(w, http.StatusBadRequest, p+" is a translation, not a source file")
		return
	}
	d, err := srv.store.PreviewSource(r.Context(), t.File.ID, f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := diffJSON(d)
	if withFile {
		out["file"], out["path"], out["new_file"] = t.File.Name, t.File.Template, t.New()
	}
	writeJSON(w, http.StatusOK, out)
}

// importSource imports a source file. Translations of changed strings
// become outdated unless the keep or remove parameters name them. With
// revision, the choices were made on a preview of that revision, so a newer
// source is a conflict.
func (srv *Server) importSource(w http.ResponseWriter, r *http.Request, t store.Target, f *arb.File, withFile bool) {
	d, err := srv.store.PreviewSource(r.Context(), t.File.ID, f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	if rev := q.Get("revision"); rev != "" && rev != strconv.Itoa(d.Revision) {
		jsonError(w, http.StatusConflict, "the source is at revision "+strconv.Itoa(d.Revision)+", not "+rev)
		return
	}
	decide := map[string]string{}
	for _, v := range []string{store.KeepTranslations, store.RemoveTranslations} {
		for _, k := range q[v] {
			if !slices.ContainsFunc(d.Changed, func(c store.Change) bool { return c.Key == k }) {
				jsonError(w, http.StatusBadRequest, "not a changed string: "+k)
				return
			}
			if decide[k] != "" {
				jsonError(w, http.StatusBadRequest, "both kept and removed: "+k)
				return
			}
			decide[k] = v
		}
	}
	file := t.File
	var res store.SourceResult
	if t.New() {
		file, res, err = srv.store.ImportNewSource(r.Context(), t.File, f)
	} else {
		res, err = srv.store.ImportSource(r.Context(), t.File.ID, f, decide)
	}
	var pe *store.PathError
	if errors.As(err, &pe) {
		jsonError(w, http.StatusConflict, pe.Msg)
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{
		"revision": res.Revision, "added": res.Added, "changed": res.Changed, "removed": res.Removed, "unchanged": res.Unchanged,
		"kept": res.Kept, "outdated": res.Outdated, "cleared": res.Cleared,
	}
	if withFile {
		out["file"], out["path"], out["new_file"] = file.Name, file.Template, t.New()
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) importTranslations(w http.ResponseWriter, r *http.Request, file store.File, lang string, f *arb.File, withFile bool) {
	res, err := srv.store.ImportTranslations(r.Context(), file.ID, lang, f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{
		"added": res.Added, "updated": res.Updated, "unchanged": res.Unchanged,
		"skipped": nonNil(res.Skipped), "stale": nonNil(res.Stale), "unknown": nonNil(res.Unknown), "invalid": nonNil(res.Invalid),
	}
	if withFile {
		out["file"], out["language"] = file.Name, lang
	}
	writeJSON(w, http.StatusOK, out)
}

// apiPutFile imports a file by its path: a source file updates the source
// strings of its file, or creates the file; a translation seeds that
// language.
func (srv *Server) apiPutFile(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	f, ok := readARB(w, r)
	if !ok {
		return
	}
	t, ok := srv.apiTarget(w, r, p, f)
	if !ok {
		return
	}
	if t.Source {
		srv.importSource(w, r, t, f, true)
	} else {
		srv.importTranslations(w, r, t.File, t.Lang, f, true)
	}
}

// apiPreview is the API's review page.
func (srv *Server) apiPreview(w http.ResponseWriter, r *http.Request) {
	srv.preview(w, r, r.PathValue("path"), true)
}

func (srv *Server) apiGetFile(w http.ResponseWriter, r *http.Request) {
	srv.apiWriteARB(w, r, r.PathValue("path"))
}

func (srv *Server) apiWriteARB(w http.ResponseWriter, r *http.Request, p string) {
	err := srv.writeARB(w, r, p, false)
	var he httpError
	switch {
	case err == nil:
	case errors.As(err, &he) && he.status == http.StatusBadRequest:
		jsonError(w, he.status, he.msg)
	case errors.As(err, &he) || errors.Is(err, store.ErrNotFound):
		jsonError(w, http.StatusNotFound, "no such file")
	default:
		jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

// apiFilesZip sends all translated files at their paths, or with ?file=
// those of one file.
func (srv *Server) apiFilesZip(w http.ResponseWriter, r *http.Request) {
	var id int64
	if name := r.URL.Query().Get("file"); name != "" {
		f, err := srv.store.FileByName(r.Context(), name)
		if errors.Is(err, store.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "no file named "+name)
			return
		} else if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		id = f.ID
	}
	if err := srv.writeZip(w, r, id, false); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

func (srv *Server) apiFiles(w http.ResponseWriter, r *http.Request) {
	files, err := srv.store.FileStats(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, f := range files {
		out = append(out, map[string]any{"name": f.Name, "path": f.Template, "revision": f.Revision, "strings": f.Strings})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiSourcePreview previews the source of the first file. Deprecated.
func (srv *Server) apiSourcePreview(w http.ResponseWriter, r *http.Request) {
	p, err := srv.firstSource(r)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	srv.preview(w, r, p, false)
}

// apiSource imports the source of the first file. Deprecated.
func (srv *Server) apiSource(w http.ResponseWriter, r *http.Request) {
	p, err := srv.firstSource(r)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, ok := readARB(w, r)
	if !ok {
		return
	}
	t, ok := srv.apiTarget(w, r, p, f)
	if !ok {
		return
	}
	srv.importSource(w, r, t, f, false)
}

// apiTranslations seeds a language of the first file. Deprecated.
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
	file, err := srv.store.FirstFile(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusBadRequest, "there is no source file yet; import it first")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	srv.importTranslations(w, r, file, lang, f, false)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// apiExport sends a file found by its file name alone. Deprecated.
func (srv *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	srv.apiWriteARB(w, r, r.PathValue("file"))
}

// apiExportZip sends all translated files without folders. Deprecated.
func (srv *Server) apiExportZip(w http.ResponseWriter, r *http.Request) {
	if err := srv.writeZip(w, r, 0, true); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

func statsJSON(s store.LangStats) map[string]any {
	return map[string]any{"total": s.Total, "done": s.Done, "outdated": s.Outdated, "missing": s.Missing, "human": s.Human, "approved": s.Approved}
}

// apiStats counts per language, in total and per file.
func (srv *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	stats, err := srv.store.Stats(r.Context(), 0)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files, err := srv.store.Files(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]map[string]any{}
	perFile := map[string]map[string]any{}
	for _, s := range stats {
		out[s.Lang] = statsJSON(s)
		perFile[s.Lang] = map[string]any{}
		out[s.Lang]["files"] = perFile[s.Lang]
	}
	for _, f := range files {
		fs, err := srv.store.Stats(r.Context(), f.ID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, s := range fs {
			perFile[s.Lang][f.Name] = statsJSON(s)
		}
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
