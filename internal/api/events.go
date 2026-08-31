package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

type eventResponse struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Title          string          `json:"title"`
	StartsAt       time.Time       `json:"starts_at"`
	SignupDeadline time.Time       `json:"signup_deadline"`
	CompTemplate   json.RawMessage `json:"comp_template"`
	MessageID      *string         `json:"message_id,omitempty"`
	ChannelID      *string         `json:"channel_id,omitempty"`
	Difficulty     *string         `json:"difficulty,omitempty"`
	// ReminderLeadMinutes is the resolved lead time rather than what the caller asked
	// for: a create that named none reads back the guild default. 0 means no reminder.
	ReminderLeadMinutes *int32 `json:"reminder_lead_minutes,omitempty"`
	// WarcraftLogsURL is absent until a raid lead attaches a report. Clients render the
	// warcraftlogs link rather than building this URL themselves.
	WarcraftLogsURL *string `json:"warcraftlogs_url,omitempty"`
	// SignupCounts is this event's signups tallied by status, every status present even
	// at zero. Which of them reads as "coming" is the client's to decide from what it is
	// rendering, so no total is sent: a calendar cell wants confirmed, a raid lead
	// chasing answers wants the silence.
	//
	// Present on reads, absent on a create or an edit response. A write is answered with
	// the event that was written, not with a tally the caller did not ask for and, on a
	// create, could only ever be zeros.
	SignupCounts map[string]int `json:"signup_counts,omitempty"`
	Links        Links          `json:"_links"`
}

// withSignupCounts attaches a tally to a response. Separate from eventToResponse
// because only the two read paths have one: the write paths would have to run a query
// purely to answer a question nobody asked.
func withSignupCounts(resp eventResponse, counts map[db.SignupStatus]int) eventResponse {
	if counts == nil {
		return resp
	}
	out := make(map[string]int, len(counts))
	for status, n := range counts {
		out[string(status)] = n
	}
	resp.SignupCounts = out
	return resp
}

func eventToResponse(e signup.Event, actor Actor) eventResponse {
	href := "/api/events/" + e.ID.String()

	links := Links{}
	links.add(true, "self", href, "")
	links.add(true, "signups", href+"/signups", "")
	links.add(true, "comps", href+"/comps", "")
	links.add(actor.IsRaidLead, "edit", href, "PATCH")
	links.add(actor.IsRaidLead, "delete", href, "DELETE")
	links.add(actor.IsRaidLead, "late-requests", href+"/late-requests", "")
	// Attaching a report is an edit, but it gets its own rel: a client showing a
	// "Link the logs" control must not have to infer that edit covers it, and the rel
	// disappears for a raider exactly as edit does.
	links.add(actor.IsRaidLead, "set-warcraftlogs", href, "PATCH")
	// The report itself, once there is one. An external href, because that is where
	// the resource actually lives.
	if e.WarcraftLogsURL != nil {
		links.add(true, "warcraftlogs", *e.WarcraftLogsURL, "")
	}

	return eventResponse{
		ID:             e.ID.String(),
		Type:           string(e.Type),
		Title:          e.Title,
		StartsAt:       e.StartsAt,
		SignupDeadline: e.SignupDeadline,
		CompTemplate:   e.CompTemplate,
		MessageID:      snowflakePtrToString(e.MessageID),
		ChannelID:      snowflakePtrToString(e.ChannelID),
		Difficulty:     raidDifficultyToString(e.Difficulty),

		ReminderLeadMinutes: e.ReminderLeadMinutes,
		WarcraftLogsURL:     e.WarcraftLogsURL,
		Links:               links,
	}
}

func snowflakePtrToString(id *int64) *string {
	if id == nil {
		return nil
	}
	s := strconv.FormatInt(*id, 10)
	return &s
}

func stringToSnowflakePtr(s *string) (*int64, error) {
	if s == nil {
		return nil, nil
	}
	id, err := strconv.ParseInt(*s, 10, 64)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func raidDifficultyToString(d *db.RaidDifficulty) *string {
	if d == nil {
		return nil
	}
	s := string(*d)
	return &s
}

type createEventRequest struct {
	Type           string          `json:"type"`
	Title          string          `json:"title"`
	StartsAt       time.Time       `json:"starts_at"`
	SignupDeadline time.Time       `json:"signup_deadline"`
	CompTemplate   json.RawMessage `json:"comp_template"`
	Difficulty     *string         `json:"difficulty,omitempty"`
	// ReminderLeadMinutes omitted means the guild default. 0 means no reminder.
	ReminderLeadMinutes *int32 `json:"reminder_lead_minutes,omitempty"`
	// Announce asks the service to queue the signup sheet for the bot to post. The bot
	// posts its own and leaves this out; a client with no way to write in Discord sets
	// it, and the guild's events channel has to be configured for it to work.
	Announce bool `json:"announce,omitempty"`
}

// maxReminderLeadMinutes is a day. Past that the 24-hour reminder is the one that
// should be moving, and a lead longer than the notice a raid gets fires immediately.
const maxReminderLeadMinutes int32 = 1440

func validReminderLead(minutes *int32) bool {
	return minutes == nil || (*minutes >= 0 && *minutes <= maxReminderLeadMinutes)
}

// createEventHandler creates an event and, in the same store transaction, schedules
// its reminder/deadline jobs (schedule.go's jobsFor). Raid lead only.
func createEventHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		guildID, ok := requireGuildPath(w, r, logger, "gid")
		if !ok {
			return
		}

		var body createEventRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		compTemplate := body.CompTemplate
		if len(compTemplate) == 0 {
			compTemplate = []byte("{}")
		}

		eventType, err := parseEventType(body.Type)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		var difficulty *db.RaidDifficulty
		if body.Difficulty != nil {
			d, err := parseRaidDifficulty(*body.Difficulty)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, err.Error())
				return
			}
			difficulty = &d
		}

		if !validReminderLead(body.ReminderLeadMinutes) {
			writeError(w, logger, http.StatusBadRequest,
				"reminder_lead_minutes must be between 0 and 1440")
			return
		}

		event, err := events.Create(r.Context(), signup.CreateEventInput{
			DiscordGuildID: guildID,
			Type:           eventType,
			Title:          body.Title,
			StartsAt:       body.StartsAt,
			SignupDeadline: body.SignupDeadline,
			CompTemplate:   compTemplate,
			Difficulty:     difficulty,

			ReminderLeadMinutes: body.ReminderLeadMinutes,
			Announce:            body.Announce,
		})
		if err != nil {
			// A guild-state problem the caller can fix, not a server fault: they asked
			// for an announcement in a guild that has not said where events go.
			if errors.Is(err, signup.ErrNoEventsChannel) {
				writeError(w, logger, http.StatusConflict, err.Error())
				return
			}
			logger.ErrorContext(r.Context(), "creating event", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusCreated, eventToResponse(event, actor))
	}
}

// listGuildEventsHandler returns a guild's events. ?scope=past returns the ones that
// have already started, most recent first; anything else defaults to upcoming, which is
// what this endpoint returned before the parameter existed.
func listGuildEventsHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		guildID, ok := requireGuildPath(w, r, logger, "gid")
		if !ok {
			return
		}

		scope := r.URL.Query().Get("scope")
		if scope != "" && scope != "upcoming" && scope != "past" {
			writeError(w, logger, http.StatusBadRequest, "scope must be upcoming or past")
			return
		}

		var (
			list []signup.Event
			err  error
		)
		if scope == "past" {
			list, err = events.ListPast(r.Context(), guildID)
		} else {
			list, err = events.ListUpcoming(r.Context(), guildID)
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "listing events", "error", err, "scope", scope)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		ids := make([]uuid.UUID, len(list))
		for i, e := range list {
			ids[i] = e.ID
		}
		counts, err := events.SignupCounts(r.Context(), ids)
		if err != nil {
			logger.ErrorContext(r.Context(), "counting signups", "error", err, "scope", scope)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]eventResponse, len(list))
		for i, e := range list {
			out[i] = withSignupCounts(eventToResponse(e, actor), counts[e.ID])
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// getEventHandler loads one event, scoped to the actor's guild: a foreign guild's
// event id reads as 404, not 403, so its existence is not confirmed either way.
func getEventHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		event, ok := requireEventInGuild(w, r, events, logger, id)
		if !ok {
			return
		}

		counts, err := events.SignupCounts(r.Context(), []uuid.UUID{event.ID})
		if err != nil {
			logger.ErrorContext(r.Context(), "counting signups", "error", err, "event_id", event.ID)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, withSignupCounts(eventToResponse(event, actor), counts[event.ID]))
	}
}

type updateEventRequest struct {
	Title          *string         `json:"title,omitempty"`
	StartsAt       *time.Time      `json:"starts_at,omitempty"`
	SignupDeadline *time.Time      `json:"signup_deadline,omitempty"`
	CompTemplate   json.RawMessage `json:"comp_template,omitempty"`
	Difficulty     *string         `json:"difficulty,omitempty"`
	MessageID      *string         `json:"message_id,omitempty"`
	ChannelID      *string         `json:"channel_id,omitempty"`

	ReminderLeadMinutes *int32 `json:"reminder_lead_minutes,omitempty"`
	// WarcraftLogsURL omitted leaves the stored report alone. Sent as "" takes it off.
	WarcraftLogsURL *string `json:"warcraftlogs_url,omitempty"`
}

// patchEventHandler applies a partial edit. Raid lead only. Rescheduling on a
// starts_at/signup_deadline/reminder_lead_minutes change happens inside Events.Update's
// store transaction; message_id/channel_id (the bot learning its own post) never
// touches scheduled_jobs.
func patchEventHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if _, ok := requireEventInGuild(w, r, events, logger, id); !ok {
			return
		}

		var body updateEventRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		messageID, err := stringToSnowflakePtr(body.MessageID)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "message_id: "+err.Error())
			return
		}
		channelID, err := stringToSnowflakePtr(body.ChannelID)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "channel_id: "+err.Error())
			return
		}
		var difficulty *db.RaidDifficulty
		if body.Difficulty != nil {
			d, err := parseRaidDifficulty(*body.Difficulty)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, err.Error())
				return
			}
			difficulty = &d
		}
		if !validReminderLead(body.ReminderLeadMinutes) {
			writeError(w, logger, http.StatusBadRequest,
				"reminder_lead_minutes must be between 0 and 1440")
			return
		}
		warcraftLogsURL := body.WarcraftLogsURL
		if warcraftLogsURL != nil {
			normalized, err := signup.NormalizeWarcraftLogsURL(*warcraftLogsURL)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest,
					"warcraftlogs_url must be a warcraftlogs.com report link")
				return
			}
			warcraftLogsURL = &normalized
		}

		event, err := events.Update(r.Context(), signup.UpdateEventInput{
			ID:             id,
			Title:          body.Title,
			StartsAt:       body.StartsAt,
			SignupDeadline: body.SignupDeadline,
			CompTemplate:   body.CompTemplate,
			Difficulty:     difficulty,
			MessageID:      messageID,
			ChannelID:      channelID,

			ReminderLeadMinutes: body.ReminderLeadMinutes,
			WarcraftLogsURL:     warcraftLogsURL,
		})
		if err != nil {
			logger.ErrorContext(r.Context(), "updating event", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, eventToResponse(event, actor))
	}
}

// deleteEventHandler removes an event. Raid lead only. Signups, scheduled_jobs,
// comps, and notifications all carry ON DELETE CASCADE.
func deleteEventHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if _, ok := requireEventInGuild(w, r, events, logger, id); !ok {
			return
		}

		if err := events.Delete(r.Context(), id); err != nil {
			logger.ErrorContext(r.Context(), "deleting event", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

type putEventMessageRequest struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
}

// putEventMessageHandler records the post the bot made for an event it was told to
// announce. It takes the shared key alone and no actor headers, the same reasoning as
// the notifications and catalog routes: this is the bot reporting its own action, not
// something done on a raider's behalf.
//
// The bot cannot use PATCH /api/events/{id} for this. A poller has no interaction to
// read a member from, so it speaks as itself with no roles, and PATCH is raid lead
// only. Without this route an announced event would be posted and then immediately
// forgotten, leaving nothing for a redraw or a reminder to find.
func putEventMessageHandler(events *signup.Events, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		var body putEventMessageRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		channelID, err := strconv.ParseInt(body.ChannelID, 10, 64)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "channel_id: "+err.Error())
			return
		}
		messageID, err := strconv.ParseInt(body.MessageID, 10, 64)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "message_id: "+err.Error())
			return
		}

		if _, err := events.Get(r.Context(), id); err != nil {
			writeError(w, logger, http.StatusNotFound, "event not found")
			return
		}

		if _, err := events.Update(r.Context(), signup.UpdateEventInput{
			ID:        id,
			MessageID: &messageID,
			ChannelID: &channelID,
		}); err != nil {
			logger.ErrorContext(r.Context(), "recording event message", "error", err, "event_id", id)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
