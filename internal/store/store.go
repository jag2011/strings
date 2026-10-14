// Package store keeps all data in one SQLite file. One database is one project.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Store has one connection for writes and a pool for reads. SQLite allows a
// single writer; funneling writes through one connection avoids SQLITE_BUSY.
type Store struct {
	w *sql.DB
	r *sql.DB
}

func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + q.Encode()
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	s := &Store{w: w, r: r}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.r.Close()
	return s.w.Close()
}

// migrate applies schema/NNN_*.sql files newer than PRAGMA user_version.
func (s *Store) migrate() error {
	var version int
	if err := s.w.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	files, _ := fs.Glob(schemaFS, "schema/*.sql")
	sort.Strings(files)
	for _, f := range files {
		n, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(f, "schema/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", f)
		}
		if n <= version {
			continue
		}
		body, _ := schemaFS.ReadFile(f)
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Settings with their defaults.
var defaults = map[string]string{
	"name":                 "strings",
	"accent":               "#2f6fde",
	"source_lang":          "en",
	"file_prefix":          "intl_",
	"arb_locale":           "0", // write "@@locale" into exports
	"arb_trailing_newline": "0",
}

func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	m := map[string]string{}
	for k, v := range defaults {
		m[k] = v
	}
	rows, err := s.r.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (s *Store) SetSettings(ctx context.Context, m map[string]string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return setSettings(ctx, tx, m) })
}

func setSettings(ctx context.Context, tx *sql.Tx, m map[string]string) error {
	for k, v := range m {
		if _, ok := defaults[k]; !ok {
			return fmt.Errorf("unknown setting %q", k)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Languages(ctx context.Context) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT code FROM languages ORDER BY code")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) HasLanguage(ctx context.Context, code string) (bool, error) {
	var n int
	err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM languages WHERE code = ?", code).Scan(&n)
	return n > 0, err
}

func (s *Store) AddLanguage(ctx context.Context, code string) error {
	_, err := s.w.ExecContext(ctx, "INSERT OR IGNORE INTO languages (code) VALUES (?)", code)
	return err
}

// RemoveLanguage hides a language. Its translations and history stay, so
// adding it again restores them.
func (s *Store) RemoveLanguage(ctx context.Context, code string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM languages WHERE code = ?", code)
	return err
}

// NewToken returns a random token and the hash to store for it.
func NewToken() (token, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
