// Package raidlog reads a WarcraftLogs report a raid lead attached to an event and
// keeps what it said: the pulls, damage and healing per raider over the night, deaths,
// and who the log actually saw against who said they were coming.
//
// It reads a log. It plans nothing. There is no cooldown timeline here, no per-boss
// assignment, no note export and no parse colour, and those stay out.
package raidlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

// reportFetcher is the WarcraftLogs dependency the ingestor needs. Declared here, by the
// consumer, rather than on the warcraftlogs package.
type reportFetcher interface {
	Fetch(ctx context.Context, ref warcraftlogs.ReportRef) (warcraftlogs.Report, warcraftlogs.Budget, error)
}

// Credentials is one WarcraftLogs API client, and which guild's budget it spends.
type Credentials struct {
	ClientID string
	Key      string
	// Guild is true when these came from the guild's own settings rather than the
	// instance's. Only used for logging: a guild fetching on its own key and a guild
	// falling back to the instance's fail differently and an operator needs to see
	// which.
	Guild bool
}

// Due is one report the worker has claimed and now has to refresh.
type Due struct {
	EventID uuid.UUID
	GuildID int64
	Ref     warcraftlogs.ReportRef
	// Revision is what was stored last time, nil on a report never read. A fetch that
	// comes back with the same number returned the same numbers, which is how a night is
	// known to have stopped moving.
	Revision *int32
}

// Fetched is everything one successful read produced, ready to store.
type Fetched struct {
	Report  warcraftlogs.Report
	Matched []Matched
	// PerFight is the same matching applied to each pull, keyed by fight id.
	PerFight map[int32][]Matched
	Turnout  Turnout
	// NextAttempt is nil when the report has settled and the worker should stop asking.
	NextAttempt *time.Time
}

// ingestStore is the persistence the ingestor needs. Declared here, by the consumer.
type ingestStore interface {
	ClaimDue(ctx context.Context, limit int32) ([]Due, error)
	// CredentialsFor resolves the guild's own WarcraftLogs client, falling back to the
	// instance's. ok is false when neither is configured, which is not an error: it is
	// an instance that has not turned the feature on.
	CredentialsFor(ctx context.Context, guildID int64) (creds Credentials, ok bool, err error)
	// RosterFor is the characters the log's actors are matched against.
	RosterFor(ctx context.Context, guildID int64) ([]RosterCharacter, error)
	// ExpectedFor is the characters whose signup says they were coming. Tentative
	// signups are not in it: "maybe" was never a promise.
	ExpectedFor(ctx context.Context, eventID uuid.UUID) ([]uuid.UUID, error)
	Store(ctx context.Context, eventID uuid.UUID, fetched Fetched) error
	MarkFailed(ctx context.Context, eventID uuid.UUID, status string, reason string, next *time.Time) error
	Reschedule(ctx context.Context, eventID uuid.UUID, next time.Time) error
}

// Ingestor refreshes stored reports from WarcraftLogs.
type Ingestor struct {
	fetcher reportFetcher
	store   ingestStore
	logger  *slog.Logger

	// liveRefresh is how often a report still being written to is re-read.
	liveRefresh time.Duration

	// budgets is the last observed point allowance per client id. Per credential rather
	// than per instance, which is the whole point of letting a guild bring its own: one
	// guild's spending must never starve another's.
	//
	// In memory rather than a table because the worker is one process, and a restart
	// simply learns the number again on its next fetch.
	budgets map[string]warcraftlogs.Budget
}

// budgetGuard is the share of the hourly allowance at which the ingestor stops spending
// on a credential. Nine tenths leaves room for the fetch already in flight.
const budgetGuard = 0.9

// maxBackoff caps the wait after a transient failure. A report that WarcraftLogs could
// not serve six hours ago is worth one more try, not one every five minutes.
const maxBackoff = 6 * time.Hour

func NewIngestor(fetcher reportFetcher, store ingestStore, liveRefresh time.Duration, logger *slog.Logger) *Ingestor {
	return &Ingestor{
		fetcher:     fetcher,
		store:       store,
		logger:      logger,
		liveRefresh: liveRefresh,
		budgets:     map[string]warcraftlogs.Budget{},
	}
}

// FetchDue refreshes up to limit reports whose next attempt is due.
//
// One report's failure does not stop the batch. A spent budget or a rejected credential
// stops the rest of that guild's work but not another guild's, because with per-guild
// keys those are facts about one credential rather than about WarcraftLogs.
func (i *Ingestor) FetchDue(ctx context.Context, limit int32) error {
	due, err := i.store.ClaimDue(ctx, limit)
	if err != nil {
		return fmt.Errorf("claiming reports due: %w", err)
	}

	// Credentials that ran out of budget or were rejected part way through this batch.
	// Everything left on them goes back on the queue untouched rather than being burned.
	blocked := map[string]time.Time{}

	for _, report := range due {
		creds, ok, err := i.store.CredentialsFor(ctx, report.GuildID)
		if err != nil {
			i.logger.ErrorContext(ctx, "resolving warcraftlogs credentials",
				"event_id", report.EventID, "error", err)
			continue
		}
		if !ok {
			// The instance turned the feature off, or a guild cleared its key and the
			// instance has none. Not a failure of this report: park it and stop asking.
			i.parkUnconfigured(ctx, report)
			continue
		}

		if until, stopped := blocked[creds.ClientID]; stopped {
			i.reschedule(ctx, report.EventID, until)
			continue
		}
		if budget, seen := i.budgets[creds.ClientID]; seen && budget.Spent(budgetGuard) {
			until := time.Now().Add(budget.ResetIn)
			blocked[creds.ClientID] = until
			i.logger.WarnContext(ctx, "warcraftlogs budget nearly spent, backing off",
				"guild_key", creds.Guild, "resets_in", budget.ResetIn)
			i.reschedule(ctx, report.EventID, until)
			continue
		}

		budget, err := i.fetchOne(ctx, report)
		if budget.LimitPerHour > 0 {
			i.budgets[creds.ClientID] = budget
		}
		if err == nil {
			continue
		}

		switch {
		case errors.Is(err, warcraftlogs.ErrRateLimited):
			// WarcraftLogs just said exactly when it will answer again.
			until := time.Now().Add(resetIn(budget))
			blocked[creds.ClientID] = until
			i.reschedule(ctx, report.EventID, until)
		case errors.Is(err, warcraftlogs.ErrInvalidCredentials):
			// An operator problem, or a guild that pasted a key wrong. Either way every
			// remaining fetch on this credential fails identically, and no raider needs
			// to see it.
			until := time.Now().Add(time.Hour)
			blocked[creds.ClientID] = until
			i.logger.ErrorContext(ctx, "warcraftlogs rejected the credentials",
				"guild_id", report.GuildID, "guild_key", creds.Guild)
			i.reschedule(ctx, report.EventID, until)
		default:
			i.logger.ErrorContext(ctx, "fetching warcraftlogs report",
				"event_id", report.EventID, "error", err)
		}
	}

	return nil
}

// fetchOne reads one report and stores what it said, or records why it could not.
func (i *Ingestor) fetchOne(ctx context.Context, due Due) (warcraftlogs.Budget, error) {
	report, budget, err := i.fetcher.Fetch(ctx, due.Ref)
	if err != nil {
		status, terminal := statusFor(err)
		if status == "" {
			return budget, err
		}
		var next *time.Time
		if !terminal {
			when := time.Now().Add(backoff(1))
			next = &when
		}
		if markErr := i.store.MarkFailed(ctx, due.EventID, status, err.Error(), next); markErr != nil {
			i.logger.ErrorContext(ctx, "recording report failure", "event_id", due.EventID, "error", markErr)
		}
		// Handled: the row now says what happened and a client can render it.
		return budget, nil
	}

	roster, err := i.store.RosterFor(ctx, due.GuildID)
	if err != nil {
		return budget, fmt.Errorf("reading roster: %w", err)
	}
	expected, err := i.store.ExpectedFor(ctx, due.EventID)
	if err != nil {
		return budget, fmt.Errorf("reading who was expected: %w", err)
	}

	matched := Match(report.Raiders, roster)
	perFight := map[int32][]Matched{}
	for fightID, actors := range report.PerFight {
		perFight[fightID] = Match(actors, roster)
	}
	fetched := Fetched{
		Report:   report,
		Matched:  matched,
		PerFight: perFight,
		Turnout:  Reckon(matched, expected),
	}

	// Settled when the report stopped changing and no pull is still running. A report
	// from a raid three months ago cannot change, so re-reading it would spend budget to
	// learn nothing.
	moved := due.Revision == nil || *due.Revision != report.Revision
	if moved || report.Live {
		when := time.Now().Add(i.liveRefresh)
		fetched.NextAttempt = &when
	}

	if err := i.store.Store(ctx, due.EventID, fetched); err != nil {
		return budget, fmt.Errorf("storing report: %w", err)
	}
	return budget, nil
}

// parkUnconfigured stops asking about a report the instance cannot fetch. Nothing is
// wrong with the report, so it keeps whatever status it had.
func (i *Ingestor) parkUnconfigured(ctx context.Context, due Due) {
	if err := i.store.MarkFailed(ctx, due.EventID, "UNAVAILABLE", "no warcraftlogs credentials configured", nil); err != nil {
		i.logger.ErrorContext(ctx, "parking an unconfigured report", "event_id", due.EventID, "error", err)
	}
}

func (i *Ingestor) reschedule(ctx context.Context, eventID uuid.UUID, until time.Time) {
	if err := i.store.Reschedule(ctx, eventID, until); err != nil {
		i.logger.ErrorContext(ctx, "rescheduling a report", "event_id", eventID, "error", err)
	}
}

// statusFor maps a fetch error to the status a client renders, and whether the ingestor
// should stop retrying. An empty status means the error is not about this report.
func statusFor(err error) (status string, terminal bool) {
	switch {
	case errors.Is(err, warcraftlogs.ErrReportNotFound):
		return "NOT_FOUND", true
	case errors.Is(err, warcraftlogs.ErrReportPrivate):
		// Terminal because reaching a private report needs the report owner's own OAuth
		// consent, which this service never asks for. The recovery is theirs: set it to
		// unlisted, which is one click on the report page.
		return "PRIVATE", true
	case errors.Is(err, warcraftlogs.ErrReportArchived):
		return "ARCHIVED", true
	case errors.Is(err, warcraftlogs.ErrRateLimited), errors.Is(err, warcraftlogs.ErrInvalidCredentials):
		// Facts about the credential, not the report. The caller handles them.
		return "", false
	default:
		return "UNAVAILABLE", false
	}
}

// backoff is the wait after a transient failure, doubling per attempt to a cap.
func backoff(attempts int16) time.Duration {
	wait := 5 * time.Minute
	for range attempts {
		wait *= 2
		if wait >= maxBackoff {
			return maxBackoff
		}
	}
	return wait
}

// resetIn is how long WarcraftLogs said the budget takes to come back, with a floor so
// a missing number does not turn into a hot loop.
func resetIn(budget warcraftlogs.Budget) time.Duration {
	if budget.ResetIn <= 0 {
		return 15 * time.Minute
	}
	return budget.ResetIn
}
