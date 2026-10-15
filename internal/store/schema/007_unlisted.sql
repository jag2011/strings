-- Unlisted users don't appear under "Top members", for example an admin
-- whose credits come from importing.
ALTER TABLE users ADD COLUMN unlisted INTEGER NOT NULL DEFAULT 0;
