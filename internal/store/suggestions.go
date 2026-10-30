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
	Same      bool // the same as the current, up-to-date translation
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// sameSuggestion is true when suggestion g for source string s is already
// translation t, up to date.
const sameSuggestion = "coalesce(t.text = g.text AND t.source_hash = s.hash, 0)"

// pendingSuggestion is the condition for suggestions g waiting to be picked:
// made for the current source text and not the same as translation t.
const pendingSuggestion = "g.source_hash = s.hash AND NOT " + sameSuggestion

// promotableSuggestion is the condition for suggestions g that can become
// translation t of source string s without picking between them: made for the
// current source text, for a string that is missing or outdated and not approved.
const promotableSuggestion = "g.source_hash = s.hash AND (t.key IS NULL OR (t.source_hash != s.hash AND t.approved_by IS NULL))"

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

// suggestions returns the suggestions for a string made for the current
// source text, those the same as the current translation last.
func suggestions(ctx context.Context, q querier, lang, key string) ([]Suggestion, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT g.id, g.text, g.user_id, u.name, coalesce(u.ai_model, ''), g.created_at, `+sameSuggestion+` AS same
		FROM suggestions g
		JOIN source_strings s ON s.key = g.key
		JOIN users u ON u.id = g.user_id
		LEFT JOIN translations t ON t.lang = g.lang AND t.key = g.key
		WHERE g.lang = ? AND g.key = ? AND g.source_hash = s.hash
		ORDER BY same, g.created_at, g.id`, lang, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var g Suggestion
		if err := rows.Scan(&g.ID, &g.Text, &g.UserID, &g.UserName, &g.AIModel, &g.CreatedAt, &g.Same); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// suggestionCounts counts the pending suggestions per key of a language,
// leaving out approved strings, which nobody needs to look at again.
func (s *Store) suggestionCounts(ctx context.Context, lang string) (map[string]int, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT g.key, count(*) FROM suggestions g
		JOIN source_strings s ON s.key = g.key
		LEFT JOIN translations t ON t.lang = g.lang AND t.key = g.key
		WHERE g.lang = ? AND t.approved_by IS NULL AND `+pendingSuggestion+`
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

// PromoteSuggestions makes userID's suggestions the translations of the
// strings that are missing or outdated in lang, as UseSuggestion does: credited
// to userID, unapproved. It returns how many it promoted.
func (s *Store) PromoteSuggestions(ctx context.Context, lang string, userID int64) (int, error) {
	type pick struct {
		id                int64
		key, text, origin string
	}
	var picks []pick
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT g.id, g.key, g.text, iif(u.ai_model IS NULL, 'user', 'machine')
			FROM suggestions g
			JOIN source_strings s ON s.key = g.key AND s.removed = 0 AND s.draft = 0
			JOIN users u ON u.id = g.user_id
			LEFT JOIN translations t ON t.lang = g.lang AND t.key = g.key
			WHERE g.lang = ? AND g.user_id = ? AND `+promotableSuggestion+`
			ORDER BY s.position`, lang, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p pick
			if err := rows.Scan(&p.id, &p.key, &p.text, &p.origin); err != nil {
				rows.Close()
				return err
			}
			picks = append(picks, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range picks {
			if _, err := save(ctx, tx, lang, p.key, p.text, p.origin, userID, userID, -1, KeepApproval); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM suggestions WHERE id = ?", p.id); err != nil {
				return err
			}
		}
		return nil
	})
	return len(picks), err
}
