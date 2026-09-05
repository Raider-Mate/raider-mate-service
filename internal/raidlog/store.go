package raidlog

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/secretbox"
	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

// Store implements ingestStore over Postgres, and is also the read side the API uses.
type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	box     *secretbox.Box
	// fallback is the instance's own WarcraftLogs client, used by a guild that has not
	// supplied one. Empty when the instance has none, which turns the feature off rather
	// than breaking it.
	fallback Credentials
}

func NewStore(pool *pgxpool.Pool, box *secretbox.Box, clientID, key string) *Store {
	return &Store{
		pool:     pool,
		queries:  db.New(pool),
		box:      box,
		fallback: Credentials{ClientID: clientID, Key: key},
	}
}

func (s *Store) ClaimDue(ctx context.Context, limit int32) ([]Due, error) {
	rows, err := s.queries.ClaimDueReports(ctx, limit)
	if err != nil {
		return nil, err
	}

	out := make([]Due, 0, len(rows))
	for _, row := range rows {
		// The guild is on the event, not on the report row: a report belongs to a night,
		// and a night belongs to a guild.
		event, err := s.queries.GetEvent(ctx, row.EventID)
		if err != nil {
			// The event went away between the claim and here. The cascade will have
			// taken the report row with it.
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("reading the event behind a report: %w", err)
		}
		out = append(out, Due{
			EventID:  row.EventID,
			GuildID:  event.DiscordGuildID,
			Ref:      warcraftlogs.ReportRef{Host: row.Host, Code: row.Code},
			Revision: row.Revision,
		})
	}
	return out, nil
}

// CredentialsFor resolves the guild's own client, falling back to the instance's.
//
// A guild that supplies its own gets its own hourly point budget, which is the whole
// reason the columns exist: the budget is per client key, and one large guild on a
// shared key can spend it for everybody.
func (s *Store) CredentialsFor(ctx context.Context, guildID int64) (Credentials, bool, error) {
	row, err := s.queries.GetGuildWarcraftLogsCredentials(ctx, guildID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Credentials{}, false, err
	}

	if err == nil && row.WarcraftLogsClientID != nil && len(row.WarcraftLogsKeySealed) > 0 {
		key, openErr := s.box.Open(row.WarcraftLogsKeySealed)
		if openErr != nil {
			// The encryption key changed or the value is damaged. Falling through to the
			// instance credentials is the useful behaviour: the guild's reports keep
			// working while somebody sorts out why their key cannot be read.
			return s.instanceCredentials()
		}
		return Credentials{ClientID: *row.WarcraftLogsClientID, Key: key, Guild: true}, true, nil
	}

	return s.instanceCredentials()
}

func (s *Store) instanceCredentials() (Credentials, bool, error) {
	if s.fallback.ClientID == "" || s.fallback.Key == "" {
		return Credentials{}, false, nil
	}
	return s.fallback, true, nil
}

// SetGuildCredentials seals a guild's key and stores it.
//
// Refuses outright when the instance has no encryption key configured. A missing key is
// a configuration mistake, and storing a guild's secret in the clear because of one
// would turn that mistake into a breach.
func (s *Store) SetGuildCredentials(ctx context.Context, guildID int64, clientID, key string) error {
	if !s.box.Configured() {
		return secretbox.ErrNoKey
	}

	sealed, err := s.box.Seal(key)
	if err != nil {
		return err
	}
	return s.queries.SetGuildWarcraftLogsCredentials(ctx, db.SetGuildWarcraftLogsCredentialsParams{
		DiscordGuildID: guildID,
		ClientID:       &clientID,
		KeySealed:      sealed,
	})
}

func (s *Store) ClearGuildCredentials(ctx context.Context, guildID int64) error {
	return s.queries.ClearGuildWarcraftLogsCredentials(ctx, guildID)
}

func (s *Store) RosterFor(ctx context.Context, guildID int64) ([]RosterCharacter, error) {
	// Archived characters included. A raider who left after the raid still did the
	// damage they did, and dropping them would move their night into the unknown list.
	rows, err := s.queries.ListCharactersInGuild(ctx, db.ListCharactersInGuildParams{
		DiscordGuildID:  guildID,
		IncludeArchived: true,
	})
	if err != nil {
		return nil, err
	}

	out := make([]RosterCharacter, 0, len(rows))
	for _, row := range rows {
		out = append(out, RosterCharacter{ID: row.ID, Name: row.Name, Realm: row.Realm})
	}
	return out, nil
}

func (s *Store) ExpectedFor(ctx context.Context, eventID uuid.UUID) ([]uuid.UUID, error) {
	return s.queries.ListExpectedCharactersForEvent(ctx, eventID)
}

// Store writes one fetched report: the row, its fights and its raiders, in one
// transaction, so a reader never sees last night's fights beside tonight's raiders.
func (s *Store) Store(ctx context.Context, eventID uuid.UUID, fetched Fetched) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)
	report := fetched.Report

	if err := q.MarkReportFetched(ctx, db.MarkReportFetchedParams{
		EventID:        eventID,
		Revision:       &report.Revision,
		Live:           report.Live,
		Title:          nilIfEmpty(report.Title),
		ZoneName:       report.ZoneName,
		Region:         report.Region,
		ReportStartsAt: stamp(report.StartsAt),
		ReportEndsAt:   stamp(report.EndsAt),
		NextAttemptAt:  stampPtr(fetched.NextAttempt),
	}); err != nil {
		return fmt.Errorf("marking a report fetched: %w", err)
	}

	// Pulls that vanished from the report are pulls WarcraftLogs reprocessed away, and
	// leaving them would keep a phantom on the timeline forever. The insert below is
	// still an upsert, because the delete and the insert share a transaction and the
	// upsert is what makes a retry safe.
	if err := q.DeleteEventReportFights(ctx, eventID); err != nil {
		return fmt.Errorf("clearing old fights: %w", err)
	}
	for _, fight := range report.Fights {
		if err := q.InsertEventReportFight(ctx, db.InsertEventReportFightParams{
			EventID:         eventID,
			FightID:         fight.ID,
			EncounterID:     fight.EncounterID,
			Name:            fight.Name,
			Difficulty:      fight.Difficulty,
			RaidSize:        fight.Size,
			Kill:            fight.Kill,
			BossPercentage:  numeric(fight.BossPercentage),
			FightPercentage: numeric(fight.FightPercentage),
			StartsAt:        stamp(fight.StartsAt),
			EndsAt:          stamp(fight.EndsAt),
		}); err != nil {
			return fmt.Errorf("storing fight %d: %w", fight.ID, err)
		}
	}

	if err := q.DeleteEventReportRaiders(ctx, eventID); err != nil {
		return fmt.Errorf("clearing old raiders: %w", err)
	}
	for _, entry := range fetched.Matched {
		if err := q.InsertEventReportRaider(ctx, db.InsertEventReportRaiderParams{
			EventID:     eventID,
			ActorID:     entry.Actor.ID,
			ActorName:   entry.Actor.Name,
			ActorServer: entry.Actor.Server,
			Class:       nilIfEmpty(entry.Actor.Class),
			CharacterID: characterUUID(entry.CharacterID),
			Damage:      entry.Actor.Damage,
			Healing:     entry.Actor.Healing,
			Deaths:      entry.Actor.Deaths,
		}); err != nil {
			return fmt.Errorf("storing raider %d: %w", entry.Actor.ID, err)
		}
	}

	if err := q.DeleteEventReportFightRaiders(ctx, eventID); err != nil {
		return fmt.Errorf("clearing old per-pull rows: %w", err)
	}
	for fightID, entries := range fetched.PerFight {
		for _, entry := range entries {
			if err := q.InsertEventReportFightRaider(ctx, db.InsertEventReportFightRaiderParams{
				EventID:     eventID,
				FightID:     fightID,
				ActorID:     entry.Actor.ID,
				ActorName:   entry.Actor.Name,
				ActorServer: entry.Actor.Server,
				Class:       nilIfEmpty(entry.Actor.Class),
				CharacterID: characterUUID(entry.CharacterID),
				Damage:      entry.Actor.Damage,
				Healing:     entry.Actor.Healing,
				Deaths:      entry.Actor.Deaths,
			}); err != nil {
				return fmt.Errorf("storing raider %d on fight %d: %w", entry.Actor.ID, fightID, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, eventID uuid.UUID, status, reason string, next *time.Time) error {
	return s.queries.MarkReportFailed(ctx, db.MarkReportFailedParams{
		EventID:       eventID,
		Status:        db.ReportStatus(status),
		NextAttemptAt: stampPtr(next),
		FailureReason: &reason,
	})
}

func (s *Store) Reschedule(ctx context.Context, eventID uuid.UUID, next time.Time) error {
	return s.queries.RescheduleReport(ctx, db.RescheduleReportParams{
		EventID:       eventID,
		NextAttemptAt: stamp(next),
	})
}

// characterUUID turns an optional match into the null-capable uuid the column takes.
// Absent means nobody on the roster answers to that name, which is a fact worth storing
// rather than a row to drop.
func characterUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func stampPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return stamp(*t)
}

// numeric turns an optional percentage into Postgres numeric. Absent stays absent: a
// fight WarcraftLogs could not measure has no boss percentage, and zero would read as a
// kill.
func numeric(v *float64) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{}
	}
	// Two decimal places, which is what WarcraftLogs reports and more than a raid lead
	// reads off a wipe.
	scaled := big.NewInt(int64(*v*100 + 0.5))
	return pgtype.Numeric{Int: scaled, Exp: -2, Valid: true}
}
