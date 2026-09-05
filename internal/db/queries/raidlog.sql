-- name: UpsertEventReport :one
-- Called in the same transaction as the event's URL write. A URL stored with no queue
-- row behind it is a report nobody will ever fetch, so the two land together.
--
-- A changed report is a new report: the code moving means the old fights and raiders
-- describe a different night, and the cascade takes them with the row's reset. Due
-- immediately, because a raid lead who just pasted a link is waiting for it.
INSERT INTO event_reports (event_id, host, code, status, next_attempt_at)
VALUES (sqlc.arg(event_id), sqlc.arg(host), sqlc.arg(code), 'PENDING', now())
ON CONFLICT (event_id) DO UPDATE SET
    host = EXCLUDED.host,
    code = EXCLUDED.code,
    status = CASE
        WHEN event_reports.code = EXCLUDED.code THEN event_reports.status
        ELSE 'PENDING'
    END,
    revision = CASE WHEN event_reports.code = EXCLUDED.code THEN event_reports.revision END,
    next_attempt_at = now(),
    attempts = 0,
    failure_reason = NULL
RETURNING *;

-- name: DeleteEventReport :exec
-- Taking the log off an event. The fights and raiders go with it by cascade.
DELETE FROM event_reports WHERE event_id = sqlc.arg(event_id);

-- name: GetEventReport :one
SELECT * FROM event_reports WHERE event_id = sqlc.arg(event_id);

-- name: GetEventReportStatuses :many
-- The status for a page of events at once, so the event list does not run a query per
-- row to decide which link rel to offer.
SELECT event_id, status FROM event_reports
WHERE event_id = ANY(sqlc.arg(event_ids)::uuid[]);

-- name: ClaimDueReports :many
-- One statement, so two workers cannot take the same row.
--
-- The ten minute push is a lease rather than a schedule: whatever the fetch decides
-- overwrites it, and a worker that dies mid-fetch has its rows redelivered ten minutes
-- later instead of never. Same at-least-once trade the notification outbox makes.
UPDATE event_reports SET next_attempt_at = now() + interval '10 minutes'
WHERE event_id IN (
    SELECT event_id FROM event_reports
    WHERE next_attempt_at IS NOT NULL AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkReportFetched :exec
-- A successful read. next_attempt_at NULL is the row saying it has settled and the
-- worker may stop asking; a timestamp is a report still being written to.
UPDATE event_reports SET
    status = 'READY',
    revision = sqlc.arg(revision),
    live = sqlc.arg(live),
    title = sqlc.narg(title),
    zone_name = sqlc.narg(zone_name),
    region = sqlc.narg(region),
    report_starts_at = sqlc.narg(report_starts_at),
    report_ends_at = sqlc.narg(report_ends_at),
    fetched_at = now(),
    next_attempt_at = sqlc.narg(next_attempt_at),
    attempts = 0,
    failure_reason = NULL
WHERE event_id = sqlc.arg(event_id);

-- name: MarkReportFailed :exec
-- A failure leaves whatever numbers are already stored alone. A report that read fine
-- yesterday and could not be reached today is still the best answer there is, and
-- blanking it because one fetch failed would lose a night to an outage.
UPDATE event_reports SET
    status = sqlc.arg(status),
    fetched_at = fetched_at,
    next_attempt_at = sqlc.narg(next_attempt_at),
    attempts = attempts + 1,
    failure_reason = sqlc.narg(failure_reason)
WHERE event_id = sqlc.arg(event_id);

-- name: RescheduleReport :exec
-- Puts a claimed row back without counting an attempt against it. For the cases where
-- nothing was learned about the report itself: the point budget was spent, or the
-- credentials were rejected.
UPDATE event_reports SET next_attempt_at = sqlc.arg(next_attempt_at)
WHERE event_id = sqlc.arg(event_id);

-- name: DeleteEventReportFights :exec
DELETE FROM event_report_fights WHERE event_id = sqlc.arg(event_id);

-- name: InsertEventReportFight :exec
INSERT INTO event_report_fights (
    event_id, fight_id, encounter_id, name, difficulty, raid_size,
    kill, boss_percentage, fight_percentage, starts_at, ends_at
) VALUES (
    sqlc.arg(event_id), sqlc.arg(fight_id), sqlc.arg(encounter_id), sqlc.arg(name),
    sqlc.narg(difficulty), sqlc.narg(raid_size), sqlc.arg(kill),
    sqlc.narg(boss_percentage), sqlc.narg(fight_percentage),
    sqlc.arg(starts_at), sqlc.arg(ends_at)
)
ON CONFLICT (event_id, fight_id) DO UPDATE SET
    encounter_id = EXCLUDED.encounter_id,
    name = EXCLUDED.name,
    difficulty = EXCLUDED.difficulty,
    raid_size = EXCLUDED.raid_size,
    kill = EXCLUDED.kill,
    boss_percentage = EXCLUDED.boss_percentage,
    fight_percentage = EXCLUDED.fight_percentage,
    starts_at = EXCLUDED.starts_at,
    ends_at = EXCLUDED.ends_at;

-- name: ListEventReportFights :many
-- In the order they happened, which is the order a raid night is read in.
SELECT * FROM event_report_fights
WHERE event_id = sqlc.arg(event_id)
ORDER BY starts_at, fight_id;

-- name: DeleteEventReportRaiders :exec
DELETE FROM event_report_raiders WHERE event_id = sqlc.arg(event_id);

-- name: InsertEventReportRaider :exec
-- The night's totals. Per-pull rows live in their own table, so a reader of this one sees
-- one row per raider and cannot double-count.
INSERT INTO event_report_raiders (
    event_id, actor_id, actor_name, actor_server, class, character_id,
    damage, healing, deaths
) VALUES (
    sqlc.arg(event_id), sqlc.arg(actor_id), sqlc.arg(actor_name), sqlc.arg(actor_server),
    sqlc.narg(class), sqlc.narg(character_id),
    sqlc.arg(damage), sqlc.arg(healing), sqlc.arg(deaths)
)
ON CONFLICT (event_id, actor_id) DO UPDATE SET
    actor_name = EXCLUDED.actor_name,
    actor_server = EXCLUDED.actor_server,
    class = EXCLUDED.class,
    -- Rematched on every fetch, so a raider who registers their alt on Thursday sees
    -- Wednesday's log light up without anybody touching it.
    character_id = EXCLUDED.character_id,
    damage = EXCLUDED.damage,
    healing = EXCLUDED.healing,
    deaths = EXCLUDED.deaths;

-- name: ListEventReportRaiders :many
-- The night's totals, one row per raider.
--
-- Damage first, because the damage board is what the page opens on. The read side
-- reorders for the other two metrics rather than asking again.
SELECT * FROM event_report_raiders
WHERE event_id = sqlc.arg(event_id)
ORDER BY damage DESC, actor_name;

-- name: DeleteEventReportFightRaiders :exec
DELETE FROM event_report_fight_raiders WHERE event_id = sqlc.arg(event_id);

-- name: InsertEventReportFightRaider :exec
INSERT INTO event_report_fight_raiders (
    event_id, fight_id, actor_id, actor_name, actor_server, class, character_id,
    damage, healing, deaths
) VALUES (
    sqlc.arg(event_id), sqlc.arg(fight_id), sqlc.arg(actor_id), sqlc.arg(actor_name), sqlc.arg(actor_server),
    sqlc.narg(class), sqlc.narg(character_id),
    sqlc.arg(damage), sqlc.arg(healing), sqlc.arg(deaths)
)
ON CONFLICT (event_id, fight_id, actor_id) DO UPDATE SET
    actor_name = EXCLUDED.actor_name,
    actor_server = EXCLUDED.actor_server,
    class = EXCLUDED.class,
    character_id = EXCLUDED.character_id,
    damage = EXCLUDED.damage,
    healing = EXCLUDED.healing,
    deaths = EXCLUDED.deaths;

-- name: ListEventReportFightRaiders :many
-- Every pull's rows in one query, so a client selecting a pull costs no round trip.
SELECT * FROM event_report_fight_raiders
WHERE event_id = sqlc.arg(event_id)
ORDER BY fight_id, damage DESC, actor_name;

-- name: ListExpectedCharactersForEvent :many
-- Who said they were coming. CONFIRMED and LATE only: a tentative signup was never a
-- promise, and listing a maybe as missing would put somebody on a no-show list for
-- answering honestly.
SELECT character_id FROM signups
WHERE event_id = sqlc.arg(event_id)
  AND status IN ('CONFIRMED', 'LATE');

-- name: GetGuildWarcraftLogsCredentials :one
-- The sealed key never leaves the service; this is the only query that reads it, and it
-- is called by the worker, never by a request handler.
SELECT warcraft_logs_client_id, warcraft_logs_key_sealed
FROM guild_settings
WHERE discord_guild_id = sqlc.arg(discord_guild_id);

-- name: SetGuildWarcraftLogsCredentials :exec
-- Upsert, because a guild that has never been configured has no settings row.
INSERT INTO guild_settings (discord_guild_id, warcraft_logs_client_id, warcraft_logs_key_sealed, warcraft_logs_key_set_at)
VALUES (sqlc.arg(discord_guild_id), sqlc.arg(client_id), sqlc.arg(key_sealed), now())
ON CONFLICT (discord_guild_id) DO UPDATE
SET warcraft_logs_client_id = excluded.warcraft_logs_client_id,
    warcraft_logs_key_sealed = excluded.warcraft_logs_key_sealed,
    warcraft_logs_key_set_at = now(),
    updated_at = now();

-- name: ClearGuildWarcraftLogsCredentials :exec
-- Revoking. All three columns together, so a cleared key can never leave an id behind
-- that looks configured.
UPDATE guild_settings SET
    warcraft_logs_client_id = NULL,
    warcraft_logs_key_sealed = NULL,
    warcraft_logs_key_set_at = NULL,
    updated_at = now()
WHERE discord_guild_id = sqlc.arg(discord_guild_id);
