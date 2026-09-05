-- +goose Up
-- Per-pull numbers get their own table.
--
-- Migration 00021 put them in event_report_raiders behind a fight_id of 0 for the night.
-- That was additive to the schema and destructive to anything already running: every
-- reader of that table predating the change had no fight_id filter, because until then
-- there was nothing to filter, so it read the night totals plus every pull as one list and
-- served each raider eleven times. A migration cannot assume the processes reading the
-- table restart with it.
--
-- A separate table is the version that cannot do that. Old code keeps reading
-- event_report_raiders and keeps seeing exactly what it always saw; new code reads this
-- one beside it. It is also the truer shape: the night total and one pull's numbers are
-- different facts with different lifetimes, and a sentinel id in a key column was always
-- going to be read wrong by somebody.
CREATE TABLE event_report_fight_raiders (
    event_id     uuid NOT NULL REFERENCES event_reports (event_id) ON DELETE CASCADE,
    -- WarcraftLogs' fight id inside the report. No sentinel: every row here is one pull.
    fight_id     integer NOT NULL,
    actor_id     integer NOT NULL,
    actor_name   text NOT NULL,
    actor_server text NOT NULL,
    class        text,
    -- SET NULL rather than CASCADE, matching the night table: removing a character
    -- unlinks them from the pull without deleting the record that the pull happened.
    character_id uuid REFERENCES characters (id) ON DELETE SET NULL,
    damage       bigint NOT NULL DEFAULT 0,
    healing      bigint NOT NULL DEFAULT 0,
    deaths       integer NOT NULL DEFAULT 0,
    PRIMARY KEY (event_id, fight_id, actor_id)
);

CREATE INDEX event_report_fight_raiders_character_id
    ON event_report_fight_raiders (character_id);

-- Move what 00021 wrote, rather than making every guild's report be read again.
INSERT INTO event_report_fight_raiders (
    event_id, fight_id, actor_id, actor_name, actor_server, class, character_id,
    damage, healing, deaths
)
SELECT event_id, fight_id, actor_id, actor_name, actor_server, class, character_id,
       damage, healing, deaths
FROM event_report_raiders
WHERE fight_id <> 0;

DELETE FROM event_report_raiders WHERE fight_id <> 0;

ALTER TABLE event_report_raiders DROP CONSTRAINT event_report_raiders_pkey;
ALTER TABLE event_report_raiders DROP COLUMN fight_id;
ALTER TABLE event_report_raiders ADD PRIMARY KEY (event_id, actor_id);

-- +goose Down
ALTER TABLE event_report_raiders DROP CONSTRAINT event_report_raiders_pkey;
ALTER TABLE event_report_raiders ADD COLUMN fight_id integer NOT NULL DEFAULT 0;
ALTER TABLE event_report_raiders ADD PRIMARY KEY (event_id, fight_id, actor_id);

INSERT INTO event_report_raiders (
    event_id, fight_id, actor_id, actor_name, actor_server, class, character_id,
    damage, healing, deaths
)
SELECT event_id, fight_id, actor_id, actor_name, actor_server, class, character_id,
       damage, healing, deaths
FROM event_report_fight_raiders;

DROP TABLE event_report_fight_raiders;
