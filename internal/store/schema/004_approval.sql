-- Proofreaders do what translators do and also approve translations.
-- SQLite can't change a CHECK constraint, so users is rebuilt (foreign keys
-- are off while migrating).
CREATE TABLE users_new (
    id            INTEGER PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,     -- stored lowercase
    name          TEXT NOT NULL,            -- shown in history and credits
    password_hash TEXT,                     -- NULL until an invite is used
    role          TEXT NOT NULL CHECK (role IN ('admin', 'proofreader', 'translator')),
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO users_new (id, email, name, password_hash, role, disabled, created_at)
    SELECT id, email, name, password_hash, role, disabled, created_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- Approved translations can't be changed until a proofreader or admin removes
-- the approval. Outdating a translation removes its approval.
ALTER TABLE translations ADD COLUMN approved_by INTEGER REFERENCES users(id);
ALTER TABLE translations ADD COLUMN approved_at TEXT;
