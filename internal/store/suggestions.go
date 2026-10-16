package store

import (
	"context"
	"database/sql"
	"errors"
)

// Suggestion is an alternative translation, e.g. from an AI user, that
// someone can pick as the current one.
type Suggestion struct {
	ID        int64
	Text      string
	UserID    int64
	UserName  string
	AIModel   string // empty if a person made it
	CreatedAt string
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// visibleSuggestion is the condition for suggestions g worth showing for
// source string s and translation t: made for the current source text and
// not already the current, up-to-date translation.
const visibleSuggestion = "g.source_hash = s.hash AND NOT coalesce(t.text = g.text AND t.source_hash = s.hash, 0)"

// Suggest stores userID's suggestion for a string, replacing their previous one.
func (s *Store) Suggest(ctx context.Context, lang, key, text string, userID int64) error {
	return suggest(ctx, s.w, lang, key, text, userID)
}

func suggest(ctx context.Context, x execer, lang, key, text string, userID int64) error {
	res, err := x.ExecContext(ctx, `
		INSERT INTO suggestions (lang, key, text, source_hash, user_id)
		SELECT ?, key, ?, hash, ? FROM source_strings WHERE key = ? AND removed = 0 AND draft = 0
		ON CONFLICT (lang, key, user_id) DO UPDATE SET
			text = excluded.text, source_hash = excluded.source_hash, created_at = datetime('now')`,
		lang, text, userID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func suggestions(ctx context.Context, q querier, lang, key string) ([]Suggestion, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT g.id, g.text, g.user_id, u.name, coalesce(u.ai_model, ''), g.created_at
		FROM suggestions g
		JOIN source_strings s ON s.key = g.key
		JOIN users u ON u.id = g.user_id
		LEFT JOIN translations t ON t.lang = g.lang AND t.key = g.key
		WHERE g.lang = ? AND g.key = ? AND `+visibleSuggestion+`
		ORDER BY g.created_at, g.id`, lang, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var g Suggestion
		if err := rows.Scan(&g.ID, &g.Text, &g.UserID, &g.UserName, &g.AIModel, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// suggestionCounts counts the suggestions per key of a language, leaving out
// approved strings, which nobody needs to look at again.
func (s *Store) suggestionCounts(ctx context.Context, lang string) (map[string]int, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT g.key, count(*) FROM suggestions g
		JOIN source_strings s ON s.key = g.key
		LEFT JOIN translations t ON t.lang = g.lang AND t.key = g.key
		WHERE g.lang = ? AND t.approved_by IS NULL AND `+visibleSuggestion+`
		GROUP BY g.key`, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// UseSuggestion makes a suggestion the current translation, credited to the
// user who suggested it, and removes it from the suggestions. Proofreaders
// and admins may approve it at the same time; using a suggestion for an
// approved string removes the approval. base works as in Save.
func (s *Store) UseSuggestion(ctx context.Context, lang, key string, id int64, u *User, base int64, approve bool) error {
	if approve && !u.CanApprove() {
		return ErrForbidden
	}
	approval := KeepApproval
	switch {
	case approve:
		approval = Approve
	case u.CanApprove():
		approval = Unapprove
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var text, origin string
		var author int64
		err := tx.QueryRowContext(ctx, `
			SELECT g.text, g.user_id, iif(u.ai_model IS NULL, 'user', 'machine')
			FROM suggestions g
			JOIN source_strings s ON s.key = g.key AND s.hash = g.source_hash
			JOIN users u ON u.id = g.user_id
			WHERE g.id = ? AND g.lang = ? AND g.key = ?`, id, lang, key).Scan(&text, &author, &origin)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := save(ctx, tx, lang, key, text, origin, author, u.ID, base, approval); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM suggestions WHERE id = ?", id)
		return err
	})
}

// DismissSuggestion deletes a suggestion. Only proofreaders and admins may.
func (s *Store) DismissSuggestion(ctx context.Context, lang, key string, id int64, u *User) error {
	if !u.CanApprove() {
		return ErrForbidden
	}
	res, err := s.w.ExecContext(ctx, "DELETE FROM suggestions WHERE id = ? AND lang = ? AND key = ?", id, lang, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
