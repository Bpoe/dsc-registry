ALTER TABLE machines ADD COLUMN name_folded TEXT COLLATE "C";
CREATE UNIQUE INDEX machines_name_folded_unique ON machines(name_folded);

-- Validate existing rows after the Go Unicode backfill in the same transaction.
ALTER TABLE machines ADD CONSTRAINT machines_name_folded_required
    CHECK (name_folded IS NOT NULL AND name_folded <> '') NOT VALID;
