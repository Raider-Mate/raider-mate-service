package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/comp"
	"github.com/Raider-Mate/raider-mate-service/internal/db"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
)

type compInfoResponse struct {
	Name  string `json:"name"`
	Mode  string `json:"mode"`
	Links Links  `json:"_links"`
}

type assignmentResponse struct {
	CharacterID string            `json:"character_id"`
	Character   *characterSummary `json:"character,omitempty"`
	Role        string            `json:"role"`
	SlotIndex   int16             `json:"slot_index"`
	IsBench     bool              `json:"is_bench"`
	Reason      string            `json:"reason"`
}

// unseatedResponse is one raider on the event who holds no slot on this board. Reason
// is the service's own sentence, meant to be rendered as it arrives, the same way
// assignmentResponse.Reason is.
type unseatedResponse struct {
	CharacterID string            `json:"character_id"`
	Character   *characterSummary `json:"character,omitempty"`
	Status      string            `json:"status"`
	SignedUpAt  time.Time         `json:"signed_up_at"`
	Reason      string            `json:"reason"`
}

type advisoryResponse struct {
	Role    string `json:"role,omitempty"`
	Message string `json:"message"`
}

type boardResponse struct {
	Name  string               `json:"name"`
	Mode  string               `json:"mode"`
	Slots []assignmentResponse `json:"slots"`
	// Unseated is who signed up and did not make the board. Absent when everybody who
	// could be placed was, which is what a board looks like the moment it is locked.
	Unseated   []unseatedResponse `json:"unseated,omitempty"`
	Advisories []advisoryResponse `json:"advisories,omitempty"`
	Links      Links              `json:"_links"`
}

func compHref(eventID, name string) string {
	return "/api/events/" + eventID + "/comps/" + name
}

// compLinks is the HATEOAS decision for one comp. lock is absent on a manual comp
// and save is absent on an auto one, because each would be refused: the two modes own
// their slots exclusively, and the absent link is the answer.
//
// rename is offered in both modes. A name is a label a raid lead chose, not a claim on
// who owns the board, so nothing about the mode makes it more or less renameable.
func compLinks(eventID, name string, mode db.CompMode, isRaidLead bool) Links {
	href := compHref(eventID, name)
	links := Links{}
	links.add(true, "self", href, "")
	links.add(isRaidLead && mode != db.CompModeMANUAL, "lock", href+"/lock", "POST")
	links.add(isRaidLead && mode == db.CompModeMANUAL, "save", href, "PUT")
	links.add(isRaidLead, "mode", href+"/mode", "PUT")
	links.add(isRaidLead, "rename", href, "PATCH")
	return links
}

// assignmentsToResponse renders a board. byID may be nil, in which case the slots
// carry bare ids: a board is unreadable without names, so every caller passes one.
func assignmentsToResponse(assignments []comp.Assignment, byID map[uuid.UUID]characterSummary) []assignmentResponse {
	out := make([]assignmentResponse, len(assignments))
	for i, a := range assignments {
		out[i] = assignmentResponse{
			CharacterID: a.CharacterID.String(), Character: lookupCharacter(byID, a.CharacterID),
			Role:      string(a.Role),
			SlotIndex: a.SlotIndex, IsBench: a.IsBench, Reason: a.Reason,
		}
	}
	return out
}

func unseatedToResponse(unseated []comp.Unseated, byID map[uuid.UUID]characterSummary) []unseatedResponse {
	if len(unseated) == 0 {
		return nil
	}
	out := make([]unseatedResponse, len(unseated))
	for i, u := range unseated {
		out[i] = unseatedResponse{
			CharacterID: u.CharacterID.String(), Character: lookupCharacter(byID, u.CharacterID),
			Status: string(u.Status), SignedUpAt: u.SignedUpAt, Reason: u.Reason,
		}
	}
	return out
}

func advisoriesToResponse(advisories []comp.Advisory) []advisoryResponse {
	if len(advisories) == 0 {
		return nil
	}
	out := make([]advisoryResponse, len(advisories))
	for i, a := range advisories {
		out[i] = advisoryResponse{Role: string(a.Role), Message: a.Message}
	}
	return out
}

// listCompsHandler returns every named comp for an event.
func listCompsHandler(reader *comp.Reader, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
			return
		}

		list, err := reader.List(r.Context(), eventID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing comps", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]compInfoResponse, len(list))
		for i, info := range list {
			out[i] = compInfoResponse{
				Name: info.Name, Mode: string(info.Mode),
				Links: compLinks(eventID.String(), info.Name, info.Mode, actor.IsRaidLead),
			}
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// getCompHandler returns one named comp's mode and slots.
func getCompHandler(reader *comp.Reader, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		name := r.PathValue("name")

		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}

		board, found, err := reader.Get(r.Context(), eventID, name)
		if err != nil {
			logger.ErrorContext(r.Context(), "loading comp", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		if !found {
			writeError(w, logger, http.StatusNotFound, "comp not found")
			return
		}

		byID, err := guildRoster(r.Context(), characters, event.DiscordGuildID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing guild characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, boardResponse{
			Name: board.Name, Mode: string(board.Mode), Slots: assignmentsToResponse(board.Slots, byID),
			Unseated:   unseatedToResponse(board.Unseated, byID),
			Advisories: advisoriesToResponse(board.Advisories),
			Links:      compLinks(eventID.String(), name, board.Mode, actor.IsRaidLead),
		})
	}
}

// lockCompHandler runs the assigner and persists the result. Raid lead only. A
// manual comp refuses the lock (ErrCompIsManual) rather than overwriting a
// hand-built board.
func lockCompHandler(locker *comp.Locker, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
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
		name := r.PathValue("name")

		// Being a raid lead somewhere is not being a raid lead here.
		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}

		result, err := locker.Lock(r.Context(), eventID, name)
		if errors.Is(err, comp.ErrCompIsManual) {
			writeError(w, logger, http.StatusConflict, "comp is manual; convert it before locking")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "locking comp", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		byID, err := guildRoster(r.Context(), characters, event.DiscordGuildID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing guild characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, boardResponse{
			Name: name, Mode: string(db.CompModeAUTO), Slots: assignmentsToResponse(result.Assignments, byID),
			Advisories: advisoriesToResponse(result.Advisories),
			Links:      compLinks(eventID.String(), name, db.CompModeAUTO, actor.IsRaidLead),
		})
	}
}

type savedSlotRequest struct {
	CharacterID string `json:"character_id"`
	Role        string `json:"role"`
	IsBench     bool   `json:"is_bench"`
}

type saveCompRequest struct {
	Slots []savedSlotRequest `json:"slots"`
}

// saveCompHandler writes a hand-built board. Raid lead only. The whole board arrives
// at once because that is what the domain persists: slot_index falls out of the
// submitted order and there is no partial state to reconcile between two raid leads.
//
// Nothing here validates the board. A healer placed as a tank is written as asked;
// design.md is explicit that an assigner overriding raid-lead judgement gets switched
// off, and the same goes for the API in front of it.
func saveCompHandler(manual *comp.Manual, characters *roster.Characters, events eventLookup, logger *slog.Logger) http.HandlerFunc {
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
		name := r.PathValue("name")

		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}

		var body saveCompRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		placements := make([]comp.Placement, len(body.Slots))
		for i, s := range body.Slots {
			characterID, err := uuid.Parse(s.CharacterID)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "slots["+strconv.Itoa(i)+"].character_id: "+err.Error())
				return
			}
			role, err := parseRole(s.Role)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "slots["+strconv.Itoa(i)+"].role: "+err.Error())
				return
			}
			placements[i] = comp.Placement{CharacterID: characterID, Role: role, IsBench: s.IsBench}
		}

		err = manual.Save(r.Context(), eventID, name, placements)
		switch {
		case errors.Is(err, comp.ErrCompIsAuto):
			writeError(w, logger, http.StatusConflict, "comp is auto; convert it before saving")
		case errors.Is(err, comp.ErrInvalidBoard):
			writeError(w, logger, http.StatusBadRequest, err.Error())
		case err != nil:
			logger.ErrorContext(r.Context(), "saving comp", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			byID, err := guildRoster(r.Context(), characters, event.DiscordGuildID)
			if err != nil {
				logger.ErrorContext(r.Context(), "listing guild characters", "error", err)
				writeError(w, logger, http.StatusInternalServerError, "internal error")
				return
			}
			writeJSON(w, logger, http.StatusOK, boardResponse{
				Name: name, Mode: string(db.CompModeMANUAL), Slots: placementsToResponse(placements, byID),
				Links: compLinks(eventID.String(), name, db.CompModeMANUAL, true),
			})
		}
	}
}

type setCompModeRequest struct {
	Mode string `json:"mode"`
}

// setCompModeHandler converts a comp between raid-lead-owned and assigner-owned. The
// slots are left alone, so converting an auto comp hands the raid lead the assigner's
// last output as a starting point.
func setCompModeHandler(manual *comp.Manual, events eventLookup, logger *slog.Logger) http.HandlerFunc {
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
		name := r.PathValue("name")

		if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
			return
		}

		var body setCompModeRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		mode, err := parseCompMode(body.Mode)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "mode: "+err.Error())
			return
		}

		if err := manual.SetMode(r.Context(), eventID, name, mode); err != nil {
			logger.ErrorContext(r.Context(), "setting comp mode", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, compInfoResponse{
			Name: name, Mode: string(mode),
			Links: compLinks(eventID.String(), name, mode, true),
		})
	}
}

type renameCompRequest struct {
	Name string `json:"name"`
}

// renameCompHandler moves a comp to a new name. Raid lead only.
//
// The slots come with it. A rename is a label change, and rebuilding the board to
// perform one would throw away exactly the work the name is being changed to describe.
func renameCompHandler(manual *comp.Manual, events eventLookup, logger *slog.Logger) http.HandlerFunc {
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
		name := r.PathValue("name")

		if _, ok := requireEventInGuild(w, r, events, logger, eventID); !ok {
			return
		}

		var body renameCompRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		mode, err := manual.Rename(r.Context(), eventID, name, body.Name)
		switch {
		case errors.Is(err, comp.ErrInvalidName):
			writeError(w, logger, http.StatusBadRequest, "name: must not be empty")
		case errors.Is(err, comp.ErrCompNotFound):
			writeError(w, logger, http.StatusNotFound, "comp not found")
		case errors.Is(err, comp.ErrCompNameTaken):
			writeError(w, logger, http.StatusConflict, "this event already has a comp with that name")
		case err != nil:
			logger.ErrorContext(r.Context(), "renaming comp", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
		default:
			// The name it now answers to, which is not always the one that was sent:
			// Rename trims, and a caller that posted padding needs the trimmed value to
			// build its next request against.
			renamed := strings.TrimSpace(body.Name)
			writeJSON(w, logger, http.StatusOK, compInfoResponse{
				Name: renamed, Mode: string(mode),
				Links: compLinks(eventID.String(), renamed, mode, true),
			})
		}
	}
}

func placementsToResponse(placements []comp.Placement, byID map[uuid.UUID]characterSummary) []assignmentResponse {
	out := make([]assignmentResponse, len(placements))
	var seated, benched int16
	for i, p := range placements {
		index := seated
		if p.IsBench {
			index = benched
			benched++
		} else {
			seated++
		}
		out[i] = assignmentResponse{
			CharacterID: p.CharacterID.String(), Character: lookupCharacter(byID, p.CharacterID),
			Role:      string(p.Role),
			SlotIndex: index, IsBench: p.IsBench, Reason: comp.ManualReason,
		}
	}
	return out
}
