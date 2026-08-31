package signup

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// fakeOutboxStore stands in for Postgres for Outbox.
type fakeOutboxStore struct {
	notification StoredNotification
	event        Event
	roleIDs      []int64

	delivered []uuid.UUID
	inserted  []Notification
}

func (s *fakeOutboxStore) ClaimNotifications(context.Context, *int64, time.Time, int32) ([]StoredNotification, error) {
	return nil, nil
}

func (s *fakeOutboxStore) GetNotification(context.Context, uuid.UUID) (StoredNotification, error) {
	return s.notification, nil
}

func (s *fakeOutboxStore) MarkNotificationDelivered(_ context.Context, id uuid.UUID, _ *int64) error {
	s.delivered = append(s.delivered, id)
	return nil
}

func (s *fakeOutboxStore) GetEvent(context.Context, uuid.UUID) (Event, error) {
	return s.event, nil
}

func (s *fakeOutboxStore) RaidLeadRoleIDs(context.Context, int64) ([]int64, error) {
	return s.roleIDs, nil
}

func (s *fakeOutboxStore) InsertNotification(_ context.Context, n Notification) error {
	s.inserted = append(s.inserted, n)
	return nil
}

// A reminder Discord refused used to be acked and forgotten, so a guild whose bot had
// lost Send Messages in the events channel simply stopped getting reminders with
// nothing anywhere saying so.
func TestMarkFailedAcksAndTellsTheRaidLead(t *testing.T) {
	id := uuid.New()
	channelID := int64(42)
	store := &fakeOutboxStore{
		notification: StoredNotification{
			ID:             id,
			DiscordGuildID: 100,
			EventID:        uuid.New(),
			Kind:           db.NotificationKindREMINDERPREEVENT,
		},
		event:   Event{Title: "Prog Night", ChannelID: &channelID},
		roleIDs: []int64{7, 8},
	}

	if err := NewOutbox(store).MarkFailed(context.Background(), id, "missing permissions (50013)"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	if len(store.delivered) != 1 || store.delivered[0] != id {
		t.Errorf("delivered = %v, want [%s]: the same send would be refused again", store.delivered, id)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted = %d, want 1 report", len(store.inserted))
	}

	report := store.inserted[0]
	if report.Kind != db.NotificationKindDELIVERYFAILED || report.TargetKind != db.NotificationTargetROLE {
		t.Errorf("report = %+v, want a DELIVERY_FAILED ROLE row", report)
	}
	if len(report.RoleIDs) != 2 || report.ChannelID == nil || *report.ChannelID != channelID {
		t.Errorf("report addressed to roles %v in channel %v, want [7 8] in 42", report.RoleIDs, report.ChannelID)
	}

	var payload deliveryFailedPayload
	if err := json.Unmarshal(report.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Title != "Prog Night" || payload.FailedKind != string(db.NotificationKindREMINDERPREEVENT) {
		t.Errorf("payload = %+v, want the event title and the kind that failed", payload)
	}
	if payload.Reason != "missing permissions (50013)" {
		t.Errorf("reason = %q, want the bot's own words", payload.Reason)
	}
}

// Otherwise a broken events channel files a report about the report it could not file,
// on every claim, forever.
func TestMarkFailedDoesNotReportAFailedReport(t *testing.T) {
	id := uuid.New()
	channelID := int64(42)
	store := &fakeOutboxStore{
		notification: StoredNotification{ID: id, Kind: db.NotificationKindDELIVERYFAILED},
		event:        Event{Title: "Prog Night", ChannelID: &channelID},
		roleIDs:      []int64{7},
	}

	if err := NewOutbox(store).MarkFailed(context.Background(), id, "missing access (50001)"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	if len(store.delivered) != 1 {
		t.Errorf("delivered = %v, want the report acked", store.delivered)
	}
	if len(store.inserted) != 0 {
		t.Errorf("inserted = %+v, want none: a report about a report loops", store.inserted)
	}
}

// An event with no channel has nowhere to be told, and there is no second address: a
// raid lead is a role id here, not somebody to DM.
func TestMarkFailedWithNoChannelStillAcks(t *testing.T) {
	id := uuid.New()
	store := &fakeOutboxStore{
		notification: StoredNotification{ID: id, Kind: db.NotificationKindREMINDER24H},
		event:        Event{Title: "Prog Night"},
	}

	if err := NewOutbox(store).MarkFailed(context.Background(), id, "cannot send messages to this user (50007)"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	if len(store.delivered) != 1 {
		t.Errorf("delivered = %v, want the row acked", store.delivered)
	}
	if len(store.inserted) != 0 {
		t.Errorf("inserted = %+v, want none: nowhere to report", store.inserted)
	}
}
