package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

type lateRequestResponse struct {
	ID          string     `json:"id"`
	CharacterID string     `json:"character_id"`
	Status      string     `json:"status"`
	Note        *string    `json:"note,omitempty"`
	LateUntil   *time.Time `json:"late_until,omitempty"`
	State       string     `json:"state"`
	Links       Links      `json:"_links"`
}

func lateRequestToResponse(req signup.LateRequest, isRaidLead bool) lateRequestResponse {
	href := "/api/events/" + req.EventID.String() + "/late-requests/" + req.ID.String()
	pending := req.State == "PENDING"

	links := Links{}
	links.add(true, "self", href, "")
	links.add(isRaidLead && pending, "approve", href+"/approve", "POST")
	links.add(isRaidLead && pending, "reject", href+"/reject", "POST")

	return lateRequestResponse{
		ID:          req.ID.String(),
		CharacterID: req.CharacterID.String(),
		Status:      string(req.Status),
		Note:        req.Note,
		LateUntil:   req.LateUntil,
		State:       string(req.State),
		Links:       links,
	}
}

// listLateRequestsHandler returns every late request for an event, most recent
// first. Raid lead only: this is their queue to work through.
func listLateRequestsHandler(lateRequests *signup.LateRequests, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
			return
		}

		list, err := lateRequests.List(r.Context(), eventID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing late requests", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]lateRequestResponse, len(list))
		for i, req := range list {
			out[i] = lateRequestToResponse(req, true)
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// requireLateRequestInGuild resolves the {rid} request and confirms it belongs to the
// {id} event and that the event belongs to the actor's guild. Both halves matter: the
// handlers act on the request id alone, so without the first check a raid lead could
// decide a request filed against a different event, and without the second, one filed
// in a different guild.
func requireLateRequestInGuild(w http.ResponseWriter, r *http.Request, lateRequests *signup.LateRequests, events eventLookup, logger *slog.Logger) (uuid.UUID, bool) {
	eventID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, err.Error())
		return uuid.Nil, false
	}
	requestID, err := pathUUID(r, "rid")
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, err.Error())
		return uuid.Nil, false
	}

	if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
		return uuid.Nil, false
	}

	req, err := lateRequests.Get(r.Context(), requestID)
	if err != nil {
		writeError(w, logger, http.StatusNotFound, "late request not found")
		return uuid.Nil, false
	}
	if req.EventID != eventID {
		writeError(w, logger, http.StatusNotFound, "late request not found")
		return uuid.Nil, false
	}

	return requestID, true
}

// approveLateRequestHandler upserts the signup with the requested status and marks
// the request decided. Raid lead only, and only while the request is still pending:
// the approve link disappears once decided, so a call arriving anyway is a stale
// button or a second raid lead racing the first, and 409 says so.
func approveLateRequestHandler(lateRequests *signup.LateRequests, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		id, ok := requireLateRequestInGuild(w, r, lateRequests, events, logger)
		if !ok {
			return
		}

		err := lateRequests.Approve(r.Context(), id)
		switch {
		case errors.Is(err, signup.ErrEventStarted):
			writeError(w, logger, http.StatusConflict, "That raid has already started, so the sheet is final.")
		case errors.Is(err, signup.ErrRequestDecided):
			writeError(w, logger, http.StatusConflict, "late request already decided")
		case err != nil:
			logger.ErrorContext(r.Context(), "approving late request", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// rejectLateRequestHandler marks the request decided without touching the signup.
// Raid lead only, pending only, for the same reason as approve.
func rejectLateRequestHandler(lateRequests *signup.LateRequests, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "raid lead required")
			return
		}

		id, ok := requireLateRequestInGuild(w, r, lateRequests, events, logger)
		if !ok {
			return
		}

		err := lateRequests.Reject(r.Context(), id)
		switch {
		case errors.Is(err, signup.ErrRequestDecided):
			writeError(w, logger, http.StatusConflict, "late request already decided")
		case err != nil:
			logger.ErrorContext(r.Context(), "rejecting late request", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
