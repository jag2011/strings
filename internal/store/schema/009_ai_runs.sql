-- AI translation runs: an agent translates a language as an AI user. The run
-- counts what it did; which strings are left is worked out from translations
-- and suggestions, so a run started again continues where it stopped.
CREATE TABLE ai_runs (
    id            INTEGER PRIMARY KEY,
    lang          TEXT NOT NULL,
    user_id       INTEGER NOT NULL REFERENCES users(id),  -- the AI user, credited for everything
    started_by    INTEGER REFERENCES users(id),
    model         TEXT NOT NULL,            -- the AI user's model when queued
    input         TEXT NOT NULL CHECK (input IN ('missing', 'all')),
    suggest       INTEGER NOT NULL DEFAULT 0, -- save everything as suggestions
    notes         TEXT NOT NULL DEFAULT '', -- style notes for the prompt
    max_cost      REAL NOT NULL,            -- USD, as OpenRouter reports it
    status        TEXT NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled', 'interrupted')),
    translated    INTEGER NOT NULL DEFAULT 0,
    suggested     INTEGER NOT NULL DEFAULT 0,
    failed        INTEGER NOT NULL DEFAULT 0, -- strings the agent couldn't save without errors
    steps         INTEGER NOT NULL DEFAULT 0, -- model calls
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0, -- part of input_tokens
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost          REAL NOT NULL DEFAULT 0,
    summary       TEXT NOT NULL DEFAULT '', -- the agent's final words
    error         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    started_at    TEXT,
    finished_at   TEXT
);
CREATE INDEX ai_runs_status ON ai_runs (status);

-- The run's log: one line per tool call or status change.
CREATE TABLE ai_run_events (
    id         INTEGER PRIMARY KEY,
    run_id     INTEGER NOT NULL REFERENCES ai_runs(id) ON DELETE CASCADE,
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX ai_run_events_run ON ai_run_events (run_id);
