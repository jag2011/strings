-- Every change of approval status, for the activity pages. Append-only.
CREATE TABLE approval_log (
    id         INTEGER PRIMARY KEY,
    lang       TEXT NOT NULL,
    key        TEXT NOT NULL,
    approved   INTEGER NOT NULL,            -- 1 approved, 0 approval removed
    history_id INTEGER REFERENCES history(id), -- the version approved or unapproved
    user_id    INTEGER REFERENCES users(id),   -- NULL when a source change removed the approval
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX approval_log_key ON approval_log (lang, key);

-- Approvals made before the log existed.
INSERT INTO approval_log (lang, key, approved, history_id, user_id, created_at)
    SELECT lang, key, 1, history_id, approved_by, approved_at FROM translations
    WHERE approved_by IS NOT NULL ORDER BY approved_at;
