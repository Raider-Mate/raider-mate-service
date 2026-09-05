package signup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// Notification is one outbox row. Both LateRequests (LATE_REQUEST_FILED, filed the
// moment a player requests one) and the worker's Runner (commit 3, the scheduled
// kinds) produce these; neither talks to Discord directly.
type Notification struct {
	DiscordGuildID int64
	EventID        uuid.UUID
	Kind           db.NotificationKind
	TargetKind     db.NotificationTarget
	DiscordID      *int64
	RoleIDs        []int64
	// DiscordIDs are the users a CHANNEL notification mentions. Roles and users render
	// with different mention syntax, so they cannot share RoleIDs.
	DiscordIDs []int64
	ChannelID  *int64
	Payload    []byte
}

// ErrRequestDecided means a late request has already been approved or rejected. The
// approve/reject links disappear once that happens, so reaching the endpoint anyway
// is a stale bot button or a second raid lead racing the first.
var ErrRequestDecided = errors.New("late request already decided")

// LateRequest is a late_signup_requests row, translated out of pgtype.
type LateRequest struct {
	ID          uuid.UUID
	EventID     uuid.UUID
	CharacterID uuid.UUID
	Status      db.SignupStatus
	Note        *string
	LateUntil   *time.Time
	State       db.RequestState
	CreatedAt   time.Time
	DecidedAt   *time.Time
}

// LateRequestWrite is what a player files: the status they were trying to write when
// the deadline gate closed on them. A withdrawal past the deadline files DECLINED;
// the table is named for when this happens, not for the status it carries.
//
// LateUntil rides along because it is the field that makes a LATE status actionable:
// dropping it here would approve "I'll be late" with no idea how late.
type LateRequestWrite struct {
	EventID     uuid.UUID
	CharacterID uuid.UUID
	Status      db.SignupStatus
	Note        *string
	LateUntil   *time.Time
}

// lateRequestPayload is what a bot needs to render a LATE_REQUEST_FILED notification.
type lateRequestPayload struct {
	EventTitle  string          `json:"event_title"`
	CharacterID uuid.UUID       `json:"character_id"`
	Status      db.SignupStatus `json:"status"`
}

// lateStore is the persistence LateRequests needs. Declared here, by the consumer.
type lateStore interface {
	GetEvent(ctx context.Context, id uuid.UUID) (Event, error)
	UpsertSignup(ctx context.Context, in SignupWrite) (Signup, []string, error)
	UpsertLateRequest(ctx context.Context, in LateRequestWrite) (LateRequest, error)
	GetLateRequest(ctx context.Context, id uuid.UUID) (LateRequest, error)
	ListLateRequests(ctx context.Context, eventID uuid.UUID) ([]LateRequest, error)
	DecideLateRequest(ctx context.Context, id uuid.UUID, state db.RequestState) error
	RaidLeadRoleIDs(ctx context.Context, discordGuildID int64) ([]int64, error)
	InsertNotification(ctx context.Context, n Notification) error
	TransactLate(ctx context.Context, fn func(ctx context.Context, tx lateStore) error) error
}

// LateRequests files, lists, and decides late signup requests: what a player's write
// becomes once Signups.Write has returned ErrSignupsClosed.
type LateRequests struct {
	store  lateStore
	logger *slog.Logger
}

// NewLateRequests builds a LateRequests. The logger is here for one case: an event
// with no channel_id cannot be notified about, and that has to be visible to whoever
// is wondering why the raid leads never heard about a request.
func NewLateRequests(store lateStore, logger *slog.Logger) *LateRequests {
	return &LateRequests{store: store, logger: logger}
}

// File records a request and notifies the raid lead. Filing writes a
// LATE_REQUEST_FILED notification with no scheduled_jobs row behind it: it fires the
// instant a player asks, not on a poll.
//
// Both land in one transaction. A request sitting in the queue that nobody was told
// about is the failure this endpoint exists to prevent, so it must not be the state a
// failed second insert leaves behind.
func (l *LateRequests) File(ctx context.Context, in LateRequestWrite) (LateRequest, error) {
	var req LateRequest
	err := l.store.TransactLate(ctx, func(ctx context.Context, tx lateStore) error {
		// Nothing gets filed against a raid that has already begun. The queue exists so a
		// raid lead can wave somebody in before the pull; after it there is nothing left
		// to approve, and a pending row would sit in their queue forever.
		event, err := tx.GetEvent(ctx, in.EventID)
		if err != nil {
			return fmt.Errorf("loading event: %w", err)
		}
		if !time.Now().Before(event.StartsAt) {
			return ErrEventStarted
		}

		filed, err := tx.UpsertLateRequest(ctx, in)
		if err != nil {
			return fmt.Errorf("filing late request: %w", err)
		}
		req = filed

		// A ROLE notification with no channel to post in is worse than none; the bot
		// has not learned this event's channel yet (see events.channel_id). The request
		// is still filed and still shows up in the raid lead's queue, but nothing pushes
		// it at them, so this is logged rather than swallowed: a bot that never PATCHes
		// channel_id otherwise looks like a working system that quietly notifies nobody.
		if event.ChannelID == nil {
			l.logger.WarnContext(ctx, "late request filed with no channel to notify in",
				"event_id", event.ID, "request_id", filed.ID)
			return nil
		}

		roleIDs, err := tx.RaidLeadRoleIDs(ctx, event.DiscordGuildID)
		if err != nil {
			return fmt.Errorf("loading raid lead roles: %w", err)
		}
		payload, err := json.Marshal(lateRequestPayload{
			EventTitle: event.Title, CharacterID: in.CharacterID, Status: in.Status,
		})
		if err != nil {
			return fmt.Errorf("encoding notification payload: %w", err)
		}
		if err := tx.InsertNotification(ctx, Notification{
			DiscordGuildID: event.DiscordGuildID,
			EventID:        event.ID,
			Kind:           db.NotificationKindLATEREQUESTFILED,
			TargetKind:     db.NotificationTargetROLE,
			RoleIDs:        roleIDs,
			ChannelID:      event.ChannelID,
			Payload:        payload,
		}); err != nil {
			return fmt.Errorf("writing late-request notification: %w", err)
		}
		return nil
	})
	if err != nil {
		return LateRequest{}, err
	}
	return req, nil
}

// List returns every late request for an event, most recent first.
func (l *LateRequests) List(ctx context.Context, eventID uuid.UUID) ([]LateRequest, error) {
	reqs, err := l.store.ListLateRequests(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("listing late requests: %w", err)
	}
	return reqs, nil
}

// Get loads one late request. The API layer needs it to check the request actually
// belongs to the event in the path before acting on it.
func (l *LateRequests) Get(ctx context.Context, id uuid.UUID) (LateRequest, error) {
	req, err := l.store.GetLateRequest(ctx, id)
	if err != nil {
		return LateRequest{}, fmt.Errorf("loading late request: %w", err)
	}
	return req, nil
}

// Approve upserts the signup with the requested status and marks the request decided.
// Approving something already decided is refused: without the guard, a stale button
// can flip a rejection into a real signup.
//
// One transaction, so the guard means something. Read the state, write the signup, and
// decide the request in separate transactions and two raid leads hitting the same
// button both read PENDING and both get through.
func (l *LateRequests) Approve(ctx context.Context, id uuid.UUID) error {
	return l.store.TransactLate(ctx, func(ctx context.Context, tx lateStore) error {
		req, err := tx.GetLateRequest(ctx, id)
		if err != nil {
			return fmt.Errorf("loading late request: %w", err)
		}
		if req.State != db.RequestStatePENDING {
			return fmt.Errorf("approving late request: %w", ErrRequestDecided)
		}
		// The raid started while this sat in the queue. Approving now would write a
		// signup onto a night that already happened.
		event, err := tx.GetEvent(ctx, req.EventID)
		if err != nil {
			return fmt.Errorf("loading event: %w", err)
		}
		if !time.Now().Before(event.StartsAt) {
			return ErrEventStarted
		}
		_, droppedFrom, err := tx.UpsertSignup(ctx, SignupWrite{
			EventID:     req.EventID,
			CharacterID: req.CharacterID,
			Status:      req.Status,
			Note:        req.Note,
			LateUntil:   req.LateUntil,
		})
		if err != nil {
			return fmt.Errorf("writing approved signup: %w", err)
		}
		if err := tx.DecideLateRequest(ctx, id, db.RequestStateAPPROVED); err != nil {
			return fmt.Errorf("marking late request approved: %w", err)
		}

		// An approval puts a raider back on the sheet, so the message is now wrong in
		// the channel everyone reads. Unconditional, unlike the dropped-slot notice
		// below, which only applies when a locked comp lost a seat.
		if err := notifySignupChanged(ctx, tx, event); err != nil {
			return err
		}

		// Approving a request carrying DECLINED is the case that strands a seat: the
		// raid lead accepted a withdrawal filed after the comp was locked.
		if len(droppedFrom) == 0 {
			return nil
		}
		return notifyCompSlotsDropped(ctx, tx, l.logger, event, req.CharacterID, &req.Status, droppedFrom)
	})
}

// Reject marks the request decided without touching the signup.
func (l *LateRequests) Reject(ctx context.Context, id uuid.UUID) error {
	req, err := l.store.GetLateRequest(ctx, id)
	if err != nil {
		return fmt.Errorf("loading late request: %w", err)
	}
	if req.State != db.RequestStatePENDING {
		return fmt.Errorf("rejecting late request: %w", ErrRequestDecided)
	}
	if err := l.store.DecideLateRequest(ctx, id, db.RequestStateREJECTED); err != nil {
		return fmt.Errorf("marking late request rejected: %w", err)
	}
	return nil
}
