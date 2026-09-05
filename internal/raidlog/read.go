package raidlog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNoReport means no report is attached to the event. Distinct from a report that is
// attached and unreadable, which has a status of its own.
var ErrNoReport = errors.New("no report attached")

// Stored is one raid night as it was read back from WarcraftLogs and kept.
type Stored struct {
	Status string
	// Live is true while WarcraftLogs still has a pull in progress. The numbers are real
	// but not final.
	Live      bool
	URL       string
	Title     *string
	Zone      *string
	StartsAt  *time.Time
	EndsAt    *time.Time
	FetchedAt *time.Time
	Fights    []StoredFight
	Raiders   []StoredRaider
	// PerFight is the same rows cut by pull, keyed by fight id. A client selecting a pull
	// reads from here rather than asking again.
	PerFight map[int32][]StoredRaider
	Turnout  Turnout
}

// StoredFight is one boss pull.
type StoredFight struct {
	FightID     int32
	Encounter   string
	Difficulty  *int32
	RaidSize    *int32
	Kill        bool
	BossPercent *float64
	StartsAt    time.Time
	EndsAt      time.Time
}

// Duration is how long the pull ran.
func (f StoredFight) Duration() time.Duration {
	return f.EndsAt.Sub(f.StartsAt)
}

// StoredRaider is one player's night.
type StoredRaider struct {
	// CharacterID is set when the log's actor matched a character on this guild's
	// roster. Absent means nobody on the roster answers to that name.
	CharacterID *uuid.UUID
	// Name and Realm are as the log recorded them, not as the roster spells them.
	Name    string
	Realm   string
	Class   *string
	Damage  int64
	Healing int64
	Deaths  int32
}

// Report reads back everything stored for one event.
func (s *Store) Report(ctx context.Context, eventID uuid.UUID, url string) (Stored, error) {
	row, err := s.queries.GetEventReport(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, ErrNoReport
	}
	if err != nil {
		return Stored{}, err
	}

	stored := Stored{
		Status:    string(row.Status),
		Live:      row.Live,
		URL:       url,
		Title:     row.Title,
		Zone:      row.ZoneName,
		StartsAt:  timePtr(row.ReportStartsAt),
		EndsAt:    timePtr(row.ReportEndsAt),
		FetchedAt: timePtr(row.FetchedAt),
	}

	fights, err := s.queries.ListEventReportFights(ctx, eventID)
	if err != nil {
		return Stored{}, err
	}
	for _, fight := range fights {
		stored.Fights = append(stored.Fights, StoredFight{
			FightID:     fight.FightID,
			Encounter:   fight.Name,
			Difficulty:  fight.Difficulty,
			RaidSize:    fight.RaidSize,
			Kill:        fight.Kill,
			BossPercent: floatPtr(fight.BossPercentage),
			StartsAt:    fight.StartsAt.Time,
			EndsAt:      fight.EndsAt.Time,
		})
	}

	raiders, err := s.queries.ListEventReportRaiders(ctx, eventID)
	if err != nil {
		return Stored{}, err
	}

	seen := map[uuid.UUID]bool{}
	for _, raider := range raiders {
		entry := StoredRaider{
			Name:    raider.ActorName,
			Realm:   raider.ActorServer,
			Class:   raider.Class,
			Damage:  raider.Damage,
			Healing: raider.Healing,
			Deaths:  raider.Deaths,
		}
		if raider.CharacterID.Valid {
			id := uuid.UUID(raider.CharacterID.Bytes)
			entry.CharacterID = &id
			if !seen[*entry.CharacterID] {
				seen[*entry.CharacterID] = true
				stored.Turnout.Attended = append(stored.Turnout.Attended, *entry.CharacterID)
			}
		} else {
			stored.Turnout.Unknown++
		}
		stored.Raiders = append(stored.Raiders, entry)
	}

	// The per-pull rows, from their own table. Only the night above feeds the turnout: a
	// raider turned up once, not once per pull they were in.
	perFight, err := s.queries.ListEventReportFightRaiders(ctx, eventID)
	if err != nil {
		return Stored{}, err
	}
	stored.PerFight = map[int32][]StoredRaider{}
	for _, raider := range perFight {
		entry := StoredRaider{
			Name:    raider.ActorName,
			Realm:   raider.ActorServer,
			Class:   raider.Class,
			Damage:  raider.Damage,
			Healing: raider.Healing,
			Deaths:  raider.Deaths,
		}
		if raider.CharacterID.Valid {
			id := uuid.UUID(raider.CharacterID.Bytes)
			entry.CharacterID = &id
		}
		stored.PerFight[raider.FightID] = append(stored.PerFight[raider.FightID], entry)
	}

	if len(stored.Raiders) > 0 {
		matched := len(stored.Raiders) - stored.Turnout.Unknown
		stored.Turnout.Overlap = float64(matched) / float64(len(stored.Raiders))
	}

	// Who said they were coming and is not in the log. Worked out on read rather than
	// stored, because a signup can change after the report was fetched and a stale
	// no-show list is worse than none.
	expected, err := s.queries.ListExpectedCharactersForEvent(ctx, eventID)
	if err != nil {
		return Stored{}, err
	}
	for _, id := range expected {
		if !seen[id] {
			stored.Turnout.Missing = append(stored.Turnout.Missing, id)
		}
	}

	return stored, nil
}

// StatusFor is the one status the event resource needs to pick a link rel. Empty when no
// report is attached, or when the instance never wrote a row because it has no
// WarcraftLogs credentials at all.
func (s *Store) StatusFor(ctx context.Context, eventID uuid.UUID) (string, error) {
	row, err := s.queries.GetEventReport(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(row.Status), nil
}

// StatusesFor is the same answer for a page of events, in one query, so a list of thirty
// does not run thirty of them.
func (s *Store) StatusesFor(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	rows, err := s.queries.GetEventReportStatuses(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		out[row.EventID] = string(row.Status)
	}
	return out, nil
}

// RequestRefresh puts a report back at the head of the queue.
func (s *Store) RequestRefresh(ctx context.Context, eventID uuid.UUID) error {
	return s.Reschedule(ctx, eventID, time.Now())
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	at := t.Time
	return &at
}

// floatPtr turns a stored percentage back into a number. Absent stays absent: a fight
// WarcraftLogs could not measure has no boss percentage, and zero would read as a kill.
func floatPtr(n pgtype.Numeric) *float64 {
	if !n.Valid || n.NaN {
		return nil
	}
	value, err := n.Float64Value()
	if err != nil || !value.Valid {
		return nil
	}
	return &value.Float64
}
