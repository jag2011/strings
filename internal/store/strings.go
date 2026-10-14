package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jag2011/strings/internal/arb"
)

func sourceHash(text string) string {
	h := sha1.Sum([]byte(text))
	return hex.EncodeToString(h[:])[:12]
}

type SourceResult struct {
	Added, Changed, Removed, Unchanged int
}

// ImportSource makes f the source strings. Keys missing from f are marked
// removed: they are kept for history but no longer exported.
func (s *Store) ImportSource(ctx context.Context, f *arb.File) (SourceResult, error) {
	var res SourceResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		existing := map[string]string{} // key -> hash, for keys not removed
		rows, err := tx.QueryContext(ctx, "SELECT key, hash, removed FROM source_strings")
		if err != nil {
			return err
		}
		for rows.Next() {
			var k, h string
			var removed bool
			if err := rows.Scan(&k, &h, &removed); err != nil {
				rows.Close()
				return err
			}
			if !removed {
				existing[k] = h
			}
		}
		rows.Close()

		for i, m := range f.Messages {
			hash := sourceHash(m.Text)
			var meta any
			if len(m.Meta) > 0 {
				meta = string(m.Meta)
			}
			switch old, ok := existing[m.Key]; {
			case !ok:
				res.Added++
			case old != hash:
				res.Changed++
			default:
				res.Unchanged++
			}
			delete(existing, m.Key)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO source_strings (key, text, hash, meta, draft, position, removed) VALUES (?, ?, ?, ?, ?, ?, 0)
				ON CONFLICT (key) DO UPDATE SET
					text = excluded.text, hash = excluded.hash, meta = excluded.meta, draft = excluded.draft,
					position = excluded.position, removed = 0,
					updated_at = CASE WHEN source_strings.hash != excluded.hash OR source_strings.removed
						THEN datetime('now') ELSE source_strings.updated_at END`,
				m.Key, m.Text, hash, meta, arb.Draft(m.Meta), i); err != nil {
				return err
			}
		}
		for k := range existing {
			res.Removed++
			if _, err := tx.ExecContext(ctx, "UPDATE source_strings SET removed = 1, updated_at = datetime('now') WHERE key = ?", k); err != nil {
				return err
			}
		}
		return setSettings(ctx, tx, map[string]string{
			"arb_locale":           boolSetting(f.Locale != ""),
			"arb_trailing_newline": boolSetting(f.TrailingNewline),
		})
	})
	return res, err
}

func boolSetting(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

type TranslationResult struct {
	Added, Updated, Unchanged int
	Skipped                   []string // keys with edits made in strings, which an import never overwrites
	Unknown                   []string // keys not in the source
	Invalid                   []string // keys failing validation, e.g. broken braces; not imported
	Empty                     []string // empty translations of non-empty source text; left missing
	Warned                    []string // imported, but with warnings like an unused placeholder
}

// ImportTranslations seeds a language from an existing ARB file. Imported
// texts count as up to date with the current source. Texts edited in strings
// are never overwritten.
func (s *Store) ImportTranslations(ctx context.Context, lang string, f *arb.File) (TranslationResult, error) {
	var res TranslationResult
	if !arb.ValidLocale(lang) {
		return res, fmt.Errorf("invalid language code %q", lang)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO languages (code) VALUES (?)", lang); err != nil {
			return err
		}
		for _, m := range f.Messages {
			var srcText, hash string
			err := tx.QueryRowContext(ctx, "SELECT text, hash FROM source_strings WHERE key = ? AND removed = 0", m.Key).Scan(&srcText, &hash)
			if errors.Is(err, sql.ErrNoRows) {
				res.Unknown = append(res.Unknown, m.Key)
				continue
			} else if err != nil {
				return err
			}
			if strings.TrimSpace(m.Text) == "" && strings.TrimSpace(srcText) != "" {
				res.Empty = append(res.Empty, m.Key)
				continue
			}
			errs, warns := arb.Check(srcText, m.Text)
			if len(errs) > 0 {
				res.Invalid = append(res.Invalid, m.Key)
				continue
			}
			if len(warns) > 0 {
				res.Warned = append(res.Warned, m.Key)
			}
			var curText, curOrigin string
			err = tx.QueryRowContext(ctx, "SELECT text, origin FROM translations WHERE lang = ? AND key = ?", lang, m.Key).Scan(&curText, &curOrigin)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				res.Added++
			case err != nil:
				return err
			case curText == m.Text:
				res.Unchanged++
				continue
			case curOrigin != "import":
				res.Skipped = append(res.Skipped, m.Key)
				continue
			default:
				res.Updated++
			}
			if err := write(ctx, tx, lang, m.Key, m.Text, hash, "import", 0); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

// write adds a history row and makes it the current translation.
func write(ctx context.Context, tx *sql.Tx, lang, key, text, hash, origin string, userID int64) error {
	uid := sql.NullInt64{Int64: userID, Valid: userID != 0}
	var hid int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO history (lang, key, text, source_hash, origin, user_id) VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		lang, key, text, hash, origin, uid).Scan(&hid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO translations (lang, key, text, source_hash, origin, user_id, history_id) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (lang, key) DO UPDATE SET
			text = excluded.text, source_hash = excluded.source_hash, origin = excluded.origin,
			user_id = excluded.user_id, history_id = excluded.history_id, updated_at = datetime('now')`,
		lang, key, text, hash, origin, uid, hid)
	return err
}

// ErrConflict means the translation changed since the editor was opened.
var ErrConflict = errors.New("changed by someone else in the meantime")

// Save stores a user's translation. base is the history id the user's editor
// showed (0 if there was none); a mismatch returns ErrConflict. base < 0
// skips the check. Saving the unchanged text of an outdated translation marks
// it up to date.
func (s *Store) Save(ctx context.Context, lang, key, text string, userID, base int64) (saved bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var hash string
		err := tx.QueryRowContext(ctx, "SELECT hash FROM source_strings WHERE key = ? AND removed = 0 AND draft = 0", key).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var curID int64
		var curText, curHash string
		err = tx.QueryRowContext(ctx, "SELECT history_id, text, source_hash FROM translations WHERE lang = ? AND key = ?",
			lang, key).Scan(&curID, &curText, &curHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if base >= 0 && curID != base {
			return ErrConflict
		}
		if curID != 0 && curText == text && curHash == hash {
			return nil
		}
		saved = true
		return write(ctx, tx, lang, key, text, hash, "user", userID)
	})
	return saved, err
}

// Revert makes an older version current again, as a new history entry.
func (s *Store) Revert(ctx context.Context, lang, key string, historyID, userID int64) error {
	var text string
	err := s.r.QueryRowContext(ctx, "SELECT text FROM history WHERE id = ? AND lang = ? AND key = ?", historyID, lang, key).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err = s.Save(ctx, lang, key, text, userID, -1)
	return err
}

type LangStats struct {
	Lang                                  string
	Total, Done, Outdated, Missing, Human int
}

func (l LangStats) Percent() int {
	if l.Total == 0 {
		return 100
	}
	return l.Done * 100 / l.Total
}

// Stats counts per language. Human = up-to-date strings written by people
// (imported or edited here), as opposed to machine translations. Drafts are
// not counted.
func (s *Store) Stats(ctx context.Context) ([]LangStats, error) {
	langs, err := s.Languages(ctx)
	if err != nil {
		return nil, err
	}
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM source_strings WHERE removed = 0 AND draft = 0").Scan(&total); err != nil {
		return nil, err
	}
	byLang := map[string]*LangStats{}
	var out []LangStats
	for _, l := range langs {
		byLang[l] = &LangStats{Lang: l, Total: total}
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT t.lang,
			coalesce(sum(t.source_hash = s.hash), 0),
			coalesce(sum(t.source_hash != s.hash), 0),
			coalesce(sum(t.source_hash = s.hash AND t.origin != 'machine'), 0)
		FROM translations t JOIN source_strings s ON s.key = t.key AND s.removed = 0 AND s.draft = 0
		GROUP BY t.lang`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		var done, outdated, human int
		if err := rows.Scan(&l, &done, &outdated, &human); err != nil {
			return nil, err
		}
		if st := byLang[l]; st != nil {
			st.Done, st.Outdated, st.Human = done, outdated, human
		}
	}
	for _, l := range langs {
		st := byLang[l]
		st.Missing = st.Total - st.Done - st.Outdated
		out = append(out, *st)
	}
	return out, rows.Err()
}

// Filters for the string list.
const (
	FilterTodo     = "todo" // missing or outdated
	FilterMissing  = "missing"
	FilterOutdated = "outdated"
	FilterDone     = "done"
	FilterAll      = "all"
)

var Filters = []string{FilterTodo, FilterMissing, FilterOutdated, FilterDone, FilterAll}

func filterSQL(filter string) string {
	switch filter {
	case FilterTodo:
		return "(t.key IS NULL OR t.source_hash != s.hash)"
	case FilterMissing:
		return "t.key IS NULL"
	case FilterOutdated:
		return "t.source_hash != s.hash"
	case FilterDone:
		return "t.source_hash = s.hash"
	}
	return "1"
}

type Row struct {
	Key, Source string
	Position    int
	Text        string // empty when missing
	Origin      string
	HistoryID   int64
	UserName    string
	UpdatedAt   string
	Has         bool
	Outdated    bool
}

func (r Row) State() string {
	switch {
	case !r.Has:
		return "missing"
	case r.Outdated:
		return "outdated"
	}
	return "done"
}

const rowSelect = `
	SELECT s.key, s.text, s.position, coalesce(t.text, ''), coalesce(t.origin, ''), coalesce(t.history_id, 0),
		coalesce(u.name, ''), coalesce(t.updated_at, ''), t.key IS NOT NULL, coalesce(t.source_hash != s.hash, 0)
	FROM source_strings s
	LEFT JOIN translations t ON t.key = s.key AND t.lang = ?
	LEFT JOIN users u ON u.id = t.user_id`

func scanRow(row interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := row.Scan(&r.Key, &r.Source, &r.Position, &r.Text, &r.Origin, &r.HistoryID, &r.UserName, &r.UpdatedAt, &r.Has, &r.Outdated)
	return r, err
}

func searchSQL(q string) (string, []any) {
	if q == "" {
		return "", nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	return ` AND (s.key LIKE ? ESCAPE '\' OR s.text LIKE ? ESCAPE '\' OR t.text LIKE ? ESCAPE '\')`, []any{like, like, like}
}

// List returns one page of strings and the number of matches.
func (s *Store) List(ctx context.Context, lang, filter, q string, offset, limit int) ([]Row, int, error) {
	where := " WHERE s.removed = 0 AND s.draft = 0 AND " + filterSQL(filter)
	search, sargs := searchSQL(q)
	args := append([]any{lang}, sargs...)

	var count int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM source_strings s
		LEFT JOIN translations t ON t.key = s.key AND t.lang = ?`+where+search, args...).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, rowSelect+where+search+" ORDER BY s.position LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, count, rows.Err()
}

// Next returns the key after position that matches filter and q, wrapping
// around to the start. Empty if nothing else matches.
func (s *Store) Next(ctx context.Context, lang, filter, q, currentKey string, position int) (string, error) {
	search, sargs := searchSQL(q)
	base := `SELECT s.key FROM source_strings s LEFT JOIN translations t ON t.key = s.key AND t.lang = ?
		WHERE s.removed = 0 AND s.draft = 0 AND s.key != ? AND ` + filterSQL(filter) + search
	args := append([]any{lang, currentKey}, sargs...)
	var key string
	err := s.r.QueryRowContext(ctx, base+" AND s.position > ? ORDER BY s.position LIMIT 1", append(args, position)...).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.r.QueryRowContext(ctx, base+" ORDER BY s.position LIMIT 1", args...).Scan(&key)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

type Entry struct {
	Row
	Meta        json.RawMessage
	SourceSince string // when the source text last changed
	History     []Version
}

type Version struct {
	ID         int64
	Text       string
	Origin     string
	UserName   string
	CreatedAt  string
	Current    bool
	ForOldText bool // written for an earlier source text
}

func (s *Store) Entry(ctx context.Context, lang, key string) (*Entry, error) {
	r, err := scanRow(s.r.QueryRowContext(ctx, rowSelect+" WHERE s.key = ? AND s.removed = 0 AND s.draft = 0", lang, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	e := &Entry{Row: r}
	var meta sql.NullString
	var hash string
	if err := s.r.QueryRowContext(ctx, "SELECT meta, updated_at, hash FROM source_strings WHERE key = ?", key).Scan(&meta, &e.SourceSince, &hash); err != nil {
		return nil, err
	}
	if meta.Valid {
		e.Meta = json.RawMessage(meta.String)
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT h.id, h.text, h.origin, coalesce(u.name, ''), h.created_at, h.source_hash != ?
		FROM history h LEFT JOIN users u ON u.id = h.user_id
		WHERE h.lang = ? AND h.key = ? ORDER BY h.id DESC LIMIT 50`, hash, lang, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Text, &v.Origin, &v.UserName, &v.CreatedAt, &v.ForOldText); err != nil {
			return nil, err
		}
		v.Current = v.ID == r.HistoryID
		e.History = append(e.History, v)
	}
	return e, rows.Err()
}

// Export builds the ARB file for a language: keys in source order, missing
// translations left out (apps fall back to the source language), outdated
// ones included (stale beats missing). Exporting the source language returns
// the source strings with their metadata.
func (s *Store) Export(ctx context.Context, lang string) (*arb.File, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	f := &arb.File{TrailingNewline: set["arb_trailing_newline"] == "1"}
	if set["arb_locale"] == "1" {
		f.Locale = lang
	}
	var rows *sql.Rows
	if lang == set["source_lang"] {
		rows, err = s.r.QueryContext(ctx, "SELECT key, text, coalesce(meta, '') FROM source_strings WHERE removed = 0 ORDER BY position")
	} else {
		rows, err = s.r.QueryContext(ctx, `
			SELECT s.key, t.text, '' FROM source_strings s JOIN translations t ON t.key = s.key AND t.lang = ?
			WHERE s.removed = 0 ORDER BY s.position`, lang)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m arb.Message
		var meta string
		if err := rows.Scan(&m.Key, &m.Text, &meta); err != nil {
			return nil, err
		}
		if meta != "" {
			m.Meta = json.RawMessage(meta)
		}
		f.Messages = append(f.Messages, m)
	}
	return f, rows.Err()
}

type Contributor struct {
	Name  string `json:"name"`
	Edits int    `json:"edits"`
}

// Contributors counts, per language, the current translations each user wrote.
func (s *Store) Contributors(ctx context.Context) (map[string][]Contributor, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT t.lang, u.name, count(*) FROM translations t
		JOIN users u ON u.id = t.user_id
		JOIN source_strings s ON s.key = t.key AND s.removed = 0
		JOIN languages l ON l.code = t.lang
		GROUP BY t.lang, u.id ORDER BY t.lang, count(*) DESC, u.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Contributor{}
	for rows.Next() {
		var lang string
		var c Contributor
		if err := rows.Scan(&lang, &c.Name, &c.Edits); err != nil {
			return nil, err
		}
		out[lang] = append(out[lang], c)
	}
	return out, rows.Err()
}

type Edit struct {
	Lang, Key, Text, CreatedAt string
	Current                    bool
}

// UserEdits lists a user's most recent edits.
func (s *Store) UserEdits(ctx context.Context, userID int64, limit int) ([]Edit, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT h.lang, h.key, h.text, h.created_at, t.history_id IS NOT NULL
		FROM history h LEFT JOIN translations t ON t.history_id = h.id
		WHERE h.user_id = ? ORDER BY h.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edit
	for rows.Next() {
		var e Edit
		if err := rows.Scan(&e.Lang, &e.Key, &e.Text, &e.CreatedAt, &e.Current); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ImportResult describes what ImportFiles did with each file.
type ImportResult struct {
	File   string
	Lang   string
	Source *SourceResult
	Trans  *TranslationResult
	Err    error
}

// ImportFiles imports ARB files named like "intl_de.arb". The file in the
// source language is imported first and sets the export file prefix; the
// others seed translations and add their languages.
func (s *Store) ImportFiles(ctx context.Context, files map[string][]byte) ([]ImportResult, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		_, li, _ := arb.SplitFilename(names[i])
		_, lj, _ := arb.SplitFilename(names[j])
		if (li == set["source_lang"]) != (lj == set["source_lang"]) {
			return li == set["source_lang"]
		}
		return names[i] < names[j]
	})
	var out []ImportResult
	for _, name := range names {
		r := ImportResult{File: name}
		prefix, lang, ok := arb.SplitFilename(name)
		var f *arb.File
		if !ok {
			r.Err = fmt.Errorf("file name must look like intl_de.arb or app_pt_BR.arb")
		} else if f, r.Err = arb.Parse(files[name]); r.Err == nil {
			r.Lang = lang
			if f.Locale != "" && f.Locale != lang {
				r.Err = fmt.Errorf("@@locale %q does not match the file name", f.Locale)
			} else if lang == set["source_lang"] {
				var res SourceResult
				if res, r.Err = s.ImportSource(ctx, f); r.Err == nil {
					r.Source = &res
					r.Err = s.SetSettings(ctx, map[string]string{"file_prefix": prefix})
				}
			} else {
				var res TranslationResult
				res, r.Err = s.ImportTranslations(ctx, lang, f)
				r.Trans = &res
			}
		}
		out = append(out, r)
	}
	return out, nil
}
