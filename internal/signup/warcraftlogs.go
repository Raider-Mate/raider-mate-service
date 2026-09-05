package signup

import (
	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

// ErrNotAWarcraftLogsReport is returned when the URL a raid lead pasted is not a
// WarcraftLogs report. It is a caller error, so the API answers 400 rather than 500.
var ErrNotAWarcraftLogsReport = warcraftlogs.ErrNotAReport

// NormalizeWarcraftLogsURL checks that a pasted URL is a WarcraftLogs report and
// returns it stripped of the query string and fragment.
//
// The rule itself lives in internal/warcraftlogs, beside the fetcher that has to split
// the same URL into a host and a report code. One parser, because a second one here
// would drift the moment either side learned something new about report links.
func NormalizeWarcraftLogsURL(raw string) (string, error) {
	return warcraftlogs.NormalizeURL(raw)
}
