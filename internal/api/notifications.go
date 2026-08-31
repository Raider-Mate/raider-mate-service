package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

type notificationResponse struct {
	ID string `json:"id"`
	// DiscordGuildID is here because one claim now spans every guild: without it the
	// bot cannot tell which guild's session to deliver through, or log which guild a
	// failed delivery belonged to.
	DiscordGuildID string   `json:"discord_guild_id"`
	EventID        string   `json:"event_id"`
	Kind           string   `json:"kind"`
	TargetKind     string   `json:"target_kind"`
	DiscordID      *string  `json:"discord_id,omitempty"`
	RoleIDs        []string `json:"role_ids,omitempty"`
	// DiscordIDs are the users a CHANNEL notification mentions, kept apart from RoleIDs
	// because the two render with different mention syntax.
	DiscordIDs []string `json:"discord_ids,omitempty"`
	ChannelID  *string  `json:"channel_id,omitempty"`
	Payload    rawJSON  `json:"payload"`
	Links      Links    `json:"_links"`
}

// rawJSON marshals its bytes verbatim: the payload column is already JSON.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

func notificationToResponse(n signup.StoredNotification) notificationResponse {
	roleIDs := make([]string, len(n.RoleIDs))
	for i, id := range n.RoleIDs {
		roleIDs[i] = strconv.FormatInt(id, 10)
	}
	discordIDs := make([]string, len(n.DiscordIDs))
	for i, id := range n.DiscordIDs {
		discordIDs[i] = strconv.FormatInt(id, 10)
	}

	links := Links{}
	links.add(true, "delivered", "/api/notifications/"+n.ID.String()+"/delivered", "POST")
	links.add(true, "failed", "/api/notifications/"+n.ID.String()+"/failed", "POST")

	return notificationResponse{
		ID:             n.ID.String(),
		DiscordGuildID: strconv.FormatInt(n.DiscordGuildID, 10),
		EventID:        n.EventID.String(),
		Kind:           string(n.Kind),
		TargetKind:     string(n.TargetKind),
		DiscordID:      snowflakePtrToString(n.DiscordID),
		RoleIDs:        roleIDs,
		DiscordIDs:     discordIDs,
		ChannelID:      snowflakePtrToString(n.ChannelID),
		Payload:        n.Payload,
		Links:          links,
	}
}

const (
	defaultNotificationLimit = 50
	maxNotificationLimit     = 200
)

// listNotificationsHandler claims and returns undelivered notifications across every
// guild.
//
// This route sits behind requireServiceKey, not requireAuth, so there is no actor and
// no guild to scope to. That is deliberate: the outbox has exactly one reader, the bot
// process, and a per-guild scope would cost it one request per guild per tick to poll
// a table it is entitled to read whole. No route reachable by a raider's interaction
// reaches this handler, so recipient ids, channel ids and DM payloads stay out of
// their hands.
func listNotificationsHandler(outbox *signup.Outbox, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := int32(defaultNotificationLimit)
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "limit: "+err.Error())
				return
			}
			// Unclamped, ?limit=-1 reaches Postgres as a negative LIMIT and 500s, and
			// ?limit=0 quietly returns nothing forever.
			if n < 1 || n > maxNotificationLimit {
				writeError(w, logger, http.StatusBadRequest, "limit: must be between 1 and 200")
				return
			}
			limit = int32(n)
		}

		list, err := outbox.Claim(r.Context(), nil, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "claiming notifications", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]notificationResponse, len(list))
		for i, n := range list {
			out[i] = notificationToResponse(n)
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// markNotificationDeliveredHandler acks one notification by id. The guild scope the
// ack used to carry existed to stop one guild's authenticated raider suppressing
// another's reminders; behind requireServiceKey there is no raider to stop, and the
// bot acks whatever it just claimed.
func markNotificationDeliveredHandler(outbox *signup.Outbox, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		err = outbox.MarkDelivered(r.Context(), id, nil)
		switch {
		case errors.Is(err, signup.ErrNotificationNotFound):
			writeError(w, logger, http.StatusNotFound, "notification not found")
		case err != nil:
			logger.ErrorContext(r.Context(), "marking notification delivered", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// markNotificationFailedHandler is the bot reporting a send Discord refused. It acks
// the row, because the same send would be refused again on the next lease, and queues a
// line to the raid lead saying what did not arrive. A reminder that vanished silently
// was the whole reason this route exists.
type markNotificationFailedRequest struct {
	// Reason is what the bot will say to the raid lead. Its Discord error code belongs
	// in here too: 50013 reads differently from a timeout.
	Reason string `json:"reason"`
}

// maxFailureReasonLength keeps a raid lead's channel readable. The bot writes this
// text, so the cap is a guard rather than validation of anything a raider typed.
const maxFailureReasonLength = 300

func markNotificationFailedHandler(outbox *signup.Outbox, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		var req markNotificationFailedRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		if req.Reason == "" {
			writeError(w, logger, http.StatusBadRequest, "reason is required")
			return
		}
		if len(req.Reason) > maxFailureReasonLength {
			req.Reason = req.Reason[:maxFailureReasonLength]
		}

		err = outbox.MarkFailed(r.Context(), id, req.Reason)
		switch {
		case errors.Is(err, signup.ErrNotificationNotFound):
			writeError(w, logger, http.StatusNotFound, "notification not found")
		case err != nil:
			logger.ErrorContext(r.Context(), "marking notification failed", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// queueWatcher is the wake-up signal the stream serves. Declared here, by the consumer,
// so the handler needs nothing from the database and its test needs no container.
type queueWatcher interface {
	Subscribe() (<-chan struct{}, func())
}

// streamHeartbeat is how often an idle stream writes a comment line. A bot whose
// connection died silently, behind a proxy or a laptop lid, finds out on the next one
// instead of waiting for a notification that may be hours away.
const streamHeartbeat = 30 * time.Second

// streamNotificationsHandler holds a Server-Sent Events stream open and writes one
// event whenever something lands in the outbox. The event carries no data: the bot
// answers it by calling the claim route, which is what makes this a signal rather than
// a second delivery path with its own ack semantics to get wrong.
//
// It sits behind requireServiceKey with the other two outbox routes. Losing the stream
// costs latency and nothing else, since the bot still polls on a slow timer.
func streamNotificationsHandler(watcher queueWatcher, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The server sets a 30s write deadline for ordinary handlers, which would cut
		// this one mid-stream. Clearing it is per-response, so the timeout still covers
		// every other route. A ResponseWriter that cannot do this is not fatal: the
		// stream is cut every 30s and the bot reconnects, which is worse than it needs
		// to be but still correct.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			logger.WarnContext(r.Context(), "stream write deadline not clearable", "error", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		queued, unsubscribe := watcher.Subscribe()
		defer unsubscribe()

		heartbeat := time.NewTicker(streamHeartbeat)
		defer heartbeat.Stop()

		// One event before anything happens, so rows queued while the bot was away are
		// claimed on connect rather than sitting until the next insert.
		if !writeStreamEvent(w, rc, "notification") {
			return
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case <-queued:
				if !writeStreamEvent(w, rc, "notification") {
					return
				}
			case <-heartbeat.C:
				if !writeStreamComment(w, rc) {
					return
				}
			}
		}
	}
}

// writeStreamEvent writes one SSE event and flushes it. It reports whether the stream
// is still usable: a failed write means the bot is gone, which is ordinary and not
// worth logging on every disconnect.
func writeStreamEvent(w http.ResponseWriter, rc *http.ResponseController, name string) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", name); err != nil {
		return false
	}
	return rc.Flush() == nil
}

func writeStreamComment(w http.ResponseWriter, rc *http.ResponseController) bool {
	if _, err := io.WriteString(w, ":\n\n"); err != nil {
		return false
	}
	return rc.Flush() == nil
}
