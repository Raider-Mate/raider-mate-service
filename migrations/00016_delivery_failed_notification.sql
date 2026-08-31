-- +goose Up
-- What the bot could not deliver. Discord refuses a send for reasons the service cannot
-- see: the bot lost Send Messages in the events channel, the channel was deleted, a
-- raider closed their DMs. Until now the bot logged that once and acked the row, so the
-- reminder was gone and the guild had no way to learn it never arrived.
--
-- The report goes to the raid leads, in the events channel, because a raid lead is
-- known here as a role id rather than as a person to DM. If the events channel is
-- itself the broken thing then the report cannot land either; scheduled_jobs.skip_reason
-- and the dashboard's event page still show it.
--
-- Only the value is added here. Postgres refuses to use a new enum value in the
-- transaction that created it, and goose runs a migration in one.
ALTER TYPE notification_kind ADD VALUE 'DELIVERY_FAILED';

-- +goose Down
-- Postgres cannot drop an enum value. Leaving it is harmless once the emitting query
-- is gone.
SELECT 1;
