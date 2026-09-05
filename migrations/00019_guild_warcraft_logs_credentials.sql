-- +goose Up
-- A guild's own WarcraftLogs API client, so its report fetches come out of its own
-- hourly point budget rather than the instance's.
--
-- The budget is 3600 points an hour per client key, and a full report costs about
-- eighteen. One instance-wide key means every guild shares that, and a single large
-- guild logging four nights a week can spend it for everybody. A guild that supplies its
-- own gets its own ceiling; one that has not falls back to the instance pair, and an
-- instance with neither has the feature off for that guild.
ALTER TABLE guild_settings
    -- Not a secret. WarcraftLogs shows the client id in the clear on its own client
    -- page, and a raid lead has to be able to see which client is configured.
    ADD COLUMN warcraft_logs_client_id text,
    -- This one is. Sealed with AES-256-GCM under WARCRAFT_LOGS_ENCRYPTION_KEY before it
    -- reaches this column, and never selected into anything that is serialised to a
    -- client: the API returns the id and the timestamp below, and never the key itself.
    ADD COLUMN warcraft_logs_key_sealed bytea,
    -- When it was set, which is the only thing about the key a raid lead is ever shown.
    ADD COLUMN warcraft_logs_key_set_at timestamptz;

-- +goose Down
ALTER TABLE guild_settings
    DROP COLUMN warcraft_logs_client_id,
    DROP COLUMN warcraft_logs_key_sealed,
    DROP COLUMN warcraft_logs_key_set_at;
