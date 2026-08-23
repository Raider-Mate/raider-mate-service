-- name: ListAssignmentPoolForEvent :many
-- Confirmed and late signups only; declined, tentative, absent, and no-show never
-- reach the assigner.
SELECT
    s.character_id,
    c.name,
    c.is_main,
    c.ilvl,
    c.mplus_score,
    s.created_at AS signed_up_at
FROM signups s
JOIN characters c ON c.id = s.character_id
WHERE s.event_id = $1 AND s.status IN ('CONFIRMED', 'LATE')
ORDER BY s.created_at ASC, s.id ASC;

-- name: ListUnseatedForComp :many
-- Who is in the assignment pool and holds no slot on this comp. A board is a snapshot
-- taken by the last lock, and signups carry on after it, so this is how a raider who
-- arrived since then is visible at all.
--
-- The pool test sits here rather than in Go for the reason DropCompSlotsForCharacter
-- gives below: it has to stay next to ListAssignmentPoolForEvent above.
--
-- has_roles is what separates the two ways to end up here. Assign drops a character
-- with an empty role menu, because comp_slots.role is NOT NULL and there is no role to
-- record, so they are unseated by every lock rather than by arriving late.
--
-- Newest first: the recent arrivals are what this answers.
SELECT
    s.character_id,
    s.status,
    s.created_at AS signed_up_at,
    EXISTS (SELECT 1 FROM character_roles cr WHERE cr.character_id = s.character_id) AS has_roles
FROM signups s
WHERE s.event_id = $1
  AND s.status IN ('CONFIRMED', 'LATE')
  AND NOT EXISTS (
      SELECT 1 FROM comp_slots cs
      WHERE cs.event_id = s.event_id
        AND cs.comp_name = $2
        AND cs.character_id = s.character_id
  )
ORDER BY s.created_at DESC, s.id DESC;

-- name: ListRolesForCharacters :many
SELECT * FROM character_roles
WHERE character_id = ANY(sqlc.arg(character_ids)::uuid[])
ORDER BY character_id, priority, role;

-- name: UpsertComp :one
-- Creating a comp is idempotent, but the mode is set once at creation: flipping an
-- existing comp between AUTO and MANUAL is SetCompMode's job, so a stray create
-- cannot quietly hand a raid lead's hand-built comp back to the assigner.
INSERT INTO comps (id, event_id, name, mode)
VALUES ($1, $2, $3, $4)
ON CONFLICT (event_id, name) DO UPDATE SET name = excluded.name
RETURNING *;

-- name: GetComp :one
SELECT * FROM comps
WHERE event_id = $1 AND name = $2;

-- name: ListComps :many
SELECT * FROM comps
WHERE event_id = $1
ORDER BY name;

-- name: SetCompMode :exec
UPDATE comps SET mode = $3
WHERE event_id = $1 AND name = $2;

-- name: RenameComp :exec
-- The slots follow, through the ON UPDATE CASCADE added in migration 00011. Renaming
-- rather than rebuilding is the point: changing a label should not cost the board
-- underneath it.
UPDATE comps SET name = sqlc.arg(new_name)
WHERE event_id = sqlc.arg(event_id) AND name = sqlc.arg(name);

-- name: DeleteComp :exec
DELETE FROM comps
WHERE event_id = $1 AND name = $2;

-- name: DeleteCompSlots :exec
DELETE FROM comp_slots
WHERE event_id = $1 AND comp_name = $2;

-- name: DropCompSlotsForCharacter :many
-- A signup that leaves the assignment pool takes its seats with it. Without this the
-- locked comp keeps a slot for someone who has said they are not coming, and
-- CountCompSlotsForEvent still reads the event as locked.
--
-- The pool test is written here rather than in Go so it stays next to the one in
-- ListAssignmentPoolForEvent above: two definitions of "holds a seat" that drift are
-- how a raider ends up assigned and absent at once. Run it in the same transaction as
-- the write it follows: after an upsert it reads back the status just written, and
-- after a delete it finds no signup at all, which is the strongest form of not in the
-- pool and is what a withdrawal means.
--
-- One row per comp the character was in, never more: comp_slots is unique on
-- (event_id, comp_name, character_id), so nobody holds two seats in one comp.
DELETE FROM comp_slots cs
WHERE cs.event_id = $1 AND cs.character_id = $2
  AND NOT EXISTS (
      SELECT 1 FROM signups s
      WHERE s.event_id = cs.event_id
        AND s.character_id = cs.character_id
        AND s.status IN ('CONFIRMED', 'LATE')
  )
RETURNING cs.comp_name;

-- name: InsertCompSlot :exec
INSERT INTO comp_slots (id, event_id, comp_name, character_id, role, slot_index, is_bench, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListCompSlots :many
SELECT * FROM comp_slots
WHERE event_id = $1 AND comp_name = $2
ORDER BY is_bench, slot_index;

-- name: ClearAssignedRoles :exec
-- Locking any comp_name overwrites the whole event's assigned_role, since that
-- column is single-valued while comp_slots holds multiple named drafts.
UPDATE signups SET assigned_role = NULL
WHERE event_id = $1;

-- name: SetSignupAssignedRole :exec
UPDATE signups SET assigned_role = $3
WHERE event_id = $1 AND character_id = $2;
