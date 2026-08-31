-- +goose Up
-- A job that finished without notifying anybody looked exactly like one that reminded
-- the whole raid: both ended SENT. A pre-event ping for an event with no channel to
-- post in, a deadline nobody could be told about, a kind that is no longer sent, all
-- of them recorded success. That is why a reminder nobody received could go unnoticed
-- for weeks and why a fix could not be confirmed from the data.
--
-- NULL means the job did what it says: it wrote notifications. Anything else names why
-- it did not. No backfill, because the rows already SENT cannot say which they were.
ALTER TABLE scheduled_jobs ADD COLUMN skip_reason text;

-- +goose Down
ALTER TABLE scheduled_jobs DROP COLUMN skip_reason;
