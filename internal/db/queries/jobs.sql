-- name: ScheduleJob :exec
INSERT INTO scheduled_jobs (id, event_id, job_type, run_at)
VALUES ($1, $2, $3, $4);

-- name: ClaimDueJobs :many
SELECT * FROM scheduled_jobs
WHERE status = 'PENDING' AND run_at <= now()
ORDER BY run_at
LIMIT sqlc.arg(row_limit)
FOR UPDATE SKIP LOCKED;

-- name: MarkJobSent :exec
UPDATE scheduled_jobs SET status = 'SENT'
WHERE id = $1;

-- name: MarkJobSkipped :exec
-- Done, but it notified nobody, and the reason says why. Status stays SENT because the
-- job is finished either way; what changes is that the row no longer claims a send it
-- did not make.
UPDATE scheduled_jobs SET status = 'SENT', skip_reason = $2
WHERE id = $1;

-- name: MarkJobFailed :exec
-- Caller decides PENDING (retry) or FAILED (give up) based on the attempts it read
-- off the claimed row; this just records the attempt.
UPDATE scheduled_jobs SET status = $2, attempts = attempts + 1
WHERE id = $1;

-- name: CancelJobsForEvent :exec
UPDATE scheduled_jobs SET status = 'CANCELED'
WHERE event_id = $1 AND status = 'PENDING';

-- name: GetPreEventReminderJob :one
-- The pre-event reminder an event currently has, for reporting what became of it.
-- An edit cancels the old jobs and schedules new ones, so a long-lived event has more
-- than one row here: the live one is what to report, and CANCELED sorts last.
SELECT * FROM scheduled_jobs
WHERE event_id = $1 AND job_type = 'REMINDER_PRE_EVENT'
ORDER BY (status = 'CANCELED'), run_at DESC
LIMIT 1;
