package signup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// StoredNotification is a notifications row, translated out of pgtype.
type StoredNotification struct {
	ID             uuid.UUID
	DiscordGuildID int64
	EventID        uuid.UUID
	Kind           db.NotificationKind
	TargetKind     db.NotificationTarget
	DiscordID      *int64
	RoleIDs        []int64
	DiscordIDs     []int64
	ChannelID      *int64
	Payload        []byte
	CreatedAt      time.Time
}

// ErrNotificationNotFound means the id does not exist, or belongs to another guild.
// The two are deliberately indistinguishable to the caller.
var ErrNotificationNotFound = errors.New("notification not found")

// claimLease is how long a claimed notification stays off other pollers' lists. Long
// enough that a bot sending a batch of DMs is not raced, short enough that a bot which
// died mid-batch has its work redelivered promptly.
const claimLease = 5 * time.Minute

// outboxStore is the persistence Outbox needs. Declared here, by the consumer.
type outboxStore interface {
	ClaimNotifications(ctx context.Context, guildID *int64, claimedBefore time.Time, limit int32) ([]StoredNotification, error)
	GetNotification(ctx context.Context, id uuid.UUID) (StoredNotification, error)
	MarkNotificationDelivered(ctx context.Context, id uuid.UUID, discordGuildID *int64) error
	GetEvent(ctx context.Context, id uuid.UUID) (Event, error)
	RaidLeadRoleIDs(ctx context.Context, discordGuildID int64) ([]int64, error)
	InsertNotification(ctx context.Context, n Notification) error
}

// deliveryFailedPayload is what the raid lead is told: which event, what kind of
// message, and what Discord said. Short, because it is read in a channel.
type deliveryFailedPayload struct {
	Title string `json:"title"`
	// FailedKind is the notification_kind that could not be sent.
	FailedKind string `json:"failed_kind"`
	// Reason is the bot's own words, Discord's error code included where it had one.
	Reason string `json:"reason"`
}

// Outbox is the bot's read/ack side of the notifications table: it claims undelivered
// rows and acks each after sending. Delivery is at-least-once: a bot that sends a DM
// and dies before acking will send it again once its claim lease expires, acceptable
// for reminders and simpler than a two-phase ack. What the claim removes is the case
// that is not acceptable, two pollers sending the same batch on every tick.
type Outbox struct {
	store outboxStore
}

// NewOutbox builds an Outbox.
func NewOutbox(store outboxStore) *Outbox {
	return &Outbox{store: store}
}

// Claim takes up to limit undelivered notifications for one guild and marks them
// claimed, so a concurrent poller gets a different batch.
func (o *Outbox) Claim(ctx context.Context, guildID *int64, limit int32) ([]StoredNotification, error) {
	notifications, err := o.store.ClaimNotifications(ctx, guildID, time.Now().Add(-claimLease), limit)
	if err != nil {
		return nil, fmt.Errorf("claiming notifications: %w", err)
	}
	return notifications, nil
}

// MarkFailed acks one notification the bot could not deliver, and tells the raid lead
// it did not arrive. Discord refuses sends for reasons the service cannot see: the bot
// losing Send Messages in the events channel, the channel being deleted, a raider
// closing their DMs. Retrying any of those on the next lease sends the same message to
// the same refusal, so the row is acked and the guild is told instead
// (docs/design.md section on delivery: continue the job, report what did not reach).
//
// The report is addressed to the raid lead roles in the event's channel, because a raid
// lead is known here as a role id rather than as somebody to DM. When that channel is
// itself what is broken, the report cannot land either; scheduled_jobs.skip_reason and
// the event's reminder state still record it.
//
// The ack and the report are separate writes. A failure between them leaves the
// original acked and unreported, which is the same outcome as before this existed and
// better than a report for a row that is still pending.
func (o *Outbox) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	failed, err := o.store.GetNotification(ctx, id)
	if err != nil {
		return fmt.Errorf("reading failed notification: %w", err)
	}

	if err := o.store.MarkNotificationDelivered(ctx, id, nil); err != nil {
		return fmt.Errorf("acking failed notification: %w", err)
	}

	// A report that cannot be delivered would file a report about itself, forever.
	if failed.Kind == db.NotificationKindDELIVERYFAILED {
		return nil
	}

	event, err := o.store.GetEvent(ctx, failed.EventID)
	if err != nil {
		return fmt.Errorf("loading event to report on: %w", err)
	}
	if event.ChannelID == nil {
		return nil
	}

	roleIDs, err := o.store.RaidLeadRoleIDs(ctx, failed.DiscordGuildID)
	if err != nil {
		return fmt.Errorf("loading raid lead roles: %w", err)
	}

	payload, err := json.Marshal(deliveryFailedPayload{
		Title: event.Title, FailedKind: string(failed.Kind), Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("encoding payload: %w", err)
	}

	if err := o.store.InsertNotification(ctx, Notification{
		DiscordGuildID: failed.DiscordGuildID,
		EventID:        failed.EventID,
		Kind:           db.NotificationKindDELIVERYFAILED,
		TargetKind:     db.NotificationTargetROLE,
		RoleIDs:        roleIDs,
		ChannelID:      event.ChannelID,
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("queueing delivery failure report: %w", err)
	}
	return nil
}

// MarkDelivered acks one notification. A nil discordGuildID acks by id alone, which
// only the bot's service-key route may do; anything a raider can reach passes their
// guild, or they could suppress another guild's reminders.
func (o *Outbox) MarkDelivered(ctx context.Context, id uuid.UUID, discordGuildID *int64) error {
	if err := o.store.MarkNotificationDelivered(ctx, id, discordGuildID); err != nil {
		return fmt.Errorf("marking notification delivered: %w", err)
	}
	return nil
}
