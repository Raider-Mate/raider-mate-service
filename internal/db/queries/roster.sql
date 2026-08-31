-- name: UpsertUser :one
INSERT INTO users (id, discord_id, discord_guild_id)
VALUES ($1, $2, $3)
ON CONFLICT (discord_id, discord_guild_id) DO UPDATE SET discord_id = excluded.discord_id
RETURNING *;

-- name: GetUserByDiscord :one
SELECT * FROM users
WHERE discord_id = $1 AND discord_guild_id = $2;

-- name: CreateCharacter :one
-- is_main is decided here rather than taken from the caller. "My main" is a fact about
-- the whole roster, not about the row being written, and a client that sends true on
-- every registration would have each new alt steal the flag from the character that
-- holds it. The caller's value is a request, granted only while the raider has no main
-- yet; moving it afterwards is SetCharacterMain, which demotes the old one first.
INSERT INTO characters (id, user_id, name, realm, region, is_main)
SELECT
    sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(realm), sqlc.arg(region),
    sqlc.arg(is_main)::boolean
        AND NOT EXISTS (
            SELECT 1 FROM characters x WHERE x.user_id = sqlc.arg(user_id) AND x.is_main
        )
RETURNING *;

-- name: GetCharacterInGuild :one
SELECT c.* FROM characters c
JOIN users u ON u.id = c.user_id
WHERE c.id = $1 AND u.discord_guild_id = $2;

-- name: GetCharacterOwner :one
-- A signup write is "self, or raid lead": self means the caller's discord id owns
-- the character, which the signup and late-request handlers cannot decide without
-- this.
SELECT u.discord_id FROM characters c
JOIN users u ON u.id = c.user_id
WHERE c.id = $1 AND u.discord_guild_id = $2;

-- name: DeleteCharacter :execrows
-- Guild-scoped so a character id alone cannot delete another guild's row. The
-- affected-row count is what tells the caller "no such character here" apart from
-- "deleted", which a plain :exec cannot.
DELETE FROM characters c
USING users u
WHERE c.user_id = u.id AND c.id = $1 AND u.discord_guild_id = $2;

-- name: ClearMainForCharacterOwner :exec
-- Demotes whichever character currently holds main for the owner of $1. Its own
-- statement because characters_one_main_per_user is a partial unique index, and
-- Postgres cannot defer one: a single UPDATE that demotes and promotes in one pass
-- violates it whenever the scan reaches the promoted row first, which makes the bug
-- depend on heap order rather than on the data. Run this and SetCharacterMain inside
-- one transaction. A character in another guild matches nothing, so the subquery
-- returning NULL is the guild scoping.
UPDATE characters SET is_main = false
WHERE is_main
  AND user_id = (
      SELECT c.user_id FROM characters c
      JOIN users u ON u.id = c.user_id
      WHERE c.id = $1 AND u.discord_guild_id = $2
  );

-- name: SetCharacterMain :execrows
-- Same guild scoping and the same reason for :execrows. Only is_main is settable:
-- name, realm and region are the Raider.IO identity the sync job keys on, so
-- changing them is a delete and a re-register, not an edit.
UPDATE characters c SET is_main = $3
FROM users u
WHERE c.user_id = u.id AND c.id = $1 AND u.discord_guild_id = $2;

-- name: ListCharactersByUser :many
SELECT * FROM characters
WHERE user_id = $1
ORDER BY name;

-- name: ListCharactersByDiscord :many
SELECT c.* FROM characters c
JOIN users u ON u.id = c.user_id
WHERE u.discord_id = $1 AND u.discord_guild_id = $2
ORDER BY c.name;

-- name: SetCharacterRole :exec
INSERT INTO character_roles (character_id, role, priority)
VALUES ($1, $2, $3)
ON CONFLICT (character_id, role) DO UPDATE SET priority = excluded.priority;

-- name: DeleteCharacterRoles :exec
DELETE FROM character_roles
WHERE character_id = $1;

-- name: ListCharacterRoles :many
SELECT * FROM character_roles
WHERE character_id = $1
-- Nothing stops two roles sharing a priority; role keeps the menu order stable.
ORDER BY priority, role;

-- name: ListCharactersInGuild :many
SELECT c.* FROM characters c
JOIN users u ON u.id = c.user_id
WHERE u.discord_guild_id = $1
ORDER BY c.name;

-- name: ListCharactersDueForSync :many
-- Oldest-attempted first so a backlog drains fairly instead of starving whoever is
-- always at the bottom of an ID-ordered scan. Ordering by attempt rather than by
-- last_synced keeps a character whose fetch keeps failing from holding the head of
-- every batch.
SELECT * FROM characters
WHERE (last_synced IS NULL OR last_synced < sqlc.arg(stale_before))
  AND (sync_attempted_at IS NULL OR sync_attempted_at < sqlc.arg(stale_before))
ORDER BY sync_attempted_at ASC NULLS FIRST
LIMIT sqlc.arg(row_limit);

-- name: UpdateCharacterFromSync :exec
-- COALESCE so a response missing class or spec leaves the stored value alone
-- instead of blanking it.
--
-- The gear-derived columns below are written straight, NULL included, unlike class and
-- spec. A raider who unequips their tier really does have no tier, and COALESCEing that
-- away would leave last season's count on the row forever.
UPDATE characters SET
    class = COALESCE(sqlc.narg(class), class),
    spec = COALESCE(sqlc.narg(spec), spec),
    ilvl = $2,
    mplus_score = $3,
    enchants_missing = sqlc.narg(enchants_missing),
    enchants_expected = sqlc.narg(enchants_expected),
    tier_pieces = sqlc.narg(tier_pieces),
    raid_slug = sqlc.narg(raid_slug),
    raid_bosses = sqlc.narg(raid_bosses),
    raid_normal_killed = sqlc.narg(raid_normal_killed),
    raid_heroic_killed = sqlc.narg(raid_heroic_killed),
    raid_mythic_killed = sqlc.narg(raid_mythic_killed),
    last_synced = now(),
    sync_attempted_at = now()
WHERE id = $1;

-- name: TouchCharacterSynced :exec
UPDATE characters SET last_synced = now(), sync_attempted_at = now()
WHERE id = $1;

-- name: MarkCharacterSyncAttempted :exec
-- A failed fetch. last_synced stays put because the cached data did not get any
-- fresher; only the queue position moves.
UPDATE characters SET sync_attempted_at = now()
WHERE id = $1;

-- name: ListGuildsForDiscordUser :many
-- The guilds Raider Mate actually knows this person in, which is not the same as the
-- guilds they are in on Discord. Joined through characters on purpose: a users row with
-- nothing attached would send a client to a guild with an empty roster.
SELECT DISTINCT u.discord_guild_id
FROM users u
JOIN characters c ON c.user_id = u.id
WHERE u.discord_id = $1
ORDER BY u.discord_guild_id;
