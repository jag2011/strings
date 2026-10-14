-- One database = one project. Instance-wide values live in settings.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE languages (
    code TEXT PRIMARY KEY                   -- Flutter locale as in file names: de, pt_BR, zh_Hant
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,     -- stored lowercase
    name          TEXT NOT NULL,            -- shown in history and credits
    password_hash TEXT,                     -- NULL until an invite is used
    role          TEXT NOT NULL CHECK (role IN ('admin', 'translator')),
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,            -- sha256 of the cookie value
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE user_tokens (                  -- invite and password reset links
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE api_tokens (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE source_strings (
    key        TEXT PRIMARY KEY,
    text       TEXT NOT NULL,
    hash       TEXT NOT NULL,               -- sha1(text), 12 hex chars
    meta       TEXT,                        -- the "@key" object as JSON
    position   INTEGER NOT NULL,
    removed    INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Every version ever written. Append-only.
CREATE TABLE history (
    id          INTEGER PRIMARY KEY,
    lang        TEXT NOT NULL,
    key         TEXT NOT NULL,
    text        TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    origin      TEXT NOT NULL CHECK (origin IN ('import', 'user', 'machine')),
    user_id     INTEGER REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX history_key ON history (lang, key);
CREATE INDEX history_user ON history (user_id);

-- The current version per (lang, key). Outdated = source_hash != source_strings.hash.
CREATE TABLE translations (
    lang        TEXT NOT NULL,
    key         TEXT NOT NULL REFERENCES source_strings(key),
    text        TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    origin      TEXT NOT NULL,
    user_id     INTEGER REFERENCES users(id),
    history_id  INTEGER NOT NULL REFERENCES history(id),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (lang, key)
);
