-- Users who sign in through an external account server (see web.External).
-- Their name and email follow that account.
ALTER TABLE users ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX users_external_id ON users (external_id) WHERE external_id IS NOT NULL;
