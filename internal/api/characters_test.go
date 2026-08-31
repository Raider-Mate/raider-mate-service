package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
)

// fakeCharacterStore satisfies the unexported interface roster.NewCharacters takes.
// Only the methods these tests reach are given behaviour; the rest exist to satisfy
// the interface and fail loudly if a handler starts calling them.
type fakeCharacterStore struct {
	guildID   int64
	ownerID   int64
	character roster.Character
	roles     []roster.RoleChoice
	inGuild   bool

	deleted  bool
	mainSet  *bool
	notFound bool
	// exists makes RegisterCharacter report the raider already has this name, realm
	// and region, which the real store maps from a 23505 on the unique index.
	exists bool

	// guilds is what ListGuildsForDiscordUser answers, and askedAbout records who it
	// was asked about, so a test can prove the guard refused before querying.
	guilds     []int64
	askedAbout int64
}

func (f *fakeCharacterStore) RegisterCharacter(context.Context, roster.RegisterInput) (roster.Character, error) {
	if f.exists {
		return roster.Character{}, roster.ErrCharacterExists
	}
	return f.character, nil
}

func (f *fakeCharacterStore) GetCharacterInGuild(_ context.Context, _ uuid.UUID, discordGuildID int64) (roster.Character, error) {
	if !f.inGuild || discordGuildID != f.guildID {
		return roster.Character{}, pgx.ErrNoRows
	}
	return f.character, nil
}

func (f *fakeCharacterStore) GetCharacterOwner(_ context.Context, _ uuid.UUID, discordGuildID int64) (int64, error) {
	if !f.inGuild || discordGuildID != f.guildID {
		return 0, pgx.ErrNoRows
	}
	return f.ownerID, nil
}

func (f *fakeCharacterStore) DeleteCharacter(context.Context, uuid.UUID, int64) (bool, error) {
	if f.notFound {
		return false, nil
	}
	f.deleted = true
	return true, nil
}

func (f *fakeCharacterStore) SetCharacterMain(_ context.Context, _ uuid.UUID, _ int64, isMain bool) (bool, error) {
	if f.notFound {
		return false, nil
	}
	f.mainSet = &isMain
	return true, nil
}

func (f *fakeCharacterStore) ListCharactersInGuild(context.Context, int64) ([]roster.Character, error) {
	return []roster.Character{f.character}, nil
}

func (f *fakeCharacterStore) ListCharactersByDiscord(context.Context, int64, int64) ([]roster.Character, error) {
	return []roster.Character{f.character}, nil
}

func (f *fakeCharacterStore) ReplaceCharacterRoles(context.Context, uuid.UUID, []roster.RoleChoice) error {
	return nil
}

func (f *fakeCharacterStore) ListCharacterRoles(context.Context, uuid.UUID) ([]roster.RoleChoice, error) {
	return f.roles, nil
}

func (f *fakeCharacterStore) ListRolesForCharacters(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]roster.RoleChoice, error) {
	out := make(map[uuid.UUID][]roster.RoleChoice, len(ids))
	for _, id := range ids {
		out[id] = f.roles
	}
	return out, nil
}

// newCharacterFixture wires a Characters over a fake holding one character owned by
// ownerDiscordID, living in homeGuild.
func newCharacterFixture(ownerDiscordID int64) (*fakeCharacterStore, *roster.Characters, uuid.UUID) {
	id := uuid.New()
	store := &fakeCharacterStore{
		guildID: homeGuild,
		ownerID: ownerDiscordID,
		inGuild: true,
		character: roster.Character{
			ID: id, Name: "Thrall", Realm: "Draenor", Region: "eu", IsMain: true,
		},
		roles: []roster.RoleChoice{
			{Role: db.RoleEnumTANK, Priority: 1},
			{Role: db.RoleEnumMDPS, Priority: 2},
		},
	}
	return store, roster.NewCharacters(store), id
}

// characterRequest builds a request with {cid} already resolved, as the mux would.
func characterRequest(method, target string, actor Actor, id uuid.UUID, body string) *http.Request {
	r := requestAs(method, target, actor, body)
	r.SetPathValue("cid", id.String())
	return r
}

func TestGetCharacterRolesReturnsTheMenuInPriorityOrder(t *testing.T) {
	_, characters, id := newCharacterFixture(1)
	w := httptest.NewRecorder()

	getCharacterRolesHandler(characters, testLogger())(w, characterRequest(
		http.MethodGet, "/api/characters/"+id.String()+"/roles", homeActor(false), id, ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got []roleChoiceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if len(got) != 2 || got[0].Role != string(db.RoleEnumTANK) {
		t.Errorf("roles = %v, want TANK first: the bot pre-ticks the select from this", got)
	}
}

func TestGetCharacterRolesHidesAForeignGuildsCharacter(t *testing.T) {
	store, characters, id := newCharacterFixture(1)
	store.inGuild = false
	w := httptest.NewRecorder()

	getCharacterRolesHandler(characters, testLogger())(w, characterRequest(
		http.MethodGet, "/api/characters/"+id.String()+"/roles", homeActor(false), id, ""))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetCharacterRolesAllowsAGuildmate(t *testing.T) {
	_, characters, id := newCharacterFixture(999) // owned by somebody else
	w := httptest.NewRecorder()

	getCharacterRolesHandler(characters, testLogger())(w, characterRequest(
		http.MethodGet, "/api/characters/"+id.String()+"/roles", homeActor(false), id, ""))

	// A guildmate's role menu is already on every signup list and comp board; hiding
	// it here would only stop the bot rendering what it can already see.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestDeleteCharacterRefusesSomebodyElsesCharacter(t *testing.T) {
	store, characters, id := newCharacterFixture(999)
	w := httptest.NewRecorder()

	deleteCharacterHandler(characters, testLogger())(w, characterRequest(
		http.MethodDelete, "/api/characters/"+id.String(), homeActor(false), id, ""))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if store.deleted {
		t.Error("deleted = true, want the delete refused before it reached the store")
	}
}

func TestDeleteCharacterAllowsARaidLeadCleaningUpTheRoster(t *testing.T) {
	store, characters, id := newCharacterFixture(999)
	w := httptest.NewRecorder()

	deleteCharacterHandler(characters, testLogger())(w, characterRequest(
		http.MethodDelete, "/api/characters/"+id.String(), homeActor(true), id, ""))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if !store.deleted {
		t.Error("deleted = false, want a raid lead able to remove a mistyped registration")
	}
}

func TestDeleteCharacterHidesAForeignGuildsCharacter(t *testing.T) {
	store, characters, id := newCharacterFixture(1)
	store.inGuild = false
	w := httptest.NewRecorder()

	deleteCharacterHandler(characters, testLogger())(w, characterRequest(
		http.MethodDelete, "/api/characters/"+id.String(), homeActor(true), id, ""))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if store.deleted {
		t.Error("deleted = true, want a raid lead confined to their own guild's roster")
	}
}

func TestPatchCharacterSetsIsMain(t *testing.T) {
	store, characters, id := newCharacterFixture(1)
	w := httptest.NewRecorder()

	patchCharacterHandler(characters, testLogger())(w, characterRequest(
		http.MethodPatch, "/api/characters/"+id.String(), homeActor(false), id, `{"is_main":false}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body %s", w.Code, http.StatusOK, w.Body)
	}
	if store.mainSet == nil || *store.mainSet {
		t.Errorf("mainSet = %v, want false written through", store.mainSet)
	}
}

func TestPatchCharacterRejectsAnEmptyBody(t *testing.T) {
	store, characters, id := newCharacterFixture(1)
	w := httptest.NewRecorder()

	patchCharacterHandler(characters, testLogger())(w, characterRequest(
		http.MethodPatch, "/api/characters/"+id.String(), homeActor(false), id, `{}`))

	// is_main is a pointer so that an absent field is distinguishable from false;
	// without that, {} would silently demote the raider's main.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if store.mainSet != nil {
		t.Errorf("mainSet = %v, want nothing written for an absent field", *store.mainSet)
	}
}

func TestCharacterLinksSeparateRoleEditingFromRosterHygiene(t *testing.T) {
	character := roster.Character{ID: uuid.New(), Name: "Thrall"}

	raidLead := characterToResponse(character, false, true)
	if _, ok := raidLead.Links["roles"]; ok {
		t.Error("links has roles, want a role menu to stay self-only even for a raid lead")
	}
	if _, ok := raidLead.Links["delete"]; !ok {
		t.Error("links has no delete, want a raid lead able to clear up the roster")
	}

	owner := characterToResponse(character, true, false)
	if _, ok := owner.Links["roles"]; !ok {
		t.Error("links has no roles, want the owner able to edit their own menu")
	}

	stranger := characterToResponse(character, false, false)
	if _, ok := stranger.Links["delete"]; ok {
		t.Error("links has delete, want the absence of a link to be the answer")
	}
	if _, ok := stranger.Links["role-menu"]; !ok {
		t.Error("links has no role-menu, want a guildmate able to read the menu")
	}
}

func TestCharacterSummaryCarriesTheRoleMenuInPriorityOrder(t *testing.T) {
	id := uuid.New()
	byID := characterSummaries(
		[]roster.Character{{ID: id, Name: "Danthrax"}},
		map[uuid.UUID][]roster.RoleChoice{id: {
			{Role: db.RoleEnumTANK, Priority: 1},
			{Role: db.RoleEnumMDPS, Priority: 2},
		}},
	)

	got := byID[id].Roles
	want := []roleChoiceResponse{{Role: "TANK", Priority: 1}, {Role: "MDPS", Priority: 2}}
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("roles[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// A raider who registered no roles must serialise as [], not null: the bot ranges over
// this to draw the flex marker and would have to special-case a nil.
func TestCharacterSummaryWithNoRoleMenuSerialisesAsAnEmptyArray(t *testing.T) {
	id := uuid.New()
	byID := characterSummaries([]roster.Character{{ID: id, Name: "Danthrax"}}, nil)

	encoded, err := json.Marshal(byID[id])
	if err != nil {
		t.Fatalf("marshalling summary: %v", err)
	}
	if !strings.Contains(string(encoded), `"roles":[]`) {
		t.Errorf("summary = %s, want roles to be an empty array", encoded)
	}
}

// Both shapes carry the link, and the summary carries it without carrying region.
func TestCharacterResponsesCarryTheRaiderIOProfileURL(t *testing.T) {
	id := uuid.New()
	character := roster.Character{ID: id, Name: "Thrall", Realm: "Draenor", Region: "eu"}
	want := "https://raider.io/characters/eu/Draenor/Thrall"

	if got := characterToResponse(character, true, false).RaiderIOURL; got != want {
		t.Errorf("characterResponse.RaiderIOURL = %q, want %q", got, want)
	}

	byID := characterSummaries([]roster.Character{character}, nil)
	if got := byID[id].RaiderIOURL; got != want {
		t.Errorf("characterSummary.RaiderIOURL = %q, want %q", got, want)
	}
}

func TestLookupCharacterOmitsAnUnknownID(t *testing.T) {
	byID := characterSummaries([]roster.Character{{ID: uuid.New(), Name: "Thrall"}}, nil)

	if got := lookupCharacter(byID, uuid.New()); got != nil {
		t.Errorf("summary = %v, want nil rather than an invented name", got)
	}
}

// guildCharacterRequest builds a request with {gid} already resolved, as the mux would.
func guildCharacterRequest(method, target string, actor Actor, body string) *http.Request {
	r := requestAs(method, target, actor, body)
	r.SetPathValue("gid", strconv.FormatInt(homeGuild, 10))
	return r
}

func TestCreateCharacterReportsARetypedRegistration(t *testing.T) {
	store, characters, _ := newCharacterFixture(1)
	store.exists = true
	w := httptest.NewRecorder()

	createCharacterHandler(characters, testLogger())(w, guildCharacterRequest(
		http.MethodPost, "/api/guilds/1/characters", homeActor(false),
		`{"name":"Danthrax","realm":"Draenor","region":"eu","is_main":true}`))

	// 409 and not 500: raider-mate-bot shows an APIError's message only when the
	// status is below 500, so a 500 here reaches the raider as "the roster service is
	// having a bad time" and tells them nothing they can act on.
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body %s", w.Code, http.StatusConflict, w.Body)
	}
	if !strings.Contains(w.Body.String(), "already registered") {
		t.Errorf("body = %s, want a message the bot can pass through to Discord", w.Body)
	}
}

func TestCreateCharacterReturnsTheRegisteredCharacter(t *testing.T) {
	_, characters, id := newCharacterFixture(1)
	w := httptest.NewRecorder()

	createCharacterHandler(characters, testLogger())(w, guildCharacterRequest(
		http.MethodPost, "/api/guilds/1/characters", homeActor(false),
		`{"name":"Thrall","realm":"Draenor","region":"eu","is_main":true}`))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body %s", w.Code, http.StatusCreated, w.Body)
	}
	var got characterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if got.ID != id.String() {
		t.Errorf("id = %s, want %s", got.ID, id)
	}
}

func (f *fakeCharacterStore) ListGuildsForDiscordUser(_ context.Context, discordID int64) ([]int64, error) {
	f.askedAbout = discordID
	return f.guilds, nil
}

// Zero and "not established" are different answers, and JSON has one obvious way to
// get them confused. A raider who enchanted nothing is 0 of 8; a season the worker has
// no rules for is no field at all. omitempty drops a nil pointer and keeps a pointer
// to zero, which is the whole reason these are pointers.
func TestCharacterResponseTellsZeroApartFromUnknown(t *testing.T) {
	none := 0
	character := roster.Character{
		ID:              uuid.New(),
		Name:            "Thrall",
		EnchantsMissing: &none,
		TierPieces:      &none,
	}

	body, err := json.Marshal(characterToResponse(character, true, false))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, `"enchants_missing":0`) {
		t.Errorf("body = %s, want enchants_missing present at 0", got)
	}
	if !strings.Contains(got, `"tier_pieces":0`) {
		t.Errorf("body = %s, want tier_pieces present at 0", got)
	}
	// Never counted, so never claimed. EnchantsExpected was left nil above.
	if strings.Contains(got, "enchants_expected") {
		t.Errorf("body = %s, want an unestablished count left out entirely", got)
	}
	if strings.Contains(got, "progression") {
		t.Errorf("body = %s, want no progression for a character who has not raided", got)
	}
}

func TestCharacterResponseCarriesTheRaidSlugWithItsCounts(t *testing.T) {
	character := roster.Character{
		ID:   uuid.New(),
		Name: "Thrall",
		Progression: &roster.RaidProgress{
			Slug: "liberation-of-undermine", Bosses: 8, NormalKilled: 8, HeroicKilled: 6, MythicKilled: 2,
		},
	}

	got := characterToResponse(character, true, false).Progression
	if got == nil {
		t.Fatal("progression is absent, want the tracked raid")
	}
	// The slug travels with the numbers so a client never guesses which tier they
	// describe, and a row left over from last tier is obvious rather than wrong.
	if got.Raid != "liberation-of-undermine" {
		t.Errorf("raid = %q, want liberation-of-undermine", got.Raid)
	}
	if got.Bosses != 8 || got.Normal != 8 || got.Heroic != 6 || got.Mythic != 2 {
		t.Errorf("counts = %+v, want 8/8/6/2", got)
	}
}
