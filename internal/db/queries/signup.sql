-- name: CreateEvent :one
-- difficulty is NULL for MYTHIC_PLUS events, which have no difficulty of their own.
-- For a raid it decides the comp size rule, so the assigner cannot tell a Mythic
-- raid from a flex one without it.
INSERT INTO events (id, discord_guild_id, type, title, starts_at, signup_deadline, comp_template, message_id, channel_id, difficulty, reminder_lead_minutes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: SetEventDifficulty :exec
UPDATE events SET difficulty = $2
WHERE id = $1;

-- name: UpdateEvent :one
-- Partial update: a field left NULL keeps its stored value, same COALESCE pattern as
-- UpdateCharacterFromSync. Covers message_id/channel_id (learned only after the bot
-- posts) and the schedule fields a PATCH reschedules jobs from.
UPDATE events SET
    title = COALESCE(sqlc.narg(title), title),
    starts_at = COALESCE(sqlc.narg(starts_at), starts_at),
    signup_deadline = COALESCE(sqlc.narg(signup_deadline), signup_deadline),
    comp_template = COALESCE(sqlc.narg(comp_template), comp_template),
    difficulty = COALESCE(sqlc.narg(difficulty), difficulty),
    message_id = COALESCE(sqlc.narg(message_id), message_id),
    channel_id = COALESCE(sqlc.narg(channel_id), channel_id),
    reminder_lead_minutes = COALESCE(sqlc.narg(reminder_lead_minutes), reminder_lead_minutes),
    -- The one field a PATCH can also clear. NULL keeps the stored value like every
    -- other column here, so an empty string is what "take the log off this event"
    -- looks like on the wire. An empty string is not a URL, so nothing is lost by
    -- spending it as the sentinel.
    warcraftlogs_url = CASE
        WHEN sqlc.narg(warcraftlogs_url)::text = '' THEN NULL
        ELSE COALESCE(sqlc.narg(warcraftlogs_url), warcraftlogs_url)
    END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteEvent :exec
DELETE FROM events
WHERE id = $1;

-- name: GetEvent :one
SELECT * FROM events
WHERE id = $1;

-- name: ListUpcomingEvents :many
SELECT * FROM events
WHERE discord_guild_id = $1 AND starts_at >= now()
ORDER BY starts_at ASC;

-- name: ListPastEvents :many
-- The complement of ListUpcomingEvents, so every event is in exactly one of the two.
-- Newest first: a raid lead attaching a log is looking for last night, not for the
-- guild's first ever raid.
SELECT * FROM events
WHERE discord_guild_id = $1 AND starts_at < now()
ORDER BY starts_at DESC;

-- name: UpsertSignup :one
-- late_until is a plain write-through field, same as note: internal/signup owns the
-- rule that it is only meaningful alongside status = LATE and nils it otherwise.
INSERT INTO signups (id, event_id, character_id, status, note, late_until)
VALUES ($1, $2, $3, $4, $5, $6)
-- A status change invalidates whatever the comp lock decided, so the assignment is
-- dropped. Editing only the note leaves an existing assignment alone.
ON CONFLICT (event_id, character_id) DO UPDATE SET
    status = excluded.status,
    note = excluded.note,
    late_until = excluded.late_until,
    assigned_role = CASE
        WHEN signups.status IS DISTINCT FROM excluded.status THEN NULL
        ELSE signups.assigned_role
    END
RETURNING *;

-- name: DeleteSignup :exec
DELETE FROM signups
WHERE event_id = $1 AND character_id = $2;

-- name: ListSignupsForEvent :many
SELECT * FROM signups
WHERE event_id = $1
-- created_at is transaction start time, so it ties for signups written together.
ORDER BY created_at ASC, id ASC;

-- name: CountSignupsByStatusForEvents :many
-- The tally behind an event's signup_counts, for a whole guild's list in one query.
-- Grouped in SQL rather than by reading every signup row and counting in Go: a month
-- of raid nights is a request per event otherwise, each one dragging back full signup
-- rows in order to learn how many there are.
--
-- Only statuses actually present come back. Seeding the absent ones at zero is the
-- caller's job, because the enum lives in Go and a client rendering "0 absent" needs
-- the key present.
SELECT event_id, status, count(*) AS total
FROM signups
WHERE event_id = ANY(sqlc.arg(event_ids)::uuid[])
GROUP BY event_id, status;

-- name: ListUndecidedForEvent :many
-- Grouped by discord_id, not by character: a raider with four alts and no signup is
-- one person who has not answered, and four DMs would be a bug.
--
-- NOT EXISTS over the whole user, not a LEFT JOIN per character. A join emits one row
-- per *unsigned character*, so a raider who answered on their main but owns three
-- untouched alts still matched and got nagged about an event they had already
-- answered. Answering on any one character answers for the person.
SELECT DISTINCT u.discord_id
FROM users u
JOIN events e ON e.discord_guild_id = u.discord_guild_id
WHERE e.id = $1
  AND EXISTS (
      SELECT 1 FROM characters c
      WHERE c.user_id = u.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM signups s
      JOIN characters c2 ON c2.id = s.character_id
      WHERE s.event_id = e.id AND c2.user_id = u.id
  )
ORDER BY u.discord_id;

-- name: ListAttendingForEvent :many
-- Everyone who said they are turning up, seated or not. The pre-event reminder does not
-- read the comp: a raider left out of a locked twenty still wants to know the raid is
-- about to pull, and an unlocked event has no assignments at all.
--
-- DISTINCT ON collapses a person's alts to one row, preferring the alt that holds a
-- seat so the DM can still name a role. Without it, four signed-up alts is four pings
-- of the same person.
SELECT DISTINCT ON (u.discord_id) u.discord_id, s.assigned_role
FROM signups s
JOIN characters c ON c.id = s.character_id
JOIN users u ON u.id = c.user_id
WHERE s.event_id = $1
  AND s.status IN ('CONFIRMED', 'LATE', 'TENTATIVE')
ORDER BY u.discord_id, (s.assigned_role IS NULL);

-- name: CountCompSlotsForEvent :one
SELECT count(*) FROM comp_slots WHERE event_id = $1;

-- name: UpsertLateRequest :one
-- A re-request resets state to PENDING and clears any prior decision, since the
-- unique constraint makes this an upsert rather than a pile of rows.
INSERT INTO late_signup_requests (id, event_id, character_id, status, note, late_until)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (event_id, character_id) DO UPDATE SET
    status = excluded.status,
    note = excluded.note,
    late_until = excluded.late_until,
    state = 'PENDING',
    decided_at = NULL
RETURNING *;

-- name: GetLateRequest :one
-- Approving needs the event/character/status the request was filed with; the
-- decide endpoints only carry the request id.
SELECT * FROM late_signup_requests
WHERE id = $1;

-- name: ListLateRequests :many
SELECT * FROM late_signup_requests
WHERE event_id = $1
ORDER BY created_at DESC;

-- name: DecideLateRequest :exec
UPDATE late_signup_requests SET state = $2, decided_at = now()
WHERE id = $1;
