package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

type signupResponse struct {
	ID          string `json:"id"`
	CharacterID string `json:"character_id"`
	// Character is populated on list responses, where a client is rendering many rows
	// at once and would otherwise fetch the roster itself to turn ids into names. The
	// single-signup writes leave it nil: the caller just named the character.
	Character    *characterSummary `json:"character,omitempty"`
	Status       string            `json:"status"`
	AssignedRole *string           `json:"assigned_role,omitempty"`
	LateUntil    *time.Time        `json:"late_until,omitempty"`
	Note         *string           `json:"note,omitempty"`
	// AllowedStatuses is what this caller may PUT, so a client renders the buttons it
	// has rather than discovering a 403. Empty for a caller who cannot act at all,
	// the same way they get no links.
	AllowedStatuses []string `json:"allowed_statuses,omitempty"`
	Links           Links    `json:"_links"`
}

// signupLinks is the HATEOAS decision for one signup: self and withdraw are visible
// to the character's owner or a raid lead, and to nobody else. The absence of a
// link is the authorization answer for anyone just looking at the character's own
// event, not acting on someone else's.
// signupLinks offers the two writes separately, because they are no longer the same
// permission: a raid lead may write NO_SHOW on somebody else's signup, and may not take
// that person's name off the sheet.
func signupLinks(eventID, characterID string, mayWrite, mayWithdraw bool) Links {
	href := "/api/events/" + eventID + "/signups/" + characterID
	links := Links{}
	links.add(mayWrite, "self", href, "PUT")
	links.add(mayWithdraw, "withdraw", href, "DELETE")
	return links
}

// started is whether the raid has begun. Past that the sheet is history: nobody may take
// their name off it, and the only status left is a raid lead's NO_SHOW.
func signupToResponse(s signup.Signup, owned, isRaidLead, started bool) signupResponse {
	var assignedRole *string
	if s.AssignedRole != nil {
		r := string(*s.AssignedRole)
		assignedRole = &r
	}

	var allowed []string
	for _, status := range signup.AllowedStatuses(owned, isRaidLead, started) {
		allowed = append(allowed, string(status))
	}

	return signupResponse{
		ID:              s.ID.String(),
		CharacterID:     s.CharacterID.String(),
		Status:          string(s.Status),
		AssignedRole:    assignedRole,
		LateUntil:       s.LateUntil,
		Note:            s.Note,
		AllowedStatuses: allowed,
		Links:           signupLinks(s.EventID.String(), s.CharacterID.String(), len(allowed) > 0, owned && !started),
	}
}

type putSignupRequest struct {
	Status    string     `json:"status"`
	Note      *string    `json:"note,omitempty"`
	LateUntil *time.Time `json:"late_until,omitempty"`
}

// putSignupHandler writes a signup, self or raid lead. A player who has missed
// signup_deadline never sees a bare error here: Signups.Write returns
// ErrSignupsClosed, and this handler files a late_signup_requests row with the same
// status instead, so the response the bot renders is a request the raid lead can
// act on rather than a dead end.
func putSignupHandler(signups *signup.Signups, lateRequests *signup.LateRequests, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		characterID, err := pathUUID(r, "cid")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		// Before the ownership check, not inside it: the raid-lead branch below skips
		// ownership, so scoping here is what stops one guild's lead writing another's.
		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}
		started := !time.Now().Before(event.StartsAt)

		// Two different questions. A raid lead may act on anyone's character, but only
		// one of their own guild's: without this, a foreign character id would be
		// written onto their event.
		inGuild, err := characters.InGuild(r.Context(), characterID, int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "checking character guild", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if !inGuild {
			writeError(w, logger, http.StatusNotFound, "character not found")
			return
		}

		// Asked on every path now, not only a raider's: a raid lead's own signup and a
		// raid lead's write onto somebody else's are different permissions, and the
		// difference is exactly this answer.
		owned, err := characters.OwnedByDiscord(r.Context(), characterID, int64(actor.GuildID), int64(actor.DiscordID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "checking character ownership", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if !owned && !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "not your character")
			return
		}

		var body putSignupRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		status, err := parseSignupStatus(body.Status)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		written, err := signups.Write(r.Context(), signup.SignupWrite{
			EventID: eventID, CharacterID: characterID, Status: status, Note: body.Note, LateUntil: body.LateUntil,
		}, owned, actor.IsRaidLead)

		switch {
		case errors.Is(err, signup.ErrEventStarted):
			// Deliberately not a late request. Signups closing is what that queue is
			// for; a raid that already happened is past anything a raid lead could
			// approve.
			writeError(w, logger, http.StatusConflict, "That raid has already started, so the sheet is final.")
		case errors.Is(err, signup.ErrSignupsClosed):
			fileLateRequest(w, r, lateRequests, logger, eventID, characterID, status, body.Note, body.LateUntil, actor.IsRaidLead)
		case errors.Is(err, signup.ErrStatusRequiresRaidLead):
			writeError(w, logger, http.StatusForbidden, "status requires raid lead")
		case errors.Is(err, signup.ErrSignupNotYours):
			writeError(w, logger, http.StatusForbidden, "only the raider may set that status")
		case err != nil:
			logger.ErrorContext(r.Context(), "writing signup", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			writeJSON(w, logger, http.StatusOK, signupToResponse(written, owned, actor.IsRaidLead, started))
		}
	}
}

// deleteSignupHandler withdraws a signup. The owner alone, raid lead included: taking
// somebody's name off the sheet is rewriting their answer, and only they may do that.
//
// Past the deadline this closes for players the same way a new signup would: the
// withdrawal becomes a late request carrying DECLINED, not a silent delete.
func deleteSignupHandler(signups *signup.Signups, lateRequests *signup.LateRequests, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		characterID, err := pathUUID(r, "cid")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
			return
		}

		// Two different questions. A raid lead may act on anyone's character, but only
		// one of their own guild's: without this, a foreign character id would be
		// written onto their event.
		inGuild, err := characters.InGuild(r.Context(), characterID, int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "checking character guild", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if !inGuild {
			writeError(w, logger, http.StatusNotFound, "character not found")
			return
		}

		owned, err := characters.OwnedByDiscord(r.Context(), characterID, int64(actor.GuildID), int64(actor.DiscordID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "checking character ownership", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if !owned {
			writeError(w, logger, http.StatusForbidden, "not your character")
			return
		}

		err = signups.Withdraw(r.Context(), eventID, characterID, actor.IsRaidLead)
		switch {
		case errors.Is(err, signup.ErrEventStarted):
			writeError(w, logger, http.StatusConflict, "That raid has already started, so the sheet is final.")
		case errors.Is(err, signup.ErrSignupsClosed):
			fileLateRequest(w, r, lateRequests, logger, eventID, characterID, db.SignupStatusDECLINED, nil, nil, actor.IsRaidLead)
		case err != nil:
			logger.ErrorContext(r.Context(), "withdrawing signup", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// fileLateRequest renders the request with the caller's real capabilities. A player
// who just tripped the deadline is not a raid lead, so they must not be handed
// approve/reject links they would get a 403 from: the absence of a link is the
// authorization answer (hard rule 7).
func fileLateRequest(w http.ResponseWriter, r *http.Request, lateRequests *signup.LateRequests, logger *slog.Logger, eventID, characterID uuid.UUID, status db.SignupStatus, note *string, lateUntil *time.Time, isRaidLead bool) {
	req, err := lateRequests.File(r.Context(), signup.LateRequestWrite{
		EventID: eventID, CharacterID: characterID, Status: status, Note: note, LateUntil: lateUntil,
	})
	if errors.Is(err, signup.ErrEventStarted) {
		// The raid began between the signup write being refused and this filing. There is
		// no longer anything a raid lead could approve.
		writeError(w, logger, http.StatusConflict, "That raid has already started, so the sheet is final.")
		return
	}
	if err != nil {
		logger.ErrorContext(r.Context(), "filing late request", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, logger, http.StatusAccepted, lateRequestToResponse(req, isRaidLead))
}

// listSignupsHandler returns every signup for an event. self/withdraw links appear
// on a raid lead's own view of every row, and on a player's view of only the rows
// for characters they own: the same authorization the write endpoints enforce,
// reflected back as what the caller could actually do next.
func listSignupsHandler(signups *signup.Signups, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}
		started := !time.Now().Before(event.StartsAt)

		list, err := signups.List(r.Context(), eventID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing signups", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		// Asked for a raid lead too. This used to be skipped for them, which was fine
		// while they could act on every signup regardless; now their own row is the one
		// place they get the self-reported statuses, so ownership has to be known.
		owned := map[uuid.UUID]bool{}
		mine, err := characters.ListForUser(r.Context(), int64(actor.DiscordID), int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "listing actor characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		for _, c := range mine {
			owned[c.ID] = true
		}

		byID, err := guildRoster(r.Context(), characters, int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "listing guild characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]signupResponse, len(list))
		for i, s := range list {
			out[i] = signupToResponse(s, owned[s.CharacterID], actor.IsRaidLead, started)
			out[i].Character = lookupCharacter(byID, s.CharacterID)
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}
