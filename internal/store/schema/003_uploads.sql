-- Web uploads that contain the source file wait here until an admin has
-- reviewed the changes.
CREATE TABLE uploads (
    id         TEXT PRIMARY KEY,            -- random, part of the review URL
    files      BLOB NOT NULL,               -- JSON: file name -> base64 content
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
