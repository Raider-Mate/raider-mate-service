-- +goose Up
-- Two facts a roster had nowhere to put: a raider who has left, and a character the
-- worker can no longer find.
--
-- archived_at is how a departure is recorded, because deleting the row is not. Every
-- foreign key into characters cascades, so a DELETE takes the raider's signups, their
-- comp slots and their gear snapshots with it, and attendance is computed from exactly
-- those rows. A guild pruning last tier's leavers would quietly rewrite its own raid
-- history. Archiving keeps the history and takes the name off the roster, and it is
-- reversible, which matters because most departures turn out to be holidays.
ALTER TABLE characters ADD COLUMN archived_at timestamptz;

-- not_found_since is when Raider.IO started answering 404 for this character. Until
-- now a missing character was marked freshly synced and kept its last known numbers
-- forever, so a rename, a transfer or a deletion read as a raider standing still. NULL
-- means the last fetch found them; a raid lead deciding who to archive wants the date.
ALTER TABLE characters ADD COLUMN not_found_since timestamptz;

-- The roster read filters on this every time. Partial, because the archived are the
-- few and every other query wants the rest.
CREATE INDEX characters_archived_at ON characters (archived_at) WHERE archived_at IS NOT NULL;

-- +goose Down
DROP INDEX characters_archived_at;
ALTER TABLE characters DROP COLUMN not_found_since;
ALTER TABLE characters DROP COLUMN archived_at;
