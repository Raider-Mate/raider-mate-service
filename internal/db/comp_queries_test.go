//go:build integration

package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func seedEventForComp(ctx context.Context, t *testing.T, q *Queries, discordID int64) (Event, Character) {
	t.Helper()

	user, err := q.UpsertUser(ctx, UpsertUserParams{ID: NewID(), DiscordID: discordID, DiscordGuildID: 100})
	if err != nil {
		t.Fatalf("upserting user: %v", err)
	}
	character, err := q.CreateCharacter(ctx, CreateCharacterParams{
		ID:     NewID(),
		UserID: user.ID, Name: "Danthrax", Realm: "Area-52", Region: "us", IsMain: true,
	})
	if err != nil {
		t.Fatalf("creating character: %v", err)
	}
	event, err := q.CreateEvent(ctx, CreateEventParams{
		ID:             NewID(),
		DiscordGuildID: 100,
		Type:           EventTypeRAID,
		Title:          "Prog Night",
		StartsAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
		SignupDeadline: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		CompTemplate:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}
	if _, err := q.UpsertSignup(ctx, UpsertSignupParams{
		ID:      NewID(),
		EventID: event.ID, CharacterID: character.ID, Status: SignupStatusCONFIRMED,
	}); err != nil {
		t.Fatalf("signing up character: %v", err)
	}

	// comp_slots carries an FK to comps, so the comps these tests write slots into
	// have to exist first.
	for _, name := range []string{"prog", "farm"} {
		if _, err := q.UpsertComp(ctx, UpsertCompParams{
			ID:      NewID(),
			EventID: event.ID, Name: name, Mode: CompModeAUTO,
		}); err != nil {
			t.Fatalf("creating comp %q: %v", name, err)
		}
	}

	return event, character
}

func TestCreateEventPersistsDifficulty(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	mythic := RaidDifficultyMYTHIC
	event, err := q.CreateEvent(ctx, CreateEventParams{
		ID:             NewID(),
		DiscordGuildID: 100,
		Type:           EventTypeRAID,
		Title:          "Mythic Prog",
		StartsAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
		SignupDeadline: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		CompTemplate:   []byte(`{}`),
		Difficulty:     &mythic,
	})
	if err != nil {
		t.Fatalf("creating mythic event: %v", err)
	}

	// The assigner reads difficulty back through GetEvent to pick its size rule, so a
	// value that does not survive the round trip silently turns Mythic into flex.
	got, err := q.GetEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("reading event back: %v", err)
	}
	if got.Difficulty == nil || *got.Difficulty != RaidDifficultyMYTHIC {
		t.Errorf("difficulty = %v, want MYTHIC", got.Difficulty)
	}
}

func TestListAssignmentPoolAndRolesFeedTheAssigner(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 21)

	if err := q.SetCharacterRole(ctx, SetCharacterRoleParams{
		CharacterID: character.ID, Role: RoleEnumTANK, Priority: 1,
	}); err != nil {
		t.Fatalf("setting tank role: %v", err)
	}
	if err := q.SetCharacterRole(ctx, SetCharacterRoleParams{
		CharacterID: character.ID, Role: RoleEnumMDPS, Priority: 2,
	}); err != nil {
		t.Fatalf("setting mdps role: %v", err)
	}

	pool, err := q.ListAssignmentPoolForEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("listing assignment pool: %v", err)
	}
	if len(pool) != 1 || pool[0].CharacterID != character.ID {
		t.Fatalf("pool = %+v, want the one confirmed signup", pool)
	}

	roles, err := q.ListRolesForCharacters(ctx, []uuid.UUID{character.ID})
	if err != nil {
		t.Fatalf("listing roles: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %+v, want both choices", roles)
	}
	if roles[0].Role != RoleEnumTANK || roles[0].Priority != 1 {
		t.Errorf("first role = %+v, want TANK at priority 1", roles[0])
	}
}

func TestUpsertCompKeepsTheModeOfAnExistingComp(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, _ := seedEventForComp(ctx, t, q, 22)

	if _, err := q.UpsertComp(ctx, UpsertCompParams{
		ID:      NewID(),
		EventID: event.ID, Name: "hand", Mode: CompModeMANUAL,
	}); err != nil {
		t.Fatalf("creating manual comp: %v", err)
	}

	// The assigner's write path upserts with AUTO. It must not convert a raid lead's
	// comp back to assigner-owned as a side effect.
	if _, err := q.UpsertComp(ctx, UpsertCompParams{
		ID:      NewID(),
		EventID: event.ID, Name: "hand", Mode: CompModeAUTO,
	}); err != nil {
		t.Fatalf("re-upserting comp: %v", err)
	}

	got, err := q.GetComp(ctx, GetCompParams{EventID: event.ID, Name: "hand"})
	if err != nil {
		t.Fatalf("reading comp back: %v", err)
	}
	if got.Mode != CompModeMANUAL {
		t.Errorf("mode = %s, want MANUAL preserved", got.Mode)
	}
}

func TestCompSlotRequiresItsComp(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 23)

	err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "ghost", CharacterID: character.ID,
		Role: RoleEnumTANK, SlotIndex: 0, IsBench: false, Reason: "MANUAL: placed by a raid lead",
	})
	if err == nil {
		t.Fatalf("inserting a slot for a comp that does not exist succeeded, want an FK violation")
	}
}

func TestDeleteCompCascadesToItsSlots(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 24)

	if _, err := q.UpsertComp(ctx, UpsertCompParams{
		ID:      NewID(),
		EventID: event.ID, Name: "hand", Mode: CompModeMANUAL,
	}); err != nil {
		t.Fatalf("creating comp: %v", err)
	}
	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "hand", CharacterID: character.ID,
		Role: RoleEnumTANK, SlotIndex: 0, IsBench: false, Reason: "MANUAL: placed by a raid lead",
	}); err != nil {
		t.Fatalf("inserting comp slot: %v", err)
	}

	if err := q.DeleteComp(ctx, DeleteCompParams{EventID: event.ID, Name: "hand"}); err != nil {
		t.Fatalf("deleting comp: %v", err)
	}

	slots, err := q.ListCompSlots(ctx, ListCompSlotsParams{EventID: event.ID, CompName: "hand"})
	if err != nil {
		t.Fatalf("listing slots: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("slots = %+v, want none after the comp was deleted", slots)
	}
}

func TestCompSlotRoundTripsWithReason(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 20)

	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "prog", CharacterID: character.ID,
		Role: RoleEnumTANK, SlotIndex: 0, IsBench: false,
		Reason: "TANK: priority 1, main, first signup",
	}); err != nil {
		t.Fatalf("inserting comp slot: %v", err)
	}

	slots, err := q.ListCompSlots(ctx, ListCompSlotsParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing comp slots: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if slots[0].Reason != "TANK: priority 1, main, first signup" {
		t.Errorf("reason = %q, want it to round-trip", slots[0].Reason)
	}
	if slots[0].Role != RoleEnumTANK || slots[0].IsBench {
		t.Errorf("slot = %+v, want TANK, not benched", slots[0])
	}
}

func TestRelockingReplacesRatherThanColliding(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 21)

	insert := func(role RoleEnum, reason string) error {
		return q.InsertCompSlot(ctx, InsertCompSlotParams{
			ID:      NewID(),
			EventID: event.ID, CompName: "prog", CharacterID: character.ID,
			Role: role, SlotIndex: 0, IsBench: false, Reason: reason,
		})
	}

	if err := insert(RoleEnumTANK, "first lock"); err != nil {
		t.Fatalf("first lock insert: %v", err)
	}

	if err := q.DeleteCompSlots(ctx, DeleteCompSlotsParams{EventID: event.ID, CompName: "prog"}); err != nil {
		t.Fatalf("clearing before relock: %v", err)
	}
	if err := insert(RoleEnumHEALER, "second lock"); err != nil {
		t.Fatalf("second lock insert: %v", err)
	}

	slots, err := q.ListCompSlots(ctx, ListCompSlotsParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing comp slots: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("got %d slots after relock, want 1 (replaced, not accumulated)", len(slots))
	}
	if slots[0].Role != RoleEnumHEALER || slots[0].Reason != "second lock" {
		t.Errorf("slot = %+v, want the second lock's HEALER assignment", slots[0])
	}
}

func TestTwoCompNamesCoexistOnOneEvent(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 22)

	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "prog", CharacterID: character.ID,
		Role: RoleEnumTANK, SlotIndex: 0, IsBench: false, Reason: "prog comp",
	}); err != nil {
		t.Fatalf("inserting prog slot: %v", err)
	}
	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "farm", CharacterID: character.ID,
		Role: RoleEnumHEALER, SlotIndex: 0, IsBench: false, Reason: "farm comp",
	}); err != nil {
		t.Fatalf("inserting farm slot: %v", err)
	}

	prog, err := q.ListCompSlots(ctx, ListCompSlotsParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing prog slots: %v", err)
	}
	farm, err := q.ListCompSlots(ctx, ListCompSlotsParams{EventID: event.ID, CompName: "farm"})
	if err != nil {
		t.Fatalf("listing farm slots: %v", err)
	}

	if len(prog) != 1 || prog[0].Role != RoleEnumTANK {
		t.Errorf("prog slots = %+v, want one TANK slot", prog)
	}
	if len(farm) != 1 || farm[0].Role != RoleEnumHEALER {
		t.Errorf("farm slots = %+v, want one HEALER slot", farm)
	}
}

func TestLockingSetsAssignedRoleAndLeavesStatusAlone(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 23)

	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "prog", CharacterID: character.ID,
		Role: RoleEnumTANK, SlotIndex: 0, IsBench: false, Reason: "TANK: priority 1, main, first signup",
	}); err != nil {
		t.Fatalf("inserting comp slot: %v", err)
	}

	if err := q.ClearAssignedRoles(ctx, event.ID); err != nil {
		t.Fatalf("clearing assigned roles: %v", err)
	}
	tank := RoleEnumTANK
	if err := q.SetSignupAssignedRole(ctx, SetSignupAssignedRoleParams{
		EventID: event.ID, CharacterID: character.ID, AssignedRole: &tank,
	}); err != nil {
		t.Fatalf("setting assigned role: %v", err)
	}

	signups, err := q.ListSignupsForEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("listing signups: %v", err)
	}
	if len(signups) != 1 {
		t.Fatalf("got %d signups, want 1", len(signups))
	}
	if signups[0].AssignedRole == nil || *signups[0].AssignedRole != RoleEnumTANK {
		t.Errorf("assigned_role = %v, want TANK", signups[0].AssignedRole)
	}
	if signups[0].Status != SignupStatusCONFIRMED {
		t.Errorf("status = %s, want CONFIRMED untouched by the lock", signups[0].Status)
	}
}

func TestClearAssignedRolesNullsOutBenchedSignups(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 24)

	tank := RoleEnumTANK
	if err := q.SetSignupAssignedRole(ctx, SetSignupAssignedRoleParams{
		EventID: event.ID, CharacterID: character.ID, AssignedRole: &tank,
	}); err != nil {
		t.Fatalf("setting assigned role from a prior lock: %v", err)
	}

	// A relock where this character ends up benched: ClearAssignedRoles runs, and
	// nothing sets it again for this character.
	if err := q.ClearAssignedRoles(ctx, event.ID); err != nil {
		t.Fatalf("clearing assigned roles: %v", err)
	}

	signups, err := q.ListSignupsForEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("listing signups: %v", err)
	}
	if len(signups) != 1 {
		t.Fatalf("got %d signups, want 1", len(signups))
	}
	if signups[0].AssignedRole != nil {
		t.Errorf("assigned_role = %v, want NULL after a relock that benched this character", signups[0].AssignedRole)
	}
	if signups[0].Status != SignupStatusCONFIRMED {
		t.Errorf("status = %s, want CONFIRMED untouched", signups[0].Status)
	}
}

// seedLockedSlots puts the character in both of seedEventForComp's comps, the state a
// comp lock leaves behind.
func seedLockedSlots(ctx context.Context, t *testing.T, q *Queries, event Event, character Character) {
	t.Helper()

	for i, name := range []string{"prog", "farm"} {
		if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
			ID:      NewID(),
			EventID: event.ID, CompName: name, CharacterID: character.ID,
			Role: RoleEnumTANK, SlotIndex: int16(i), IsBench: false,
			Reason: "TANK: priority 1, main, first signup",
		}); err != nil {
			t.Fatalf("inserting comp slot in %q: %v", name, err)
		}
	}
}

// A raider who goes absent leaves a hole in every draft they were in, not only the one
// someone happens to be looking at.
func TestDropCompSlotsForCharacterEmptiesEveryCompAndReportsWhich(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 25)
	seedLockedSlots(ctx, t, q, event, character)

	if _, err := q.UpsertSignup(ctx, UpsertSignupParams{
		ID:      NewID(),
		EventID: event.ID, CharacterID: character.ID, Status: SignupStatusABSENT,
	}); err != nil {
		t.Fatalf("writing ABSENT signup: %v", err)
	}

	dropped, err := q.DropCompSlotsForCharacter(ctx, DropCompSlotsForCharacterParams{
		EventID: event.ID, CharacterID: character.ID,
	})
	if err != nil {
		t.Fatalf("dropping comp slots: %v", err)
	}
	slices.Sort(dropped)
	if !slices.Equal(dropped, []string{"farm", "prog"}) {
		t.Errorf("dropped = %v, want both comps named", dropped)
	}

	// CountCompSlotsForEvent is what COMP_NAG reads as "locked", so a slot left behind
	// keeps the event looking locked around a seat nobody is in.
	count, err := q.CountCompSlotsForEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("counting comp slots: %v", err)
	}
	if count != 0 {
		t.Errorf("comp slots = %d, want 0", count)
	}
}

// LATE is in the assignment pool: "I'll be 20 minutes late" is still a seat, and
// dropping it would rebuild the comp around someone who is on their way.
func TestDropCompSlotsForCharacterKeepsSeatsStillInThePool(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 26)
	seedLockedSlots(ctx, t, q, event, character)

	until := time.Now().Add(20 * time.Minute)
	if _, err := q.UpsertSignup(ctx, UpsertSignupParams{
		ID:      NewID(),
		EventID: event.ID, CharacterID: character.ID, Status: SignupStatusLATE,
		LateUntil: pgtype.Timestamptz{Time: until, Valid: true},
	}); err != nil {
		t.Fatalf("writing LATE signup: %v", err)
	}

	dropped, err := q.DropCompSlotsForCharacter(ctx, DropCompSlotsForCharacterParams{
		EventID: event.ID, CharacterID: character.ID,
	})
	if err != nil {
		t.Fatalf("dropping comp slots: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none: LATE still holds a seat", dropped)
	}

	count, err := q.CountCompSlotsForEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("counting comp slots: %v", err)
	}
	if count != 2 {
		t.Errorf("comp slots = %d, want 2", count)
	}
}

// A withdrawal deletes the signup outright, which is the strongest form of "not in the
// pool": the NOT EXISTS finds no row at all rather than a row with the wrong status.
func TestDropCompSlotsForCharacterEmptiesSeatsOfAWithdrawnSignup(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, character := seedEventForComp(ctx, t, q, 27)
	seedLockedSlots(ctx, t, q, event, character)

	if err := q.DeleteSignup(ctx, DeleteSignupParams{
		EventID: event.ID, CharacterID: character.ID,
	}); err != nil {
		t.Fatalf("deleting signup: %v", err)
	}

	dropped, err := q.DropCompSlotsForCharacter(ctx, DropCompSlotsForCharacterParams{
		EventID: event.ID, CharacterID: character.ID,
	})
	if err != nil {
		t.Fatalf("dropping comp slots: %v", err)
	}
	slices.Sort(dropped)
	if !slices.Equal(dropped, []string{"farm", "prog"}) {
		t.Errorf("dropped = %v, want both comps named", dropped)
	}
}

// seedLateArrival adds a second raider to an event that already has a locked board,
// which is the situation ListUnseatedForComp exists for.
func seedLateArrival(
	ctx context.Context, t *testing.T, q *Queries, event Event, discordID int64, status SignupStatus,
) Character {
	t.Helper()

	user, err := q.UpsertUser(ctx, UpsertUserParams{ID: NewID(), DiscordID: discordID, DiscordGuildID: 100})
	if err != nil {
		t.Fatalf("upserting user: %v", err)
	}
	character, err := q.CreateCharacter(ctx, CreateCharacterParams{
		ID:     NewID(),
		UserID: user.ID, Name: "Latehealz", Realm: "Area-52", Region: "us", IsMain: true,
	})
	if err != nil {
		t.Fatalf("creating character: %v", err)
	}
	if _, err := q.UpsertSignup(ctx, UpsertSignupParams{
		ID:      NewID(),
		EventID: event.ID, CharacterID: character.ID, Status: status,
	}); err != nil {
		t.Fatalf("signing up late arrival: %v", err)
	}
	return character
}

// The whole point: a board is the snapshot the last lock took, and somebody who signed
// up afterwards holds no slot. Without this they are on the event and on no board.
func TestListUnseatedForCompNamesWhoHoldsNoSlot(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, seated := seedEventForComp(ctx, t, q, 27)
	seedLockedSlots(ctx, t, q, event, seated)

	arrival := seedLateArrival(ctx, t, q, event, 127, SignupStatusCONFIRMED)
	if err := q.SetCharacterRole(ctx, SetCharacterRoleParams{
		CharacterID: arrival.ID, Role: RoleEnumHEALER, Priority: 1,
	}); err != nil {
		t.Fatalf("setting healer role: %v", err)
	}

	rows, err := q.ListUnseatedForComp(ctx, ListUnseatedForCompParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing unseated: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("unseated = %d rows, want 1", len(rows))
	}
	if rows[0].CharacterID != arrival.ID {
		t.Errorf("unseated = %v, want the late arrival %v", rows[0].CharacterID, arrival.ID)
	}
	// A raider with a role menu is unseated because the board predates them, not because
	// the assigner could not place them. The two read differently to a raid lead.
	if !rows[0].HasRoles {
		t.Error("has_roles = false, want true: the arrival set a role")
	}
}

// Declined, tentative, absent and no-show never reach the assigner, so they were never
// candidates for a seat and reporting them as missing one is noise.
func TestListUnseatedForCompIgnoresSignupsOutsideThePool(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, seated := seedEventForComp(ctx, t, q, 28)
	seedLockedSlots(ctx, t, q, event, seated)
	seedLateArrival(ctx, t, q, event, 128, SignupStatusDECLINED)

	rows, err := q.ListUnseatedForComp(ctx, ListUnseatedForCompParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing unseated: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("unseated = %d rows, want 0: a decline is not a missing seat", len(rows))
	}
}

// Assign drops a character with an empty role menu, because comp_slots.role is NOT NULL
// and there is no role to record. They are unseated by every lock rather than by
// arriving late, and has_roles is what tells the two apart.
func TestListUnseatedForCompFlagsAnEmptyRoleMenu(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, seated := seedEventForComp(ctx, t, q, 29)
	seedLockedSlots(ctx, t, q, event, seated)
	seedLateArrival(ctx, t, q, event, 129, SignupStatusCONFIRMED)

	rows, err := q.ListUnseatedForComp(ctx, ListUnseatedForCompParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing unseated: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("unseated = %d rows, want 1", len(rows))
	}
	if rows[0].HasRoles {
		t.Error("has_roles = true, want false: the arrival set no roles")
	}
}

// One event holds several named drafts, and a raider seated in "farm" is still missing
// from "prog". Answering per event rather than per comp would name the wrong people.
func TestListUnseatedForCompAnswersPerComp(t *testing.T) {
	ctx := context.Background()
	q, _ := newTxQueries(ctx, t)

	event, seated := seedEventForComp(ctx, t, q, 30)
	seedLockedSlots(ctx, t, q, event, seated)

	arrival := seedLateArrival(ctx, t, q, event, 130, SignupStatusCONFIRMED)
	if err := q.InsertCompSlot(ctx, InsertCompSlotParams{
		ID:      NewID(),
		EventID: event.ID, CompName: "farm", CharacterID: arrival.ID,
		Role: RoleEnumHEALER, SlotIndex: 2, IsBench: false, Reason: "HEALER: priority 1",
	}); err != nil {
		t.Fatalf("seating the arrival on farm: %v", err)
	}

	farm, err := q.ListUnseatedForComp(ctx, ListUnseatedForCompParams{EventID: event.ID, CompName: "farm"})
	if err != nil {
		t.Fatalf("listing unseated on farm: %v", err)
	}
	if len(farm) != 0 {
		t.Errorf("farm unseated = %d rows, want 0: everybody in the pool holds a seat", len(farm))
	}

	prog, err := q.ListUnseatedForComp(ctx, ListUnseatedForCompParams{EventID: event.ID, CompName: "prog"})
	if err != nil {
		t.Fatalf("listing unseated on prog: %v", err)
	}
	if len(prog) != 1 || prog[0].CharacterID != arrival.ID {
		t.Errorf("prog unseated = %v, want the arrival %v", prog, arrival.ID)
	}
}
