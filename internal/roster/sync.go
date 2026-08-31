// Package roster owns characters, roles, alts, and guild membership, including
// keeping cached Raider.IO data current.
package roster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
)

// profileFetcher is the Raider.IO dependency the syncer needs. Declared here, by
// the consumer, rather than on the raiderio package.
type profileFetcher interface {
	CharacterProfile(ctx context.Context, region, realm, name string) (raiderio.Profile, error)
}

// applySyncParams carries what a sync writes for one character.
type applySyncParams struct {
	characterID uuid.UUID
	profile     raiderio.Profile
	gearJSON    []byte
	detail      gearDetail
	changed     bool
}

// gearDetail is what the syncer works out that the profile does not carry: enchant
// compliance and tier count against this season's rules, and the one raid the roster
// is measured on. Every field is optional, and nil means "not established" rather
// than zero.
type gearDetail struct {
	enchantsMissing  *int16
	enchantsExpected *int16
	tierPieces       *int16
	raid             *raiderio.RaidProgress
}

// syncStore is the persistence the syncer needs. Declared here, by the consumer.
type syncStore interface {
	DueForSync(ctx context.Context, staleBefore time.Time, limit int32) ([]db.Character, error)
	LatestSnapshotGear(ctx context.Context, characterID uuid.UUID) (gear []byte, ilvl, mplusScore float64, found bool, err error)
	ApplySync(ctx context.Context, arg applySyncParams) error
	TouchSynced(ctx context.Context, characterID uuid.UUID) error
	MarkSyncAttempted(ctx context.Context, characterID uuid.UUID) error
	MarkNotFound(ctx context.Context, characterID uuid.UUID) error
}

// Syncer refreshes cached character data from Raider.IO.
type Syncer struct {
	fetcher profileFetcher
	store   syncStore
	rules   GearRules
	logger  *slog.Logger
}

// NewSyncer builds a Syncer. rules is the season's game data; a zero value is legal
// and means tier counting and progression stay unestablished.
func NewSyncer(fetcher profileFetcher, store syncStore, rules GearRules, logger *slog.Logger) *Syncer {
	return &Syncer{fetcher: fetcher, store: store, rules: rules, logger: logger}
}

// SyncDue fetches and writes fresh data for up to limit characters whose
// last_synced is older than staleAfter (or unset). One character's failure does
// not stop the rest of the batch; being rate limited or holding a rejected access
// key does, since every remaining request would be rejected too.
func (s *Syncer) SyncDue(ctx context.Context, staleAfter time.Duration, limit int32) error {
	due, err := s.store.DueForSync(ctx, time.Now().Add(-staleAfter), limit)
	if err != nil {
		return fmt.Errorf("listing characters due for sync: %w", err)
	}

	for i, c := range due {
		err := s.syncOne(ctx, c)
		if err == nil {
			continue
		}
		// Both of these are facts about the worker's access to Raider.IO, not about
		// this character, so every remaining request in the batch would fail the same
		// way. Carrying on would burn the whole batch and push all of it a full
		// staleAfter into the future for a reason that has nothing to do with them.
		if errors.Is(err, raiderio.ErrRateLimited) || errors.Is(err, raiderio.ErrInvalidAPIKey) {
			return fmt.Errorf("abandoning batch after %d of %d characters: %w", i, len(due), err)
		}

		s.logger.ErrorContext(ctx, "syncing character", "character_id", c.ID, "error", err)
		// Without this the character keeps its old queue position and is picked
		// first again next tick, so a permanent failure starves the batch.
		if err := s.store.MarkSyncAttempted(ctx, c.ID); err != nil {
			s.logger.ErrorContext(ctx, "marking sync attempt", "character_id", c.ID, "error", err)
		}
	}

	return nil
}

func (s *Syncer) syncOne(ctx context.Context, c db.Character) error {
	profile, err := s.fetcher.CharacterProfile(ctx, c.Region, c.Realm, c.Name)
	if errors.Is(err, raiderio.ErrCharacterNotFound) {
		// Not a successful sync, which is what this used to record. A renamed,
		// transferred or deleted character kept its last known numbers and a fresh
		// last_synced, so it read as a raider standing perfectly still. The row now
		// says since when nobody has been able to confirm any of it, which is the
		// evidence a raid lead needs to decide whether they have left.
		s.logger.WarnContext(ctx, "character not found on raider.io", "character_id", c.ID)
		return s.store.MarkNotFound(ctx, c.ID)
	}
	if err != nil {
		return fmt.Errorf("fetching profile: %w", err)
	}

	gearJSON, err := json.Marshal(profile.Gear)
	if err != nil {
		return fmt.Errorf("marshalling gear: %w", err)
	}

	detail := s.deriveGear(profile)

	unchanged, err := s.unchanged(ctx, c, profile, detail)
	if err != nil {
		return fmt.Errorf("comparing against latest snapshot: %w", err)
	}
	if unchanged {
		return s.store.TouchSynced(ctx, c.ID)
	}

	return s.store.ApplySync(ctx, applySyncParams{
		characterID: c.ID,
		profile:     profile,
		gearJSON:    gearJSON,
		detail:      detail,
		changed:     true,
	})
}

// deriveGear reads the season's rules over one profile.
func (s *Syncer) deriveGear(profile raiderio.Profile) gearDetail {
	var d gearDetail

	// A profile carrying no gear at all establishes nothing. Writing 0 missing of 0
	// expected would read as a fully compliant raider rather than as an empty answer.
	if missing, expected := EnchantCompliance(profile.Gear); expected > 0 {
		d.enchantsMissing = int16Ptr(missing)
		d.enchantsExpected = int16Ptr(expected)
	}
	if pieces, ok := s.rules.TierPieces(profile.Gear); ok {
		d.tierPieces = int16Ptr(pieces)
	}
	if raid, ok := s.rules.CurrentRaid(profile.Progression); ok {
		d.raid = &raid
	}

	return d
}

// unchanged reports whether a write would be a no-op. Gear and the numbers come
// from the last snapshot; class and spec live only on the character row, so a
// re-spec with no gear movement still counts as a change.
func (s *Syncer) unchanged(ctx context.Context, c db.Character, profile raiderio.Profile, detail gearDetail) (bool, error) {
	prevGear, prevIlvl, prevScore, found, err := s.store.LatestSnapshotGear(ctx, c.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	if prevIlvl != profile.ItemLevel || prevScore != profile.MythicPlusScore {
		return false, nil
	}
	if !sameString(c.Class, profile.Class) || !sameString(c.Spec, profile.Spec) {
		return false, nil
	}

	// Comparing gear alone would miss two things. Progression is not in the snapshot at
	// all, and a change to the season's rules moves the tier count without a single
	// item having moved.
	if !sameDetail(c, detail) {
		return false, nil
	}

	// The stored column is jsonb, which reorders keys and reformats whitespace, so
	// the raw bytes never match a fresh json.Marshal. Compare the decoded values.
	var stored []raiderio.GearItem
	if err := json.Unmarshal(prevGear, &stored); err != nil {
		return false, fmt.Errorf("decoding stored gear: %w", err)
	}

	return sameGear(stored, profile.Gear), nil
}

// sameString treats an absent incoming value as "no change", matching the COALESCE
// in UpdateCharacterFromSync: Raider.IO omitting a field must not blank the column.
func sameString(stored *string, incoming string) bool {
	if incoming == "" {
		return true
	}
	return stored != nil && *stored == incoming
}

// sameGear compares slot by slot. Both sides are sorted by slot, so order is stable.
func sameGear(a, b []raiderio.GearItem) bool {
	return slices.EqualFunc(a, b, func(x, y raiderio.GearItem) bool {
		return x.Slot == y.Slot &&
			x.ItemID == y.ItemID &&
			x.ItemLevel == y.ItemLevel &&
			x.EnchantID == y.EnchantID &&
			slices.Equal(x.GemIDs, y.GemIDs)
	})
}

// sameDetail compares the derived columns against what the character row already holds.
func sameDetail(c db.Character, d gearDetail) bool {
	return sameInt16(c.EnchantsMissing, d.enchantsMissing) &&
		sameInt16(c.EnchantsExpected, d.enchantsExpected) &&
		sameInt16(c.TierPieces, d.tierPieces) &&
		sameRaid(c, d.raid)
}

// sameInt16 treats nil as its own value rather than as zero: "no answer" and "none"
// are different states in every one of these columns.
func sameInt16(a, b *int16) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameRaid(c db.Character, raid *raiderio.RaidProgress) bool {
	if raid == nil {
		return c.RaidSlug == nil
	}
	return c.RaidSlug != nil &&
		*c.RaidSlug == raid.Slug &&
		sameInt16(c.RaidBosses, int16Ptr(raid.Bosses)) &&
		sameInt16(c.RaidNormalKilled, int16Ptr(raid.NormalKilled)) &&
		sameInt16(c.RaidHeroicKilled, int16Ptr(raid.HeroicKilled)) &&
		sameInt16(c.RaidMythicKilled, int16Ptr(raid.MythicKilled))
}

func int16Ptr(n int) *int16 {
	v := int16(n)
	return &v
}

// numeric renders a float64 as the decimal string pgtype.Numeric.Scan expects.
func numeric(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
