package signup

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// fakeSignupStore stands in for Postgres for both Signups and LateRequests: the two
// share GetEvent and UpsertSignup, same precedent as fakeCompStore in internal/comp.
type fakeSignupStore struct {
	event Event

	written []SignupWrite
	deleted []uuid.UUID
	listed  []Signup
	// dropFrom is what UpsertSignup and DeleteSignup report having emptied. The pool
	// rule that decides this lives in the real store, so the fake answers what the
	// test set.
	dropFrom []string

	lateWritten []LateRequestWrite
	lateReqs    map[uuid.UUID]LateRequest
	decided     map[uuid.UUID]db.RequestState
	roleIDs     []int64
	notified    []Notification
	notifyErr   error
}

func newFakeSignupStore() *fakeSignupStore {
	return &fakeSignupStore{
		// A raid in the future by default. The zero Event reads as one that started at
		// the epoch, and every signup write is refused on an event that has begun, so
		// leaving it zero would make every test here a test of that one rule.
		event:    Event{StartsAt: time.Now().Add(24 * time.Hour), SignupDeadline: time.Now().Add(12 * time.Hour)},
		lateReqs: map[uuid.UUID]LateRequest{},
		decided:  map[uuid.UUID]db.RequestState{},
	}
}

// The two Transact methods just invoke fn against the fake, same shape as
// fakeReminderStore.Transact: no real transaction, but the call the caller makes.
func (s *fakeSignupStore) TransactSignups(ctx context.Context, fn func(context.Context, signupStore) error) error {
	return fn(ctx, s)
}

func (s *fakeSignupStore) TransactLate(ctx context.Context, fn func(context.Context, lateStore) error) error {
	return fn(ctx, s)
}

func (s *fakeSignupStore) GetEvent(context.Context, uuid.UUID) (Event, error) {
	return s.event, nil
}

func (s *fakeSignupStore) UpsertSignup(_ context.Context, in SignupWrite) (Signup, []string, error) {
	s.written = append(s.written, in)
	return Signup{EventID: in.EventID, CharacterID: in.CharacterID, Status: in.Status, Note: in.Note, LateUntil: in.LateUntil}, s.dropFrom, nil
}

func (s *fakeSignupStore) DeleteSignup(_ context.Context, _, characterID uuid.UUID) ([]string, error) {
	s.deleted = append(s.deleted, characterID)
	return s.dropFrom, nil
}

func (s *fakeSignupStore) ListSignupsForEvent(context.Context, uuid.UUID) ([]Signup, error) {
	return s.listed, nil
}

func (s *fakeSignupStore) UpsertLateRequest(_ context.Context, in LateRequestWrite) (LateRequest, error) {
	s.lateWritten = append(s.lateWritten, in)
	req := LateRequest{
		ID: uuid.New(), EventID: in.EventID, CharacterID: in.CharacterID,
		Status: in.Status, Note: in.Note, LateUntil: in.LateUntil, State: db.RequestStatePENDING,
	}
	s.lateReqs[req.ID] = req
	return req, nil
}

func (s *fakeSignupStore) GetLateRequest(_ context.Context, id uuid.UUID) (LateRequest, error) {
	return s.lateReqs[id], nil
}

func (s *fakeSignupStore) ListLateRequests(context.Context, uuid.UUID) ([]LateRequest, error) {
	reqs := make([]LateRequest, 0, len(s.lateReqs))
	for _, r := range s.lateReqs {
		reqs = append(reqs, r)
	}
	return reqs, nil
}

// DecideLateRequest updates the stored row too, not just the audit map: the state
// guard in Approve/Reject reads it back, so a fake that only recorded the call would
// make a decided request look pending forever.
func (s *fakeSignupStore) DecideLateRequest(_ context.Context, id uuid.UUID, state db.RequestState) error {
	s.decided[id] = state
	if req, ok := s.lateReqs[id]; ok {
		req.State = state
		s.lateReqs[id] = req
	}
	return nil
}

func (s *fakeSignupStore) RaidLeadRoleIDs(context.Context, int64) ([]int64, error) {
	return s.roleIDs, nil
}

func (s *fakeSignupStore) InsertNotification(_ context.Context, n Notification) error {
	if s.notifyErr != nil {
		return s.notifyErr
	}
	s.notified = append(s.notified, n)
	return nil
}

func TestWritePassesBeforeTheDeadlineForAPlayer(t *testing.T) {
	now := time.Now()
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: now.Add(time.Hour)}

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{Status: db.SignupStatusCONFIRMED}, true, false)
	if err != nil {
		t.Fatalf("Write before deadline: %v", err)
	}
	if len(store.written) != 1 {
		t.Fatalf("wrote %d signups, want 1", len(store.written))
	}
}

func TestWriteRejectsAPlayerPastTheDeadline(t *testing.T) {
	now := time.Now()
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: now.Add(-time.Hour)}

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{Status: db.SignupStatusCONFIRMED}, true, false)
	if !errors.Is(err, ErrSignupsClosed) {
		t.Fatalf("err = %v, want ErrSignupsClosed", err)
	}
	if len(store.written) != 0 {
		t.Errorf("wrote %d signups, want none", len(store.written))
	}
}

func TestWritePassesForARaidLeadPastTheDeadline(t *testing.T) {
	now := time.Now()
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: now.Add(-time.Hour)}

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{Status: db.SignupStatusCONFIRMED}, true, true)
	if err != nil {
		t.Fatalf("Write past deadline as raid lead: %v", err)
	}
	if len(store.written) != 1 {
		t.Fatalf("wrote %d signups, want 1", len(store.written))
	}
}

// Both statuses report what is happening on the night, so the gate for them is the
// pull rather than the signup deadline.
func TestWriteAcceptsLateAndAbsentPastTheDeadline(t *testing.T) {
	now := time.Now()
	for _, status := range []db.SignupStatus{db.SignupStatusLATE, db.SignupStatusABSENT} {
		t.Run(string(status), func(t *testing.T) {
			store := newFakeSignupStore()
			store.event = Event{SignupDeadline: now.Add(-time.Hour), StartsAt: now.Add(time.Hour)}

			_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{Status: status}, true, false)
			if err != nil {
				t.Fatalf("Write %s past deadline: %v", status, err)
			}
			if len(store.written) != 1 {
				t.Fatalf("wrote %d signups, want 1", len(store.written))
			}
		})
	}
}

func TestWriteRejectsLateOnceTheRaidHasStarted(t *testing.T) {
	now := time.Now()
	store := newFakeSignupStore()
	store.event = Event{SignupDeadline: now.Add(-2 * time.Hour), StartsAt: now.Add(-time.Hour)}

	lateUntil := now.Add(time.Hour)
	_, err := NewSignups(store, newTestLogger()).Write(context.Background(),
		SignupWrite{Status: db.SignupStatusLATE, LateUntil: &lateUntil}, true, false)
	// ErrEventStarted rather than ErrSignupsClosed: a closed sheet is what the late queue
	// is for, and a raid that has begun is past anything a raid lead could approve.
	if !errors.Is(err, ErrEventStarted) {
		t.Fatalf("err = %v, want ErrEventStarted", err)
	}
	if len(store.written) != 0 {
		t.Errorf("wrote %d signups, want none", len(store.written))
	}
}

// TestWriteStatusAuthority walks every status against who is writing and whose signup
// it is.
//
// A signup is the raider's own answer. On their own character everyone writes the
// self-reported set and nobody writes NO_SHOW. On somebody else's, a raid lead writes
// NO_SHOW and nothing else, because that records what happened on the night rather than
// rewriting what that person said they would do.
func TestWriteStatusAuthority(t *testing.T) {
	tests := []struct {
		status     db.SignupStatus
		owned      bool
		isRaidLead bool
		wantErr    error
	}{
		{db.SignupStatusCONFIRMED, true, false, nil},
		{db.SignupStatusTENTATIVE, true, false, nil},
		{db.SignupStatusDECLINED, true, false, nil},
		{db.SignupStatusLATE, true, false, nil},
		{db.SignupStatusABSENT, true, false, nil},
		{db.SignupStatusNOSHOW, true, false, ErrStatusRequiresRaidLead},

		{db.SignupStatusABSENT, true, true, nil},
		{db.SignupStatusNOSHOW, true, true, ErrStatusRequiresRaidLead},

		// The whole point: a raid lead cannot answer for somebody else.
		{db.SignupStatusCONFIRMED, false, true, ErrSignupNotYours},
		{db.SignupStatusDECLINED, false, true, ErrSignupNotYours},
		{db.SignupStatusABSENT, false, true, ErrSignupNotYours},
		{db.SignupStatusNOSHOW, false, true, nil},
	}

	for _, tt := range tests {
		who := "player"
		if tt.isRaidLead {
			who = "raid lead"
		}
		whose := "own"
		if !tt.owned {
			whose = "another's"
		}
		name := string(tt.status) + "/" + who + "/" + whose
		t.Run(name, func(t *testing.T) {
			store := newFakeSignupStore()
			store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour)}

			_, err := NewSignups(store, newTestLogger()).Write(
				context.Background(), SignupWrite{Status: tt.status}, tt.owned, tt.isRaidLead)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}

			want := 1
			if tt.wantErr != nil {
				want = 0
			}
			if len(store.written) != want {
				t.Errorf("wrote %d signups, want %d", len(store.written), want)
			}
		})
	}
}

func TestWriteNotifiesWhenTheWriteEmptiesALockedComp(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}
	store.dropFrom = []string{"prog comp"}

	if _, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusABSENT,
	}, true, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(store.notified))
	}
	if got := store.notified[0].Kind; got != db.NotificationKindCOMPSLOTDROPPED {
		t.Errorf("kind = %v, want COMP_SLOT_DROPPED", got)
	}
}

// The write and its notification share a transaction, so a notification that cannot be
// queued has to fail the write rather than leave a raider pulled out of a locked comp
// with nothing telling the raid lead about the hole.
func TestWriteFailsWhenTheDroppedSlotNotificationCannotBeQueued(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}
	store.dropFrom = []string{"prog comp"}
	store.notifyErr = errors.New("outbox unavailable")

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusABSENT,
	}, true, false)
	if !errors.Is(err, store.notifyErr) {
		t.Fatalf("err = %v, want the notification failure surfaced", err)
	}
}

func TestWriteNotifiesNobodyWhenNoCompSlotWasHeld(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}

	if _, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusDECLINED,
	}, true, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("queued %d notifications, want none", len(store.notified))
	}
}

func TestWriteClearsLateUntilWhenStatusIsNotLate(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour)}
	lateUntil := time.Now().Add(20 * time.Minute)

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusCONFIRMED, LateUntil: &lateUntil,
	}, true, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if store.written[0].LateUntil != nil {
		t.Errorf("late_until = %v, want nil: only meaningful alongside LATE", store.written[0].LateUntil)
	}
}

func TestWriteKeepsLateUntilWhenStatusIsLate(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour)}
	lateUntil := time.Now().Add(20 * time.Minute)

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusLATE, LateUntil: &lateUntil,
	}, true, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if store.written[0].LateUntil == nil || !store.written[0].LateUntil.Equal(lateUntil) {
		t.Errorf("late_until = %v, want %v", store.written[0].LateUntil, lateUntil)
	}
}

// Taking a name off the sheet gives up a seat the same as going absent does, and the
// payload carries no status: "someone withdrew" is not "someone is declined".
func TestWithdrawDropsTheSeatAndNotifiesWithNoStatus(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}
	store.dropFrom = []string{"prog comp"}

	if err := NewSignups(store, newTestLogger()).Withdraw(
		context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(store.notified))
	}
	if got := store.notified[0].Kind; got != db.NotificationKindCOMPSLOTDROPPED {
		t.Fatalf("kind = %v, want COMP_SLOT_DROPPED", got)
	}

	var payload compSlotsDroppedPayload
	if err := json.Unmarshal(store.notified[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Status != nil {
		t.Errorf("status = %v, want none on a withdrawal", *payload.Status)
	}
	if len(payload.CompNames) != 1 || payload.CompNames[0] != "prog comp" {
		t.Errorf("comp names = %v, want the comp that lost the seat", payload.CompNames)
	}
}

func TestWithdrawNotifiesNobodyWhenNoSeatWasHeld(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}

	if err := NewSignups(store, newTestLogger()).Withdraw(
		context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if len(store.notified) != 0 {
		t.Errorf("queued %d notifications, want none", len(store.notified))
	}
}

func TestWithdrawRejectsAPlayerPastTheDeadline(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(-time.Hour)}

	err := NewSignups(store, newTestLogger()).Withdraw(context.Background(), uuid.New(), uuid.New(), false)
	if !errors.Is(err, ErrSignupsClosed) {
		t.Fatalf("err = %v, want ErrSignupsClosed", err)
	}
	if len(store.deleted) != 0 {
		t.Errorf("deleted %d signups, want none", len(store.deleted))
	}
}

// postedEvent is an event the bot has already posted a message for, which is the only
// state a redraw makes sense in.
func postedEvent() Event {
	channelID, messageID := int64(42), int64(777)
	return Event{
		StartsAt:       time.Now().Add(2 * time.Hour),
		SignupDeadline: time.Now().Add(time.Hour),
		ChannelID:      &channelID,
		MessageID:      &messageID,
	}
}

func TestWriteAsksForARedrawOfTheEventMessage(t *testing.T) {
	store := newFakeSignupStore()
	store.event = postedEvent()

	if _, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusCONFIRMED,
	}, true, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(store.notified))
	}
	n := store.notified[0]
	if n.Kind != db.NotificationKindSIGNUPCHANGED {
		t.Errorf("kind = %v, want SIGNUP_CHANGED", n.Kind)
	}
	// MESSAGE is what makes this an edit of the card rather than something written at
	// somebody. A bot reads the target before it reads the kind.
	if n.TargetKind != db.NotificationTargetMESSAGE {
		t.Errorf("target = %v, want MESSAGE", n.TargetKind)
	}
}

func TestWithdrawAsksForARedrawOfTheEventMessage(t *testing.T) {
	store := newFakeSignupStore()
	store.event = postedEvent()

	if err := NewSignups(store, newTestLogger()).Withdraw(
		context.Background(), uuid.New(), uuid.New(), false,
	); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("queued %d notifications, want 1", len(store.notified))
	}
	if got := store.notified[0].Kind; got != db.NotificationKindSIGNUPCHANGED {
		t.Errorf("kind = %v, want SIGNUP_CHANGED", got)
	}
}

// An event the bot never posted has no message to edit. Queueing a redraw for it would
// hand the poller work it can only log and drop.
func TestWriteAsksForNoRedrawWhenThereIsNoMessageToEdit(t *testing.T) {
	channelID := int64(42)
	store := newFakeSignupStore()
	store.event = Event{StartsAt: time.Now().Add(2 * time.Hour), SignupDeadline: time.Now().Add(time.Hour), ChannelID: &channelID}

	if _, err := NewSignups(store, newTestLogger()).Write(context.Background(), SignupWrite{
		Status: db.SignupStatusCONFIRMED,
	}, true, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("queued %d notifications, want none", len(store.notified))
	}
}

// Once the pull happens the sheet is history. These are the rule the dashboard and the
// bot both inherit, so they are asserted here rather than in either client.
func TestWriteRejectsEveryRaiderStatusOnceTheRaidHasStarted(t *testing.T) {
	for _, status := range selfReported {
		t.Run(string(status), func(t *testing.T) {
			store := newFakeSignupStore()
			store.event = Event{
				SignupDeadline: time.Now().Add(-2 * time.Hour),
				StartsAt:       time.Now().Add(-time.Hour),
			}

			_, err := NewSignups(store, newTestLogger()).Write(context.Background(),
				SignupWrite{Status: status}, true, false)
			if !errors.Is(err, ErrEventStarted) {
				t.Fatalf("err = %v, want ErrEventStarted", err)
			}
			if len(store.written) != 0 {
				t.Errorf("wrote %d signups, want none", len(store.written))
			}
		})
	}
}

// The one write that outlives the pull, because it records what happened rather than
// changing what anybody said they would do.
func TestWriteAllowsARaidLeadToMarkNoShowAfterTheRaidHasStarted(t *testing.T) {
	store := newFakeSignupStore()
	store.event = Event{
		SignupDeadline: time.Now().Add(-2 * time.Hour),
		StartsAt:       time.Now().Add(-time.Hour),
	}

	_, err := NewSignups(store, newTestLogger()).Write(context.Background(),
		SignupWrite{Status: db.SignupStatusNOSHOW}, false, true)
	if err != nil {
		t.Fatalf("Write NO_SHOW after the raid started: %v", err)
	}
	if len(store.written) != 1 {
		t.Fatalf("wrote %d signups, want 1", len(store.written))
	}
}

// Nobody, raid lead included: a signup is the record a no-show is judged against, and
// deleting it after the night erases the evidence.
func TestWithdrawIsRefusedOnceTheRaidHasStarted(t *testing.T) {
	for _, isRaidLead := range []bool{false, true} {
		store := newFakeSignupStore()
		store.event = Event{
			SignupDeadline: time.Now().Add(-2 * time.Hour),
			StartsAt:       time.Now().Add(-time.Hour),
		}

		err := NewSignups(store, newTestLogger()).Withdraw(
			context.Background(), uuid.New(), uuid.New(), isRaidLead)
		if !errors.Is(err, ErrEventStarted) {
			t.Fatalf("raid lead %v: err = %v, want ErrEventStarted", isRaidLead, err)
		}
		if len(store.deleted) != 0 {
			t.Errorf("raid lead %v: deleted %d signups, want none", isRaidLead, len(store.deleted))
		}
	}
}

func TestAllowedStatusesAfterTheRaidHasStarted(t *testing.T) {
	if got := AllowedStatuses(true, false, true); len(got) != 0 {
		t.Errorf("a raider is offered %v, want nothing", got)
	}
	if got := AllowedStatuses(true, true, true); len(got) != 1 || got[0] != db.SignupStatusNOSHOW {
		t.Errorf("a raid lead is offered %v, want NO_SHOW alone", got)
	}
	if got := AllowedStatuses(false, false, true); len(got) != 0 {
		t.Errorf("a stranger is offered %v, want nothing", got)
	}
}
