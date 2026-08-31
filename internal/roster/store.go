package roster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// Store implements syncStore over Postgres.
type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

// NewStore builds a Store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, queries: db.New(pool)}
}

func (s *Store) DueForSync(ctx context.Context, staleBefore time.Time, limit int32) ([]db.Character, error) {
	return s.queries.ListCharactersDueForSync(ctx, db.ListCharactersDueForSyncParams{
		StaleBefore: pgtype.Timestamptz{Time: staleBefore, Valid: true},
		RowLimit:    limit,
	})
}

func (s *Store) LatestSnapshotGear(ctx context.Context, characterID uuid.UUID) (gear []byte, ilvl, mplusScore float64, found bool, err error) {
	snap, err := s.queries.GetLatestCharacterSnapshot(ctx, characterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, 0, false, nil
	}
	if err != nil {
		return nil, 0, 0, false, err
	}

	// A NULL number is not the same as zero and there is nothing to compare against,
	// so report no usable snapshot and let the caller write a fresh one.
	if !snap.Ilvl.Valid || !snap.MplusScore.Valid {
		return nil, 0, 0, false, nil
	}

	ilvlF, err := numericToFloat64(snap.Ilvl)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("converting stored ilvl: %w", err)
	}
	scoreF, err := numericToFloat64(snap.MplusScore)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("converting stored mplus_score: %w", err)
	}

	return snap.Gear, ilvlF, scoreF, true, nil
}

func (s *Store) ApplySync(ctx context.Context, arg applySyncParams) error {
	var ilvl, score pgtype.Numeric
	if err := ilvl.Scan(numeric(arg.profile.ItemLevel)); err != nil {
		return fmt.Errorf("encoding ilvl: %w", err)
	}
	if err := score.Scan(numeric(arg.profile.MythicPlusScore)); err != nil {
		return fmt.Errorf("encoding mplus_score: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	update := db.UpdateCharacterFromSyncParams{
		ID:               arg.characterID,
		Class:            nilIfEmpty(arg.profile.Class),
		Spec:             nilIfEmpty(arg.profile.Spec),
		Ilvl:             ilvl,
		MplusScore:       score,
		EnchantsMissing:  arg.detail.enchantsMissing,
		EnchantsExpected: arg.detail.enchantsExpected,
		TierPieces:       arg.detail.tierPieces,
	}
	// All five progression columns move together or none of them do. A slug with no
	// counts beside it, or counts with no slug, would be a row nothing can read.
	if raid := arg.detail.raid; raid != nil {
		update.RaidSlug = &raid.Slug
		update.RaidBosses = int16Ptr(raid.Bosses)
		update.RaidNormalKilled = int16Ptr(raid.NormalKilled)
		update.RaidHeroicKilled = int16Ptr(raid.HeroicKilled)
		update.RaidMythicKilled = int16Ptr(raid.MythicKilled)
	}
	if err := q.UpdateCharacterFromSync(ctx, update); err != nil {
		return fmt.Errorf("updating character: %w", err)
	}

	if _, err := q.InsertCharacterSnapshot(ctx, db.InsertCharacterSnapshotParams{
		ID:          db.NewID(),
		CharacterID: arg.characterID,
		Ilvl:        ilvl,
		MplusScore:  score,
		Gear:        arg.gearJSON,
	}); err != nil {
		return fmt.Errorf("inserting snapshot: %w", err)
	}

	// In the same transaction as the snapshot: a raid message is only asked to redraw
	// for data that actually landed. ApplySync runs only when the syncer found a real
	// difference, so this does not fire on a no-op refresh.
	events, err := q.ListEventsNeedingRosterRedraw(ctx, arg.characterID)
	if err != nil {
		return fmt.Errorf("listing events needing redraw: %w", err)
	}
	for _, e := range events {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			ID:             db.NewID(),
			DiscordGuildID: e.DiscordGuildID,
			EventID:        e.ID,
			Kind:           db.NotificationKindROSTERUPDATED,
			TargetKind:     db.NotificationTargetMESSAGE,
			ChannelID:      e.ChannelID,
			Payload:        []byte("{}"),
		}); err != nil {
			return fmt.Errorf("queueing roster redraw for event %s: %w", e.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}

	return nil
}

func (s *Store) TouchSynced(ctx context.Context, characterID uuid.UUID) error {
	return s.queries.TouchCharacterSynced(ctx, characterID)
}

func (s *Store) MarkSyncAttempted(ctx context.Context, characterID uuid.UUID) error {
	return s.queries.MarkCharacterSyncAttempted(ctx, characterID)
}

func (s *Store) MarkNotFound(ctx context.Context, characterID uuid.UUID) error {
	return s.queries.MarkCharacterNotFound(ctx, characterID)
}

// nilIfEmpty maps a missing field to SQL NULL, which UpdateCharacterFromSync
// COALESCEs back to the stored value rather than blanking the column.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func numericToFloat64(n pgtype.Numeric) (float64, error) {
	f, err := n.Float64Value()
	if err != nil {
		return 0, err
	}
	return f.Float64, nil
}

// RegisterCharacter creates the owning user and the character together. One
// transaction because a failure between the two leaves a user row owning nothing,
// which the next registration would silently reuse and no code path ever cleans up.
func (s *Store) RegisterCharacter(ctx context.Context, in RegisterInput) (Character, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Character{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	user, err := q.UpsertUser(ctx, db.UpsertUserParams{
		ID: db.NewID(), DiscordID: in.DiscordID, DiscordGuildID: in.DiscordGuildID,
	})
	if err != nil {
		return Character{}, fmt.Errorf("upserting user: %w", err)
	}

	row, err := q.CreateCharacter(ctx, db.CreateCharacterParams{
		ID:     db.NewID(),
		UserID: user.ID, Name: in.Name, Realm: in.Realm, Region: in.Region, IsMain: in.IsMain,
	})
	if err != nil {
		if isUniqueViolation(err, "characters_user_id_name_realm_region_key") {
			return Character{}, ErrCharacterExists
		}
		return Character{}, fmt.Errorf("creating character: %w", err)
	}

	character, err := characterFromRow(row)
	if err != nil {
		return Character{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Character{}, fmt.Errorf("committing tx: %w", err)
	}
	return character, nil
}

// isUniqueViolation reports whether err is Postgres 23505 against a named constraint.
// The constraint name is part of the check on purpose: characters carries two unique
// indexes and they mean different things to the caller.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func (s *Store) GetCharacterInGuild(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (Character, error) {
	row, err := s.queries.GetCharacterInGuild(ctx, db.GetCharacterInGuildParams{
		ID: characterID, DiscordGuildID: discordGuildID,
	})
	if err != nil {
		return Character{}, err
	}
	return characterFromRow(row)
}

func (s *Store) GetCharacterOwner(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (int64, error) {
	return s.queries.GetCharacterOwner(ctx, db.GetCharacterOwnerParams{
		ID: characterID, DiscordGuildID: discordGuildID,
	})
}

func (s *Store) DeleteCharacter(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (bool, error) {
	rows, err := s.queries.DeleteCharacter(ctx, db.DeleteCharacterParams{
		ID: characterID, DiscordGuildID: discordGuildID,
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// SetCharacterMain moves the main flag, which is the dashboard's "switch mains". The
// demote and the promote are two statements in one transaction because
// characters_one_main_per_user is a partial unique index and Postgres has no partial
// unique constraint to defer: doing both in one UPDATE, or in a data-modifying CTE,
// violates the index depending on which row the scan reaches first.
func (s *Store) SetCharacterMain(ctx context.Context, characterID uuid.UUID, discordGuildID int64, isMain bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	// Only when promoting. Demoting is the raider giving up a main without naming a
	// replacement, and clearing first would make that a no-op against itself.
	if isMain {
		if err := q.ClearMainForCharacterOwner(ctx, db.ClearMainForCharacterOwnerParams{
			ID: characterID, DiscordGuildID: discordGuildID,
		}); err != nil {
			return false, fmt.Errorf("clearing current main: %w", err)
		}
	}

	rows, err := q.SetCharacterMain(ctx, db.SetCharacterMainParams{
		ID: characterID, DiscordGuildID: discordGuildID, IsMain: isMain,
	})
	if err != nil {
		return false, fmt.Errorf("setting main: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing tx: %w", err)
	}
	return rows > 0, nil
}

func (s *Store) ListCharactersInGuild(ctx context.Context, discordGuildID int64, includeArchived bool) ([]Character, error) {
	rows, err := s.queries.ListCharactersInGuild(ctx, db.ListCharactersInGuildParams{
		DiscordGuildID: discordGuildID, IncludeArchived: includeArchived,
	})
	if err != nil {
		return nil, err
	}
	return charactersFromRows(rows)
}

func (s *Store) SetCharacterArchived(ctx context.Context, characterID uuid.UUID, discordGuildID int64, archived bool) (bool, error) {
	rows, err := s.queries.SetCharacterArchived(ctx, db.SetCharacterArchivedParams{
		ID: characterID, DiscordGuildID: discordGuildID, Archived: archived,
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) ListGuildsForDiscordUser(ctx context.Context, discordID int64) ([]int64, error) {
	return s.queries.ListGuildsForDiscordUser(ctx, discordID)
}

func (s *Store) ListCharactersByDiscord(ctx context.Context, discordID, discordGuildID int64) ([]Character, error) {
	rows, err := s.queries.ListCharactersByDiscord(ctx, db.ListCharactersByDiscordParams{
		DiscordID: discordID, DiscordGuildID: discordGuildID,
	})
	if err != nil {
		return nil, err
	}
	return charactersFromRows(rows)
}

// ReplaceCharacterRoles clears and rewrites the whole role menu in one transaction,
// matching the ephemeral select's whole-set submit.
func (s *Store) ReplaceCharacterRoles(ctx context.Context, characterID uuid.UUID, roles []RoleChoice) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	if err := q.DeleteCharacterRoles(ctx, characterID); err != nil {
		return fmt.Errorf("clearing existing roles: %w", err)
	}
	for _, r := range roles {
		if err := q.SetCharacterRole(ctx, db.SetCharacterRoleParams{
			CharacterID: characterID, Role: r.Role, Priority: r.Priority,
		}); err != nil {
			return fmt.Errorf("setting role %s: %w", r.Role, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

func (s *Store) ListCharacterRoles(ctx context.Context, characterID uuid.UUID) ([]RoleChoice, error) {
	rows, err := s.queries.ListCharacterRoles(ctx, characterID)
	if err != nil {
		return nil, err
	}
	out := make([]RoleChoice, len(rows))
	for i, r := range rows {
		out[i] = RoleChoice{Role: r.Role, Priority: r.Priority}
	}
	return out, nil
}

// ListRolesForCharacters reads several role menus at once, keyed by character id.
// Rendering a signup list or a comp board needs every raider's menu, and one query
// per raider would be twenty-five round trips to draw one embed.
func (s *Store) ListRolesForCharacters(ctx context.Context, characterIDs []uuid.UUID) (map[uuid.UUID][]RoleChoice, error) {
	rows, err := s.queries.ListRolesForCharacters(ctx, characterIDs)
	if err != nil {
		return nil, err
	}
	// The query orders by character_id, priority, role, so appending preserves the
	// priority order the select menu renders in.
	out := make(map[uuid.UUID][]RoleChoice, len(characterIDs))
	for _, r := range rows {
		out[r.CharacterID] = append(out[r.CharacterID], RoleChoice{Role: r.Role, Priority: r.Priority})
	}
	return out, nil
}

func characterFromRow(row db.Character) (Character, error) {
	ilvl, err := nullableFloat64(row.Ilvl)
	if err != nil {
		return Character{}, fmt.Errorf("converting ilvl: %w", err)
	}
	mplusScore, err := nullableFloat64(row.MplusScore)
	if err != nil {
		return Character{}, fmt.Errorf("converting mplus_score: %w", err)
	}

	c := Character{
		ID:         row.ID,
		UserID:     row.UserID,
		Name:       row.Name,
		Realm:      row.Realm,
		Region:     row.Region,
		Class:      row.Class,
		Spec:       row.Spec,
		Ilvl:       ilvl,
		MplusScore: mplusScore,
		IsMain:     row.IsMain,
		Synced:     row.LastSynced.Valid,

		EnchantsMissing:  intPtr(row.EnchantsMissing),
		EnchantsExpected: intPtr(row.EnchantsExpected),
		TierPieces:       intPtr(row.TierPieces),

		ArchivedAt:    timePtr(row.ArchivedAt),
		NotFoundSince: timePtr(row.NotFoundSince),
	}

	// The slug is what says the other four mean anything. A row written before the
	// worker had a raid configured carries none, and reads back with no progression at
	// all rather than with four zeros.
	if row.RaidSlug != nil {
		c.Progression = &RaidProgress{
			Slug:         *row.RaidSlug,
			Bosses:       intOrZero(row.RaidBosses),
			NormalKilled: intOrZero(row.RaidNormalKilled),
			HeroicKilled: intOrZero(row.RaidHeroicKilled),
			MythicKilled: intOrZero(row.RaidMythicKilled),
		}
	}

	return c, nil
}

// intPtr widens a nullable smallint for the domain, keeping NULL as nil: every one of
// these columns has a real difference between "none" and "not established".
func intPtr(n *int16) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

func intOrZero(n *int16) int {
	if n == nil {
		return 0
	}
	return int(*n)
}

func charactersFromRows(rows []db.Character) ([]Character, error) {
	out := make([]Character, len(rows))
	for i, row := range rows {
		c, err := characterFromRow(row)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

// timePtr converts a possibly-NULL timestamp column. NULL is the answer these two
// columns give most of the time and it means something: not archived, not missing.
func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// nullableFloat64 converts a possibly-NULL numeric column. NULL is a different fact
// than zero here (no ilvl on file versus a genuine 0), so it stays nil rather than
// collapsing to 0.
func nullableFloat64(n pgtype.Numeric) (*float64, error) {
	if !n.Valid {
		return nil, nil
	}
	f, err := numericToFloat64(n)
	if err != nil {
		return nil, err
	}
	return &f, nil
}
