-- +goose Up
-- Three facts the roster wanted and the API could not answer, all derived from data
-- the sync already fetched or was one field away from fetching.
--
-- They live on characters rather than on character_snapshots because the roster list
-- reads characters, once, for a whole guild. Deriving them per row from the snapshot's
-- gear jsonb would put a jsonb walk on the one query every dashboard page load makes.
-- The history is not lost: the gear jsonb still carries every enchant id, so a
-- compliance graph can be built from snapshots later without these columns.
--
-- Every column is nullable, and NULL means "not established", never zero. A character
-- that has never synced, and a season whose tier item ids the operator has not
-- configured, both read as absent rather than as a compliant raider with no tier.

-- How many equipped slots take an enchant, and how many of those are missing one.
-- Both, rather than a single percentage: the service divides nowhere, and "2 of 8"
-- says more to a raid lead than "75%".
ALTER TABLE characters
    ADD COLUMN enchants_missing  smallint,
    ADD COLUMN enchants_expected smallint;

-- Equipped pieces from the current season's class set. The set bonuses land at two and
-- four, so the raw count is what a client needs; which bonuses that earns is the
-- client's rendering, not this service's arithmetic.
ALTER TABLE characters
    ADD COLUMN tier_pieces smallint;

-- Progression in the raid the worker is configured to track. The slug rides along so a
-- client never has to guess which tier the numbers describe, and so a stale row is
-- obvious after a tier change rather than silently wrong.
ALTER TABLE characters
    ADD COLUMN raid_slug          text,
    ADD COLUMN raid_bosses        smallint,
    ADD COLUMN raid_normal_killed smallint,
    ADD COLUMN raid_heroic_killed smallint,
    ADD COLUMN raid_mythic_killed smallint;

-- +goose Down
ALTER TABLE characters
    DROP COLUMN raid_mythic_killed,
    DROP COLUMN raid_heroic_killed,
    DROP COLUMN raid_normal_killed,
    DROP COLUMN raid_bosses,
    DROP COLUMN raid_slug,
    DROP COLUMN tier_pieces,
    DROP COLUMN enchants_expected,
    DROP COLUMN enchants_missing;
