-- The system prompt a run worked with, since the prompt can be edited.
ALTER TABLE ai_runs ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
