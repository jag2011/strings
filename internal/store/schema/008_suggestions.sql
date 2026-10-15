-- AI users translate through a language model instead of signing in. They
-- have no password, and their email is a placeholder.
ALTER TABLE users ADD COLUMN ai_model TEXT;  -- NULL for people

-- Alternative translations, e.g. from several AI users, waiting for someone
-- to pick one. A suggestion is shown only while it was made for the current
-- source text (source_hash) and differs from the current translation.
CREATE TABLE suggestions (
    id          INTEGER PRIMARY KEY,
    lang        TEXT NOT NULL,
    key         TEXT NOT NULL REFERENCES source_strings(key),
    text        TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    user_id     INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (lang, key, user_id)             -- a new suggestion replaces the user's previous one
);
