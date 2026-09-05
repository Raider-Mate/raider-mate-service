package warcraftlogs

import (
	"errors"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain report", "https://www.warcraftlogs.com/reports/aBcD1234", "https://www.warcraftlogs.com/reports/aBcD1234"},
		{"no subdomain", "https://warcraftlogs.com/reports/aBcD1234", "https://warcraftlogs.com/reports/aBcD1234"},
		{"classic subdomain", "https://classic.warcraftlogs.com/reports/aBcD1234", "https://classic.warcraftlogs.com/reports/aBcD1234"},
		{"uppercase host", "https://WWW.WarcraftLogs.com/reports/aBcD1234", "https://www.warcraftlogs.com/reports/aBcD1234"},
		{"surrounding space", "  https://www.warcraftlogs.com/reports/aBcD1234  ", "https://www.warcraftlogs.com/reports/aBcD1234"},
		{"trailing slash", "https://www.warcraftlogs.com/reports/aBcD1234/", "https://www.warcraftlogs.com/reports/aBcD1234"},
		// What copying the address bar mid-analysis actually gives you.
		{"fragment dropped", "https://www.warcraftlogs.com/reports/aBcD1234#fight=12&type=damage-done", "https://www.warcraftlogs.com/reports/aBcD1234"},
		{"query dropped", "https://www.warcraftlogs.com/reports/aBcD1234?fight=3", "https://www.warcraftlogs.com/reports/aBcD1234"},
		{"empty takes the log off", "", ""},
		{"blank takes the log off", "   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			if err != nil {
				t.Fatalf("NormalizeURL(%q) returned %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeURLRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"another site", "https://raider.io/reports/aBcD1234"},
		// A lookalike domain is the whole reason this matches on the suffix with a dot
		// rather than on Contains.
		{"lookalike host", "https://notwarcraftlogs.com/reports/aBcD1234"},
		{"host as a path", "https://evil.example.com/warcraftlogs.com/reports/aBcD1234"},
		{"plain http", "http://www.warcraftlogs.com/reports/aBcD1234"},
		{"character page, not a report", "https://www.warcraftlogs.com/character/eu/silvermoon/someone"},
		{"no report code", "https://www.warcraftlogs.com/reports/"},
		{"a page inside a report", "https://www.warcraftlogs.com/reports/aBcD1234/deeper"},
		{"not a url", "this is not a url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			if !errors.Is(err, ErrNotAReport) {
				t.Fatalf("NormalizeURL(%q) = %q, %v; want ErrNotAReport", tt.in, got, err)
			}
		})
	}
}

// The host is half of what a fetch needs: classic and fresh are separate sites with
// separate reports, and a Classic guild's link is not on www.
func TestParseReportURLKeepsTheHost(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantCode string
	}{
		{"https://www.warcraftlogs.com/reports/aBcD1234", "www.warcraftlogs.com", "aBcD1234"},
		{"https://classic.warcraftlogs.com/reports/xyz", "classic.warcraftlogs.com", "xyz"},
		{"https://fresh.warcraftlogs.com/reports/xyz#fight=2", "fresh.warcraftlogs.com", "xyz"},
		{"https://warcraftlogs.com/reports/xyz", "warcraftlogs.com", "xyz"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ref, err := ParseReportURL(tt.in)
			if err != nil {
				t.Fatalf("ParseReportURL(%q): %v", tt.in, err)
			}
			if ref.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", ref.Host, tt.wantHost)
			}
			if ref.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", ref.Code, tt.wantCode)
			}
		})
	}
}

// An empty string is how a raid lead takes a log back off an event, which NormalizeURL
// passes through. It is not a report, so parsing it is an error.
func TestParseReportURLRejectsEmpty(t *testing.T) {
	if _, err := ParseReportURL(""); !errors.Is(err, ErrNotAReport) {
		t.Errorf("err = %v, want ErrNotAReport", err)
	}
}

func TestReportRefURLRoundTrips(t *testing.T) {
	const in = "https://classic.warcraftlogs.com/reports/aBcD1234"
	ref, err := ParseReportURL(in)
	if err != nil {
		t.Fatalf("ParseReportURL: %v", err)
	}
	if got := ref.URL(); got != in {
		t.Errorf("URL() = %q, want %q", got, in)
	}
}
