package warcraftlogs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/report.json is a real response, captured from a real public report
// (PW92KGB63pNm4gMA, a 21-pull progression night) and trimmed of the per-ability
// breakdowns nothing here reads. The real WarcraftLogs API is never called from a test.
const fixture = "testdata/report.json"

// serve stands up a WarcraftLogs that hands out a token and then answers the report
// query with whatever body is given.
func serve(t *testing.T, body string, status int) (*Client, *int) {
	t.Helper()
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			tokenCalls++
			id, secret, ok := r.BasicAuth()
			if !ok || id != "id" || secret != "key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":31536000}`))
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		w.WriteHeader(status)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return NewClient("id", "key", server.URL, 0), &tokenCalls
}

func fixtureBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(raw)
}

func TestFetchParsesARealReport(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "PW92KGB63pNm4gMA"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(report.Fights) != 21 {
		t.Errorf("fights = %d, want 21", len(report.Fights))
	}
	if report.Visibility != "public" {
		t.Errorf("visibility = %q, want public", report.Visibility)
	}
	if report.Live {
		t.Error("Live = true on a finished report")
	}
	if report.ZoneName == nil || *report.ZoneName == "" {
		t.Error("zone name missing")
	}

	kills := 0
	for _, fight := range report.Fights {
		if fight.Kill {
			kills++
		}
	}
	if kills != 4 {
		t.Errorf("kills = %d, want 4", kills)
	}
}

// The relative-to-absolute conversion is the likeliest bug in this package: fight times
// arrive as milliseconds from the report's start, not as timestamps.
func TestFetchMakesFightTimesAbsolute(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for _, fight := range report.Fights {
		if fight.StartsAt.Before(report.StartsAt) {
			t.Fatalf("fight %d starts %s, before the report at %s", fight.ID, fight.StartsAt, report.StartsAt)
		}
		if fight.EndsAt.After(report.EndsAt) {
			t.Fatalf("fight %d ends %s, after the report at %s", fight.ID, fight.EndsAt, report.EndsAt)
		}
		if !fight.EndsAt.After(fight.StartsAt) {
			t.Fatalf("fight %d ends before it starts", fight.ID)
		}
		// A pull measured from the epoch is what a missed conversion looks like.
		if fight.StartsAt.Year() < 2020 {
			t.Fatalf("fight %d starts in %d, so the report offset was not applied", fight.ID, fight.StartsAt.Year())
		}
	}
}

// Deaths is one row per death, not one row per actor with a count. Nothing else in the
// response says how many times somebody died.
func TestFetchCountsDeathsPerRaider(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	total := int32(0)
	worst := int32(0)
	for _, raider := range report.Raiders {
		total += raider.Deaths
		if raider.Deaths > worst {
			worst = raider.Deaths
		}
	}
	if total != 200 {
		t.Errorf("deaths across the night = %d, want 200", total)
	}
	if worst != 15 {
		t.Errorf("worst raider died %d times, want 15", worst)
	}
}

// The tables are the roll call, not masterData: the report saw 81 actors, but only the
// ones who were in a boss pull belong on a damage board.
func TestFetchTakesRaidersFromTheTablesNotMasterData(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(report.Raiders) == 0 || len(report.Raiders) >= 81 {
		t.Fatalf("raiders = %d, want more than none and fewer than the 81 actors in the report", len(report.Raiders))
	}

	withDamage := 0
	for _, raider := range report.Raiders {
		if raider.Damage > 0 {
			withDamage++
		}
		if raider.Name == "" {
			t.Errorf("raider %d has no name", raider.ID)
		}
	}
	if withDamage == 0 {
		t.Error("nobody did any damage, so the damage table was not read")
	}
}

// Names and realms come off the log as the log spelled them, including scripts that are
// not Latin. A parser that mangles them matches nobody on the roster.
func TestFetchKeepsNonLatinNamesAndServers(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	servers := 0
	for _, raider := range report.Raiders {
		if raider.Server != "" {
			servers++
		}
	}
	if servers == 0 {
		t.Fatal("no raider carried a server, so masterData was not folded in")
	}
}

// Two fetches, one token exchange.
func TestTokenIsCachedBetweenFetches(t *testing.T) {
	client, tokenCalls := serve(t, fixtureBody(t), http.StatusOK)

	for range 2 {
		if _, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	if *tokenCalls != 1 {
		t.Errorf("token exchanges = %d, want 1", *tokenCalls)
	}
}

func TestFetchReadsTheBudgetFromTheSameResponse(t *testing.T) {
	client, _ := serve(t, fixtureBody(t), http.StatusOK)

	_, budget, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if budget.LimitPerHour != 3600 {
		t.Errorf("limit = %d, want 3600", budget.LimitPerHour)
	}
	if budget.SpentThisHour <= 0 {
		t.Errorf("spent = %v, want the cost of the query just made", budget.SpentThisHour)
	}
	if budget.ResetIn <= 0 || budget.ResetIn > time.Hour {
		t.Errorf("resetIn = %v, want inside the hour", budget.ResetIn)
	}
}

func TestBudgetSpent(t *testing.T) {
	tests := []struct {
		name   string
		budget Budget
		share  float64
		want   bool
	}{
		{"fresh", Budget{LimitPerHour: 3600, SpentThisHour: 10}, 0.9, false},
		{"at the line", Budget{LimitPerHour: 3600, SpentThisHour: 3240}, 0.9, true},
		{"past it", Budget{LimitPerHour: 3600, SpentThisHour: 3599}, 0.9, true},
		// Nothing observed yet is not the same as nothing left.
		{"unknown limit", Budget{}, 0.9, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.budget.Spent(tt.share); got != tt.want {
				t.Errorf("Spent(%v) = %v, want %v", tt.share, got, tt.want)
			}
		})
	}
}

func TestFetchMapsFailuresToSentinels(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "no such report",
			status: http.StatusOK,
			body:   `{"data":{"reportData":{"report":null}},"errors":[{"message":"Report not found."}]}`,
			want:   ErrReportNotFound,
		},
		{
			name:   "private report",
			status: http.StatusOK,
			body:   `{"data":{"reportData":{"report":null}},"errors":[{"message":"You do not have permission to view this private report."}]}`,
			want:   ErrReportPrivate,
		},
		{
			name:   "private by visibility",
			status: http.StatusOK,
			body:   `{"data":{"reportData":{"report":{"code":"x","visibility":"private"}}}}`,
			want:   ErrReportPrivate,
		},
		{
			name:   "archived and unreadable",
			status: http.StatusOK,
			body:   `{"data":{"reportData":{"report":{"code":"x","visibility":"public","archiveStatus":{"isArchived":true,"isAccessible":false}}}}}`,
			want:   ErrReportArchived,
		},
		{
			name:   "rate limited",
			status: http.StatusTooManyRequests,
			body:   `{}`,
			want:   ErrRateLimited,
		},
		{
			name:   "credentials rejected",
			status: http.StatusUnauthorized,
			body:   `{}`,
			want:   ErrInvalidCredentials,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := serve(t, tt.body, tt.status)
			_, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// An archived report whose detail is still readable is a normal report.
func TestFetchAcceptsAnArchivedButAccessibleReport(t *testing.T) {
	body := `{"data":{"reportData":{"report":{"code":"x","visibility":"public","archiveStatus":{"isArchived":true,"isAccessible":true},"fights":[]}}}}`
	client, _ := serve(t, body, http.StatusOK)

	if _, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"}); err != nil {
		t.Errorf("Fetch: %v", err)
	}
}

func TestTokenExchangeRejectsBadCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClient("wrong", "wrong", server.URL, 0)
	_, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want ErrInvalidCredentials", err)
	}
}

// A live report is one with a pull still running, and the flag has to survive whatever
// else the report says.
func TestFetchMarksALiveReport(t *testing.T) {
	body := `{"data":{"reportData":{"report":{"code":"x","visibility":"public","startTime":1700000000000,"endTime":1700000600000,"fights":[{"id":1,"name":"Boss","startTime":0,"endTime":60000,"inProgress":true}]}}}}`
	client, _ := serve(t, body, http.StatusOK)

	report, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !report.Live {
		t.Error("Live = false with a pull in progress")
	}
}

// The query is sent as one document, and every table needs a window: WarcraftLogs
// rejects a table query carrying neither fightIDs nor a start and end.
func TestFetchSendsAWindowWithEveryTable(t *testing.T) {
	var sent struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"data":{"reportData":{"report":{"code":"x","visibility":"public","fights":[]}}}}`))
	}))
	defer server.Close()

	client := NewClient("id", "key", server.URL, 0)
	if _, _, err := client.Fetch(context.Background(), ReportRef{Host: "www.warcraftlogs.com", Code: "abc"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got := strings.Count(sent.Query, "endTime: $end"); got != 3 {
		t.Errorf("tables carrying a window = %d, want 3", got)
	}
	if sent.Variables["code"] != "abc" {
		t.Errorf("code = %v, want abc", sent.Variables["code"])
	}
	if _, ok := sent.Variables["end"]; !ok {
		t.Error("no end variable sent")
	}
}
