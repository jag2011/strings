-- A project can have several source files, like the app strings and the
-- store listing. Each is a separate set of keys. A file is identified by a
-- path template relative to the repository root with one {locale}.
CREATE TABLE files (
    id               INTEGER PRIMARY KEY,     -- referenced everywhere, so the path can change
    path             TEXT NOT NULL UNIQUE,    -- like lib/l10n/intl_{locale}.arb
    name             TEXT NOT NULL UNIQUE,    -- [a-z0-9_-], for URLs and labels
    position         INTEGER NOT NULL,        -- order in lists and exports
    revision         INTEGER NOT NULL DEFAULT 0, -- counts source imports that changed something
    arb_locale       INTEGER NOT NULL DEFAULT 0, -- write "@@locale", as the source file does
    trailing_newline INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

-- The one file so far, from the settings that described it. A database
-- without strings gets its first file from the first source import.
INSERT INTO files (id, path, name, position, revision, arb_locale, trailing_newline)
SELECT 1, p.prefix || '{locale}.arb',
    coalesce(nullif(trim(lower(replace(rtrim(p.prefix, '_.-'), '.', '-')), '_-'), ''), 'app'), 1,
    coalesce((SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'source_revision'), 0),
    coalesce((SELECT value = '1' FROM settings WHERE key = 'arb_locale'), 0),
    coalesce((SELECT value = '1' FROM settings WHERE key = 'arb_trailing_newline'), 0)
FROM (SELECT coalesce((SELECT value FROM settings WHERE key = 'file_prefix'), 'intl_') AS prefix) p
WHERE EXISTS (SELECT 1 FROM source_strings);

DELETE FROM settings WHERE key IN ('file_prefix', 'source_revision', 'arb_locale', 'arb_trailing_newline');

-- Keys are unique per file. SQLite can't change a primary key, so these
-- tables are rebuilt (foreign keys are off while migrating).
CREATE TABLE source_strings_new (
    file_id    INTEGER NOT NULL REFERENCES files(id),
    key        TEXT NOT NULL,
    text       TEXT NOT NULL,
    hash       TEXT NOT NULL,               -- sha1(text), 12 hex chars
    meta       TEXT,                        -- the "@key" object as JSON
    position   INTEGER NOT NULL,
    removed    INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    draft      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (file_id, key)
);
INSERT INTO source_strings_new (file_id, key, text, hash, meta, position, removed, updated_at, draft)
    SELECT 1, key, text, hash, meta, position, removed, updated_at, draft FROM source_strings;

-- The current version per (lang, file, key). Outdated = source_hash != source_strings.hash.
CREATE TABLE translations_new (
    lang        TEXT NOT NULL,
    file_id     INTEGER NOT NULL,
    key         TEXT NOT NULL,
    text        TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    origin      TEXT NOT NULL,
    user_id     INTEGER REFERENCES users(id),
    history_id  INTEGER NOT NULL REFERENCES history(id),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    approved_by INTEGER REFERENCES users(id),
    approved_at TEXT,
    PRIMARY KEY (lang, file_id, key),
    FOREIGN KEY (file_id, key) REFERENCES source_strings(file_id, key)
);
INSERT INTO translations_new (lang, file_id, key, text, source_hash, origin, user_id, history_id, updated_at, approved_by, approved_at)
    SELECT lang, 1, key, text, source_hash, origin, user_id, history_id, updated_at, approved_by, approved_at FROM translations;

CREATE TABLE suggestions_new (
    id          INTEGER PRIMARY KEY,
    lang        TEXT NOT NULL,
    file_id     INTEGER NOT NULL,
    key         TEXT NOT NULL,
    text        TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    user_id     INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (lang, file_id, key, user_id),   -- a new suggestion replaces the user's previous one
    FOREIGN KEY (file_id, key) REFERENCES source_strings(file_id, key)
);
INSERT INTO suggestions_new (id, lang, file_id, key, text, source_hash, user_id, created_at)
    SELECT id, lang, 1, key, text, source_hash, user_id, created_at FROM suggestions;

DROP TABLE suggestions;
DROP TABLE translations;
DROP TABLE source_strings;
ALTER TABLE source_strings_new RENAME TO source_strings;
ALTER TABLE translations_new RENAME TO translations;
ALTER TABLE suggestions_new RENAME TO suggestions;

-- Append-only tables keep their rows and get the file of the first one.
ALTER TABLE history ADD COLUMN file_id INTEGER NOT NULL DEFAULT 1 REFERENCES files(id);
DROP INDEX history_key;
CREATE INDEX history_key ON history (lang, file_id, key);

ALTER TABLE approval_log ADD COLUMN file_id INTEGER NOT NULL DEFAULT 1 REFERENCES files(id);
DROP INDEX approval_log_key;
CREATE INDEX approval_log_key ON approval_log (lang, file_id, key);

-- An AI run translates one file.
ALTER TABLE ai_runs ADD COLUMN file_id INTEGER NOT NULL DEFAULT 1 REFERENCES files(id);
