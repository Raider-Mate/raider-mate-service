package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/raidlog"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
)

// eventReportResponse is one raid night as the log recorded it.
//
// Status is what a client branches on once it has followed a link and needs the reason.
// The link rel on the event is what it branches on before it gets here, which is why
// exactly one of report, report-pending and report-failed is ever present.
type eventReportResponse struct {
	// Status is one of PENDING, READY, PRIVATE, NOT_FOUND, ARCHIVED, UNAVAILABLE.
	Status string `json:"status"`
	// Live is true while WarcraftLogs still has a pull in progress. The numbers below
	// are real but not final, and a client that does not say so will be asked why the
	// damage table changed.
	Live bool `json:"live"`
	// URL is the report on warcraftlogs.com, for the raider who wants the real thing.
	URL       string     `json:"url"`
	Title     *string    `json:"title,omitempty"`
	Zone      *string    `json:"zone,omitempty"`
	StartsAt  *time.Time `json:"starts_at,omitempty"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	// Fights is every boss pull, in the order they happened. Trash is not in here: a
	// raid night is measured in pulls, and the run to the instance is not one.
	Fights []reportFightResponse `json:"fights"`
	// Raiders is everyone the log saw, roster or not, with their totals for the night.
	Raiders []reportRaiderResponse `json:"raiders"`
	// Turnout is the log checked against the signup sheet.
	Turnout reportTurnoutResponse `json:"turnout"`
	Links   Links                 `json:"_links"`
}

// reportFightResponse is one pull.
type reportFightResponse struct {
	// FightID is WarcraftLogs' own id inside the report, which is what a deep link into
	// the report needs: #fight=12.
	FightID   int32  `json:"fight_id"`
	Encounter string `json:"encounter"`
	// Difficulty is NORMAL, HEROIC or MYTHIC where the log used one this service
	// recognises. Absent otherwise, and DifficultyID still carries the raw number, so a
	// client can say "difficulty 12" rather than pretend the pull did not happen.
	Difficulty   *string `json:"difficulty,omitempty"`
	DifficultyID *int32  `json:"difficulty_id,omitempty"`
	RaidSize     *int32  `json:"raid_size,omitempty"`
	Kill         bool    `json:"kill"`
	// BossPercentage is how much health was left, on a 0 to 100 scale and about zero on
	// a kill. This is the number a guild actually argues about after a wipe night.
	BossPercentage *float64  `json:"boss_percentage,omitempty"`
	StartsAt       time.Time `json:"starts_at"`
	EndsAt         time.Time `json:"ends_at"`
	// DurationSeconds is worked out here rather than left to the client, for the same
	// reason the analysis rates are: two clients subtracting two timestamps is two
	// chances to get the rounding different.
	DurationSeconds int32 `json:"duration_seconds"`
	// Raiders is this pull's own numbers, so a client selecting a pull reads them without
	// asking again. Absent on a pull WarcraftLogs was not read individually for, which is
	// the tail of a very long night.
	Raiders []reportRaiderResponse `json:"raiders,omitempty"`
}

// reportRaiderResponse is one player's night. Damage and healing are totals across every
// boss pull; per-fight numbers are not in this slice.
type reportRaiderResponse struct {
	// CharacterID is set when the log's actor matched a character on this guild's
	// roster. Absent means nobody on the roster answers to that name, which is a pug, an
	// unregistered trial, or the wrong report pasted on the event.
	CharacterID *string `json:"character_id,omitempty"`
	// Name and Realm are as the log recorded them, not as the roster spells them. A
	// raider who transferred mid-tier is why those are two different things.
	Name    string  `json:"name"`
	Realm   string  `json:"realm"`
	Class   *string `json:"class,omitempty"`
	Damage  int64   `json:"damage"`
	Healing int64   `json:"healing"`
	Deaths  int32   `json:"deaths"`
}

// reportTurnoutResponse is the log checked against the signup sheet: who said yes and
// zoned in, who zoned in without being on the roster, and who said yes and never
// appeared.
//
// A report pasted onto the wrong event shows up here as almost everybody in both
// mismatch lists, which is the cheapest wrong-log detector there is.
type reportTurnoutResponse struct {
	Attended []characterRefResponse `json:"attended"`
	Unknown  []reportUnknownActor   `json:"unknown"`
	// Missing is characters confirmed or approved late on the signup sheet that the log
	// never saw. Tentative signups are not in here: "maybe" was never a promise.
	Missing []characterRefResponse `json:"missing"`
	// RosterOverlap is matched actors over total actors, 0 to 1. A number near zero on a
	// full raid means the log is not this guild's night.
	RosterOverlap float64 `json:"roster_overlap"`
}

type reportUnknownActor struct {
	Name  string  `json:"name"`
	Realm string  `json:"realm"`
	Class *string `json:"class,omitempty"`
}

// reportReader is the raidlog dependency the API needs. Declared here, by the consumer.
type reportReader interface {
	Report(ctx context.Context, eventID uuid.UUID, url string) (raidlog.Stored, error)
	StatusFor(ctx context.Context, eventID uuid.UUID) (string, error)
	RequestRefresh(ctx context.Context, eventID uuid.UUID) error
}

// characterNamer resolves the character ids the turnout lists carry into names a person
// can read. A raid lead cannot chase a UUID.
type characterNamer interface {
	ListForGuild(ctx context.Context, discordGuildID int64, includeArchived bool) ([]roster.Character, error)
}

// reportStatusRels adds exactly one of the three status rels, plus the refresh control.
//
// The rel name carries the state, so a dashboard renders a spinner, an apology or the
// numbers without fetching the resource first. That is what makes the link set
// sufficient on its own.
//
// An empty status means the instance never wrote a row, which is what an instance with
// no WarcraftLogs credentials looks like. No report rels appear, and the event page
// keeps the plain external link it has always had.
func reportStatusRels(links Links, href, status string, isRaidLead bool) {
	if status == "" {
		return
	}
	links.add(status == "PENDING", "report-pending", href+"/report", "")
	links.add(status == "READY", "report", href+"/report", "")
	links.add(isReportFailure(status), "report-failed", href+"/report", "")
	links.add(isRaidLead, "refresh-report", href+"/report/refresh", "POST")
}

func isReportFailure(status string) bool {
	switch status {
	case "PRIVATE", "NOT_FOUND", "ARCHIVED", "UNAVAILABLE":
		return true
	default:
		return false
	}
}

// withReportStatus attaches the report rels to an event response. Separate from
// eventToResponse for the same reason withSignupCounts is: only the read paths have a
// status, and the write paths would have to run a query to answer a question nobody
// asked.
func withReportStatus(resp eventResponse, status string, isRaidLead bool) eventResponse {
	// Only ever beside a report that exists. A rel pointing at a report nobody attached
	// would be a control that leads nowhere.
	if resp.WarcraftLogsURL == nil {
		return resp
	}
	reportStatusRels(resp.Links, "/api/events/"+resp.ID, status, isRaidLead)
	return resp
}

// difficultyName maps WarcraftLogs' own integer onto the vocabulary this service uses.
// Anything else is left unnamed rather than guessed at: an unrecognised difficulty is
// still a real pull, and the raw number ships beside this for exactly that case.
func difficultyName(id *int32) *string {
	if id == nil {
		return nil
	}
	var name string
	switch *id {
	case 3:
		name = "NORMAL"
	case 4:
		name = "HEROIC"
	case 5:
		name = "MYTHIC"
	default:
		return nil
	}
	return &name
}

func getEventReportHandler(reports reportReader, characters characterNamer, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}
		if event.WarcraftLogsURL == nil {
			writeError(w, logger, http.StatusNotFound, "no report attached")
			return
		}

		stored, err := reports.Report(r.Context(), eventID, *event.WarcraftLogsURL)
		if errors.Is(err, raidlog.ErrNoReport) {
			writeError(w, logger, http.StatusNotFound, "no report attached")
			return
		}
		if err != nil {
			writeError(w, logger, http.StatusInternalServerError, "could not read the report")
			return
		}

		actor, _ := actorFromContext(r.Context())
		names, err := characters.ListForGuild(r.Context(), int64(actor.GuildID), true) //nolint:gosec
		if err != nil {
			writeError(w, logger, http.StatusInternalServerError, "could not read the roster")
			return
		}

		writeJSON(w, logger, http.StatusOK, reportToResponse(stored, eventID, names, actor))
	}
}

func refreshEventReportHandler(reports reportReader, events eventLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eventID, err := pathUUID(r, "id")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		event, ok := requireEventInGuild(w, r, events, logger, eventID)
		if !ok {
			return
		}

		actor, _ := actorFromContext(r.Context())
		if !actor.IsRaidLead {
			writeError(w, logger, http.StatusForbidden, "only a raid lead can refresh a report")
			return
		}
		if event.WarcraftLogsURL == nil {
			writeError(w, logger, http.StatusConflict, "no report attached")
			return
		}

		if err := reports.RequestRefresh(r.Context(), eventID); err != nil {
			writeError(w, logger, http.StatusInternalServerError, "could not schedule a refresh")
			return
		}

		// 202 rather than 200: nothing has been fetched yet, and the worker decides when.
		w.WriteHeader(http.StatusAccepted)
	}
}

func reportToResponse(stored raidlog.Stored, eventID uuid.UUID, names []roster.Character, actor Actor) eventReportResponse {
	href := "/api/events/" + eventID.String()

	links := Links{}
	links.add(true, "self", href+"/report", "")
	links.add(true, "event", href, "")
	links.add(stored.URL != "", "warcraftlogs", stored.URL, "")
	links.add(actor.IsRaidLead, "refresh", href+"/report/refresh", "POST")

	resp := eventReportResponse{
		Status:    stored.Status,
		Live:      stored.Live,
		URL:       stored.URL,
		Title:     stored.Title,
		Zone:      stored.Zone,
		StartsAt:  stored.StartsAt,
		EndsAt:    stored.EndsAt,
		FetchedAt: stored.FetchedAt,
		Fights:    make([]reportFightResponse, 0, len(stored.Fights)),
		Raiders:   make([]reportRaiderResponse, 0, len(stored.Raiders)),
		Links:     links,
	}

	for _, fight := range stored.Fights {
		resp.Fights = append(resp.Fights, reportFightResponse{
			FightID:         fight.FightID,
			Encounter:       fight.Encounter,
			Difficulty:      difficultyName(fight.Difficulty),
			DifficultyID:    fight.Difficulty,
			RaidSize:        fight.RaidSize,
			Kill:            fight.Kill,
			BossPercentage:  fight.BossPercent,
			StartsAt:        fight.StartsAt,
			EndsAt:          fight.EndsAt,
			DurationSeconds: int32(fight.Duration().Seconds()),
			Raiders:         raidersToResponse(stored.PerFight[fight.FightID]),
		})
	}

	byID := map[uuid.UUID]roster.Character{}
	for _, character := range names {
		byID[character.ID] = character
	}

	for _, raider := range stored.Raiders {
		entry := raiderToResponse(raider)
		if raider.CharacterID == nil {
			resp.Turnout.Unknown = append(resp.Turnout.Unknown, reportUnknownActor{
				Name:  raider.Name,
				Realm: raider.Realm,
				Class: raider.Class,
			})
		}
		resp.Raiders = append(resp.Raiders, entry)
	}

	resp.Turnout.Attended = refs(stored.Turnout.Attended, byID)
	resp.Turnout.Missing = refs(stored.Turnout.Missing, byID)
	resp.Turnout.RosterOverlap = stored.Turnout.Overlap
	if resp.Turnout.Unknown == nil {
		resp.Turnout.Unknown = []reportUnknownActor{}
	}

	return resp
}

func raiderToResponse(raider raidlog.StoredRaider) reportRaiderResponse {
	entry := reportRaiderResponse{
		Name:    raider.Name,
		Realm:   raider.Realm,
		Class:   raider.Class,
		Damage:  raider.Damage,
		Healing: raider.Healing,
		Deaths:  raider.Deaths,
	}
	if raider.CharacterID != nil {
		id := raider.CharacterID.String()
		entry.CharacterID = &id
	}
	return entry
}

func raidersToResponse(raiders []raidlog.StoredRaider) []reportRaiderResponse {
	if len(raiders) == 0 {
		return nil
	}
	out := make([]reportRaiderResponse, 0, len(raiders))
	for _, raider := range raiders {
		out = append(out, raiderToResponse(raider))
	}
	return out
}

// refs turns character ids into the same shape the attendance panel already returns, so
// a client has one raider chip component and not two.
func refs(ids []uuid.UUID, byID map[uuid.UUID]roster.Character) []characterRefResponse {
	out := make([]characterRefResponse, 0, len(ids))
	for _, id := range ids {
		character, ok := byID[id]
		if !ok {
			// Removed between the fetch and now. The id is still the truthful answer.
			out = append(out, characterRefResponse{CharacterID: id.String()})
			continue
		}
		out = append(out, characterRefResponse{
			CharacterID: id.String(),
			Name:        character.Name,
			Realm:       character.Realm,
			Class:       character.Class,
		})
	}
	return out
}
