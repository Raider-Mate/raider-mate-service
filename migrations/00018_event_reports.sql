-- +goose Up
-- What the raid night actually looked like, read back from the WarcraftLogs report a
-- raid lead attached.
--
-- Cached rather than fetched per request. The WarcraftLogs point budget is 3600 an hour
-- per client key, and a full report costs about eighteen of them, so one dashboard
-- refresh per raider would spend a guild's hour before the raid finished. It is the same
-- reasoning that keeps Raider.IO out of the request path.
--
-- One row per event, created when a report URL is attached and deleted when it is taken
-- off. The status column is also the work queue: the worker claims rows whose next
-- attempt is due, and nothing here needs a scheduled_jobs entry, because a report is not
-- a thing that happens at a time. It is a thing that keeps changing until it stops.
CREATE TYPE report_status AS ENUM (
    -- Attached, never successfully fetched. What a client shows a spinner for.
    'PENDING',
    -- Fetched. live says whether the raid is still going.
    'READY',
    -- WarcraftLogs has it, client credentials cannot read it. Terminal: reaching a
    -- private report needs the report owner's own OAuth consent, which this service
    -- never asks for. The recovery is the raid lead setting it to unlisted.
    'PRIVATE',
    -- No such report. A deleted report and a mistyped code look identical from here.
    'NOT_FOUND',
    -- WarcraftLogs archived the detail data. Fights survive, the tables do not.
    'ARCHIVED',
    -- WarcraftLogs was down, timed out, or answered something unusable. Retried.
    'UNAVAILABLE'
);

CREATE TABLE event_reports (
    event_id          uuid PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
    -- The host and code the URL parsed to. Stored rather than re-parsed every tick, and
    -- the host is which WarcraftLogs the report lives on: classic and fresh are separate
    -- sites with separate reports.
    host              text NOT NULL,
    code              text NOT NULL,
    status            report_status NOT NULL DEFAULT 'PENDING',
    -- The report's own reprocessing counter. Two fetches returning the same revision
    -- returned the same numbers, which is how the worker knows the night has settled and
    -- it can stop asking. A real report grew from 21 pulls to 23 between two fetches
    -- twenty minutes apart, which is what this exists for.
    revision          integer,
    -- True while any pull is still in progress. A client showing a damage table during
    -- the raid has to say the numbers are not final yet.
    live              boolean NOT NULL DEFAULT false,
    title             text,
    zone_name         text,
    region            text,
    report_starts_at  timestamptz,
    report_ends_at    timestamptz,
    fetched_at        timestamptz,
    -- NULL means the worker is done with this row: it settled, or it failed terminally.
    -- A timestamp is the next tick that may claim it.
    next_attempt_at   timestamptz,
    attempts          smallint NOT NULL DEFAULT 0,
    -- WarcraftLogs' own words for a failure, for the operator. Never shown to a raider:
    -- the status is what a client renders, and a raider does not need a GraphQL error.
    failure_reason    text
);

-- Shaped for the worker's claim: rows that are due, oldest first.
CREATE INDEX event_reports_due ON event_reports (next_attempt_at)
    WHERE next_attempt_at IS NOT NULL;

CREATE TABLE event_report_fights (
    event_id         uuid NOT NULL REFERENCES event_reports (event_id) ON DELETE CASCADE,
    -- WarcraftLogs' fight id inside the report, and the anchor of a #fight=12 deep link.
    -- Stable across refetches, so a refetch updates a pull rather than duplicating it.
    fight_id         integer NOT NULL,
    encounter_id     integer NOT NULL,
    name             text NOT NULL,
    -- WarcraftLogs' raw difficulty integer: 3, 4 and 5 are normal, heroic and mythic.
    -- Anything else is left as the number, because a difficulty this service has never
    -- heard of is still a real pull and dropping it would silently shorten the night.
    difficulty       integer,
    raid_size        integer,
    kill             boolean NOT NULL,
    -- How much health the boss had left, on a 0 to 100 scale and about zero on a kill.
    -- The number a guild argues about after a wipe night.
    boss_percentage  numeric,
    fight_percentage numeric,
    starts_at        timestamptz NOT NULL,
    ends_at          timestamptz NOT NULL,
    PRIMARY KEY (event_id, fight_id)
);

CREATE TABLE event_report_raiders (
    event_id     uuid NOT NULL REFERENCES event_reports (event_id) ON DELETE CASCADE,
    -- WarcraftLogs' actor id inside the report.
    actor_id     integer NOT NULL,
    -- The name and server as the log recorded them, kept even when they matched a
    -- character: a raider who transferred realms mid-tier is why the log's spelling and
    -- the roster's spelling both have to survive.
    actor_name   text NOT NULL,
    actor_server text NOT NULL,
    class        text,
    -- NULL means nobody on the roster answers to this name. That is not an error and not
    -- a hole to fill: it is a pug, a trial who never registered, or the wrong log, and
    -- the read side reports it as one of the two mismatch sets.
    --
    -- SET NULL rather than CASCADE, so removing a character unlinks that raider from the
    -- night without deleting the guild's record that the night happened.
    character_id uuid REFERENCES characters (id) ON DELETE SET NULL,
    damage       bigint NOT NULL DEFAULT 0,
    healing      bigint NOT NULL DEFAULT 0,
    deaths       integer NOT NULL DEFAULT 0,
    PRIMARY KEY (event_id, actor_id)
);

-- Supports the SET NULL above, which otherwise scans this table per character removed.
CREATE INDEX event_report_raiders_character_id ON event_report_raiders (character_id);

-- +goose Down
DROP TABLE event_report_raiders;
DROP TABLE event_report_fights;
DROP TABLE event_reports;
DROP TYPE report_status;
