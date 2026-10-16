package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

var ErrDisabled = errors.New("account disabled")

type User struct {
	ID          int64
	Email       string
	Name        string
	Role        string
	Disabled    bool
	Unlisted    bool // left out of the top members on the home page
	HasPassword bool
	External    bool   // signs in through the external account server
	AIModel     string // the language model of an AI user; empty for people
	CreatedAt   string
}

// Roles: proofreaders do what translators do and also approve translations.
// Admins can do everything.
var Roles = []string{"translator", "proofreader", "admin"}

func (u *User) IsAdmin() bool { return u != nil && u.Role == "admin" }

// IsAI says whether u is an AI user, which translates through a language
// model and can't sign in.
func (u *User) IsAI() bool { return u != nil && u.AIModel != "" }

// CanApprove says whether u may approve translations and remove approvals.
func (u *User) CanApprove() bool { return u != nil && (u.Role == "admin" || u.Role == "proofreader") }

const userCols = "id, email, name, role, disabled, unlisted, password_hash IS NOT NULL, external_id IS NOT NULL, ai_model, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var model sql.NullString
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Disabled, &u.Unlisted, &u.HasPassword, &u.External, &model, &u.CreatedAt)
	u.AIModel = model.String
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, email, name, role string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("a valid email address is required")
	}
	if name == "" {
		return nil, fmt.Errorf("a name is required")
	}
	if !slices.Contains(Roles, role) {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	row := s.w.QueryRowContext(ctx,
		"INSERT INTO users (email, name, role) VALUES (?, ?, ?) RETURNING "+userCols, email, name, role)
	u, err := scanUser(row)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return nil, fmt.Errorf("a user with this email already exists")
	}
	return u, err
}

// CreateAIUser adds an AI user. AI users are unlisted, since they would
// otherwise top the members list.
func (s *Store) CreateAIUser(ctx context.Context, name, model string) (*User, error) {
	name, model = strings.TrimSpace(name), strings.TrimSpace(model)
	if name == "" {
		return nil, fmt.Errorf("a name is required")
	}
	if err := checkModel(model); err != nil {
		return nil, err
	}
	token, _ := NewToken()
	email := "ai-" + strings.ToLower(token[:12]) + "@strings.invalid"
	return scanUser(s.w.QueryRowContext(ctx,
		"INSERT INTO users (email, name, role, unlisted, ai_model) VALUES (?, ?, 'translator', 1, ?) RETURNING "+userCols, email, name, model))
}

// checkModel wants an OpenRouter model name: provider/model.
func checkModel(model string) error {
	if model == "" {
		return fmt.Errorf("a model is required")
	}
	if p, m, ok := strings.Cut(model, "/"); !ok || p == "" || m == "" || strings.ContainsAny(model, " \t") {
		return fmt.Errorf("the model %q isn't an OpenRouter model name like anthropic/claude-opus-5.5", model)
	}
	return nil
}

// SetAIModel changes the model of an AI user.
func (s *Store) SetAIModel(ctx context.Context, id int64, model string) error {
	model = strings.TrimSpace(model)
	if err := checkModel(model); err != nil {
		return err
	}
	_, err := s.w.ExecContext(ctx, "UPDATE users SET ai_model = ? WHERE id = ? AND ai_model IS NOT NULL", model, id)
	return err
}

func (s *Store) User(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func (s *Store) Users(ctx context.Context) ([]*User, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, id int64, name, role string, disabled, unlisted bool) error {
	if !slices.Contains(Roles, role) {
		return fmt.Errorf("unknown role %q", role)
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a name is required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		// Names of external users follow their account.
		if _, err := tx.ExecContext(ctx, "UPDATE users SET name = iif(external_id IS NULL, ?, name), role = ?, disabled = ?, unlisted = ? WHERE id = ?",
			strings.TrimSpace(name), role, disabled, unlisted, id); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0").Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return fmt.Errorf("there must be at least one active admin")
		}
		if disabled {
			_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id)
			return err
		}
		return nil
	})
}

// Contributions counts the current translations credited to a user and the
// approvals they gave.
func (s *Store) Contributions(ctx context.Context, id int64) (translations, approvals int, err error) {
	err = s.r.QueryRowContext(ctx, `
		SELECT coalesce(sum(user_id = ?1), 0), coalesce(sum(approved_by = ?1), 0) FROM translations
		WHERE user_id = ?1 OR approved_by = ?1`, id).Scan(&translations, &approvals)
	return translations, approvals, err
}

// MergeUser moves the edits and approvals of one user to another and deletes
// the first, for people with two accounts. The remaining user takes over the
// external sign-in of the deleted one if they have none.
func (s *Store) MergeUser(ctx context.Context, from, into int64) error {
	if from == into {
		return fmt.Errorf("a user can't be merged into themselves")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var ext sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT external_id FROM users WHERE id = ?", from).Scan(&ext)
		if err == nil {
			err = tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = ?", into).Scan(new(int))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		for _, q := range []string{
			"UPDATE history SET user_id = ?1 WHERE user_id = ?2",
			"UPDATE translations SET user_id = ?1 WHERE user_id = ?2",
			"UPDATE translations SET approved_by = ?1 WHERE approved_by = ?2",
			"UPDATE approval_log SET user_id = ?1 WHERE user_id = ?2",
			"UPDATE ai_runs SET user_id = ?1 WHERE user_id = ?2",
			"UPDATE ai_runs SET started_by = ?1 WHERE started_by = ?2",
			// Of two suggestions for the same string, the newer one stays.
			`DELETE FROM suggestions WHERE user_id = ?1 AND EXISTS (SELECT 1 FROM suggestions f
				WHERE f.user_id = ?2 AND f.lang = suggestions.lang AND f.key = suggestions.key AND f.created_at >= suggestions.created_at)`,
			"UPDATE OR IGNORE suggestions SET user_id = ?1 WHERE user_id = ?2",
			"DELETE FROM suggestions WHERE user_id = ?2",
			"DELETE FROM users WHERE id = ?2",
			"UPDATE users SET external_id = coalesce(external_id, ?3) WHERE id = ?1",
		} {
			if _, err := tx.ExecContext(ctx, q, into, from, ext); err != nil {
				return err
			}
		}
		var admins int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0").Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return fmt.Errorf("there must be at least one active admin")
		}
		return nil
	})
}

// Login returns the user for valid credentials. Disabled users can't log in.
func (s *Store) Login(ctx context.Context, email, password string) (*User, error) {
	var id int64
	var hash sql.NullString
	err := s.r.QueryRowContext(ctx, "SELECT id, password_hash FROM users WHERE email = ? AND disabled = 0",
		strings.ToLower(strings.TrimSpace(email))).Scan(&id, &hash)
	if err != nil || !hash.Valid || !checkPassword(hash.String, password) {
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.User(ctx, id)
}

// ExternalSignIn returns the user for an account of the external account
// server and updates their name and email. A user with the same email is
// linked, since the sign-in link went to that address. Otherwise a translator
// is created.
func (s *Store) ExternalSignIn(ctx context.Context, externalID, email, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if externalID == "" || name == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("incomplete account: id %q, email %q, name %q", externalID, email, name)
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE external_id = ?", externalID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ? AND external_id IS NULL", email).Scan(&id)
		}
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, "INSERT INTO users (email, name, role, external_id) VALUES (?, ?, 'translator', ?) RETURNING id",
				email, name, externalID).Scan(&id)
			if err != nil && strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%s is used by another linked account", email)
			}
			return err
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET external_id = ?, name = ? WHERE id = ?", externalID, name, id); err != nil {
			return err
		}
		// Keeps the old email if another user has the new one.
		_, err = tx.ExecContext(ctx, "UPDATE OR IGNORE users SET email = ? WHERE id = ?", email, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	u, err := s.User(ctx, id)
	if err == nil && u.Disabled {
		return nil, ErrDisabled
	}
	return u, err
}

const sessionTTL = 30 * 24 * time.Hour

func (s *Store) CreateSession(ctx context.Context, userID int64) (token string, err error) {
	token, hash := NewToken()
	_, err = s.w.ExecContext(ctx, "INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)",
		hash, userID, sqlTime(time.Now().Add(sessionTTL)))
	return token, err
}

// SessionUser returns the user of a valid session and extends the session
// when it is more than a day old.
func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	hash := HashToken(token)
	var expires string
	var id int64
	err := s.r.QueryRowContext(ctx, `
		SELECT s.user_id, s.expires_at FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled = 0`, hash, sqlTime(time.Now())).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t, _ := time.Parse(time.DateTime, expires); time.Until(t) < sessionTTL-24*time.Hour {
		s.w.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE token_hash = ?", sqlTime(time.Now().Add(sessionTTL)), hash)
	}
	return s.User(ctx, id)
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ? OR expires_at < ?", HashToken(token), sqlTime(time.Now()))
	return err
}

const userTokenTTL = 7 * 24 * time.Hour

// CreateUserToken makes an invite or password reset link token. Older tokens
// of the user stop working.
func (s *Store) CreateUserToken(ctx context.Context, userID int64) (string, error) {
	token, hash := NewToken()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_tokens WHERE user_id = ? OR expires_at < ?", userID, sqlTime(time.Now())); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO user_tokens (token_hash, user_id, expires_at) VALUES (?, ?, ?)",
			hash, userID, sqlTime(time.Now().Add(userTokenTTL)))
		return err
	})
	return token, err
}

func (s *Store) UserTokenUser(ctx context.Context, token string) (*User, error) {
	return scanUser(s.r.QueryRowContext(ctx, "SELECT "+prefixed("u.", userCols)+`
		FROM user_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.expires_at > ? AND u.disabled = 0`, HashToken(token), sqlTime(time.Now())))
}

// UseUserToken sets the password, consumes the token and logs out other sessions.
func (s *Store) UseUserToken(ctx context.Context, token, password string) (*User, error) {
	if len(password) < 10 {
		return nil, fmt.Errorf("the password needs at least 10 characters")
	}
	u, err := s.UserTokenUser(ctx, token)
	if err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM user_tokens WHERE token_hash = ?", HashToken(token))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", hash, u.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", u.ID)
		return err
	})
	return u, err
}

type APIToken struct {
	ID        int64
	Name      string
	CreatedAt string
}

func (s *Store) CreateAPIToken(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("a name is required")
	}
	token, hash := NewToken()
	_, err := s.w.ExecContext(ctx, "INSERT INTO api_tokens (token_hash, name) VALUES (?, ?)", hash, strings.TrimSpace(name))
	return "st_" + token, err
}

func (s *Store) CheckAPIToken(ctx context.Context, token string) bool {
	token, ok := strings.CutPrefix(token, "st_")
	if !ok {
		return false
	}
	var n int
	s.r.QueryRowContext(ctx, "SELECT count(*) FROM api_tokens WHERE token_hash = ?", HashToken(token)).Scan(&n)
	return n > 0
}

func (s *Store) APITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, name, created_at FROM api_tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ?", id)
	return err
}

// Passwords: PBKDF2-SHA256 from the standard library, 600k iterations (OWASP 2023).
const pbkdf2Iter = 600_000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err != nil || err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func sqlTime(t time.Time) string { return t.UTC().Format(time.DateTime) }

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = p + c
	}
	return strings.Join(parts, ", ")
}
