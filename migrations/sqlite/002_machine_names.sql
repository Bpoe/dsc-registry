ALTER TABLE machines ADD COLUMN name_folded TEXT COLLATE BINARY;
CREATE UNIQUE INDEX machines_name_folded_unique ON machines(name_folded);

-- Existing rows are backfilled in the migration transaction before publication.
CREATE TRIGGER machines_name_folded_insert BEFORE INSERT ON machines
WHEN NEW.name_folded IS NULL OR NEW.name_folded = ''
BEGIN
    SELECT RAISE(ABORT, 'machine name key is required');
END;

CREATE TRIGGER machines_name_folded_update BEFORE UPDATE OF name_folded ON machines
WHEN NEW.name_folded IS NULL OR NEW.name_folded = ''
BEGIN
    SELECT RAISE(ABORT, 'machine name key is required');
END;
