package warcraftlogs

import (
	"errors"
	"net/url"
	"strings"
)

// ErrNotAReport is returned when the URL a raid lead pasted is not a WarcraftLogs
// report. It is a caller error, so the API answers 400 rather than 500.
var ErrNotAReport = errors.New("not a warcraftlogs report url")

const reportHost = "warcraftlogs.com"

// ReportRef is a report URL split into the two things a fetch needs.
//
// Host is kept because the subdomain is which game version the report belongs to:
// classic and fresh sit alongside www, they are separate sites with separate reports,
// and a Classic guild's link is not on www.
type ReportRef struct {
	Host string
	Code string
}

// ParseReportURL splits a pasted URL into the host and report code behind it.
//
// Raid leads copy the address bar, which on WarcraftLogs carries the fight and player
// they were looking at when they hit copy (`#fight=12&source=4`). None of that reaches
// a ReportRef: only the report itself is a report.
func ParseReportURL(raw string) (ReportRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ReportRef{}, ErrNotAReport
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return ReportRef{}, ErrNotAReport
	}
	if parsed.Scheme != "https" {
		return ReportRef{}, ErrNotAReport
	}

	host := strings.ToLower(parsed.Hostname())
	if host != reportHost && !strings.HasSuffix(host, "."+reportHost) {
		return ReportRef{}, ErrNotAReport
	}

	code, ok := strings.CutPrefix(parsed.EscapedPath(), "/reports/")
	if !ok {
		return ReportRef{}, ErrNotAReport
	}
	code = strings.Trim(code, "/")
	// A report code is one path segment. Anything deeper is a page inside the report.
	if code == "" || strings.Contains(code, "/") {
		return ReportRef{}, ErrNotAReport
	}

	return ReportRef{Host: host, Code: code}, nil
}

// NormalizeURL checks that a pasted URL is a WarcraftLogs report and returns it
// stripped of the query string and fragment.
//
// The empty string is the caller's way of taking a log back off an event and passes
// through untouched. Nothing here contacts WarcraftLogs: whether the report exists is
// its problem, not this function's.
func NormalizeURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}

	ref, err := ParseReportURL(raw)
	if err != nil {
		return "", err
	}
	return ref.URL(), nil
}

// URL is the canonical address of the report this ref points at.
func (r ReportRef) URL() string {
	return "https://" + r.Host + "/reports/" + r.Code
}
