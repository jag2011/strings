package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/jag2011/strings/internal/arb"
)

// Locale stands for the language in a file's path template.
const Locale = "{locale}"

// DefaultTemplate is the path of the first file when the source is imported
// without a path, through the old API.
const DefaultTemplate = "intl_" + Locale + ".arb"

// File is a source ARB file and its translations, identified by a path
// template relative to the repository root, like lib/l10n/intl_{locale}.arb.
// Each file is a separate set of keys: the same key in two files means two
// different strings.
type File struct {
	ID              int64
	Template        string // the path template, with Locale once
	Name            string // short and unique, for URLs and labels
	Position        int    // order in lists and exports
	Revision        int    // counts source imports that changed something
	ARBLocale       bool   // exports get "@@locale" because the source file has it
	TrailingNewline bool   // exports end with a newline because the source file does

	// Filled in by FileStats only.
	Strings int  // source strings, not counting removed ones
	Used    bool // has history, suggestions or AI runs, so it can't be deleted
}

// Path returns the file's path for a language.
func (f File) Path(lang string) string { return strings.Replace(f.Template, Locale, lang, 1) }

const fileCols = "id, path, name, position, revision, arb_locale, trailing_newline"

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.Template, &f.Name, &f.Position, &f.Revision, &f.ARBLocale, &f.TrailingNewline)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

func files(ctx context.Context, q querier) ([]File, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+fileCols+" FROM files ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Files returns the files in their order.
func (s *Store) Files(ctx context.Context) ([]File, error) { return files(ctx, s.r) }

// FileStats returns the files with their string counts and whether they are
// in use.
func (s *Store) FileStats(ctx context.Context) ([]File, error) {
	out, err := s.Files(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		f := &out[i]
		if err := s.r.QueryRowContext(ctx, `
			SELECT (SELECT count(*) FROM source_strings WHERE file_id = ?1 AND removed = 0), `+fileUsed, f.ID).Scan(&f.Strings, &f.Used); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fileUsed is true if file ?1 has anything that a deletion would lose.
const fileUsed = `EXISTS (SELECT 1 FROM history WHERE file_id = ?1) OR EXISTS (SELECT 1 FROM suggestions WHERE file_id = ?1)
	OR EXISTS (SELECT 1 FROM ai_runs WHERE file_id = ?1) OR EXISTS (SELECT 1 FROM approval_log WHERE file_id = ?1)`

func (s *Store) File(ctx context.Context, id int64) (File, error) {
	return scanFile(s.r.QueryRowContext(ctx, "SELECT "+fileCols+" FROM files WHERE id = ?", id))
}

func (s *Store) FileByName(ctx context.Context, name string) (File, error) {
	return scanFile(s.r.QueryRowContext(ctx, "SELECT "+fileCols+" FROM files WHERE name = ?", name))
}

// FirstFile returns the file that comes first, which the API endpoints
// without a path refer to. ErrNotFound if there is none yet.
func (s *Store) FirstFile(ctx context.Context) (File, error) {
	return scanFile(s.r.QueryRowContext(ctx, "SELECT "+fileCols+" FROM files ORDER BY position, id LIMIT 1"))
}

// MatchPath returns the file a path belongs to and its language. See
// matchPath for how paths without a directory match.
func (s *Store) MatchPath(ctx context.Context, p string) (File, string, error) {
	fs, err := s.Files(ctx)
	if err != nil {
		return File{}, "", err
	}
	f, lang, err := matchPath(fs, p)
	if errors.Is(err, errNoMatch) {
		err = ErrNotFound
	}
	return f, lang, err
}

// matchTemplate returns the language when p is template t filled in with a
// valid locale.
func matchTemplate(t, p string) (string, bool) {
	pre, suf, ok := strings.Cut(t, Locale)
	if !ok || len(p) < len(pre)+len(suf) || !strings.HasPrefix(p, pre) || !strings.HasSuffix(p, suf) {
		return "", false
	}
	lang := p[len(pre) : len(p)-len(suf)]
	return lang, arb.ValidLocale(lang)
}

var errNoMatch = errors.New("no file matches")

// PathError says why a path or file name can't be used, for people to read.
type PathError struct{ Msg string }

func (e *PathError) Error() string { return e.Msg }

func pathErr(format string, args ...any) error { return &PathError{fmt.Sprintf(format, args...)} }

// matchPath finds the file of a path and the path's language. A path
// without a directory, like the name of a file uploaded in a browser, can
// also match a template's file name alone, as long as only one does.
func matchPath(fs []File, p string) (File, string, error) {
	for _, f := range fs {
		if lang, ok := matchTemplate(f.Template, p); ok {
			return f, lang, nil
		}
	}
	if strings.Contains(p, "/") {
		return File{}, "", errNoMatch
	}
	var found []File
	var lang string
	for _, f := range fs {
		if l, ok := matchTemplate(path.Base(f.Template), p); ok {
			found, lang = append(found, f), l
		}
	}
	switch len(found) {
	case 0:
		return File{}, "", errNoMatch
	case 1:
		return found[0], lang, nil
	}
	var ts []string
	for _, f := range found {
		ts = append(ts, f.Template)
	}
	return File{}, "", pathErr("the name %s fits several files (%s); upload a zip with the folders instead",
		p, strings.Join(ts, ", "))
}

// fileSet is what checking and resolving paths needs.
type fileSet struct {
	files      []File
	sourceLang string
	langs      []string // for overlap checks: the languages and some samples
}

// sampleLocales cover the shapes of locale codes, for overlap checks
// before a language exists.
var sampleLocales = []string{"en", "de", "fil", "pt_BR", "es_419", "zh_Hant", "zh_Hant_TW"}

func loadFileSet(ctx context.Context, q querier) (fileSet, error) {
	var set fileSet
	var err error
	if set.files, err = files(ctx, q); err != nil {
		return set, err
	}
	set.sourceLang = defaults["source_lang"]
	err = q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'source_lang'").Scan(&set.sourceLang)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return set, err
	}
	rows, err := q.QueryContext(ctx, "SELECT code FROM languages")
	if err != nil {
		return set, err
	}
	defer rows.Close()
	set.langs = append(append(set.langs, sampleLocales...), set.sourceLang)
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return set, err
		}
		set.langs = append(set.langs, c)
	}
	return set, rows.Err()
}

var fileNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

func checkTemplate(t string) error {
	switch {
	case t == "":
		return pathErr("the path is empty")
	case strings.Count(t, Locale) != 1:
		return pathErr("the path %s needs %s once, like lib/l10n/intl_%s.arb", t, Locale, Locale)
	case !strings.HasSuffix(t, ".arb"):
		return pathErr("the path %s must end in .arb", t)
	case strings.HasPrefix(t, "/") || strings.Contains(t, `\`):
		return pathErr("the path %s must be relative to the repository root, with / between folders", t)
	case strings.ContainsAny(strings.Replace(t, Locale, "", 1), "{}") || strings.ContainsFunc(t, unicode.IsControl):
		return pathErr("the path %s may not contain braces other than %s", t, Locale)
	}
	for _, seg := range strings.Split(t, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return pathErr("the path %s may not contain empty, . or .. parts", t)
		}
	}
	return nil
}

// overlap says whether a path of template a for one of the languages
// matches template b, or the other way round.
func overlap(a, b string, langs []string) bool {
	for _, l := range langs {
		if _, ok := matchTemplate(b, strings.Replace(a, Locale, l, 1)); ok {
			return true
		}
		if _, ok := matchTemplate(a, strings.Replace(b, Locale, l, 1)); ok {
			return true
		}
	}
	return false
}

// check validates a file's path and name against the other files.
func (set fileSet) check(f File) error {
	if err := checkTemplate(f.Template); err != nil {
		return err
	}
	if !fileNameRe.MatchString(f.Name) {
		return pathErr("the name %q must be up to 40 lowercase letters, digits, _ and -", f.Name)
	}
	for _, o := range set.files {
		switch {
		case o.ID == f.ID:
		case o.Name == f.Name:
			return pathErr("the name %s is taken by %s", f.Name, o.Template)
		case o.Template == f.Template:
			return pathErr("the path %s is taken by %s", f.Template, o.Name)
		case overlap(f.Template, o.Template, set.langs):
			return pathErr("the path %s overlaps with %s of %s: some paths would fit both", f.Template, o.Template, o.Name)
		}
	}
	return nil
}

// Target is what a path in an import refers to: a language of a file, or
// the source of a new file.
type Target struct {
	Path   string
	File   File // ID 0 for a new file, with the template and name from the path
	Lang   string
	Source bool // in the source language
}

// New says whether importing the target creates a file.
func (t Target) New() bool { return t.File.ID == 0 }

// Resolve says what a path refers to. A source path that matches no file
// becomes a new file. A *PathError explains paths that can't be imported.
func (s *Store) Resolve(ctx context.Context, p string) (Target, error) {
	set, err := loadFileSet(ctx, s.r)
	if err != nil {
		return Target{Path: p}, err
	}
	return set.resolve(p)
}

// newFileName makes a name from a file name prefix: store_ → store.
func newFileName(prefix string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(prefix))
	if name = strings.Trim(name, "_-"); name == "" {
		return "app"
	}
	return name
}

func (set fileSet) resolve(p string) (Target, error) {
	t := Target{Path: p}
	p = path.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || !strings.HasSuffix(p, ".arb") {
		return t, pathErr("not a relative path to an .arb file")
	}
	t.Path = p
	f, lang, err := matchPath(set.files, p)
	if err == nil {
		t.File, t.Lang, t.Source = f, lang, lang == set.sourceLang
		return t, nil
	} else if !errors.Is(err, errNoMatch) {
		return t, err
	}
	dir, base := path.Split(p)
	prefix, lang, ok := arb.SplitFilename(base)
	if !ok {
		return t, pathErr("the file name must look like intl_de.arb or app_pt_BR.arb")
	}
	if lang != set.sourceLang {
		return t, pathErr("no file has this path; import its source file %s%s%s.arb first", dir, prefix, set.sourceLang)
	}
	t.Lang, t.Source = lang, true
	t.File = File{Template: dir + prefix + Locale + ".arb", Name: newFileName(prefix)}
	if err := set.check(t.File); err != nil {
		return t, pathErr("matches no file and can't be a new one: %v. If it is the same file, change that file's path under Settings → Files", err)
	}
	return t, nil
}

// createFile adds a file after the others.
func createFile(ctx context.Context, tx *sql.Tx, f File) (int64, error) {
	set, err := loadFileSet(ctx, tx)
	if err != nil {
		return 0, err
	}
	f.ID = 0
	if err := set.check(f); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO files (path, name, position) VALUES (?, ?, (SELECT coalesce(max(position), 0) + 1 FROM files))
		RETURNING id`, f.Template, f.Name).Scan(&id)
	return id, err
}

// UpdateFile changes the path template and name of a file.
func (s *Store) UpdateFile(ctx context.Context, id int64, template, name string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		set, err := loadFileSet(ctx, tx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(set.files, func(f File) bool { return f.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		f := set.files[i]
		f.Template, f.Name = strings.TrimSpace(template), strings.TrimSpace(name)
		if err := set.check(f); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE files SET path = ?, name = ? WHERE id = ?", f.Template, f.Name, id)
		return err
	})
}

// MoveFile moves a file one place up or down in the order.
func (s *Store) MoveFile(ctx context.Context, id int64, up bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		fs, err := files(ctx, tx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(fs, func(f File) bool { return f.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		j := i + 1
		if up {
			j = i - 1
		}
		if j < 0 || j >= len(fs) {
			return nil
		}
		fs[i], fs[j] = fs[j], fs[i]
		for n, f := range fs {
			if _, err := tx.ExecContext(ctx, "UPDATE files SET position = ? WHERE id = ?", n+1, f.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ErrFileUsed means a file has history, suggestions or AI runs, which
// deleting it would lose.
var ErrFileUsed = errors.New("the file has translations, suggestions or AI runs and can't be deleted")

// DeleteFile deletes a file and its source strings, for a mistaken upload.
// Files that have been worked on can't be deleted.
func (s *Store) DeleteFile(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var used bool
		if err := tx.QueryRowContext(ctx, "SELECT "+fileUsed, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrFileUsed
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM source_strings WHERE file_id = ?", id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM files WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
