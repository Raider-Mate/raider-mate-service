-- name: InsertNotification :execrows
-- The row count tells a suppressed insert from a real one, which is the only way a
-- caller can see the coalescing below happen.
--
-- ON CONFLICT DO NOTHING serves the roster redraws, which lean on
-- notifications_roster_updated_pending: the second character to change on the same
-- raid finds a redraw already queued and adds nothing. It is inert for every other
-- kind, since the partial index does not cover them and the id comes from db.NewID.
INSERT INTO notifications (id, discord_guild_id, event_id, kind, target_kind, discord_id, role_ids, discord_ids, channel_id, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT DO NOTHING;

-- name: ListEventsNeedingRosterRedraw :many
-- The posted, upcoming events this character is signed up to, one redraw apiece. The
-- caller inserts the notifications on the same transaction as the snapshot that
-- caused them, so no redraw is queued for a sync that was rolled back and none is
-- lost for one that was not. Two statements rather than the INSERT ... SELECT this
-- used to be, because the ids are generated in Go and the row count is not known
-- until this query has run.
--
-- Past events are skipped because nobody re-reads a raid that already happened, and
-- events with no message_id were never posted, so there is nothing to edit.
SELECT e.id, e.discord_guild_id, e.channel_id
FROM events e
JOIN signups s ON s.event_id = e.id
WHERE s.character_id = sqlc.arg(character_id)
  AND e.starts_at > now()
  AND e.message_id IS NOT NULL
  AND e.channel_id IS NOT NULL;

-- name: ClaimNotifications :many
-- Claiming, not just reading. The ack arrives in a later HTTP request, so no
-- transaction can span send and ack and row locks cannot help: two bot replicas
-- polling the same window would both read the same rows and DM every raider twice.
-- Stamping claimed_at inside the same statement hands each row to one poller.
--
-- claimed_before re-opens a lease so a bot that claimed rows and died still gets them
-- redelivered. That keeps delivery at-least-once, which reminders tolerate; what it
-- removes is the duplicate storm on every tick.
UPDATE notifications SET claimed_at = now()
WHERE id IN (
    SELECT n.id FROM notifications n
    WHERE n.delivered_at IS NULL
      AND (n.claimed_at IS NULL OR n.claimed_at < sqlc.arg(claimed_before))
      AND (sqlc.narg(guild_id)::bigint IS NULL OR n.discord_guild_id = sqlc.narg(guild_id))
    ORDER BY n.created_at ASC
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkNotificationDelivered :execrows
-- guild_id is optional, and the two callers differ. The bot acks across every guild
-- from behind the service key, so it passes NULL. Anything reached by a raider's
-- interaction must pass their guild, or acking by id alone would let them silently
-- suppress another guild's reminders. Returning the row count lets the caller tell
-- "not yours or not found" from "done".
UPDATE notifications SET delivered_at = now()
WHERE id = $1
  AND (sqlc.narg(guild_id)::bigint IS NULL OR discord_guild_id = sqlc.narg(guild_id));

-- name: GetNotification :one
-- One outbox row by id. The failure report the bot files needs to know what it could
-- not deliver and for which event.
SELECT * FROM notifications WHERE id = $1;
