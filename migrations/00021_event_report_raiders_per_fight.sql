-- +goose Up
-- Damage, healing and deaths per pull, not only over the night.
--
-- The night total was the whole of it at first, deliberately: per-fight tables cost about
-- three extra WarcraftLogs queries per pull, and a raid lead reading a damage board mostly
-- wants the night. It turns out they also want to click a wipe and see who was doing what
-- on it, which is the question the board could not answer.
--
-- fight_id 0 is the night, which is what every existing row already holds. A default of 0
-- rather than a nullable column because this is part of the key, and a key column cannot
-- be null.
ALTER TABLE event_report_raiders
    ADD COLUMN fight_id integer NOT NULL DEFAULT 0;

ALTER TABLE event_report_raiders
    DROP CONSTRAINT event_report_raiders_pkey;

ALTER TABLE event_report_raiders
    ADD PRIMARY KEY (event_id, fight_id, actor_id);

-- +goose Down
ALTER TABLE event_report_raiders
    DROP CONSTRAINT event_report_raiders_pkey;

DELETE FROM event_report_raiders WHERE fight_id <> 0;

ALTER TABLE event_report_raiders
    DROP COLUMN fight_id;

ALTER TABLE event_report_raiders
    ADD PRIMARY KEY (event_id, actor_id);
