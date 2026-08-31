package signup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// fakeReminderStateStore is the read side ReminderState needs. The write methods are
// here to satisfy eventStore and are never called.
type fakeReminderStateStore struct {
	job      ReminderJob
	jobErr   error
	settings GuildSettings
}

func (s *fakeReminderStateStore) CreateEvent(context.Context, CreateEventInput) (Event, error) {
	return Event{}, nil
}
func (s *fakeReminderStateStore) GetEvent(context.Context, uuid.UUID) (Event, error) {
	return Event{}, nil
}
func (s *fakeReminderStateStore) ListUpcomingEvents(context.Context, int64) ([]Event, error) {
	return nil, nil
}
func (s *fakeReminderStateStore) ListPastEvents(context.Context, int64) ([]Event, error) {
	return nil, nil
}
func (s *fakeReminderStateStore) UpdateEvent(context.Context, UpdateEventInput) (Event, error) {
	return Event{}, nil
}
func (s *fakeReminderStateStore) DeleteEvent(context.Context, uuid.UUID) error { return nil }

func (s *fakeReminderStateStore) CountSignupsByStatus(context.Context, []uuid.UUID) (map[uuid.UUID]map[db.SignupStatus]int, error) {
	return nil, nil
}

func (s *fakeReminderStateStore) PreEventReminderJob(context.Context, uuid.UUID) (ReminderJob, error) {
	return s.job, s.jobErr
}

func (s *fakeReminderStateStore) GuildSettings(context.Context, int64) (GuildSettings, error) {
	return s.settings, nil
}

// A guild asking "did the reminder go out?" gets one word back. Every one of these used
// to read as SENT, including the three that told nobody.
func TestReminderStateReportsWhatBecameOfTheReminder(t *testing.T) {
	runAt := time.Now().Add(30 * time.Minute)
	noChannel := skipNoChannel
	lead := int32(30)
	off := int32(0)

	tests := map[string]struct {
		lead       *int32
		job        ReminderJob
		jobErr     error
		wantState  string
		wantReason *string
		wantRunsAt bool
	}{
		"pending": {
			lead:       &lead,
			job:        ReminderJob{Status: db.JobStatusPENDING, RunAt: runAt},
			wantState:  ReminderScheduled,
			wantRunsAt: true,
		},
		"sent": {
			lead:      &lead,
			job:       ReminderJob{Status: db.JobStatusSENT},
			wantState: ReminderSent,
		},
		"sent but told nobody": {
			lead:       &lead,
			job:        ReminderJob{Status: db.JobStatusSENT, SkipReason: &noChannel},
			wantState:  ReminderSkipped,
			wantReason: &noChannel,
		},
		"failed": {
			lead:      &lead,
			job:       ReminderJob{Status: db.JobStatusFAILED},
			wantState: ReminderFailed,
		},
		"switched off": {
			lead:      &off,
			jobErr:    ErrNoReminderJob,
			wantState: ReminderOff,
		},
		"never scheduled": {
			lead:      &lead,
			jobErr:    ErrNoReminderJob,
			wantState: ReminderSkipped,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			events := NewEvents(&fakeReminderStateStore{job: tt.job, jobErr: tt.jobErr})

			state, err := events.ReminderState(context.Background(),
				Event{ID: uuid.New(), ReminderLeadMinutes: tt.lead})
			if err != nil {
				t.Fatalf("ReminderState: %v", err)
			}

			if state.State != tt.wantState {
				t.Errorf("state = %s, want %s", state.State, tt.wantState)
			}
			if tt.wantReason != nil && (state.Reason == nil || *state.Reason != *tt.wantReason) {
				t.Errorf("reason = %v, want %s", state.Reason, *tt.wantReason)
			}
			if tt.wantRunsAt != (state.RunsAt != nil) {
				t.Errorf("runs_at = %v, want set: %v", state.RunsAt, tt.wantRunsAt)
			}
		})
	}
}

// The event stores the lead its schedule was built from. An event older than the
// setting has none, and the guild's own value is what a reschedule would give it.
func TestReminderStateFallsBackToTheGuildLead(t *testing.T) {
	guildLead := int32(45)
	events := NewEvents(&fakeReminderStateStore{
		jobErr:   ErrNoReminderJob,
		settings: GuildSettings{ReminderLeadMinutes: &guildLead},
	})

	state, err := events.ReminderState(context.Background(), Event{ID: uuid.New()})
	if err != nil {
		t.Fatalf("ReminderState: %v", err)
	}
	if state.LeadMinutes != guildLead {
		t.Errorf("lead = %d, want %d", state.LeadMinutes, guildLead)
	}
	if state.Delivery != DefaultReminderDelivery {
		t.Errorf("delivery = %s, want %s", state.Delivery, DefaultReminderDelivery)
	}
}
