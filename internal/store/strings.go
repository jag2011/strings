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
	"strconv"
	"strings"

	"github.com/jag2011/strings/internal/arb"
)

func sourceHash(text string) string {
	h := sha1.Sum([]byte(text))
	return hex.EncodeToString(h[:])[:12]
}

// What happens to the translations of a source string whose text changed.
const (
	KeepTranslations    = "keep"     // still up to date, e.g. after a typo fix
	OutdateTranslations = "outdated" // flagged until someone checks them
	RemoveTranslations  = "remove"   // missing again; history keeps them
)

var Decisions = []string{KeepTranslations, OutdateTranslations, RemoveTranslations}

type SourceResult struct {
	Added, Changed, Removed, Unchanged int
	Kept, Outdated, Cleared            int // changed strings by what happened to their translations
	Revision                           int // source revision after the import
}

// SourceDiff is what importing a source file would change.
type SourceDiff struct {
	Revision  int // current revision; a modifying import makes it Revision+1
	Added     []string
	Removed   []string
	Changed   []Change
	Unchanged int
	Modified  bool // texts, metadata or order differ
}

type Change struct {
	Key, Old, New    string
	OldHash, NewHash string
	Translations     int  // in the current languages
	WasDraft         bool // translations of draft text are rarely worth keeping
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func revision(ctx context.Context, q querier) (int, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'source_revision'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	n, _ := strconv.Atoi(v)
	return n, err
}

func diffSource(ctx context.Context, q querier, f *arb.File) (*SourceDiff, error) {
	type old struct {
		text, hash, meta string
		position         int
		draft            bool
	}
	existing := map[string]old{} // keys not removed
	rows, err := q.QueryContext(ctx, "SELECT key, text, hash, coalesce(meta, ''), position, draft FROM source_strings WHERE removed = 0")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var o old
		if err := rows.Scan(&k, &o.text, &o.hash, &o.meta, &o.position, &o.draft); err != nil {
			rows.Close()
			return nil, err
		}
		existing[k] = o
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	d := &SourceDiff{}
	if d.Revision, err = revision(ctx, q); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, m := range f.Messages {
		seen[m.Key] = true
		o, ok := existing[m.Key]
		hash := sourceHash(m.Text)
		switch {
		case !ok:
			d.Added = append(d.Added, m.Key)
		case o.hash != hash:
			c := Change{Key: m.Key, Old: o.text, New: m.Text, OldHash: o.hash, NewHash: hash, WasDraft: o.draft}
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM translations t JOIN languages l ON l.code = t.lang
				WHERE t.key = ?`, m.Key).Scan(&c.Translations); err != nil {
				return nil, err
			}
			d.Changed = append(d.Changed, c)
		default:
			d.Unchanged++
			if o.meta != string(m.Meta) || o.position != i {
				d.Modified = true
			}
		}
	}
	for k := range existing {
		if !seen[k] {
			d.Removed = append(d.Removed, k)
		}
	}
	sort.Strings(d.Removed)
	d.Modified = d.Modified || len(d.Added) > 0 || len(d.Changed) > 0 || len(d.Removed) > 0
	return d, nil
}

// PreviewSource returns what ImportSource would change, without changing it.
func (s *Store) PreviewSource(ctx context.Context, f *arb.File) (*SourceDiff, error) {
	return diffSource(ctx, s.r, f)
}

// ImportSource makes f the source strings. Keys missing from f are marked
// removed: they are kept for history but no longer exported. decide says per
// changed key what happens to its translations; keys not in it (or a nil
// map) get OutdateTranslations. The revision goes up if anything changed.
func (s *Store) ImportSource(ctx context.Context, f *arb.File, decide map[string]string) (SourceResult, error) {
	var res SourceResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		d, err := diffSource(ctx, tx, f)
		if err != nil {
			return err
		}
		res.Added, res.Changed, res.Removed, res.Unchanged = len(d.Added), len(d.Changed), len(d.Removed), d.Unchanged
		res.Revision = d.Revision

		for i, m := range f.Messages {
			var meta any
			if len(m.Meta) > 0 {
				meta = string(m.Meta)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO source_strings (key, text, hash, meta, draft, position, removed) VALUES (?, ?, ?, ?, ?, ?, 0)
				ON CONFLICT (key) DO UPDATE SET
					text = excluded.text, hash = excluded.hash, meta = excluded.meta, draft = excluded.draft,
					position = excluded.position, removed = 0,
					updated_at = CASE WHEN source_strings.hash != excluded.hash OR source_strings.removed
						THEN datetime('now') ELSE source_strings.updated_at END`,
				m.Key, m.Text, sourceHash(m.Text), meta, arb.Draft(m.Meta), i); err != nil {
				return err
			}
		}
		for _, k := range d.Removed {
			if _, err := tx.ExecContext(ctx, "UPDATE source_strings SET removed = 1, updated_at = datetime('now') WHERE key = ?", k); err != nil {
				return err
			}
		}
		for _, c := range d.Changed {
			var err error
			switch decide[c.Key] {
			case KeepTranslations:
				// Only translations that were up to date; outdated ones stay outdated.
				res.Kept++
				_, err = tx.ExecContext(ctx, "UPDATE translations SET source_hash = ? WHERE key = ? AND source_hash = ?", c.NewHash, c.Key, c.OldHash)
			case RemoveTranslations:
				res.Cleared++
				_, err = tx.ExecContext(ctx, "DELETE FROM translations WHERE key = ?", c.Key)
			default:
				// Approval was for the old source text.
				res.Outdated++
				_, err = tx.ExecContext(ctx, "UPDATE translations SET approved_by = NULL, approved_at = NULL WHERE key = ?", c.Key)
			}
			if err != nil {
				return err
			}
		}
		set := map[string]string{
			"arb_locale":           boolSetting(f.Locale != ""),
			"arb_trailing_newline": boolSetting(f.TrailingNewline),
		}
		if d.Modified {
			res.Revision++
			set["source_revision"] = strconv.Itoa(res.Revision)
		}
		return setSettings(ctx, tx, set)
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
	Skipped                   []string // keys edited or approved in strings, which an import never overwrites
	Unknown                   []string // keys not in the source
	Invalid                   []string // keys failing validation, e.g. broken braces; not imported
	Empty                     []string // empty translations of non-empty source text; left missing
	Warned                    []string // imported, but with warnings like an unused placeholder
}

// ImportTranslations seeds a language from an existing ARB file. Imported
// texts count as up to date with the current source. Texts edited or approved
// in strings are never overwritten.
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
			var approved bool
			err = tx.QueryRowContext(ctx, "SELECT text, origin, approved_by IS NOT NULL FROM translations WHERE lang = ? AND key = ?",
				lang, m.Key).Scan(&curText, &curOrigin, &approved)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				res.Added++
			case err != nil:
				return err
			case curText == m.Text:
				res.Unchanged++
				continue
			case curOrigin != "import" || approved:
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

// ErrApproved means the translation is approved and can't be changed.
var ErrApproved = errors.New("approved translations can't be changed")

// Approval says what Save does with the approval of a translation.
type Approval int

const (
	KeepApproval Approval = iota // an approved translation can't be changed
	Approve                      // save and approve; changing approved text approves it again
	Unapprove                    // remove the approval and save
)

// Save stores a user's translation. base is the history id the user's editor
// showed (0 if there was none); a mismatch returns ErrConflict. base < 0
// skips the check. Saving the unchanged text of an outdated translation marks
// it up to date. Changing an approved translation without Unapprove returns
// ErrApproved. saved reports whether the text or the approval changed.
func (s *Store) Save(ctx context.Context, lang, key, text string, userID, base int64, approval Approval) (saved bool, err error) {
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
		var approved bool
		err = tx.QueryRowContext(ctx, "SELECT history_id, text, source_hash, approved_by IS NOT NULL FROM translations WHERE lang = ? AND key = ?",
			lang, key).Scan(&curID, &curText, &curHash, &approved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if base >= 0 && curID != base {
			return ErrConflict
		}
		unchanged := curID != 0 && curText == text && curHash == hash
		if approved && (unchanged && approval != Unapprove) {
			return nil
		}
		if approved && approval == KeepApproval {
			return ErrApproved
		}
		if approved && approval == Unapprove {
			saved = true
			if _, err := tx.ExecContext(ctx, "UPDATE translations SET approved_by = NULL, approved_at = NULL WHERE lang = ? AND key = ?", lang, key); err != nil {
				return err
			}
		}
		if !unchanged {
			saved = true
			if err := write(ctx, tx, lang, key, text, hash, "user", userID); err != nil {
				return err
			}
		}
		if approval == Approve && (!approved || !unchanged) {
			saved = true
			_, err := tx.ExecContext(ctx, "UPDATE translations SET approved_by = ?, approved_at = datetime('now') WHERE lang = ? AND key = ?",
				userID, lang, key)
			return err
		}
		return nil
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
	_, err = s.Save(ctx, lang, key, text, userID, -1, KeepApproval)
	return err
}

type LangStats struct {
	Lang                                  string
	Total, Done, Outdated, Missing, Human int
	Approved                              int // up to date and approved; part of Done
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
			coalesce(sum(t.source_hash = s.hash AND t.origin != 'machine'), 0),
			coalesce(sum(t.source_hash = s.hash AND t.approved_by IS NOT NULL), 0)
		FROM translations t JOIN source_strings s ON s.key = t.key AND s.removed = 0 AND s.draft = 0
		GROUP BY t.lang`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		var done, outdated, human, approved int
		if err := rows.Scan(&l, &done, &outdated, &human, &approved); err != nil {
			return nil, err
		}
		if st := byLang[l]; st != nil {
			st.Done, st.Outdated, st.Human, st.Approved = done, outdated, human, approved
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
	FilterTodo       = "todo" // missing or outdated
	FilterMissing    = "missing"
	FilterOutdated   = "outdated"
	FilterDone       = "done"
	FilterUnapproved = "unapproved" // done, waiting for a proofreader
	FilterAll        = "all"
)

var Filters = []string{FilterTodo, FilterMissing, FilterOutdated, FilterDone, FilterUnapproved, FilterAll}

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
	case FilterUnapproved:
		return "t.source_hash = s.hash AND t.approved_by IS NULL"
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
	Approved    bool
	ApprovedBy  string // name
	ApprovedAt  string
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
		coalesce(u.name, ''), coalesce(t.updated_at, ''), t.key IS NOT NULL, coalesce(t.source_hash != s.hash, 0),
		t.approved_by IS NOT NULL, coalesce(a.name, ''), coalesce(t.approved_at, '')
	FROM source_strings s
	LEFT JOIN translations t ON t.key = s.key AND t.lang = ?
	LEFT JOIN users u ON u.id = t.user_id
	LEFT JOIN users a ON a.id = t.approved_by`

func scanRow(row interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := row.Scan(&r.Key, &r.Source, &r.Position, &r.Text, &r.Origin, &r.HistoryID, &r.UserName, &r.UpdatedAt, &r.Has, &r.Outdated,
		&r.Approved, &r.ApprovedBy, &r.ApprovedAt)
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
// source language is imported first, with decide as in ImportSource, and sets
// the export file prefix; the others seed translations and add their languages.
func (s *Store) ImportFiles(ctx context.Context, files map[string][]byte, decide map[string]string) ([]ImportResult, error) {
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
				if res, r.Err = s.ImportSource(ctx, f, decide); r.Err == nil {
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

// PreviewFiles returns what importing files would change in the source
// strings, and the name of the source file. The diff is nil if files hold no
// importable source file; ImportFiles reports why.
func (s *Store) PreviewFiles(ctx context.Context, files map[string][]byte) (*SourceDiff, string, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return nil, "", err
	}
	for name, data := range files {
		_, lang, ok := arb.SplitFilename(name)
		if !ok || lang != set["source_lang"] {
			continue
		}
		f, err := arb.Parse(data)
		if err != nil || (f.Locale != "" && f.Locale != lang) {
			return nil, "", nil
		}
		d, err := s.PreviewSource(ctx, f)
		return d, name, err
	}
	return nil, "", nil
}

// SaveUpload keeps files until they are imported or dropped, and returns an
// id for them. Uploads older than a day are deleted.
func (s *Store) SaveUpload(ctx context.Context, files map[string][]byte) (string, error) {
	data, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	id, _ := NewToken()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM uploads WHERE created_at < datetime('now', '-1 day')"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO uploads (id, files) VALUES (?, ?)", id, data)
		return err
	})
	return id, err
}

func (s *Store) Upload(ctx context.Context, id string) (map[string][]byte, error) {
	var data []byte
	err := s.r.QueryRowContext(ctx, "SELECT files FROM uploads WHERE id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	var files map[string][]byte
	return files, json.Unmarshal(data, &files)
}

func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM uploads WHERE id = ?", id)
	return err
}
