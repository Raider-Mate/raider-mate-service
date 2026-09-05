-- +goose Up
-- Every event that already had a WarcraftLogs report attached before there was anything
-- to read it with.
--
-- The queue row is normally written by the PATCH that stores the URL, so without this
-- backfill a log linked yesterday would sit there forever with nothing ever fetching it:
-- the row only appears when somebody re-saves the same URL, which nobody has a reason to
-- do.
--
-- The host and code are split out of the stored URL here rather than in Go, because the
-- alternative is a startup task that runs once and then lives in the codebase forever.
-- The URL was normalised on the way in (https, a warcraftlogs.com host, /reports/<code>
-- and nothing after it), so the two substrings below are the whole of the parse.
INSERT INTO event_reports (event_id, host, code, status, next_attempt_at)
SELECT
    e.id,
    -- Between "https://" and the next slash.
    split_part(substring(e.warcraftlogs_url FROM 9), '/', 1),
    -- The last path segment, which for a normalised report URL is the report code.
    split_part(e.warcraftlogs_url, '/', 5),
    'PENDING',
    now()
FROM events e
WHERE e.warcraftlogs_url IS NOT NULL
  -- Defensive: a URL that does not split into a host and a code would queue a fetch for
  -- something that is not a report.
  AND split_part(substring(e.warcraftlogs_url FROM 9), '/', 1) <> ''
  AND split_part(e.warcraftlogs_url, '/', 5) <> ''
ON CONFLICT (event_id) DO NOTHING;

-- +goose Down
-- Only the rows this backfill could have created, and only while they are untouched. A
-- report that has since been read is real data now, not a backfill artefact.
DELETE FROM event_reports WHERE status = 'PENDING' AND fetched_at IS NULL;
