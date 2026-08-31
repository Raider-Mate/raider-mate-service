package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
)

type characterResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Realm string `json:"realm"`
	// RaiderIOURL is reference data, not a transition, so it is a plain field rather
	// than a _links entry: an absent link means "not available to you right now", and
	// this page is available to everyone always.
	RaiderIOURL string   `json:"raiderio_url"`
	Region      string   `json:"region"`
	Class       *string  `json:"class,omitempty"`
	Spec        *string  `json:"spec,omitempty"`
	Ilvl        *float64 `json:"ilvl,omitempty"`
	MplusScore  *float64 `json:"mplus_score,omitempty"`
	// EnchantsMissing and EnchantsExpected are counts, never a percentage: this service
	// divides nowhere, and "2 of 8" says more to a raid lead than "75%". Both absent
	// together, on a character carrying no gear the worker could read.
	EnchantsMissing  *int `json:"enchants_missing,omitempty"`
	EnchantsExpected *int `json:"enchants_expected,omitempty"`
	// TierPieces is equipped pieces of the current season's class set. Absent when the
	// worker has no season configured, which is not the same as a raider wearing none
	// of it. The set bonuses land at two and four; which of those a count earns is the
	// client's rendering, not this service's arithmetic.
	TierPieces *int `json:"tier_pieces,omitempty"`
	// Progression is the raid the worker tracks, absent until this character has been
	// there. Pointers rather than zeros throughout, for the same reason.
	Progression *progressionResponse `json:"progression,omitempty"`
	// Roles is the character's role menu, in priority order, so the first entry is the
	// role they signed up to play first. Only the roster read carries it: it costs a
	// second query, and the write paths answer a question nobody asked. Absent means no
	// menu registered, which is not the same as an empty one.
	Roles  []roleChoiceResponse `json:"roles,omitempty"`
	IsMain bool                 `json:"is_main"`
	Synced bool                 `json:"synced"`
	// ArchivedAt is when this character was taken off the roster, absent while they are
	// on it. Present means the row is kept for the history hanging off it and is not
	// part of the active roster.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	// NotFoundSince is when Raider.IO started answering 404 for this character. Absent
	// while the last fetch found them. Evidence, never a verdict: a rename reads
	// identically to a raider who quit the game.
	NotFoundSince *time.Time `json:"not_found_since,omitempty"`
	Links         Links      `json:"_links"`
}

// progressionResponse is a character's standing in one raid. The slug rides along so a
// client never has to guess which tier the counts describe, and so a row left over
// from last tier is obvious rather than quietly wrong.
type progressionResponse struct {
	Raid   string `json:"raid"`
	Bosses int    `json:"bosses"`
	Normal int    `json:"normal"`
	Heroic int    `json:"heroic"`
	Mythic int    `json:"mythic"`
}

// withRoles attaches a role menu to a response, the way withSignupCounts attaches a
// tally to an event. Separate from characterToResponse because only the roster read
// has the menus in hand, already batched.
func withRoles(resp characterResponse, roles []roster.RoleChoice) characterResponse {
	resp.Roles = roleChoicesToResponse(roles)
	return resp
}

// characterToResponse renders one character. The two flags gate different links,
// because the two writes have different owners: a role menu is the raider's own
// ephemeral select and stays self-only even for a raid lead, while deleting a
// mistyped registration is roster hygiene a raid lead is expected to do. Offering a
// link that would 403 is worse than omitting it; the absence is the answer.
func characterToResponse(c roster.Character, owned, isRaidLead bool) characterResponse {
	href := "/api/characters/" + c.ID.String()

	links := Links{}
	links.add(true, "self", href, "")
	links.add(true, "role-menu", href+"/roles", "")
	links.add(owned, "roles", href+"/roles", "PUT")
	links.add(owned || isRaidLead, "edit", href, "PATCH")
	links.add(owned || isRaidLead, "delete", href, "DELETE")
	// Only one of the two is ever offered, because only one of them is a move from
	// here. Same audience as delete: taking a departed raider off the roster is the
	// hygiene that delete was being misused for.
	mayArchive := owned || isRaidLead
	links.add(mayArchive && c.ArchivedAt == nil, "archive", href+"/archive", "POST")
	links.add(mayArchive && c.ArchivedAt != nil, "unarchive", href+"/unarchive", "POST")

	var progression *progressionResponse
	if p := c.Progression; p != nil {
		progression = &progressionResponse{
			Raid:   p.Slug,
			Bosses: p.Bosses,
			Normal: p.NormalKilled,
			Heroic: p.HeroicKilled,
			Mythic: p.MythicKilled,
		}
	}

	return characterResponse{
		ID:          c.ID.String(),
		Name:        c.Name,
		Realm:       c.Realm,
		RaiderIOURL: raiderio.ProfileURL(c.Region, c.Realm, c.Name),
		Region:      c.Region,
		Class:       c.Class,
		Spec:        c.Spec,
		Ilvl:        c.Ilvl,
		MplusScore:  c.MplusScore,

		EnchantsMissing:  c.EnchantsMissing,
		EnchantsExpected: c.EnchantsExpected,
		TierPieces:       c.TierPieces,
		Progression:      progression,

		IsMain:        c.IsMain,
		Synced:        c.Synced,
		ArchivedAt:    c.ArchivedAt,
		NotFoundSince: c.NotFoundSince,
		Links:         links,
	}
}

// characterSummary is the part of a character needed to render it in someone else's
// resource: a signup row, a comp slot. Enough to draw the line without a second
// request, and no links of its own, since the full character resource carries those.
type characterSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Realm string `json:"realm"`
	// RaiderIOURL saves the bot reconstructing the link per row from fields this
	// shape does not even carry: region is part of the character's identity but not
	// of how it renders, so it stays out of the summary while its URL rides along.
	RaiderIOURL string   `json:"raiderio_url"`
	Class       *string  `json:"class,omitempty"`
	Spec        *string  `json:"spec,omitempty"`
	Ilvl        *float64 `json:"ilvl,omitempty"`
	// Roles is the character's whole menu, in priority order. It rides along because
	// the bot's embed needs it on every row: the flex marker beside a name, and the
	// grouping of signups by what each raider can play before a comp is locked. One
	// batched read here beats a request per raider to redraw one message.
	Roles  []roleChoiceResponse `json:"roles"`
	IsMain bool                 `json:"is_main"`
}

// characterSummaries indexes a guild roster by character id. Signup lists and comp
// boards both carry bare character ids, and every client rendering one needs names:
// one roster read here beats the same join repeated in the bot and the dashboard.
func characterSummaries(list []roster.Character, roles map[uuid.UUID][]roster.RoleChoice) map[uuid.UUID]characterSummary {
	out := make(map[uuid.UUID]characterSummary, len(list))
	for _, c := range list {
		out[c.ID] = characterSummary{
			ID: c.ID.String(), Name: c.Name, Realm: c.Realm,
			RaiderIOURL: raiderio.ProfileURL(c.Region, c.Realm, c.Name),
			Class:       c.Class, Spec: c.Spec, Ilvl: c.Ilvl,
			Roles: roleChoicesToResponse(roles[c.ID]), IsMain: c.IsMain,
		}
	}
	return out
}

// guildRoster reads a guild's characters and their role menus, indexed for rendering
// them inside someone else's resource.
func guildRoster(ctx context.Context, characters *roster.Characters, discordGuildID int64) (map[uuid.UUID]characterSummary, error) {
	// Archived included, deliberately. This indexes the roster for rendering names on
	// signups and comp boards, and a raider archived last week still has to have a name
	// on the raid they attended the week before.
	list, err := characters.ListForGuild(ctx, discordGuildID, true)
	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	roles, err := characters.ListRolesForMany(ctx, ids)
	if err != nil {
		return nil, err
	}

	return characterSummaries(list, roles), nil
}

// lookupCharacter returns the summary for id, or nil when the roster read did not
// include it. A signup whose character was deleted mid-request is the honest case;
// omitting the field beats inventing a placeholder name.
func lookupCharacter(byID map[uuid.UUID]characterSummary, id uuid.UUID) *characterSummary {
	summary, ok := byID[id]
	if !ok {
		return nil
	}
	return &summary
}

type createCharacterRequest struct {
	Name   string `json:"name"`
	Realm  string `json:"realm"`
	Region string `json:"region"`
	// IsMain asks for the main flag; the service grants it only while the raider has
	// no main yet. A client that sends true on every registration is therefore safe,
	// and PATCH /api/characters/{cid} is what moves the flag afterwards.
	IsMain bool `json:"is_main"`
}

// createCharacterHandler registers a character for the calling actor. A new
// character has no ilvl until the next worker sync tick: hard rule 5 forbids
// calling Raider.IO from this handler, so Character.Synced tells the bot the
// truth rather than showing a stale placeholder.
func createCharacterHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		guildID, ok := requireGuildPath(w, r, logger, "gid")
		if !ok {
			return
		}

		var body createCharacterRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		character, err := characters.Register(r.Context(), roster.RegisterInput{
			DiscordID:      int64(actor.DiscordID), //nolint:gosec
			DiscordGuildID: guildID,
			Name:           body.Name,
			Realm:          body.Realm,
			Region:         body.Region,
			IsMain:         body.IsMain,
		})
		if err != nil {
			// A retyped registration is the raider's mistake, not the service's, so it
			// gets a message the bot is allowed to show instead of "internal error".
			if errors.Is(err, roster.ErrCharacterExists) {
				writeError(w, logger, http.StatusConflict, "you already registered that character")
				return
			}
			logger.ErrorContext(r.Context(), "registering character", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		// Registered for the actor themselves, so they can always edit its roles.
		writeJSON(w, logger, http.StatusCreated, characterToResponse(character, true, actor.IsRaidLead))
	}
}

// listGuildCharactersHandler returns every character registered in a guild.
func listGuildCharactersHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		guildID, ok := requireGuildPath(w, r, logger, "gid")
		if !ok {
			return
		}

		// The roster is the active roster unless the caller says otherwise. A dashboard
		// showing who has left asks for them; nothing else has to know they exist.
		includeArchived := r.URL.Query().Get("include_archived") == "true"

		list, err := characters.ListForGuild(r.Context(), guildID, includeArchived)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		// A guild roster is everyone's characters, so the roles link has to be decided
		// per row rather than per response.
		mine, err := characters.ListForUser(r.Context(), int64(actor.DiscordID), guildID) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "listing actor characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}
		owned := make(map[uuid.UUID]bool, len(mine))
		for _, c := range mine {
			owned[c.ID] = true
		}

		// The roster is the one read that groups by role, so it is the one read that
		// pays for the menus. One batched query for the whole list.
		ids := make([]uuid.UUID, len(list))
		for i, c := range list {
			ids[i] = c.ID
		}
		roles, err := characters.ListRolesForMany(r.Context(), ids)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing character roles", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]characterResponse, len(list))
		for i, c := range list {
			out[i] = withRoles(characterToResponse(c, owned[c.ID], actor.IsRaidLead), roles[c.ID])
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// listUserCharactersHandler returns one Discord user's characters within the
// actor's guild. The guild comes from the actor's headers, not the path: a bot
// process serves one guild's request at a time, and {did} alone cannot say which
// guild's roster to search.
func listUserCharactersHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		discordID, err := pathSnowflake(r, "did")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		list, err := characters.ListForUser(r.Context(), discordID, int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "listing characters", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		// Every row belongs to {did}, so one comparison covers the whole list.
		self := discordID == int64(actor.DiscordID) //nolint:gosec

		out := make([]characterResponse, len(list))
		for i, c := range list {
			out[i] = characterToResponse(c, self, actor.IsRaidLead)
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

type userGuildResponse struct {
	DiscordGuildID string `json:"discord_guild_id"`
}

// listUserGuildsHandler returns the guilds Raider Mate knows a person in.
//
// This is the one route that deliberately reaches past the actor's guild, because it is
// asked in order to decide which guild to work in, before there is a meaningful one in
// the headers. That makes it the one route where {did} has to be the caller themselves:
// without the check, a raid lead in any guild could enumerate which other guilds a
// raider belongs to, which is nobody's business and is not visible anywhere else.
//
// No _links: every guild-scoped route is bound to the actor's own guild, so a link to
// another guild's collection would be a link that answers 403. The absence of a link
// means unavailable, and inventing one here would break that promise.
func listUserGuildsHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		discordID, err := pathSnowflake(r, "did")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		if discordID != int64(actor.DiscordID) { //nolint:gosec
			writeError(w, logger, http.StatusForbidden, "you may only list your own guilds")
			return
		}

		guilds, err := characters.ListGuildsForUser(r.Context(), discordID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing guilds for user", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		out := make([]userGuildResponse, len(guilds))
		for i, g := range guilds {
			out[i] = userGuildResponse{DiscordGuildID: strconv.FormatInt(g, 10)}
		}
		writeJSON(w, logger, http.StatusOK, out)
	}
}

// requireCharacterInGuild resolves a {cid} path segment to a character in the actor's
// guild. Reading a guildmate's character is allowed: their name and role menu are
// already visible on every signup list and comp board in the guild.
func requireCharacterInGuild(w http.ResponseWriter, r *http.Request, characters *roster.Characters, logger *slog.Logger) (uuid.UUID, bool) {
	actor, _ := actorFromContext(r.Context())

	characterID, err := pathUUID(r, "cid")
	if err != nil {
		writeError(w, logger, http.StatusBadRequest, err.Error())
		return uuid.Nil, false
	}

	inGuild, err := characters.InGuild(r.Context(), characterID, int64(actor.GuildID)) //nolint:gosec
	if err != nil {
		logger.ErrorContext(r.Context(), "checking character guild", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return uuid.Nil, false
	}
	if !inGuild {
		writeError(w, logger, http.StatusNotFound, "character not found")
		return uuid.Nil, false
	}

	return characterID, true
}

// requireWritableCharacter is requireCharacterInGuild plus "and you may write to it":
// the owner, or a raid lead clearing up their own guild's roster. owned reports which
// of the two it was, because it is also the answer for the roles link: editing a role
// menu stays self-only even for a raid lead (design.md section 4, the raider's own
// ephemeral select).
func requireWritableCharacter(w http.ResponseWriter, r *http.Request, characters *roster.Characters, logger *slog.Logger) (characterID uuid.UUID, owned, ok bool) {
	actor, _ := actorFromContext(r.Context())

	characterID, ok = requireCharacterInGuild(w, r, characters, logger)
	if !ok {
		return uuid.Nil, false, false
	}

	owned, err := characters.OwnedByDiscord(r.Context(), characterID, int64(actor.GuildID), int64(actor.DiscordID)) //nolint:gosec
	if err != nil {
		logger.ErrorContext(r.Context(), "checking character ownership", "error", err)
		writeError(w, logger, http.StatusInternalServerError, "internal error")
		return uuid.Nil, false, false
	}
	if !owned && !actor.IsRaidLead {
		writeError(w, logger, http.StatusForbidden, "not your character")
		return uuid.Nil, false, false
	}

	return characterID, owned, true
}

type roleChoiceRequest struct {
	Role     string `json:"role"`
	Priority int16  `json:"priority"`
}

type roleChoiceResponse struct {
	Role     string `json:"role"`
	Priority int16  `json:"priority"`
}

func roleChoicesToResponse(roles []roster.RoleChoice) []roleChoiceResponse {
	out := make([]roleChoiceResponse, len(roles))
	for i, rc := range roles {
		out[i] = roleChoiceResponse{Role: string(rc.Role), Priority: rc.Priority}
	}
	return out
}

// getCharacterRolesHandler returns a character's current role menu. The bot needs
// this to render the role select with the raider's existing picks already ticked:
// the PUT replaces the whole menu, so a select that opens blank silently drops
// every role the raider chose last time.
func getCharacterRolesHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		characterID, ok := requireCharacterInGuild(w, r, characters, logger)
		if !ok {
			return
		}

		roles, err := characters.ListRoles(r.Context(), characterID)
		if err != nil {
			logger.ErrorContext(r.Context(), "listing character roles", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, roleChoicesToResponse(roles))
	}
}

type patchCharacterRequest struct {
	IsMain *bool `json:"is_main,omitempty"`
}

// patchCharacterHandler edits a character. is_main is the only field: name, realm and
// region are the Raider.IO identity the sync job keys on, so correcting a typo there
// is a delete and a re-register rather than an edit that would silently repoint the
// character at a different armoury profile.
func patchCharacterHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		characterID, owned, ok := requireWritableCharacter(w, r, characters, logger)
		if !ok {
			return
		}

		var body patchCharacterRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		if body.IsMain == nil {
			writeError(w, logger, http.StatusBadRequest, "is_main: required")
			return
		}

		actor, _ := actorFromContext(r.Context())
		if err := characters.SetMain(r.Context(), characterID, int64(actor.GuildID), *body.IsMain); err != nil { //nolint:gosec
			if errors.Is(err, roster.ErrCharacterNotFound) {
				writeError(w, logger, http.StatusNotFound, "character not found")
				return
			}
			logger.ErrorContext(r.Context(), "setting character main", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		character, err := characters.GetInGuild(r.Context(), characterID, int64(actor.GuildID)) //nolint:gosec
		if err != nil {
			logger.ErrorContext(r.Context(), "loading character", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, characterToResponse(character, owned, actor.IsRaidLead))
	}
}

// deleteCharacterHandler removes a character and everything hanging off it. Every
// child table cascades, so this drops the raider's signups, comp slots and gear
// snapshots too. That is right for the case it exists for, a mistyped registration,
// and wrong for a raider leaving the guild, which is not a delete at all.
func deleteCharacterHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		characterID, _, ok := requireWritableCharacter(w, r, characters, logger)
		if !ok {
			return
		}

		actor, _ := actorFromContext(r.Context())
		if err := characters.Delete(r.Context(), characterID, int64(actor.GuildID)); err != nil { //nolint:gosec
			if errors.Is(err, roster.ErrCharacterNotFound) {
				writeError(w, logger, http.StatusNotFound, "character not found")
				return
			}
			logger.ErrorContext(r.Context(), "deleting character", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// setCharacterArchivedHandler takes a character off the roster, or puts them back.
//
// Separate from DELETE on purpose, and the reason is in the schema: every foreign key
// into characters cascades, so deleting a raider who has left takes their signups,
// their comp slots and their gear snapshots with them, and attendance is computed from
// exactly those rows. Delete is for a registration typed wrong an hour ago. This is for
// a raider who left, and it is reversible, because most of them come back.
func setCharacterArchivedHandler(characters *roster.Characters, archived bool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		characterID, owned, ok := requireWritableCharacter(w, r, characters, logger)
		if !ok {
			return
		}

		actor, _ := actorFromContext(r.Context())
		guildID := int64(actor.GuildID) //nolint:gosec
		if err := characters.SetArchived(r.Context(), characterID, guildID, archived); err != nil {
			if errors.Is(err, roster.ErrCharacterNotFound) {
				writeError(w, logger, http.StatusNotFound, "character not found")
				return
			}
			logger.ErrorContext(r.Context(), "archiving character", "error", err, "archived", archived)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		// Read back rather than echoing the request: the caller needs the new links,
		// and archive and unarchive swap which of the two it is offered.
		character, err := characters.GetInGuild(r.Context(), characterID, guildID)
		if err != nil {
			logger.ErrorContext(r.Context(), "reading archived character", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, logger, http.StatusOK, characterToResponse(character, owned, actor.IsRaidLead))
	}
}

type putCharacterRolesRequest struct {
	Roles []roleChoiceRequest `json:"roles"`
}

// putCharacterRolesHandler replaces a character's whole role menu. Self only: this
// is the ephemeral role select from design.md section 4, entirely separate from the
// signup write it precedes (hard rule 2).
func putCharacterRolesHandler(characters *roster.Characters, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := actorFromContext(r.Context())

		characterID, err := pathUUID(r, "cid")
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
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

		var body putCharacterRolesRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		roles := make([]roster.RoleChoice, len(body.Roles))
		for i, rc := range body.Roles {
			role, err := parseRole(rc.Role)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "roles["+strconv.Itoa(i)+"]."+err.Error())
				return
			}
			roles[i] = roster.RoleChoice{Role: role, Priority: rc.Priority}
		}

		if err := characters.SetRoles(r.Context(), characterID, roles); err != nil {
			logger.ErrorContext(r.Context(), "setting character roles", "error", err)
			writeError(w, logger, http.StatusInternalServerError, "internal error")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
