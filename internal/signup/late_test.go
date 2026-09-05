package signup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

func TestFileWritesTheRequestAndNotifiesWhenAChannelIsKnown(t *testing.T) {
	store := newFakeSignupStore()
	channelID := int64(555)
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), DiscordGuildID: 100, ChannelID: &channelID}
	store.roleIDs = []int64{781, 799}

	req, err := NewLateRequests(store, newTestLogger()).File(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusLATE,
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if req.State != db.RequestStatePENDING {
		t.Errorf("state = %s, want PENDING", req.State)
	}
	if len(store.notified) != 1 {
		t.Fatalf("wrote %d notifications, want 1", len(store.notified))
	}
	n := store.notified[0]
	if n.Kind != db.NotificationKindLATEREQUESTFILED {
		t.Errorf("kind = %s, want LATE_REQUEST_FILED", n.Kind)
	}
	if n.TargetKind != db.NotificationTargetROLE {
		t.Errorf("target_kind = %s, want ROLE", n.TargetKind)
	}
	if len(n.RoleIDs) != 2 {
		t.Errorf("role_ids = %v, want the mapped raid lead roles", n.RoleIDs)
	}
}

func TestFileSkipsTheNotificationWithNoChannelKnown(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), DiscordGuildID: 100, ChannelID: nil}

	_, err := NewLateRequests(store, newTestLogger()).File(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusLATE,
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(store.notified) != 0 {
		t.Errorf("wrote %d notifications, want none: no channel to post in", len(store.notified))
	}
	if len(store.lateWritten) != 1 {
		t.Errorf("wrote %d late requests, want 1: filing still succeeds without a channel", len(store.lateWritten))
	}
}

func TestApproveWritesTheSignupAndMarksDecided(t *testing.T) {
	store := newFakeSignupStore()
	eventID, characterID := uuid.New(), uuid.New()
	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: eventID, CharacterID: characterID, Status: db.SignupStatusDECLINED,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}

	if err := NewLateRequests(store, newTestLogger()).Approve(context.Background(), req.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if len(store.written) != 1 {
		t.Fatalf("wrote %d signups, want 1", len(store.written))
	}
	if store.written[0].Status != db.SignupStatusDECLINED {
		t.Errorf("signup status = %s, want DECLINED (the requested status)", store.written[0].Status)
	}
	if store.decided[req.ID] != db.RequestStateAPPROVED {
		t.Errorf("decided state = %s, want APPROVED", store.decided[req.ID])
	}
}

// A withdrawal filed after the comp locked is the case that strands a seat: the write
// happens in Approve, not in Signups.Write, so the eviction has to be reported there
// too or the raid lead never hears about the hole.
func TestApproveNotifiesWhenTheApprovedSignupEmptiesALockedComp(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), ChannelID: &channelID}
	store.dropFrom = []string{"prog comp"}

	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusDECLINED,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}

	if err := NewLateRequests(store, newTestLogger()).Approve(context.Background(), req.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(store.notified))
	}
	if got := store.notified[0].Kind; got != db.NotificationKindCOMPSLOTDROPPED {
		t.Errorf("kind = %v, want COMP_SLOT_DROPPED", got)
	}
}

// Same guarantee on the late-request side: a request filed with nothing telling the
// raid lead is the failure the queue exists to prevent, so the filing fails with it.
func TestFileFailsWhenTheNotificationCannotBeQueued(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), ChannelID: &channelID}
	store.notifyErr = errors.New("outbox unavailable")

	_, err := NewLateRequests(store, newTestLogger()).File(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusLATE,
	})
	if !errors.Is(err, store.notifyErr) {
		t.Fatalf("err = %v, want the notification failure surfaced", err)
	}
}

func TestApproveCarriesLateUntilThroughToTheSignup(t *testing.T) {
	store := newFakeSignupStore()
	until := time.Now().Add(30 * time.Minute)
	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusLATE, LateUntil: &until,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}

	if err := NewLateRequests(store, newTestLogger()).Approve(context.Background(), req.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if len(store.written) != 1 {
		t.Fatalf("wrote %d signups, want 1", len(store.written))
	}
	// Without it, approving "I'll be 20 minutes late" produces a LATE signup that
	// cannot say how late, which is the only thing that makes the status actionable.
	if store.written[0].LateUntil == nil || !store.written[0].LateUntil.Equal(until) {
		t.Errorf("late_until = %v, want %v carried through from the request", store.written[0].LateUntil, until)
	}
}

func TestApproveRefusesARequestAlreadyDecided(t *testing.T) {
	store := newFakeSignupStore()
	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusCONFIRMED,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}
	lateRequests := NewLateRequests(store, newTestLogger())
	if err := lateRequests.Reject(context.Background(), req.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// A stale bot button, or a second raid lead racing the first.
	err = lateRequests.Approve(context.Background(), req.ID)

	if !errors.Is(err, ErrRequestDecided) {
		t.Fatalf("err = %v, want ErrRequestDecided", err)
	}
	if len(store.written) != 0 {
		t.Errorf("wrote %d signups, want none: a rejection must not become a signup", len(store.written))
	}
	if store.decided[req.ID] != db.RequestStateREJECTED {
		t.Errorf("state = %s, want REJECTED to stand", store.decided[req.ID])
	}
}

func TestRejectRefusesARequestAlreadyDecided(t *testing.T) {
	store := newFakeSignupStore()
	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusCONFIRMED,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}
	lateRequests := NewLateRequests(store, newTestLogger())
	if err := lateRequests.Approve(context.Background(), req.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	err = lateRequests.Reject(context.Background(), req.ID)

	if !errors.Is(err, ErrRequestDecided) {
		t.Fatalf("err = %v, want ErrRequestDecided", err)
	}
	if store.decided[req.ID] != db.RequestStateAPPROVED {
		t.Errorf("state = %s, want APPROVED to stand", store.decided[req.ID])
	}
}

func TestRejectMarksDecidedWithoutTouchingTheSignup(t *testing.T) {
	store := newFakeSignupStore()
	req, err := store.UpsertLateRequest(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusLATE,
	})
	if err != nil {
		t.Fatalf("seeding late request: %v", err)
	}

	if err := NewLateRequests(store, newTestLogger()).Reject(context.Background(), req.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if len(store.written) != 0 {
		t.Errorf("wrote %d signups, want none: a rejection never touches the signup", len(store.written))
	}
	if store.decided[req.ID] != db.RequestStateREJECTED {
		t.Errorf("decided state = %s, want REJECTED", store.decided[req.ID])
	}
}

// The queue exists so a raid lead can wave somebody in before the pull. After it there is
// nothing left to approve, and a pending row would sit in the queue forever.
func TestFileIsRefusedOnceTheRaidHasStarted(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(-time.Hour), DiscordGuildID: 100}

	_, err := NewLateRequests(store, newTestLogger()).File(context.Background(), LateRequestWrite{
		EventID: uuid.New(), CharacterID: uuid.New(), Status: db.SignupStatusCONFIRMED,
	})
	if !errors.Is(err, ErrEventStarted) {
		t.Fatalf("err = %v, want ErrEventStarted", err)
	}
	if len(store.lateWritten) != 0 {
		t.Errorf("filed %d requests, want none", len(store.lateWritten))
	}
}

// A request that was pending when the raid began cannot be approved onto it afterwards.
func TestApproveIsRefusedOnceTheRaidHasStarted(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(-time.Hour)}
	id := uuid.New()
	store.lateReqs[id] = LateRequest{
		ID: id, EventID: uuid.New(), CharacterID: uuid.New(),
		Status: db.SignupStatusCONFIRMED, State: db.RequestStatePENDING,
	}

	err := NewLateRequests(store, newTestLogger()).Approve(context.Background(), id)
	if !errors.Is(err, ErrEventStarted) {
		t.Fatalf("err = %v, want ErrEventStarted", err)
	}
	if len(store.written) != 0 {
		t.Errorf("wrote %d signups, want none", len(store.written))
	}
}
