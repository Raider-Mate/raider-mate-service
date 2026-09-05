// Package signup implements event creation, the multi-role signup flow, its deadline
// gate, and the late-request queue a player's write falls into once that deadline has
// passed. Roles live on the character (internal/roster), never here: a signup means
// "I am coming, here is my role menu," and assignment (internal/comp) happens later.
package signup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// selfReported are the statuses a raider may write about their own character. ABSENT
// is one of them: it is a planned absence ("I am out for a while"), which only the
// raider knows, as against DECLINED, which answers this one event. NO_SHOW is the
// exception, because it is a raid lead's judgement about what happened on the night,
// not something anyone reports about themselves.
var selfReported = []db.SignupStatus{
	db.SignupStatusCONFIRMED, db.SignupStatusTENTATIVE, db.SignupStatusDECLINED,
	db.SignupStatusLATE, db.SignupStatusABSENT,
}

// AllowedStatuses returns the statuses this caller may write on this signup. Write
// enforces the same list, so the API cannot advertise a status the write path refuses.
//
// started closes the sheet: once the raid has begun, a raider's own answer is history and
// the only write left is a raid lead recording a no-show. See ErrEventStarted.
//
// A signup is the raider's own answer and it stays theirs. A raid lead is offered
// nothing on somebody else's but NO_SHOW, which records what happened on the night
// rather than rewriting what that person said they would do. Who actually plays is
// decided by the comp, which is the raid lead's to build; changing a raider's stated
// answer is not the same act and must not be available to anyone but them.
func AllowedStatuses(owned, isRaidLead, started bool) []db.SignupStatus {
	if started {
		// NO_SHOW survives, and only for a raid lead, because it is the one status that
		// is about what happened rather than what somebody intended.
		if isRaidLead {
			return []db.SignupStatus{db.SignupStatusNOSHOW}
		}
		return nil
	}
	if owned {
		return slices.Clone(selfReported)
	}
	if isRaidLead {
		return []db.SignupStatus{db.SignupStatusNOSHOW}
	}
	return nil
}

// AllStatuses is every value of the enum, for input validation.
func AllStatuses() []db.SignupStatus {
	return append(slices.Clone(selfReported), db.SignupStatusNOSHOW)
}

// ErrSignupsClosed means a player wrote to an event whose signup_deadline has passed.
// A raid lead write never returns it. The caller (the API handler) is expected to
// catch this and file a late_signup_requests row instead of surfacing a bare error the
// bot has to invent a message for.
var ErrSignupsClosed = errors.New("signups closed")

// ErrEventStarted means somebody tried to change a signup after the raid began.
//
// Distinct from ErrSignupsClosed, and the difference matters: signups closing is what the
// late-request queue exists for, and a raid lead can wave somebody through. A raid that
// has already started is past all of that. Nobody signs off a night they were part of,
// and the API must not file a late request for it either, because there is no longer
// anything a raid lead could usefully approve.
//
// The one write that survives is a raid lead setting NO_SHOW, which records what happened
// rather than changing what anyone said they would do.
var ErrEventStarted = errors.New("event has started")

// ErrStatusRequiresRaidLead means a player tried to write NO_SHOW. design.md section 3
// makes it raid-lead-controlled regardless of who owns the character or where the
// deadline stands.
var ErrStatusRequiresRaidLead = errors.New("status requires raid lead")

// ErrSignupNotYours means a raid lead tried to write somebody else's answer. NO_SHOW is
// the only one of those that is theirs to write.
var ErrSignupNotYours = errors.New("only the raider may set that status")

// Signup is a character's response to an event, translated out of pgtype into plain
// Go types.
type Signup struct {
	ID           uuid.UUID
	EventID      uuid.UUID
	CharacterID  uuid.UUID
	Status       db.SignupStatus
	AssignedRole *db.RoleEnum
	LateUntil    *time.Time
	Note         *string
	CreatedAt    time.Time
}

// SignupWrite is a signup create-or-update. LateUntil is a plain write-through field:
// Write nils it out unless Status is LATE, so "I'll be 20 minutes late" only sticks
// when it is the actionable status design.md:240 describes.
type SignupWrite struct {
	EventID     uuid.UUID
	CharacterID uuid.UUID
	Status      db.SignupStatus
	Note        *string
	LateUntil   *time.Time
}

// signupStore is the persistence Signups needs. Declared here, by the consumer.
type signupStore interface {
	GetEvent(ctx context.Context, id uuid.UUID) (Event, error)
	UpsertSignup(ctx context.Context, in SignupWrite) (Signup, []string, error)
	DeleteSignup(ctx context.Context, eventID, characterID uuid.UUID) ([]string, error)
	ListSignupsForEvent(ctx context.Context, eventID uuid.UUID) ([]Signup, error)
	RaidLeadRoleIDs(ctx context.Context, discordGuildID int64) ([]int64, error)
	InsertNotification(ctx context.Context, n Notification) error
	TransactSignups(ctx context.Context, fn func(ctx context.Context, tx signupStore) error) error
}

// Signups writes and reads signups, gated by the signup deadline and by who is
// allowed to write which status.
type Signups struct {
	store  signupStore
	logger *slog.Logger
}

// NewSignups builds a Signups. The logger is here for the same case LateRequests has
// one: an event with no channel_id cannot be notified about, and that has to be
// visible to whoever is wondering why the raid leads never heard about a drop.
func NewSignups(store signupStore, logger *slog.Logger) *Signups {
	return &Signups{store: store, logger: logger}
}

// Write creates or updates a signup. owned says the caller owns the character;
// isRaidLead governs the deadline gate (a raid lead can always write) and, on a
// character that is not theirs, limits them to NO_SHOW.
func (s *Signups) Write(ctx context.Context, in SignupWrite, owned, isRaidLead bool) (Signup, error) {
	event, err := s.store.GetEvent(ctx, in.EventID)
	if err != nil {
		return Signup{}, fmt.Errorf("loading event: %w", err)
	}
	started := !time.Now().Before(event.StartsAt)

	if !slices.Contains(AllowedStatuses(owned, isRaidLead, started), in.Status) {
		// Once the raid has begun the refusal is about the clock, not about who is
		// asking, and saying so is what stops a client filing a late request nobody can
		// act on.
		if started {
			return Signup{}, ErrEventStarted
		}
		// Two different refusals: a raider reaching for the raid lead's status, and a
		// raid lead reaching into somebody else's answer.
		if owned {
			return Signup{}, ErrStatusRequiresRaidLead
		}
		return Signup{}, ErrSignupNotYours
	}

	if err := checkDeadline(event, &in.Status, isRaidLead, time.Now()); err != nil {
		return Signup{}, err
	}

	if in.Status != db.SignupStatusLATE {
		in.LateUntil = nil
	}

	// The write and the notification that reports it share a transaction. Splitting
	// them means a raider can be pulled out of a locked comp while the message saying
	// so is lost to a failed insert, and the raid lead turns up to a hole.
	var signup Signup
	err = s.store.TransactSignups(ctx, func(ctx context.Context, tx signupStore) error {
		written, droppedFrom, err := tx.UpsertSignup(ctx, in)
		if err != nil {
			return fmt.Errorf("writing signup: %w", err)
		}
		signup = written
		if err := notifySignupChanged(ctx, tx, event); err != nil {
			return err
		}
		return notifyCompSlotsDropped(ctx, tx, s.logger, event, in.CharacterID, &in.Status, droppedFrom)
	})
	if err != nil {
		return Signup{}, err
	}
	return signup, nil
}

// Withdraw deletes a signup. Taking a name off the sheet is the raider's own act, so
// the caller has to own the character; the handler refuses anyone else, raid lead
// included.
//
// Past the deadline this closes for players the same way a new signup would: the gate
// treats "I can no longer come" as a write like any other, so a late withdrawal is a
// late request carrying DECLINED, not a silent delete.
//
// Taking a name off the sheet gives up a seat the same as a status change does, so it
// drops comp slots and tells the raid lead on the same terms.
func (s *Signups) Withdraw(ctx context.Context, eventID, characterID uuid.UUID, isRaidLead bool) error {
	event, err := s.store.GetEvent(ctx, eventID)
	if err != nil {
		return fmt.Errorf("loading event: %w", err)
	}
	// Nobody, raid lead included. A signup is the record of what somebody said they would
	// do, and deleting it after the night erases the evidence a no-show is judged
	// against. Marking NO_SHOW is the write that belongs here instead.
	if !time.Now().Before(event.StartsAt) {
		return ErrEventStarted
	}
	if err := checkDeadline(event, nil, isRaidLead, time.Now()); err != nil {
		return err
	}

	return s.store.TransactSignups(ctx, func(ctx context.Context, tx signupStore) error {
		droppedFrom, err := tx.DeleteSignup(ctx, eventID, characterID)
		if err != nil {
			return fmt.Errorf("withdrawing signup: %w", err)
		}
		if err := notifySignupChanged(ctx, tx, event); err != nil {
			return err
		}
		return notifyCompSlotsDropped(ctx, tx, s.logger, event, characterID, nil, droppedFrom)
	})
}

// List returns every signup for an event, in signup order.
func (s *Signups) List(ctx context.Context, eventID uuid.UUID) ([]Signup, error) {
	signups, err := s.store.ListSignupsForEvent(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("listing signups: %w", err)
	}
	return signups, nil
}

// checkDeadline is the deadline gate: a pure function of the event's timing, the status
// being written, who is writing, and the current time.
//
// LATE and ABSENT run until the pull. Both report what is happening on the night rather
// than an intention, and a raider who finds out at ten to eight that they are held up
// has nothing useful to say if the gate shut an hour ago. Every other status still
// closes at signup_deadline. status is nil on a withdrawal, which names none.
func checkDeadline(event Event, status *db.SignupStatus, isRaidLead bool, now time.Time) error {
	if isRaidLead {
		return nil
	}

	deadline := event.SignupDeadline
	if status != nil && (*status == db.SignupStatusLATE || *status == db.SignupStatusABSENT) &&
		event.StartsAt.After(deadline) {
		deadline = event.StartsAt
	}
	if now.After(deadline) {
		return ErrSignupsClosed
	}
	return nil
}

// notifySignupChanged asks the bot to redraw the event message, because the answers it
// is showing have just changed.
//
// The bot redraws by itself only for its own button clicks. Every other route into a
// signup, the dashboard above all, used to leave the message in the channel showing an
// answer nobody had given for minutes or hours. This closes that.
//
// MESSAGE target and an empty payload: there is no sentence to write. The bot re-reads
// the event and rebuilds the card, which is why this notification does not need the bot
// to understand the kind. An event with no message to edit gets nothing queued, so a
// bot that has not posted one yet is not handed work it cannot do.
func notifySignupChanged(ctx context.Context, store compNotifier, event Event) error {
	if event.ChannelID == nil || event.MessageID == nil {
		return nil
	}
	if err := store.InsertNotification(ctx, Notification{
		DiscordGuildID: event.DiscordGuildID,
		EventID:        event.ID,
		Kind:           db.NotificationKindSIGNUPCHANGED,
		TargetKind:     db.NotificationTargetMESSAGE,
		ChannelID:      event.ChannelID,
		Payload:        []byte("{}"),
	}); err != nil {
		return fmt.Errorf("writing signup-changed notification: %w", err)
	}
	return nil
}

// compSlotsDroppedPayload is what a bot needs to render a COMP_SLOT_DROPPED
// notification: who left, what they said, and which comps now have a hole.
//
// Status is absent on a withdrawal, which deletes the signup rather than restating it.
// "Someone took their name off" is a different sentence from "someone is absent", and a
// status invented to fill the field would make the bot write the wrong one.
type compSlotsDroppedPayload struct {
	EventTitle  string           `json:"event_title"`
	CharacterID uuid.UUID        `json:"character_id"`
	Status      *db.SignupStatus `json:"status,omitempty"`
	CompNames   []string         `json:"comp_names"`
}

// compNotifier is the slice of the store the dropped-slot notification needs. Both
// Signups.Write and LateRequests.Approve write signups, so both can empty a locked
// comp and both emit this.
type compNotifier interface {
	RaidLeadRoleIDs(ctx context.Context, discordGuildID int64) ([]int64, error)
	InsertNotification(ctx context.Context, n Notification) error
}

// notifyCompSlotsDropped tells the raid lead that a locked comp just lost someone.
// Doing nothing for an empty comps is the common case: most writes happen before
// anything is locked.
func notifyCompSlotsDropped(ctx context.Context, store compNotifier, logger *slog.Logger, event Event, characterID uuid.UUID, status *db.SignupStatus, comps []string) error {
	if len(comps) == 0 {
		return nil
	}

	// Same trade as LateRequests.File: a ROLE notification with no channel to post in
	// would address nobody, and the write itself has already happened. Logged rather
	// than swallowed, so a bot that never PATCHes channel_id does not look like a
	// working system that quietly notifies no one.
	if event.ChannelID == nil {
		logger.WarnContext(ctx, "comp slots dropped with no channel to notify in",
			"event_id", event.ID, "character_id", characterID)
		return nil
	}

	roleIDs, err := store.RaidLeadRoleIDs(ctx, event.DiscordGuildID)
	if err != nil {
		return fmt.Errorf("loading raid lead roles: %w", err)
	}
	payload, err := json.Marshal(compSlotsDroppedPayload{
		EventTitle: event.Title, CharacterID: characterID, Status: status, CompNames: comps,
	})
	if err != nil {
		return fmt.Errorf("encoding notification payload: %w", err)
	}
	if err := store.InsertNotification(ctx, Notification{
		DiscordGuildID: event.DiscordGuildID,
		EventID:        event.ID,
		Kind:           db.NotificationKindCOMPSLOTDROPPED,
		TargetKind:     db.NotificationTargetROLE,
		RoleIDs:        roleIDs,
		ChannelID:      event.ChannelID,
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("writing comp-slot-dropped notification: %w", err)
	}
	return nil
}
