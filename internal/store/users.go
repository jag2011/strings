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
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

type User struct {
	ID          int64
	Email       string
	Name        string
	Role        string
	Disabled    bool
	HasPassword bool
	CreatedAt   string
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == "admin" }

const userCols = "id, email, name, role, disabled, password_hash IS NOT NULL, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Disabled, &u.HasPassword, &u.CreatedAt)
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
	if role != "admin" && role != "translator" {
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

func (s *Store) UpdateUser(ctx context.Context, id int64, name, role string, disabled bool) error {
	if role != "admin" && role != "translator" {
		return fmt.Errorf("unknown role %q", role)
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a name is required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET name = ?, role = ?, disabled = ? WHERE id = ?",
			strings.TrimSpace(name), role, disabled, id); err != nil {
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
