-- Source strings marked "x-draft": true in their "@key" metadata are not
-- ready for translation and are hidden from translators.
ALTER TABLE source_strings ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;

UPDATE source_strings SET draft = 1
WHERE json_valid(meta) AND json_type(meta, '$."x-draft"') = 'true';
