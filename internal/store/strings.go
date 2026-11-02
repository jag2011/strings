package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
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

// diffSource compares f with the source strings of a file, or of a new file
// for file 0.
func diffSource(ctx context.Context, q querier, file int64, f *arb.File) (*SourceDiff, error) {
	type old struct {
		text, hash, meta string
		position         int
		draft            bool
	}
	existing := map[string]old{} // keys not removed
	rows, err := q.QueryContext(ctx, "SELECT key, text, hash, coalesce(meta, ''), position, draft FROM source_strings WHERE file_id = ? AND removed = 0", file)
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
	if file != 0 {
		if err := q.QueryRowContext(ctx, "SELECT revision FROM files WHERE id = ?", file).Scan(&d.Revision); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
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
				WHERE t.file_id = ? AND t.key = ?`, file, m.Key).Scan(&c.Translations); err != nil {
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

// PreviewSource returns what ImportSource would change in a file, without
// changing it. For file 0, a new file, everything is added.
func (s *Store) PreviewSource(ctx context.Context, file int64, f *arb.File) (*SourceDiff, error) {
	return diffSource(ctx, s.r, file, f)
}

// ImportSource makes f the source strings of a file. Keys missing from f are
// marked removed: they are kept for history but no longer exported. Other
// files aren't touched. decide says per changed key what happens to its
// translations; keys not in it (or a nil map) get OutdateTranslations. The
// file's revision goes up if anything changed.
func (s *Store) ImportSource(ctx context.Context, file int64, f *arb.File, decide map[string]string) (SourceResult, error) {
	var res SourceResult
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		res, err = importSource(ctx, tx, file, f, decide)
		return err
	})
	return res, err
}

// ImportNewSource creates a file, as Target.File of a new target describes
// it, and imports its source strings.
func (s *Store) ImportNewSource(ctx context.Context, file File, f *arb.File) (File, SourceResult, error) {
	var res SourceResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		id, err := createFile(ctx, tx, file)
		if err != nil {
			return err
		}
		res, err = importSource(ctx, tx, id, f, nil)
		file.ID = id
		return err
	})
	if err != nil {
		return file, res, err
	}
	file, err = s.File(ctx, file.ID)
	return file, res, err
}

func importSource(ctx context.Context, tx *sql.Tx, file int64, f *arb.File, decide map[string]string) (SourceResult, error) {
	var res SourceResult
	if file == 0 {
		return res, ErrNotFound
	}
	d, err := diffSource(ctx, tx, file, f)
	if err != nil {
		return res, err
	}
	res.Added, res.Changed, res.Removed, res.Unchanged = len(d.Added), len(d.Changed), len(d.Removed), d.Unchanged
	res.Revision = d.Revision

	for i, m := range f.Messages {
		var meta any
		if len(m.Meta) > 0 {
			meta = string(m.Meta)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_strings (file_id, key, text, hash, meta, draft, position, removed) VALUES (?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT (file_id, key) DO UPDATE SET
				text = excluded.text, hash = excluded.hash, meta = excluded.meta, draft = excluded.draft,
				position = excluded.position, removed = 0,
				updated_at = CASE WHEN source_strings.hash != excluded.hash OR source_strings.removed
					THEN datetime('now') ELSE source_strings.updated_at END`,
			file, m.Key, m.Text, sourceHash(m.Text), meta, arb.Draft(m.Meta), i); err != nil {
			return res, err
		}
	}
	for _, k := range d.Removed {
		if _, err := tx.ExecContext(ctx, "UPDATE source_strings SET removed = 1, updated_at = datetime('now') WHERE file_id = ? AND key = ?", file, k); err != nil {
			return res, err
		}
	}
	for _, c := range d.Changed {
		var err error
		switch decide[c.Key] {
		case KeepTranslations:
			// Only translations that were up to date; outdated ones stay outdated.
			res.Kept++
			_, err = tx.ExecContext(ctx, "UPDATE translations SET source_hash = ? WHERE file_id = ? AND key = ? AND source_hash = ?",
				c.NewHash, file, c.Key, c.OldHash)
		case RemoveTranslations:
			res.Cleared++
			if err = logSourceUnapproval(ctx, tx, file, c.Key); err == nil {
				_, err = tx.ExecContext(ctx, "DELETE FROM translations WHERE file_id = ? AND key = ?", file, c.Key)
			}
		default:
			// Approval was for the old source text.
			res.Outdated++
			if err = logSourceUnapproval(ctx, tx, file, c.Key); err == nil {
				_, err = tx.ExecContext(ctx, "UPDATE translations SET approved_by = NULL, approved_at = NULL WHERE file_id = ? AND key = ?", file, c.Key)
			}
		}
		if err == nil && decide[c.Key] != KeepTranslations {
			// Suggestions were made for the old source text.
			_, err = tx.ExecContext(ctx, "DELETE FROM suggestions WHERE file_id = ? AND key = ?", file, c.Key)
		}
		if err != nil {
			return res, err
		}
	}
	if d.Modified {
		res.Revision++
	}
	_, err = tx.ExecContext(ctx, "UPDATE files SET revision = ?, arb_locale = ?, trailing_newline = ? WHERE id = ?",
		res.Revision, f.Locale != "", f.TrailingNewline, file)
	return res, err
}

type TranslationResult struct {
	Added, Updated, Unchanged int
	Skipped                   []string // keys edited or approved in strings, which an import never overwrites
	Stale                     []string // texts strings had before and changed or removed since: the file is older
	Unknown                   []string // keys not in the source
	Invalid                   []string // keys failing validation, e.g. broken braces; not imported
	Empty                     []string // empty translations of non-empty source text; left missing
	Untranslated              []string // copies of the source text; left missing
	Warned                    []string // imported, but with warnings like an unused placeholder
}

// ImportTranslations seeds a language of a file from an existing ARB file.
// Imported texts count as up to date with the current source. Texts edited
// or approved in strings are never overwritten, and texts it had before
// never come back. Keys of other files are unknown.
func (s *Store) ImportTranslations(ctx context.Context, file int64, lang string, f *arb.File) (TranslationResult, error) {
	var res TranslationResult
	if !arb.ValidLocale(lang) {
		return res, fmt.Errorf("invalid language code %q", lang)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM files WHERE id = ?", file).Scan(new(int)); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO languages (code) VALUES (?)", lang); err != nil {
			return err
		}
		for _, m := range f.Messages {
			var srcText, hash string
			err := tx.QueryRowContext(ctx, "SELECT text, hash FROM source_strings WHERE file_id = ? AND key = ? AND removed = 0",
				file, m.Key).Scan(&srcText, &hash)
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
			var curText, curOrigin string
			var approved bool
			err = tx.QueryRowContext(ctx, "SELECT text, origin, approved_by IS NOT NULL FROM translations WHERE lang = ? AND file_id = ? AND key = ?",
				lang, file, m.Key).Scan(&curText, &curOrigin, &approved)
			found := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Tools fill untranslated strings with a copy of the source text, so
			// a copy counts as missing. One that was edited or approved here is
			// a confirmed translation (like "Tank" in German) and stays; one
			// from an earlier import is removed, its history kept.
			if m.Text == srcText && arb.HasText(srcText) {
				switch {
				case found && curText == m.Text && (curOrigin != "import" || approved):
					res.Unchanged++
					continue
				case found && curText == m.Text:
					if _, err := tx.ExecContext(ctx, "DELETE FROM translations WHERE lang = ? AND file_id = ? AND key = ?", lang, file, m.Key); err != nil {
						return err
					}
				}
				res.Untranslated = append(res.Untranslated, m.Key)
				continue
			}
			if found && curText == m.Text {
				res.Unchanged++
				continue
			}
			// A version from the history was replaced or removed here since,
			// e.g. when the source text changed. The file predates that.
			var stale bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM history WHERE lang = ? AND file_id = ? AND key = ? AND text = ?)",
				lang, file, m.Key, m.Text).Scan(&stale); err != nil {
				return err
			}
			if stale {
				res.Stale = append(res.Stale, m.Key)
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
			switch {
			case !found:
				res.Added++
			case curOrigin != "import" || approved:
				res.Skipped = append(res.Skipped, m.Key)
				continue
			default:
				res.Updated++
			}
			if err := write(ctx, tx, file, lang, m.Key, m.Text, hash, "import", 0); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

// write adds a history row and makes it the current translation.
func write(ctx context.Context, tx *sql.Tx, file int64, lang, key, text, hash, origin string, userID int64) error {
	uid := sql.NullInt64{Int64: userID, Valid: userID != 0}
	var hid int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO history (lang, file_id, key, text, source_hash, origin, user_id) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		lang, file, key, text, hash, origin, uid).Scan(&hid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO translations (lang, file_id, key, text, source_hash, origin, user_id, history_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (lang, file_id, key) DO UPDATE SET
			text = excluded.text, source_hash = excluded.source_hash, origin = excluded.origin,
			user_id = excluded.user_id, history_id = excluded.history_id, updated_at = datetime('now')`,
		lang, file, key, text, hash, origin, uid, hid)
	return err
}

// logApproval records an approval change of the current translation.
func logApproval(ctx context.Context, tx *sql.Tx, file int64, lang, key string, approved bool, userID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO approval_log (lang, file_id, key, approved, history_id, user_id)
		SELECT lang, file_id, key, ?, history_id, ? FROM translations WHERE lang = ? AND file_id = ? AND key = ?`,
		approved, userID, lang, file, key)
	return err
}

// logSourceUnapproval records that a source change removes the approvals of
// a key's translations.
func logSourceUnapproval(ctx context.Context, tx *sql.Tx, file int64, key string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO approval_log (lang, file_id, key, approved, history_id)
		SELECT lang, file_id, key, 0, history_id FROM translations WHERE file_id = ? AND key = ? AND approved_by IS NOT NULL`, file, key)
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
func (s *Store) Save(ctx context.Context, file int64, lang, key, text string, userID, base int64, approval Approval) (saved bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) (err error) {
		saved, err = save(ctx, tx, file, lang, key, text, "user", userID, userID, base, approval)
		return err
	})
	return saved, err
}

// save is Save for text written by author with the given origin. The
// approval is changed by actor.
func save(ctx context.Context, tx *sql.Tx, file int64, lang, key, text, origin string, author, actor, base int64, approval Approval) (saved bool, err error) {
	err = func() error {
		var hash string
		err := tx.QueryRowContext(ctx, "SELECT hash FROM source_strings WHERE file_id = ? AND key = ? AND removed = 0 AND draft = 0", file, key).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var curID int64
		var curText, curHash string
		var approved bool
		err = tx.QueryRowContext(ctx, "SELECT history_id, text, source_hash, approved_by IS NOT NULL FROM translations WHERE lang = ? AND file_id = ? AND key = ?",
			lang, file, key).Scan(&curID, &curText, &curHash, &approved)
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
			if err := logApproval(ctx, tx, file, lang, key, false, actor); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE translations SET approved_by = NULL, approved_at = NULL WHERE lang = ? AND file_id = ? AND key = ?", lang, file, key); err != nil {
				return err
			}
		}
		if !unchanged {
			saved = true
			if err := write(ctx, tx, file, lang, key, text, hash, origin, author); err != nil {
				return err
			}
		}
		if approval == Approve && (!approved || !unchanged) {
			saved = true
			if _, err := tx.ExecContext(ctx, "UPDATE translations SET approved_by = ?, approved_at = datetime('now') WHERE lang = ? AND file_id = ? AND key = ?",
				actor, lang, file, key); err != nil {
				return err
			}
			return logApproval(ctx, tx, file, lang, key, true, actor)
		}
		return nil
	}()
	return saved, err
}

// Revert makes an older version current again, as a new history entry.
func (s *Store) Revert(ctx context.Context, file int64, lang, key string, historyID, userID int64) error {
	var text string
	err := s.r.QueryRowContext(ctx, "SELECT text FROM history WHERE id = ? AND lang = ? AND file_id = ? AND key = ?",
		historyID, lang, file, key).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err = s.Save(ctx, file, lang, key, text, userID, -1, KeepApproval)
	return err
}

// ErrForbidden means the user may not do this.
var ErrForbidden = errors.New("not allowed")

// DeleteVersion removes a history entry. Users may delete their own entries,
// proofreaders and admins anyone's. Deleting the current version makes the
// newest remaining one current, without approval, or removes the translation
// when none is left. Only proofreaders and admins can delete the current
// version of an approved translation.
func (s *Store) DeleteVersion(ctx context.Context, file int64, lang, key string, historyID int64, u *User) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var author sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT user_id FROM history WHERE id = ? AND lang = ? AND file_id = ? AND key = ?",
			historyID, lang, file, key).Scan(&author)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if !u.CanApprove() && author.Int64 != u.ID {
			return ErrForbidden
		}
		var curID int64
		var approved bool
		err = tx.QueryRowContext(ctx, "SELECT history_id, approved_by IS NOT NULL FROM translations WHERE lang = ? AND file_id = ? AND key = ?",
			lang, file, key).Scan(&curID, &approved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if curID == historyID {
			if approved && !u.CanApprove() {
				return ErrApproved
			}
			res, err := tx.ExecContext(ctx, `
				UPDATE translations SET (text, source_hash, origin, user_id, history_id) = (
					SELECT text, source_hash, origin, user_id, id FROM history
					WHERE lang = ?1 AND file_id = ?4 AND key = ?2 AND id != ?3 ORDER BY id DESC LIMIT 1),
					approved_by = NULL, approved_at = NULL, updated_at = datetime('now')
				WHERE lang = ?1 AND file_id = ?4 AND key = ?2
					AND EXISTS (SELECT 1 FROM history WHERE lang = ?1 AND file_id = ?4 AND key = ?2 AND id != ?3)`,
				lang, key, historyID, file)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				if _, err := tx.ExecContext(ctx, "DELETE FROM translations WHERE lang = ? AND file_id = ? AND key = ?", lang, file, key); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM approval_log WHERE history_id = ?", historyID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM history WHERE id = ?", historyID)
		return err
	})
}

// DeleteUserVersions removes the history entries a user wrote and their
// suggestions, in one language or (lang "") in all. Approved current versions
// are kept unless approved is true. Translations losing their current version
// work as in DeleteVersion: the newest remaining version becomes current,
// without approval, or the translation is removed when none is left.
func (s *Store) DeleteUserVersions(ctx context.Context, userID int64, lang string, approved bool) (versions, suggestions int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var ids string // JSON array of the history ids to delete
		err := tx.QueryRowContext(ctx, `
			SELECT json_group_array(h.id) FROM history h
			LEFT JOIN translations t ON t.history_id = h.id
			WHERE h.user_id = ?1 AND (?2 = '' OR h.lang = ?2) AND (?3 OR t.approved_by IS NULL)`,
			userID, lang, approved).Scan(&ids)
		if err != nil {
			return err
		}
		const doomed = "(SELECT value FROM json_each(?1))"
		const rest = "FROM history h WHERE h.lang = translations.lang AND h.file_id = translations.file_id AND h.key = translations.key AND h.id NOT IN " + doomed
		if _, err := tx.ExecContext(ctx, `
			UPDATE translations SET (text, source_hash, origin, user_id, history_id) = (
				SELECT text, source_hash, origin, user_id, id `+rest+` ORDER BY h.id DESC LIMIT 1),
				approved_by = NULL, approved_at = NULL, updated_at = datetime('now')
			WHERE history_id IN `+doomed+` AND EXISTS (SELECT 1 `+rest+`)`, ids); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM translations WHERE history_id IN "+doomed, ids); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM approval_log WHERE history_id IN "+doomed, ids); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM history WHERE id IN "+doomed, ids)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		versions = int(n)
		res, err = tx.ExecContext(ctx, "DELETE FROM suggestions WHERE user_id = ?1 AND (?2 = '' OR lang = ?2)", userID, lang)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		suggestions = int(n)
		return nil
	})
	return versions, suggestions, err
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

// Unapproved counts up-to-date strings waiting for a proofreader.
func (l LangStats) Unapproved() int { return l.Done - l.Approved }

func (l LangStats) ApprovedPercent() int {
	if l.Total == 0 {
		return 100
	}
	return l.Approved * 100 / l.Total
}

// fileSQL limits a query on source_strings s to one file; file 0 is all.
func fileSQL(file int64) (string, []any) {
	if file == 0 {
		return "", nil
	}
	return " AND s.file_id = ?", []any{file}
}

// Stats counts per language, in one file or (file 0) in all. Human =
// up-to-date strings written by people (imported or edited here), as
// opposed to machine translations. Drafts are not counted.
func (s *Store) Stats(ctx context.Context, file int64) ([]LangStats, error) {
	langs, err := s.Languages(ctx)
	if err != nil {
		return nil, err
	}
	inFile, fargs := fileSQL(file)
	var total int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM source_strings s WHERE s.removed = 0 AND s.draft = 0"+inFile, fargs...).Scan(&total); err != nil {
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
		FROM translations t JOIN source_strings s ON s.file_id = t.file_id AND s.key = t.key AND s.removed = 0 AND s.draft = 0
		WHERE 1`+inFile+`
		GROUP BY t.lang`, fargs...)
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

// Filters for the string list, in tab order.
const (
	FilterTodo       = "todo"       // missing or outdated
	FilterUnapproved = "unapproved" // up to date, waiting for a proofreader
	FilterAll        = "all"
)

var Filters = []string{FilterTodo, FilterUnapproved, FilterAll}

func filterSQL(filter string) string {
	switch filter {
	case FilterTodo:
		return "(t.key IS NULL OR t.source_hash != s.hash)"
	case FilterUnapproved:
		return "t.source_hash = s.hash AND t.approved_by IS NULL"
	}
	return "1"
}

// joinTranslation joins the translations t of language ? to source strings s.
const joinTranslation = " LEFT JOIN translations t ON t.file_id = s.file_id AND t.key = s.key AND t.lang = ?"

// FilterCounts returns the number of strings matching q under each filter,
// in one file or (file 0) in all.
func (s *Store) FilterCounts(ctx context.Context, file int64, lang, q string) (map[string]int, error) {
	search, sargs := searchSQL(q)
	inFile, fargs := fileSQL(file)
	var todo, unapproved, all int
	err := s.r.QueryRowContext(ctx, `SELECT
		coalesce(sum(`+filterSQL(FilterTodo)+`), 0),
		coalesce(sum(`+filterSQL(FilterUnapproved)+`), 0),
		count(*)
		FROM source_strings s`+joinTranslation+`
		WHERE s.removed = 0 AND s.draft = 0`+inFile+search, append(append([]any{lang}, fargs...), sargs...)...).Scan(&todo, &unapproved, &all)
	return map[string]int{FilterTodo: todo, FilterUnapproved: unapproved, FilterAll: all}, err
}

// Ref names a string: its file and key.
type Ref struct {
	FileID int64
	File   string // the file's name
	Key    string
}

type Row struct {
	Ref
	Source       string
	Position     int
	Text         string // empty when missing
	Origin       string
	HistoryID    int64
	UserID       int64 // 0 if unknown
	UserName     string
	UpdatedAt    string
	Has          bool
	Outdated     bool
	Approved     bool
	ApprovedBy   string // name
	ApprovedByID int64
	ApprovedAt   string
	Suggestions  int // waiting to be picked; only counted by List, and not for approved strings
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
	SELECT s.file_id, f.name, s.key, s.text, s.position, coalesce(t.text, ''), coalesce(t.origin, ''), coalesce(t.history_id, 0),
		coalesce(t.user_id, 0), coalesce(u.name, ''), coalesce(t.updated_at, ''), t.key IS NOT NULL, coalesce(t.source_hash != s.hash, 0),
		t.approved_by IS NOT NULL, coalesce(a.name, ''), coalesce(t.approved_by, 0), coalesce(t.approved_at, '')
	FROM source_strings s
	JOIN files f ON f.id = s.file_id` + joinTranslation + `
	LEFT JOIN users u ON u.id = t.user_id
	LEFT JOIN users a ON a.id = t.approved_by`

// stringOrder lists strings by file, then in source order.
const stringOrder = " ORDER BY f.position, f.id, s.position"

func scanRow(row interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := row.Scan(&r.FileID, &r.File, &r.Key, &r.Source, &r.Position, &r.Text, &r.Origin, &r.HistoryID, &r.UserID, &r.UserName, &r.UpdatedAt,
		&r.Has, &r.Outdated, &r.Approved, &r.ApprovedBy, &r.ApprovedByID, &r.ApprovedAt)
	return r, err
}

func searchSQL(q string) (string, []any) {
	if q == "" {
		return "", nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	return ` AND (s.key LIKE ? ESCAPE '\' OR s.text LIKE ? ESCAPE '\' OR t.text LIKE ? ESCAPE '\')`, []any{like, like, like}
}

// List returns one page of strings and the number of matches, in one file
// or (file 0) in all, in file order.
func (s *Store) List(ctx context.Context, file int64, lang, filter, q string, offset, limit int) ([]Row, int, error) {
	inFile, fargs := fileSQL(file)
	where := " WHERE s.removed = 0 AND s.draft = 0 AND " + filterSQL(filter) + inFile
	search, sargs := searchSQL(q)
	args := append(append([]any{lang}, fargs...), sargs...)

	var count int
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM source_strings s`+joinTranslation+where+search, args...).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.r.QueryContext(ctx, rowSelect+where+search+stringOrder+" LIMIT ? OFFSET ?",
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
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	counts, err := s.suggestionCounts(ctx, file, lang)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].Suggestions = counts[fileKey{out[i].FileID, out[i].Key}]
	}
	return out, count, nil
}

// Next returns the string after cur that matches filter and q, in one file
// or (file 0) in all, wrapping around to the start. Its key is empty if
// nothing else matches.
func (s *Store) Next(ctx context.Context, file int64, lang, filter, q string, cur Ref) (Ref, error) {
	search, sargs := searchSQL(q)
	inFile, fargs := fileSQL(file)
	base := `SELECT s.file_id, f.name, s.key FROM source_strings s JOIN files f ON f.id = s.file_id` + joinTranslation + `
		WHERE s.removed = 0 AND s.draft = 0 AND NOT (s.file_id = ? AND s.key = ?) AND ` + filterSQL(filter) + inFile + search
	args := append(append([]any{lang, cur.FileID, cur.Key}, fargs...), sargs...)
	var r Ref
	err := s.r.QueryRowContext(ctx, base+` AND (f.position, f.id, s.position) > (
			SELECT cf.position, cf.id, cs.position FROM source_strings cs JOIN files cf ON cf.id = cs.file_id WHERE cs.file_id = ? AND cs.key = ?)`+
		stringOrder+" LIMIT 1", append(args, cur.FileID, cur.Key)...).Scan(&r.FileID, &r.File, &r.Key)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.r.QueryRowContext(ctx, base+stringOrder+" LIMIT 1", args...).Scan(&r.FileID, &r.File, &r.Key)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Ref{}, nil
	}
	return r, err
}

// KeyState is one line of the editor's key list.
type KeyState struct {
	Ref
	State    string // missing, outdated or done
	Approved bool
	Matches  bool // false for the current key when it falls outside filter and q
}

// Keys lists the strings matching filter and q in one file or (file 0) in
// all, in file and source order, plus cur even when it no longer matches,
// so the editor can show where it is.
func (s *Store) Keys(ctx context.Context, file int64, lang, filter, q string, cur Ref) ([]KeyState, error) {
	search, sargs := searchSQL(q)
	inFile, fargs := fileSQL(file)
	match := filterSQL(filter) + inFile + search
	margs := append(append([]any{}, fargs...), sargs...)
	rows, err := s.r.QueryContext(ctx, `SELECT s.file_id, f.name, s.key, t.key IS NOT NULL, coalesce(t.source_hash != s.hash, 0),
			t.approved_by IS NOT NULL, coalesce(`+match+`, 0)
		FROM source_strings s JOIN files f ON f.id = s.file_id`+joinTranslation+`
		WHERE s.removed = 0 AND s.draft = 0 AND ((s.file_id = ? AND s.key = ?) OR (`+match+`))`+stringOrder,
		append(append(append([]any{}, margs...), lang, cur.FileID, cur.Key), margs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyState
	for rows.Next() {
		var k KeyState
		var has, outdated bool
		if err := rows.Scan(&k.FileID, &k.File, &k.Key, &has, &outdated, &k.Approved, &k.Matches); err != nil {
			return nil, err
		}
		k.State = Row{Has: has, Outdated: outdated}.State()
		out = append(out, k)
	}
	return out, rows.Err()
}

// FindKey returns the strings with a key, in file order, for links that
// don't name the file.
func (s *Store) FindKey(ctx context.Context, key string) ([]Ref, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT s.file_id, f.name, s.key FROM source_strings s JOIN files f ON f.id = s.file_id
		WHERE s.key = ? AND s.removed = 0 AND s.draft = 0`+stringOrder, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.FileID, &r.File, &r.Key); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Entry struct {
	Row
	Meta        json.RawMessage
	SourceSince string // when the source text last changed
	Suggestions []Suggestion
	History     []Version
}

type Version struct {
	ID         int64
	Text       string
	Origin     string
	UserID     int64 // 0 if unknown
	UserName   string
	CreatedAt  string
	Current    bool
	ForOldText bool // written for an earlier source text
}

func (s *Store) Entry(ctx context.Context, file int64, lang, key string) (*Entry, error) {
	r, err := scanRow(s.r.QueryRowContext(ctx, rowSelect+" WHERE s.file_id = ? AND s.key = ? AND s.removed = 0 AND s.draft = 0", lang, file, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	e := &Entry{Row: r}
	var meta sql.NullString
	var hash string
	if err := s.r.QueryRowContext(ctx, "SELECT meta, updated_at, hash FROM source_strings WHERE file_id = ? AND key = ?",
		file, key).Scan(&meta, &e.SourceSince, &hash); err != nil {
		return nil, err
	}
	if meta.Valid {
		e.Meta = json.RawMessage(meta.String)
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT h.id, h.text, h.origin, coalesce(h.user_id, 0), coalesce(u.name, ''), h.created_at, h.source_hash != ?
		FROM history h LEFT JOIN users u ON u.id = h.user_id
		WHERE h.lang = ? AND h.file_id = ? AND h.key = ? ORDER BY h.id DESC LIMIT 50`, hash, lang, file, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Text, &v.Origin, &v.UserID, &v.UserName, &v.CreatedAt, &v.ForOldText); err != nil {
			return nil, err
		}
		v.Current = v.ID == r.HistoryID
		e.History = append(e.History, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	e.Suggestions, err = suggestions(ctx, s.r, file, lang, key)
	return e, err
}

// Export builds a file's ARB file for a language: keys in source order,
// missing translations left out (apps fall back to the source language),
// outdated ones included (stale beats missing). Exporting the source
// language returns the source strings with their metadata.
func (s *Store) Export(ctx context.Context, file int64, lang string) (*arb.File, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	fl, err := s.File(ctx, file)
	if err != nil {
		return nil, err
	}
	f := &arb.File{TrailingNewline: fl.TrailingNewline}
	if fl.ARBLocale {
		f.Locale = lang
	}
	var rows *sql.Rows
	if lang == set["source_lang"] {
		rows, err = s.r.QueryContext(ctx, "SELECT key, text, coalesce(meta, '') FROM source_strings WHERE file_id = ? AND removed = 0 ORDER BY position", file)
	} else {
		rows, err = s.r.QueryContext(ctx, `
			SELECT s.key, t.text, '' FROM source_strings s JOIN translations t ON t.file_id = s.file_id AND t.key = s.key AND t.lang = ?
			WHERE s.file_id = ? AND s.removed = 0 ORDER BY s.position`, lang, file)
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
	ID    int64  `json:"-"`
	Name  string `json:"name"`
	Edits int    `json:"edits"`
}

// Contributors counts, per language, the current translations each user wrote.
func (s *Store) Contributors(ctx context.Context) (map[string][]Contributor, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT t.lang, u.name, count(*) FROM translations t
		JOIN users u ON u.id = t.user_id
		JOIN source_strings s ON s.file_id = t.file_id AND s.key = t.key AND s.removed = 0
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

// TopContributors counts the current translations each listed user wrote in
// one language, or (lang "") across all languages, and returns the first
// limit users, most first.
func (s *Store) TopContributors(ctx context.Context, lang string, limit int) ([]Contributor, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT u.id, u.name, count(*) FROM translations t
		JOIN users u ON u.id = t.user_id
		JOIN source_strings s ON s.file_id = t.file_id AND s.key = t.key AND s.removed = 0
		JOIN languages l ON l.code = t.lang
		WHERE u.unlisted = 0 AND (?1 = '' OR t.lang = ?1)
		GROUP BY u.id ORDER BY count(*) DESC, u.name LIMIT ?2`, lang, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contributor
	for rows.Next() {
		var c Contributor
		if err := rows.Scan(&c.ID, &c.Name, &c.Edits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type LangCount struct {
	Lang  string
	Count int
}

// UserLanguages counts the current translations a user wrote per language,
// most first, the way TopContributors counts them.
func (s *Store) UserLanguages(ctx context.Context, userID int64) ([]LangCount, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT t.lang, count(*) FROM translations t
		JOIN source_strings s ON s.file_id = t.file_id AND s.key = t.key AND s.removed = 0
		JOIN languages l ON l.code = t.lang
		WHERE t.user_id = ?
		GROUP BY t.lang ORDER BY count(*) DESC, t.lang`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LangCount
	for rows.Next() {
		var c LangCount
		if err := rows.Scan(&c.Lang, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Activity is an edit or an approval change.
type Activity struct {
	Ref
	Kind      string // "edit", "approved" or "unapproved"
	Lang      string
	Text      string // the new text, or the text whose approval changed
	Prev      string // edits: the text before
	HasPrev   bool
	Origin    string // edits: "user" or "machine"
	UserID    int64
	UserName  string // empty for approvals removed by a source change
	CreatedAt string
	Open      bool // the string can still be opened in the editor
}

// Activity lists the most recent edits and approval changes, newest first,
// in one language or (lang "") in all current languages, by one user or
// (userID 0) by anyone. Imports are left out: one would bury everything else.
func (s *Store) Activity(ctx context.Context, lang string, userID int64, offset, limit int) ([]Activity, error) {
	where, filter := "lang IN (SELECT code FROM languages)", []any{}
	if lang != "" {
		where, filter = "lang = ?", []any{lang}
	}
	if userID != 0 {
		where += " AND user_id = ?"
		filter = append(filter, userID)
	}
	args := append(append(append([]any{}, filter...), filter...), limit, offset)
	// Approvals come after the edits they were saved with.
	rows, err := s.r.QueryContext(ctx, `
		WITH page AS (
			SELECT 0 AS src, h.id, 'edit' AS kind, h.lang, h.file_id, h.key, h.id AS history_id, h.origin, h.user_id, h.created_at
			FROM history h WHERE h.origin != 'import' AND `+where+`
			UNION ALL
			SELECT 1, a.id, iif(a.approved, 'approved', 'unapproved'), a.lang, a.file_id, a.key, a.history_id, '', a.user_id, a.created_at
			FROM approval_log a WHERE `+where+`
			ORDER BY created_at DESC, src DESC, id DESC LIMIT ? OFFSET ?
		)
		SELECT p.kind, p.lang, p.file_id, coalesce(f.name, ''), p.key, coalesce(h.text, ''), p.origin, coalesce(p.user_id, 0), coalesce(u.name, ''),
			p.created_at, coalesce(s.removed = 0 AND s.draft = 0, 0),
			iif(p.kind = 'edit', (SELECT o.text FROM history o
				WHERE o.lang = p.lang AND o.file_id = p.file_id AND o.key = p.key AND o.id < p.id ORDER BY o.id DESC LIMIT 1), NULL)
		FROM page p
		LEFT JOIN files f ON f.id = p.file_id
		LEFT JOIN history h ON h.id = p.history_id
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN source_strings s ON s.file_id = p.file_id AND s.key = p.key
		ORDER BY p.created_at DESC, p.src DESC, p.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var prev sql.NullString
		if err := rows.Scan(&a.Kind, &a.Lang, &a.FileID, &a.File, &a.Key, &a.Text, &a.Origin, &a.UserID, &a.UserName, &a.CreatedAt, &a.Open, &prev); err != nil {
			return nil, err
		}
		a.Prev, a.HasPrev = prev.String, prev.Valid
		out = append(out, a)
	}
	return out, rows.Err()
}

// ImportResult describes what ImportFiles did with each path.
type ImportResult struct {
	Path    string
	File    File // the file imported into; ID 0 if there is none
	NewFile bool // the source of a new file
	Lang    string
	Source  *SourceResult
	Trans   *TranslationResult
	Err     error
}

// ImportFiles imports ARB files by their paths relative to the repository
// root, or by their file names alone (see Resolve). Source files are
// imported first, with decide[file id] as in ImportSource; one that matches
// no file creates it. The others seed translations of their files and add
// their languages.
func (s *Store) ImportFiles(ctx context.Context, files map[string][]byte, decide map[int64]map[string]string) ([]ImportResult, error) {
	set, err := loadFileSet(ctx, s.r)
	if err != nil {
		return nil, err
	}
	var sources, others []string
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if t, err := set.resolve(p); err == nil && t.Source {
			sources = append(sources, p)
		} else {
			others = append(others, p)
		}
	}
	var out []ImportResult
	for i, p := range append(sources, others...) {
		if i == len(sources) && i > 0 {
			// Translations can belong to files the sources just created.
			if set, err = loadFileSet(ctx, s.r); err != nil {
				return nil, err
			}
		}
		r := ImportResult{Path: p}
		t, err := set.resolve(p)
		var f *arb.File
		if err == nil {
			r.Path, r.File, r.NewFile, r.Lang = t.Path, t.File, t.New(), t.Lang
			f, err = arb.Parse(files[p])
		}
		if err == nil && f.Locale != "" && f.Locale != t.Lang {
			err = fmt.Errorf("@@locale %q does not match the path", f.Locale)
		}
		if r.Err = err; err == nil {
			switch {
			case t.Source && t.New():
				var res SourceResult
				if r.File, res, r.Err = s.ImportNewSource(ctx, t.File, f); r.Err == nil {
					r.Source = &res
				}
			case t.Source:
				var res SourceResult
				if res, r.Err = s.ImportSource(ctx, t.File.ID, f, decide[t.File.ID]); r.Err == nil {
					r.Source = &res
				}
			default:
				var res TranslationResult
				res, r.Err = s.ImportTranslations(ctx, t.File.ID, t.Lang, f)
				r.Trans = &res
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// FileDiff is what importing a source file would change.
type FileDiff struct {
	Path string
	File File // ID 0 for a new file, named after the path
	Diff *SourceDiff
}

func (d FileDiff) New() bool { return d.File.ID == 0 }

// PreviewFiles returns what importing files would change in the source
// strings, one diff per source file, in path order. Source files that can't
// be imported are left out; ImportFiles reports why.
func (s *Store) PreviewFiles(ctx context.Context, files map[string][]byte) ([]FileDiff, error) {
	set, err := loadFileSet(ctx, s.r)
	if err != nil {
		return nil, err
	}
	var out []FileDiff
	for _, p := range slices.Sorted(maps.Keys(files)) {
		t, err := set.resolve(p)
		if err != nil || !t.Source {
			continue
		}
		f, err := arb.Parse(files[p])
		if err != nil || (f.Locale != "" && f.Locale != t.Lang) {
			continue
		}
		d, err := s.PreviewSource(ctx, t.File.ID, f)
		if err != nil {
			return nil, err
		}
		out = append(out, FileDiff{Path: t.Path, File: t.File, Diff: d})
	}
	return out, nil
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
