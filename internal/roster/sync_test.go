package roster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
)

type fakeStore struct {
	due        []db.Character
	snapshots  map[uuid.UUID]storedSnapshot
	applied    []applySyncParams
	touched    []uuid.UUID
	attempted  []uuid.UUID
	notFound   []uuid.UUID
	dueErr     error
	latestErrs map[uuid.UUID]error
}

type storedSnapshot struct {
	gear  []byte
	ilvl  float64
	score float64
}

func (s *fakeStore) DueForSync(_ context.Context, _ time.Time, _ int32) ([]db.Character, error) {
	return s.due, s.dueErr
}

func (s *fakeStore) LatestSnapshotGear(_ context.Context, characterID uuid.UUID) ([]byte, float64, float64, bool, error) {
	if err, ok := s.latestErrs[characterID]; ok {
		return nil, 0, 0, false, err
	}
	snap, found := s.snapshots[characterID]
	if !found {
		return nil, 0, 0, false, nil
	}
	return snap.gear, snap.ilvl, snap.score, true, nil
}

func (s *fakeStore) ApplySync(_ context.Context, arg applySyncParams) error {
	s.applied = append(s.applied, arg)
	return nil
}

func (s *fakeStore) TouchSynced(_ context.Context, characterID uuid.UUID) error {
	s.touched = append(s.touched, characterID)
	return nil
}

func (s *fakeStore) MarkSyncAttempted(_ context.Context, characterID uuid.UUID) error {
	s.attempted = append(s.attempted, characterID)
	return nil
}

func (s *fakeStore) MarkNotFound(_ context.Context, characterID uuid.UUID) error {
	s.notFound = append(s.notFound, characterID)
	return nil
}

// jsonbGear mimics what Postgres hands back for a jsonb column: keys reordered and
// whitespace reformatted, so the bytes never match a fresh json.Marshal.
func jsonbGear(t *testing.T, items []raiderio.GearItem) []byte {
	t.Helper()

	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshalling gear: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding gear: %v", err)
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range decoded {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteByte('{')
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		// Postgres orders jsonb keys by length, then bytewise.
		sort.Slice(keys, func(a, b int) bool {
			if len(keys[a]) != len(keys[b]) {
				return len(keys[a]) < len(keys[b])
			}
			return keys[a] < keys[b]
		})
		for j, k := range keys {
			if j > 0 {
				buf.WriteString(", ")
			}
			v, err := json.Marshal(item[k])
			if err != nil {
				t.Fatalf("marshalling %s: %v", k, err)
			}
			fmt.Fprintf(&buf, "%q: %s", k, v)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(']')

	return buf.Bytes()
}

// fetcherFunc adapts a function to profileFetcher.
type fetcherFunc func(ctx context.Context, region, realm, name string) (raiderio.Profile, error)

func (f fetcherFunc) CharacterProfile(ctx context.Context, region, realm, name string) (raiderio.Profile, error) {
	return f(ctx, region, realm, name)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncDueUnchangedProfileTouchesOnly(t *testing.T) {
	id := uuid.New()
	class, spec := "Warrior", "Protection"
	profile := raiderio.Profile{Class: class, Spec: spec, ItemLevel: 415, MythicPlusScore: 3000, Gear: []raiderio.GearItem{{Slot: "head", ItemID: 1, ItemLevel: 415}}}

	store := &fakeStore{
		due:       []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax", Class: &class, Spec: &spec}},
		snapshots: map[uuid.UUID]storedSnapshot{id: {gear: jsonbGear(t, profile.Gear), ilvl: 415, score: 3000}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 0 {
		t.Errorf("applied = %d, want 0 for unchanged profile", len(store.applied))
	}
	if len(store.touched) != 1 || store.touched[0] != id {
		t.Errorf("touched = %v, want [%v]", store.touched, id)
	}
}

func TestSyncDueChangedIlvlWritesSnapshot(t *testing.T) {
	id := uuid.New()
	profile := raiderio.Profile{Class: "Warrior", Spec: "Protection", ItemLevel: 420, MythicPlusScore: 3000}
	gearJSON, _ := json.Marshal(profile.Gear)

	store := &fakeStore{
		due:       []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax"}},
		snapshots: map[uuid.UUID]storedSnapshot{id: {gear: gearJSON, ilvl: 415, score: 3000}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(store.applied))
	}
	if store.applied[0].characterID != id {
		t.Errorf("applied characterID = %v, want %v", store.applied[0].characterID, id)
	}
}

func TestSyncDueFirstSyncWritesSnapshot(t *testing.T) {
	id := uuid.New()
	profile := raiderio.Profile{Class: "Warrior", Spec: "Protection", ItemLevel: 415, MythicPlusScore: 3000}

	store := &fakeStore{
		due: []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax"}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 1 {
		t.Fatalf("applied = %d, want 1 for a character with no prior snapshot", len(store.applied))
	}
}

// A character Raider.IO no longer has is recorded as missing, not as synced. Marking
// it synced was the old behaviour and it made a rename, a transfer or a deletion read
// as a raider standing still with last month's item level.
func TestSyncDueCharacterNotFoundRecordsTheMiss(t *testing.T) {
	id := uuid.New()
	store := &fakeStore{due: []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Ghost"}}}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return raiderio.Profile{}, raiderio.ErrCharacterNotFound
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		// Still not an error for the batch: one raider who renamed must not stop the
		// other forty-nine from syncing.
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.notFound) != 1 {
		t.Errorf("notFound = %d, want 1", len(store.notFound))
	}
	if len(store.touched) != 0 {
		t.Errorf("touched = %d, want 0: a 404 is not a successful sync", len(store.touched))
	}
	if len(store.applied) != 0 {
		t.Errorf("applied = %d, want 0", len(store.applied))
	}
}

func TestSyncDueOneFailureDoesNotAbortBatch(t *testing.T) {
	fail := uuid.New()
	ok := uuid.New()
	profile := raiderio.Profile{Class: "Mage", ItemLevel: 410}

	store := &fakeStore{
		due: []db.Character{
			{ID: fail, Region: "eu", Realm: "ravencrest", Name: "Broken"},
			{ID: ok, Region: "eu", Realm: "ravencrest", Name: "Fine"},
		},
	}
	fetcher := fetcherFunc(func(_ context.Context, _, _, name string) (raiderio.Profile, error) {
		if name == "Broken" {
			return raiderio.Profile{}, errors.New("boom")
		}
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 1 || store.applied[0].characterID != ok {
		t.Errorf("applied = %v, want one entry for %v", store.applied, ok)
	}
	if len(store.attempted) != 1 || store.attempted[0] != fail {
		t.Errorf("attempted = %v, want [%v] so the failure loses its queue position", store.attempted, fail)
	}
	if len(store.touched) != 0 {
		t.Errorf("touched = %v, want none: a failed fetch did not refresh anything", store.touched)
	}
}

func TestSyncDueRespecWritesSnapshot(t *testing.T) {
	id := uuid.New()
	class, oldSpec := "Warrior", "Protection"
	gear := []raiderio.GearItem{{Slot: "head", ItemID: 1, ItemLevel: 415}}
	profile := raiderio.Profile{Class: class, Spec: "Fury", ItemLevel: 415, MythicPlusScore: 3000, Gear: gear}

	store := &fakeStore{
		due:       []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax", Class: &class, Spec: &oldSpec}},
		snapshots: map[uuid.UUID]storedSnapshot{id: {gear: jsonbGear(t, gear), ilvl: 415, score: 3000}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 1 {
		t.Fatalf("applied = %d, want 1: a re-spec is a change even with identical gear", len(store.applied))
	}
}

func TestSyncDueChangedGearWritesSnapshot(t *testing.T) {
	id := uuid.New()
	class, spec := "Warrior", "Protection"
	stored := []raiderio.GearItem{{Slot: "head", ItemID: 1, ItemLevel: 415, GemIDs: []int64{7}}}
	profile := raiderio.Profile{
		Class: class, Spec: spec, ItemLevel: 415, MythicPlusScore: 3000,
		Gear: []raiderio.GearItem{{Slot: "head", ItemID: 1, ItemLevel: 415, GemIDs: []int64{8}}},
	}

	store := &fakeStore{
		due:       []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax", Class: &class, Spec: &spec}},
		snapshots: map[uuid.UUID]storedSnapshot{id: {gear: jsonbGear(t, stored), ilvl: 415, score: 3000}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 1 {
		t.Fatalf("applied = %d, want 1: a regemmed slot is a change", len(store.applied))
	}
}

func TestSyncDueMissingClassKeepsStoredValue(t *testing.T) {
	id := uuid.New()
	class, spec := "Warrior", "Protection"
	gear := []raiderio.GearItem{{Slot: "head", ItemID: 1, ItemLevel: 415}}
	// Raider.IO omitted class and spec.
	profile := raiderio.Profile{ItemLevel: 415, MythicPlusScore: 3000, Gear: gear}

	store := &fakeStore{
		due:       []db.Character{{ID: id, Region: "eu", Realm: "ravencrest", Name: "Danthrax", Class: &class, Spec: &spec}},
		snapshots: map[uuid.UUID]storedSnapshot{id: {gear: jsonbGear(t, gear), ilvl: 415, score: 3000}},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return profile, nil
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.applied) != 0 {
		t.Errorf("applied = %d, want 0: an omitted class is not a change", len(store.applied))
	}
}

func TestSyncDueRateLimitedAbortsBatch(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	calls := 0

	store := &fakeStore{
		due: []db.Character{
			{ID: first, Region: "eu", Realm: "ravencrest", Name: "Danthrax"},
			{ID: second, Region: "eu", Realm: "ravencrest", Name: "Alt"},
		},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		calls++
		return raiderio.Profile{}, raiderio.ErrRateLimited
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	err := s.SyncDue(context.Background(), time.Hour, 10)
	if !errors.Is(err, raiderio.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	if calls != 1 {
		t.Errorf("fetches = %d, want 1: the rest of the batch would be rejected too", calls)
	}
	if len(store.attempted) != 0 {
		t.Errorf("attempted = %v, want none: nothing was learned about the character", store.attempted)
	}
}

func TestSyncDueInvalidAPIKeyAbortsBatch(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	calls := 0

	store := &fakeStore{
		due: []db.Character{
			{ID: first, Region: "eu", Realm: "ravencrest", Name: "Danthrax"},
			{ID: second, Region: "eu", Realm: "ravencrest", Name: "Alt"},
		},
	}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		calls++
		return raiderio.Profile{}, raiderio.ErrInvalidAPIKey
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	err := s.SyncDue(context.Background(), time.Hour, 10)
	if !errors.Is(err, raiderio.ErrInvalidAPIKey) {
		t.Fatalf("err = %v, want ErrInvalidAPIKey", err)
	}

	if calls != 1 {
		t.Errorf("fetches = %d, want 1: a rejected key rejects the rest of the batch too", calls)
	}
	// Marking them would push the whole batch a staleAfter into the future over a
	// misconfigured key, so a corrected key would not take effect until then.
	if len(store.attempted) != 0 {
		t.Errorf("attempted = %v, want none: nothing was learned about the character", store.attempted)
	}
}

func TestSyncDueInvalidRequestDoesNotTouch(t *testing.T) {
	id := uuid.New()
	store := &fakeStore{due: []db.Character{{ID: id, Region: "moon", Realm: "ravencrest", Name: "Danthrax"}}}
	fetcher := fetcherFunc(func(context.Context, string, string, string) (raiderio.Profile, error) {
		return raiderio.Profile{}, raiderio.ErrInvalidRequest
	})

	s := NewSyncer(fetcher, store, GearRules{}, testLogger())
	if err := s.SyncDue(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("SyncDue: %v", err)
	}

	if len(store.touched) != 0 {
		t.Errorf("touched = %v, want none: a rejected request says nothing about the character", store.touched)
	}
	if len(store.attempted) != 1 || store.attempted[0] != id {
		t.Errorf("attempted = %v, want [%v]", store.attempted, id)
	}
}
