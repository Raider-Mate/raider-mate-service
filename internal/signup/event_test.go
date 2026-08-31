package signup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// The bot records the post it just made through the same PATCH path a raid lead edits
// through, so this predicate is what keeps an announcement from redrawing itself.
func TestChangesWhatIsPostedIgnoresTheBotRecordingItsOwnMessage(t *testing.T) {
	messageID := int64(999)
	channelID := int64(555)

	in := UpdateEventInput{MessageID: &messageID, ChannelID: &channelID}

	if in.changesWhatIsPosted() {
		t.Fatal("recording a message id asked for a redraw of the message it just recorded")
	}
}

func TestChangesWhatIsPostedCoversEveryFieldARaiderReads(t *testing.T) {
	title := "Ulduar"
	at := time.Now()
	difficulty := db.RaidDifficultyMYTHIC
	lead := int32(30)
	report := "https://www.warcraftlogs.com/reports/abc"

	cases := map[string]UpdateEventInput{
		"title":                 {Title: &title},
		"starts_at":             {StartsAt: &at},
		"signup_deadline":       {SignupDeadline: &at},
		"comp_template":         {CompTemplate: []byte(`{"tanks":2}`)},
		"difficulty":            {Difficulty: &difficulty},
		"reminder_lead_minutes": {ReminderLeadMinutes: &lead},
		"warcraftlogs_url":      {WarcraftLogsURL: &report},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if !in.changesWhatIsPosted() {
				t.Fatalf("an edit to %s left the sheet in the channel showing the old one", name)
			}
		})
	}
}

func TestChangesWhatIsPostedIsFalseForAnEmptyEdit(t *testing.T) {
	if (UpdateEventInput{}).changesWhatIsPosted() {
		t.Fatal("an edit that changed nothing asked for a redraw")
	}
}

// countingStore implements only the method SignupCounts uses. The embedded interface
// leaves the rest nil, so a test that starts depending on them panics rather than
// quietly passing against a zero value.
type countingStore struct {
	eventStore
	counts map[uuid.UUID]map[db.SignupStatus]int
}

func (s countingStore) CountSignupsByStatus(context.Context, []uuid.UUID) (map[uuid.UUID]map[db.SignupStatus]int, error) {
	return s.counts, nil
}

func TestSignupCountsSeedsEveryStatus(t *testing.T) {
	answered := db.NewID()
	silent := db.NewID()

	events := NewEvents(countingStore{counts: map[uuid.UUID]map[db.SignupStatus]int{
		// SQL returns only the statuses somebody chose, and nothing at all for an event
		// nobody has answered.
		answered: {db.SignupStatusCONFIRMED: 18, db.SignupStatusDECLINED: 4},
	}})

	got, err := events.SignupCounts(context.Background(), []uuid.UUID{answered, silent})
	if err != nil {
		t.Fatalf("SignupCounts: %v", err)
	}

	all := AllStatuses()
	for _, id := range []uuid.UUID{answered, silent} {
		if len(got[id]) != len(all) {
			t.Errorf("event %s has %d statuses, want all %d seeded", id, len(got[id]), len(all))
		}
	}
	if got[answered][db.SignupStatusCONFIRMED] != 18 {
		t.Errorf("confirmed = %d, want 18", got[answered][db.SignupStatusCONFIRMED])
	}
	// A raider who declined and a status nobody chose are both real zeros a client
	// renders, which is why they are seeded rather than left out.
	if n, ok := got[answered][db.SignupStatusNOSHOW]; !ok || n != 0 {
		t.Errorf("no_show = (%d, present %v), want (0, true)", n, ok)
	}
	// An event nobody answered is a fact, not a gap. An absent key would leave a caller
	// guessing between "zero" and "not allowed to know".
	if n, ok := got[silent][db.SignupStatusCONFIRMED]; !ok || n != 0 {
		t.Errorf("unanswered event confirmed = (%d, present %v), want (0, true)", n, ok)
	}
}

func TestSignupCountsForNoEvents(t *testing.T) {
	// The list handler passes whatever the guild has, and a guild with no events must
	// not reach the database with an empty ANY().
	events := NewEvents(countingStore{})
	got, err := events.SignupCounts(context.Background(), nil)
	if err != nil {
		t.Fatalf("SignupCounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want none", len(got))
	}
}
