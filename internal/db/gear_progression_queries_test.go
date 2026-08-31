//go:build integration

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The sync writes these straight, NULL included, where class and spec are COALESCEd.
// A raider who unequips their tier really does have no tier, and COALESCEing that away
// would leave last season's count on the row forever.
func TestUpdateCharacterFromSyncClearsGearColumnsItIsGivenNothingFor(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	_, character := seedUserAndCharacter(ctx, t, q, 700, "Danthrax")

	class := "Warrior"
	missing, expected, pieces := int16(2), int16(8), int16(4)
	bosses, normal, heroic, mythic := int16(8), int16(8), int16(6), int16(2)
	slug := "liberation-of-undermine"

	if err := q.UpdateCharacterFromSync(ctx, UpdateCharacterFromSyncParams{
		ID:               character.ID,
		Class:            &class,
		EnchantsMissing:  &missing,
		EnchantsExpected: &expected,
		TierPieces:       &pieces,
		RaidSlug:         &slug,
		RaidBosses:       &bosses,
		RaidNormalKilled: &normal,
		RaidHeroicKilled: &heroic,
		RaidMythicKilled: &mythic,
	}); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	stored, err := q.GetCharacterInGuild(ctx, GetCharacterInGuildParams{ID: character.ID, DiscordGuildID: 100})
	if err != nil {
		t.Fatalf("reading character: %v", err)
	}
	if stored.TierPieces == nil || *stored.TierPieces != 4 {
		t.Errorf("tier_pieces = %v, want 4", stored.TierPieces)
	}
	if stored.RaidSlug == nil || *stored.RaidSlug != slug {
		t.Errorf("raid_slug = %v, want %q", stored.RaidSlug, slug)
	}
	if stored.RaidHeroicKilled == nil || *stored.RaidHeroicKilled != 6 {
		t.Errorf("raid_heroic_killed = %v, want 6", stored.RaidHeroicKilled)
	}

	// A second sync with the season rules removed. Class survives because it is
	// COALESCEd; every gear column goes back to NULL because it is not.
	if err := q.UpdateCharacterFromSync(ctx, UpdateCharacterFromSyncParams{ID: character.ID}); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	cleared, err := q.GetCharacterInGuild(ctx, GetCharacterInGuildParams{ID: character.ID, DiscordGuildID: 100})
	if err != nil {
		t.Fatalf("re-reading character: %v", err)
	}
	if cleared.Class == nil || *cleared.Class != class {
		t.Errorf("class = %v, want it left alone at %q", cleared.Class, class)
	}
	for name, got := range map[string]*int16{
		"enchants_missing":   cleared.EnchantsMissing,
		"enchants_expected":  cleared.EnchantsExpected,
		"tier_pieces":        cleared.TierPieces,
		"raid_bosses":        cleared.RaidBosses,
		"raid_normal_killed": cleared.RaidNormalKilled,
		"raid_heroic_killed": cleared.RaidHeroicKilled,
		"raid_mythic_killed": cleared.RaidMythicKilled,
	} {
		if got != nil {
			t.Errorf("%s = %d, want NULL once the worker establishes nothing for it", name, *got)
		}
	}
	if cleared.RaidSlug != nil {
		t.Errorf("raid_slug = %q, want NULL", *cleared.RaidSlug)
	}
}

func TestCountSignupsByStatusForEvents(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	answered := seedEventForJobs(ctx, t, q, 701)
	silent := seedEventForJobs(ctx, t, q, 702)

	for i, status := range []SignupStatus{
		SignupStatusCONFIRMED, SignupStatusCONFIRMED, SignupStatusDECLINED,
	} {
		_, character := seedUserAndCharacter(ctx, t, q, int64(710+i), "Raider")
		if _, err := q.UpsertSignup(ctx, UpsertSignupParams{
			ID: NewID(), EventID: answered.ID, CharacterID: character.ID, Status: status,
		}); err != nil {
			t.Fatalf("signing up: %v", err)
		}
	}

	rows, err := q.CountSignupsByStatusForEvents(ctx, []uuid.UUID{answered.ID, silent.ID})
	if err != nil {
		t.Fatalf("counting: %v", err)
	}

	got := map[SignupStatus]int64{}
	for _, row := range rows {
		if row.EventID != answered.ID {
			t.Errorf("event_id = %s, want only the answered event to appear", row.EventID)
		}
		got[row.Status] = row.Total
	}

	if got[SignupStatusCONFIRMED] != 2 {
		t.Errorf("confirmed = %d, want 2", got[SignupStatusCONFIRMED])
	}
	if got[SignupStatusDECLINED] != 1 {
		t.Errorf("declined = %d, want 1", got[SignupStatusDECLINED])
	}
	// A status nobody chose does not come back, and neither does an event nobody
	// answered. Seeding those zeros is internal/signup's job, not SQL's.
	if _, ok := got[SignupStatusNOSHOW]; ok {
		t.Error("no_show came back from SQL, want only statuses somebody chose")
	}
}
