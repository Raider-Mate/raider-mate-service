package signup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

type failedMark struct {
	id     uuid.UUID
	status db.JobStatus
}

type skippedMark struct {
	id     uuid.UUID
	reason string
}

// fakeReminderStore stands in for Postgres for Runner. Transact just invokes fn
// against itself: no real transaction, but the same call shape a caller sees.
type fakeReminderStore struct {
	event       Event
	jobs        []db.ScheduledJob
	undecided   []int64
	attending   []AttendingSignup
	signups     []Signup
	roleIDs     []int64
	settings    GuildSettings
	getEventErr error

	notified []Notification
	sentIDs  []uuid.UUID
	skipped  []skippedMark
	failed   []failedMark
}

func (s *fakeReminderStore) Transact(ctx context.Context, fn func(context.Context, reminderStore) error) error {
	return fn(ctx, s)
}

func (s *fakeReminderStore) ClaimDueJobs(context.Context, int32) ([]db.ScheduledJob, error) {
	return s.jobs, nil
}

func (s *fakeReminderStore) MarkJobSent(_ context.Context, id uuid.UUID) error {
	s.sentIDs = append(s.sentIDs, id)
	return nil
}

func (s *fakeReminderStore) MarkJobSkipped(_ context.Context, id uuid.UUID, reason string) error {
	s.skipped = append(s.skipped, skippedMark{id: id, reason: reason})
	return nil
}

func (s *fakeReminderStore) MarkJobFailed(_ context.Context, id uuid.UUID, status db.JobStatus) error {
	s.failed = append(s.failed, failedMark{id: id, status: status})
	return nil
}

func (s *fakeReminderStore) GetEvent(context.Context, uuid.UUID) (Event, error) {
	if s.getEventErr != nil {
		return Event{}, s.getEventErr
	}
	return s.event, nil
}

func (s *fakeReminderStore) ListSignupsForEvent(context.Context, uuid.UUID) ([]Signup, error) {
	return s.signups, nil
}

func (s *fakeReminderStore) ListUndecidedForEvent(context.Context, uuid.UUID) ([]int64, error) {
	return s.undecided, nil
}

func (s *fakeReminderStore) ListAttendingForEvent(context.Context, uuid.UUID) ([]AttendingSignup, error) {
	return s.attending, nil
}

func (s *fakeReminderStore) GuildSettings(context.Context, int64) (GuildSettings, error) {
	return s.settings, nil
}

func (s *fakeReminderStore) RaidLeadRoleIDs(context.Context, int64) ([]int64, error) {
	return s.roleIDs, nil
}

func (s *fakeReminderStore) InsertNotification(_ context.Context, n Notification) error {
	s.notified = append(s.notified, n)
	return nil
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunDueReminder24hEmitsOneRowPerUndecidedUser(t *testing.T) {
	jobID := uuid.New()
	store := &fakeReminderStore{
		event:     Event{Title: "Prog Night"},
		jobs:      []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumREMINDER24H}},
		undecided: []int64{111, 222, 333, 444}, // one raider's four alts already collapsed to one entry each
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 4 {
		t.Fatalf("notified = %d, want 4 (one per undecided discord id)", len(store.notified))
	}
	for _, n := range store.notified {
		if n.Kind != db.NotificationKindREMINDER24H || n.TargetKind != db.NotificationTargetUSER {
			t.Errorf("notification = %+v, want a REMINDER_24H USER row", n)
		}
	}
	if len(store.sentIDs) != 1 || store.sentIDs[0] != jobID {
		t.Errorf("sent = %v, want [%s]", store.sentIDs, jobID)
	}
}

// A guild that has configured nothing gets the ping, so the reminder lands where
// raiders are already looking rather than in a DM nobody opens before a pull.
func TestRunDuePreEventDefaultsToOnePingMentioningEveryoneAttending(t *testing.T) {
	channelID := int64(42)
	messageID := int64(7)
	tank := db.RoleEnumTANK
	store := &fakeReminderStore{
		event: Event{Title: "Prog Night", ChannelID: &channelID, MessageID: &messageID},
		jobs:  []db.ScheduledJob{{ID: uuid.New(), JobType: db.JobEnumREMINDERPREEVENT}},
		attending: []AttendingSignup{
			{DiscordID: 1, AssignedRole: &tank},
			{DiscordID: 2},
		},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("notified = %d, want 1 ping", len(store.notified))
	}
	n := store.notified[0]
	if n.Kind != db.NotificationKindREMINDERPREEVENT || n.TargetKind != db.NotificationTargetCHANNEL {
		t.Errorf("notification = %+v, want a REMINDER_PRE_EVENT CHANNEL row", n)
	}
	if len(n.DiscordIDs) != 2 || n.DiscordIDs[0] != 1 || n.DiscordIDs[1] != 2 {
		t.Errorf("discord_ids = %v, want [1 2]", n.DiscordIDs)
	}
	if n.ChannelID == nil || *n.ChannelID != channelID {
		t.Errorf("channel_id = %v, want %d", n.ChannelID, channelID)
	}

	var payload reminderPreEventPingPayload
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.MessageID == nil || *payload.MessageID != messageID {
		t.Errorf("message_id = %v, want %d, the jump link back to the signup sheet", payload.MessageID, messageID)
	}
}

func TestRunDuePreEventDMsEachAttendeeTheirAssignedRole(t *testing.T) {
	dm := db.ReminderDeliveryDM
	tank := db.RoleEnumTANK
	store := &fakeReminderStore{
		event:    Event{Title: "Prog Night"},
		jobs:     []db.ScheduledJob{{ID: uuid.New(), JobType: db.JobEnumREMINDERPREEVENT}},
		settings: GuildSettings{ReminderDelivery: &dm},
		attending: []AttendingSignup{
			{DiscordID: 1, AssignedRole: &tank},
			{DiscordID: 2},
		},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 2 {
		t.Fatalf("notified = %d, want 2 DMs", len(store.notified))
	}
	for _, n := range store.notified {
		if n.TargetKind != db.NotificationTargetUSER {
			t.Errorf("target = %s, want USER", n.TargetKind)
		}
	}

	var seated reminderPreEventDMPayload
	if err := json.Unmarshal(store.notified[0].Payload, &seated); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if seated.AssignedRole == nil || *seated.AssignedRole != tank {
		t.Errorf("assigned_role = %v, want TANK", seated.AssignedRole)
	}

	// A raider with no seat is still reminded. Their DM simply names no role, which is
	// the case for every event whose comp was never locked.
	var unseated reminderPreEventDMPayload
	if err := json.Unmarshal(store.notified[1].Payload, &unseated); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if unseated.AssignedRole != nil {
		t.Errorf("assigned_role = %v, want nil", unseated.AssignedRole)
	}
}

func TestRunDuePreEventBothSendsThePingAndTheDMs(t *testing.T) {
	both := db.ReminderDeliveryBOTH
	channelID := int64(42)
	store := &fakeReminderStore{
		event:     Event{Title: "Prog Night", ChannelID: &channelID},
		jobs:      []db.ScheduledJob{{ID: uuid.New(), JobType: db.JobEnumREMINDERPREEVENT}},
		settings:  GuildSettings{ReminderDelivery: &both},
		attending: []AttendingSignup{{DiscordID: 1}, {DiscordID: 2}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 3 {
		t.Fatalf("notified = %d, want 3 (one ping and two DMs)", len(store.notified))
	}
	if store.notified[0].TargetKind != db.NotificationTargetCHANNEL {
		t.Errorf("first target = %s, want CHANNEL", store.notified[0].TargetKind)
	}
}

// An event nobody has signed up to has nobody to remind. The job is done, not failed:
// retrying it three times would not conjure a roster. It records why it told nobody,
// so the row does not read as a reminder that went out.
func TestRunDuePreEventWithNobodyAttendingRecordsThatItToldNobody(t *testing.T) {
	jobID := uuid.New()
	channelID := int64(42)
	store := &fakeReminderStore{
		event: Event{Title: "Prog Night", ChannelID: &channelID},
		jobs:  []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumREMINDERPREEVENT}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("notified = %d, want 0", len(store.notified))
	}
	if len(store.sentIDs) != 0 {
		t.Errorf("sent = %v, want none: a job that told nobody did not send", store.sentIDs)
	}
	if len(store.skipped) != 1 || store.skipped[0].id != jobID || store.skipped[0].reason != skipNoRecipients {
		t.Errorf("skipped = %+v, want [{%s %s}]", store.skipped, jobID, skipNoRecipients)
	}
}

// The bot posts the event message after the event exists, so an event can reach its
// reminder with no channel on file. There is nowhere to ping, and inventing one is
// worse than saying nothing. What it must not do is record a send: a reminder that
// reached nobody looked exactly like one that reached the raid, which is how this went
// unnoticed in the first place.
func TestRunDuePreEventPingWithNoChannelRecordsWhyItToldNobody(t *testing.T) {
	jobID := uuid.New()
	store := &fakeReminderStore{
		event:     Event{Title: "Prog Night"},
		jobs:      []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumREMINDERPREEVENT}},
		attending: []AttendingSignup{{DiscordID: 1}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("notified = %d, want 0", len(store.notified))
	}
	if len(store.sentIDs) != 0 {
		t.Errorf("sent = %v, want none: nothing was sent", store.sentIDs)
	}
	if len(store.skipped) != 1 || store.skipped[0].id != jobID || store.skipped[0].reason != skipNoChannel {
		t.Errorf("skipped = %+v, want [{%s %s}]", store.skipped, jobID, skipNoChannel)
	}
}

// The ordinary case, asserted so the reason cannot creep onto a job that did its work.
func TestRunDuePreEventPingRecordsNoSkipReason(t *testing.T) {
	jobID := uuid.New()
	channelID := int64(42)
	store := &fakeReminderStore{
		event:     Event{Title: "Prog Night", ChannelID: &channelID},
		jobs:      []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumREMINDERPREEVENT}},
		attending: []AttendingSignup{{DiscordID: 1}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 1 {
		t.Fatalf("notified = %d, want 1", len(store.notified))
	}
	if len(store.skipped) != 0 {
		t.Errorf("skipped = %+v, want none: the ping went out", store.skipped)
	}
	if len(store.sentIDs) != 1 || store.sentIDs[0] != jobID {
		t.Errorf("sent = %v, want [%s]", store.sentIDs, jobID)
	}
}

// The payload carries a key per status even at zero, so a bot can render "0 absent"
// without hardcoding the enum. Counting only what turned up would hide exactly the
// statuses a raid lead is checking for.
func TestRunDueSignupDeadlineCountsEveryStatusIncludingZeroes(t *testing.T) {
	channelID := int64(42)
	store := &fakeReminderStore{
		event: Event{Title: "Prog Night", ChannelID: &channelID},
		jobs:  []db.ScheduledJob{{ID: uuid.New(), JobType: db.JobEnumSIGNUPDEADLINE}},
		signups: []Signup{
			{Status: db.SignupStatusCONFIRMED},
			{Status: db.SignupStatusCONFIRMED},
			{Status: db.SignupStatusABSENT},
		},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(store.notified) != 1 {
		t.Fatalf("notified = %d, want 1", len(store.notified))
	}

	var payload signupDeadlinePayload
	if err := json.Unmarshal(store.notified[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if len(payload.Counts) != len(AllStatuses()) {
		t.Errorf("counts has %d keys, want %d (one per status)", len(payload.Counts), len(AllStatuses()))
	}
	for status, want := range map[db.SignupStatus]int{
		db.SignupStatusCONFIRMED: 2,
		db.SignupStatusABSENT:    1,
		db.SignupStatusDECLINED:  0,
		db.SignupStatusNOSHOW:    0,
	} {
		if got := payload.Counts[status]; got != want {
			t.Errorf("counts[%s] = %d, want %d", status, got, want)
		}
	}
}

// COMP_NAG is no longer scheduled, but rows an older release wrote are still due. They
// drain silently rather than failing three times over an unknown job type.
func TestRunDueDrainsAnOldCompNagWithoutNotifying(t *testing.T) {
	channelID := int64(555)
	jobID := uuid.New()
	store := &fakeReminderStore{
		event: Event{Title: "Prog Night", ChannelID: &channelID},
		jobs:  []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumCOMPNAG}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("notified = %d, want none: nothing nags about comps", len(store.notified))
	}
	if len(store.failed) != 0 {
		t.Errorf("failed = %+v, want none: the job completes", store.failed)
	}
	if len(store.skipped) != 1 || store.skipped[0].id != jobID || store.skipped[0].reason != skipKindRetired {
		t.Errorf("skipped = %+v, want [{%s %s}]", store.skipped, jobID, skipKindRetired)
	}
}

func TestRunDueRoleJobWithNoChannelIsRecordedSkippedNotRetried(t *testing.T) {
	jobID := uuid.New()
	store := &fakeReminderStore{
		event: Event{Title: "Prog Night", ChannelID: nil, DiscordGuildID: 100},
		jobs:  []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumSIGNUPDEADLINE}},
	}

	if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if len(store.notified) != 0 {
		t.Errorf("notified = %d, want none: no channel to post in", len(store.notified))
	}
	if len(store.failed) != 0 {
		t.Errorf("failed = %v, want none: a missing channel is not retried", store.failed)
	}
	if len(store.skipped) != 1 || store.skipped[0].id != jobID || store.skipped[0].reason != skipNoChannel {
		t.Errorf("skipped = %+v, want [{%s %s}]", store.skipped, jobID, skipNoChannel)
	}
}

func TestRunDueRetryPolicyFailsOnTheThirdAttempt(t *testing.T) {
	tests := []struct {
		attempts   int16
		wantStatus db.JobStatus
	}{
		{attempts: 0, wantStatus: db.JobStatusPENDING},
		{attempts: 1, wantStatus: db.JobStatusPENDING},
		{attempts: 2, wantStatus: db.JobStatusFAILED},
	}

	for _, tt := range tests {
		jobID := uuid.New()
		store := &fakeReminderStore{
			jobs:        []db.ScheduledJob{{ID: jobID, JobType: db.JobEnumREMINDER24H, Attempts: tt.attempts}},
			getEventErr: errors.New("boom"),
		}

		if err := NewRunner(store, newTestLogger()).RunDue(context.Background(), 10); err != nil {
			t.Fatalf("attempts=%d: RunDue: %v", tt.attempts, err)
		}
		if len(store.sentIDs) != 0 {
			t.Errorf("attempts=%d: sent = %v, want none: resolution failed", tt.attempts, store.sentIDs)
		}
		if len(store.failed) != 1 || store.failed[0].status != tt.wantStatus {
			t.Errorf("attempts=%d: failed = %+v, want status %s", tt.attempts, store.failed, tt.wantStatus)
		}
	}
}
