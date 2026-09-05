package raidlog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

type fakeFetcher struct {
	report warcraftlogs.Report
	budget warcraftlogs.Budget
	err    error
	calls  int
}

func (f *fakeFetcher) Fetch(context.Context, warcraftlogs.ReportRef) (warcraftlogs.Report, warcraftlogs.Budget, error) {
	f.calls++
	return f.report, f.budget, f.err
}

type fakeStore struct {
	due     []Due
	creds   Credentials
	hasCred bool

	stored      map[uuid.UUID]Fetched
	failed      map[uuid.UUID]string
	rescheduled map[uuid.UUID]time.Time
}

func newStore(due ...Due) *fakeStore {
	return &fakeStore{
		due:         due,
		creds:       Credentials{ClientID: "id", Key: "key"},
		hasCred:     true,
		stored:      map[uuid.UUID]Fetched{},
		failed:      map[uuid.UUID]string{},
		rescheduled: map[uuid.UUID]time.Time{},
	}
}

func (s *fakeStore) ClaimDue(context.Context, int32) ([]Due, error) { return s.due, nil }

func (s *fakeStore) CredentialsFor(context.Context, int64) (Credentials, bool, error) {
	return s.creds, s.hasCred, nil
}

func (s *fakeStore) RosterFor(context.Context, int64) ([]RosterCharacter, error) { return nil, nil }

func (s *fakeStore) ExpectedFor(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }

func (s *fakeStore) Store(_ context.Context, eventID uuid.UUID, fetched Fetched) error {
	s.stored[eventID] = fetched
	return nil
}

func (s *fakeStore) MarkFailed(_ context.Context, eventID uuid.UUID, status, _ string, _ *time.Time) error {
	s.failed[eventID] = status
	return nil
}

func (s *fakeStore) Reschedule(_ context.Context, eventID uuid.UUID, next time.Time) error {
	s.rescheduled[eventID] = next
	return nil
}

func ingestor(fetcher reportFetcher, store ingestStore) *Ingestor {
	return NewIngestor(fetcher, store, 15*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func due(revision *int32) Due {
	return Due{
		EventID:  uuid.New(),
		GuildID:  1,
		Ref:      warcraftlogs.ReportRef{Host: "www.warcraftlogs.com", Code: "abc"},
		Revision: revision,
	}
}

func rev(n int32) *int32 { return &n }

// A report that stopped changing is settled: the worker stops asking, permanently. A
// raid from three months ago cannot change, and re-reading it spends budget to learn
// nothing.
func TestUnchangedRevisionSettlesTheRow(t *testing.T) {
	report := due(rev(7))
	store := newStore(report)
	fetcher := &fakeFetcher{report: warcraftlogs.Report{Revision: 7}}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	stored, ok := store.stored[report.EventID]
	if !ok {
		t.Fatal("nothing stored")
	}
	if stored.NextAttempt != nil {
		t.Errorf("NextAttempt = %v, want nil: the report settled", stored.NextAttempt)
	}
}

func TestNewRevisionSchedulesAnotherRead(t *testing.T) {
	report := due(rev(7))
	store := newStore(report)
	fetcher := &fakeFetcher{report: warcraftlogs.Report{Revision: 8}}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if store.stored[report.EventID].NextAttempt == nil {
		t.Error("NextAttempt = nil, want another read: the report was still moving")
	}
}

// A pull still running keeps the report live even when the revision did not move.
func TestAPullInProgressKeepsAsking(t *testing.T) {
	report := due(rev(7))
	store := newStore(report)
	fetcher := &fakeFetcher{report: warcraftlogs.Report{Revision: 7, Live: true}}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if store.stored[report.EventID].NextAttempt == nil {
		t.Error("NextAttempt = nil, want another read: a pull is still running")
	}
}

func TestFailuresMapToAStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"private", warcraftlogs.ErrReportPrivate, "PRIVATE"},
		{"deleted", warcraftlogs.ErrReportNotFound, "NOT_FOUND"},
		{"archived", warcraftlogs.ErrReportArchived, "ARCHIVED"},
		{"anything else", errors.New("boom"), "UNAVAILABLE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := due(nil)
			store := newStore(report)
			fetcher := &fakeFetcher{err: tt.err}

			if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
				t.Fatalf("FetchDue: %v", err)
			}
			if got := store.failed[report.EventID]; got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
			if _, stored := store.stored[report.EventID]; stored {
				t.Error("a failed fetch stored numbers")
			}
		})
	}
}

// A spent budget is a fact about the credential, not about any one report, so the rest
// of the batch goes back on the queue rather than being burned.
func TestRateLimitStopsTheBatchWithoutFailingReports(t *testing.T) {
	first, second := due(nil), due(nil)
	store := newStore(first, second)
	fetcher := &fakeFetcher{
		err:    warcraftlogs.ErrRateLimited,
		budget: warcraftlogs.Budget{LimitPerHour: 3600, ResetIn: 20 * time.Minute},
	}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1: the batch should stop at the first rate limit", fetcher.calls)
	}
	if len(store.failed) != 0 {
		t.Errorf("failed = %v, want none: nothing was learned about these reports", store.failed)
	}
	if len(store.rescheduled) != 2 {
		t.Errorf("rescheduled = %d, want both back on the queue", len(store.rescheduled))
	}
}

// Rejected credentials are an operator problem. No raider needs to see it, and every
// remaining fetch on that credential would fail identically.
func TestRejectedCredentialsStopTheBatchQuietly(t *testing.T) {
	first, second := due(nil), due(nil)
	store := newStore(first, second)
	fetcher := &fakeFetcher{err: warcraftlogs.ErrInvalidCredentials}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1", fetcher.calls)
	}
	if len(store.failed) != 0 {
		t.Errorf("failed = %v, want none", store.failed)
	}
}

// The guard uses the budget observed on the previous fetch, so the second report in a
// batch never walks into a 429 the first one already saw coming.
func TestBudgetGuardStopsBeforeTheLimit(t *testing.T) {
	first, second := due(nil), due(nil)
	store := newStore(first, second)
	fetcher := &fakeFetcher{
		report: warcraftlogs.Report{Revision: 1},
		budget: warcraftlogs.Budget{LimitPerHour: 3600, SpentThisHour: 3500, ResetIn: 10 * time.Minute},
	}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1: the guard should stop the second", fetcher.calls)
	}
	if _, put := store.rescheduled[second.EventID]; !put {
		t.Error("the second report was not put back on the queue")
	}
}

// An instance with no credentials is a supported configuration, not a broken report.
func TestNoCredentialsParksTheReport(t *testing.T) {
	report := due(nil)
	store := newStore(report)
	store.hasCred = false
	fetcher := &fakeFetcher{}

	if err := ingestor(fetcher, store).FetchDue(context.Background(), 10); err != nil {
		t.Fatalf("FetchDue: %v", err)
	}

	if fetcher.calls != 0 {
		t.Errorf("fetches = %d, want none", fetcher.calls)
	}
	if store.failed[report.EventID] != "UNAVAILABLE" {
		t.Errorf("status = %q, want UNAVAILABLE", store.failed[report.EventID])
	}
}

func TestBackoffDoublesToACap(t *testing.T) {
	if got := backoff(0); got != 5*time.Minute {
		t.Errorf("backoff(0) = %v, want 5m", got)
	}
	if got := backoff(1); got != 10*time.Minute {
		t.Errorf("backoff(1) = %v, want 10m", got)
	}
	if got := backoff(20); got != maxBackoff {
		t.Errorf("backoff(20) = %v, want the cap %v", got, maxBackoff)
	}
}

// A missing reset window must not turn into a hot loop.
func TestResetInHasAFloor(t *testing.T) {
	if got := resetIn(warcraftlogs.Budget{}); got != 15*time.Minute {
		t.Errorf("resetIn on an empty budget = %v, want 15m", got)
	}
	if got := resetIn(warcraftlogs.Budget{ResetIn: time.Hour}); got != time.Hour {
		t.Errorf("resetIn = %v, want what WarcraftLogs said", got)
	}
}
