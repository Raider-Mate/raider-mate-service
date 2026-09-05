package api

import (
	"slices"
	"testing"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

func TestSignupLinksPresentOnYourOwnSignup(t *testing.T) {
	links := signupLinks("event-1", "char-1", true, true)

	if _, ok := links["self"]; !ok {
		t.Errorf("links = %v, want self present", links)
	}
	if _, ok := links["withdraw"]; !ok {
		t.Errorf("links = %v, want withdraw present", links)
	}
}

// A raid lead may record that somebody did not turn up. Taking their name off the sheet
// is a different act, and it is not one they are offered.
func TestSignupLinksOfferARaidLeadNoWithdrawOnSomebodyElse(t *testing.T) {
	links := signupLinks("event-1", "char-1", true, false)

	if _, ok := links["self"]; !ok {
		t.Errorf("links = %v, want self present for the NO_SHOW write", links)
	}
	if _, ok := links["withdraw"]; ok {
		t.Errorf("links = %v, want withdraw absent: only the raider may take their name off", links)
	}
}

func TestSignupLinksAbsentForAnUnrelatedActor(t *testing.T) {
	links := signupLinks("event-1", "char-1", false, false)

	if len(links) != 0 {
		t.Errorf("links = %v, want none: the absence of a link is the authorization answer", links)
	}
}

func TestSignupAllowedStatusesMatchWhatTheCallerMayWrite(t *testing.T) {
	tests := []struct {
		name       string
		owned      bool
		isRaidLead bool
		want       []db.SignupStatus
	}{
		{"own signup", true, false, signup.AllowedStatuses(true, false, false)},
		{"own signup, raid lead", true, true, signup.AllowedStatuses(true, true, false)},
		{"another's, raid lead", false, true, []db.SignupStatus{db.SignupStatusNOSHOW}},
		{"another's, player", false, false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := signupToResponse(signup.Signup{}, tt.owned, tt.isRaidLead, false).AllowedStatuses

			want := make([]string, 0, len(tt.want))
			for _, status := range tt.want {
				want = append(want, string(status))
			}
			if len(got) != len(want) || !slices.Equal(got, want) {
				t.Errorf("allowed_statuses = %v, want %v", got, want)
			}
		})
	}
}

// NO_SHOW is the raid lead's judgement about the night, so offering it to a player
// would advertise a transition the write path answers with a 403.
func TestSignupAllowedStatusesWithholdNoShowFromAPlayer(t *testing.T) {
	got := signupToResponse(signup.Signup{}, true, false, false).AllowedStatuses

	if slices.Contains(got, string(db.SignupStatusNOSHOW)) {
		t.Errorf("allowed_statuses = %v, want NO_SHOW withheld", got)
	}
}

// The rule a toxic raid lead would otherwise be able to break: somebody else's answer
// is not theirs to change, so nothing but NO_SHOW is ever advertised on it.
func TestSignupAllowedStatusesWithholdEverythingButNoShowFromARaidLead(t *testing.T) {
	got := signupToResponse(signup.Signup{}, false, true, false).AllowedStatuses

	if len(got) != 1 || got[0] != string(db.SignupStatusNOSHOW) {
		t.Errorf("allowed_statuses = %v, want NO_SHOW alone", got)
	}
}
