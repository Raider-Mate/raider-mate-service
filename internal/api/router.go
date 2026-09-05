package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/audit"
	"github.com/Raider-Mate/raider-mate-service/internal/billing"
	"github.com/Raider-Mate/raider-mate-service/internal/comp"
	"github.com/Raider-Mate/raider-mate-service/internal/raidlog"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
	"github.com/Raider-Mate/raider-mate-service/internal/secretbox"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
)

// NewRouter builds the HTTP handler tree for the service. Wiring is by hand: each
// domain package's Store is constructed once and handed to the use cases that need
// it, the same shape cmd/worker uses for roster.Syncer and signup.Runner.
func NewRouter(pool *pgxpool.Pool, apiKey string, secrets *secretbox.Box, warcraftLogsClientID string, queued queueWatcher, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler(pool, logger))

	rosterStore := roster.NewStore(pool)
	characters := roster.NewCharacters(rosterStore)

	// Tier is read by the analysis endpoints alone today. It is wired here rather than
	// inside audit so there is one Tiers for whatever else comes to need one.
	tiers := billing.NewTiers(billing.NewStore(pool))
	analysis := audit.NewAnalysis(audit.NewStore(pool), tiers)

	compStore := comp.NewStore(pool)
	locker := comp.NewLocker(compStore)
	reader := comp.NewReader(compStore)
	manual := comp.NewManual(compStore)

	// Post-raid numbers. The store is both the worker's persistence and the API's read
	// side; the API only ever reads, since fetching from a request handler is what hard
	// rule 5 forbids. The credentials are the instance's, used by a guild that has not
	// supplied its own.
	reports := raidlog.NewStore(pool, secrets, warcraftLogsClientID, "")

	signupStore := signup.NewStore(pool)
	events := signup.NewEvents(signupStore)
	signups := signup.NewSignups(signupStore, logger)
	lateRequests := signup.NewLateRequests(signupStore, logger)
	raidLeads := signup.NewRaidLeads(signupStore)
	settings := signup.NewSettings(signupStore)
	outbox := signup.NewOutbox(signupStore)
	catalog := signup.NewGuildCatalog(signupStore)

	apiMux := http.NewServeMux()

	apiMux.HandleFunc("GET /api/guilds/{gid}/capabilities", getGuildCapabilitiesHandler(logger))

	// Analysis is the guild's own history read back to it, so it is open to anyone in
	// the guild, like the roster and the event list. What a guild may read is the
	// index's link set; the panels themselves answer 402 to a caller who went straight
	// there without one.
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis", getAnalysisIndexHandler(analysis, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis/attendance", getAttendanceHandler(analysis, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis/comp-balance", getCompBalanceHandler(analysis, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis/roster-health", getRosterHealthHandler(analysis, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis/throughput", getThroughputHandler(analysis, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/analysis/ilvl", getIlvlSeriesHandler(analysis, logger))

	apiMux.HandleFunc("GET /api/guilds/{gid}/raid-lead-roles", listRaidLeadRolesHandler(raidLeads, logger))
	apiMux.HandleFunc("PUT /api/guilds/{gid}/raid-lead-roles", putRaidLeadRolesHandler(raidLeads, logger))

	apiMux.HandleFunc("GET /api/guilds/{gid}/settings", getGuildSettingsHandler(settings, logger))
	apiMux.HandleFunc("PUT /api/guilds/{gid}/settings", putGuildSettingsHandler(settings, logger))

	apiMux.HandleFunc("GET /api/guilds/{gid}/discord-channels", listGuildChannelsHandler(catalog, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/discord-roles", listGuildRolesHandler(catalog, logger))

	apiMux.HandleFunc("POST /api/guilds/{gid}/characters", createCharacterHandler(characters, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/characters", listGuildCharactersHandler(characters, logger))
	apiMux.HandleFunc("GET /api/users/{did}/characters", listUserCharactersHandler(characters, logger))
	apiMux.HandleFunc("PATCH /api/characters/{cid}", patchCharacterHandler(characters, logger))
	apiMux.HandleFunc("DELETE /api/characters/{cid}", deleteCharacterHandler(characters, logger))
	apiMux.HandleFunc("POST /api/characters/{cid}/archive", setCharacterArchivedHandler(characters, true, logger))
	apiMux.HandleFunc("POST /api/characters/{cid}/unarchive", setCharacterArchivedHandler(characters, false, logger))
	apiMux.HandleFunc("GET /api/characters/{cid}/roles", getCharacterRolesHandler(characters, logger))
	apiMux.HandleFunc("PUT /api/characters/{cid}/roles", putCharacterRolesHandler(characters, logger))

	apiMux.HandleFunc("POST /api/guilds/{gid}/events", createEventHandler(events, logger))
	apiMux.HandleFunc("GET /api/guilds/{gid}/events", listGuildEventsHandler(events, logger))
	apiMux.HandleFunc("GET /api/events/{id}", getEventHandler(events, reports, logger))
	apiMux.HandleFunc("PATCH /api/events/{id}", patchEventHandler(events, logger))
	apiMux.HandleFunc("DELETE /api/events/{id}", deleteEventHandler(events, logger))

	apiMux.HandleFunc("PUT /api/events/{id}/signups/{cid}", putSignupHandler(signups, lateRequests, characters, events, logger))
	apiMux.HandleFunc("DELETE /api/events/{id}/signups/{cid}", deleteSignupHandler(signups, lateRequests, characters, events, logger))
	apiMux.HandleFunc("GET /api/events/{id}/signups", listSignupsHandler(signups, characters, events, logger))

	apiMux.HandleFunc("GET /api/events/{id}/report", getEventReportHandler(reports, characters, events, logger))
	apiMux.HandleFunc("POST /api/events/{id}/report/refresh", refreshEventReportHandler(reports, events, logger))

	apiMux.HandleFunc("GET /api/events/{id}/late-requests", listLateRequestsHandler(lateRequests, events, logger))
	apiMux.HandleFunc("POST /api/events/{id}/late-requests/{rid}/approve", approveLateRequestHandler(lateRequests, events, logger))
	apiMux.HandleFunc("POST /api/events/{id}/late-requests/{rid}/reject", rejectLateRequestHandler(lateRequests, events, logger))

	apiMux.HandleFunc("GET /api/events/{id}/comps", listCompsHandler(reader, events, logger))
	apiMux.HandleFunc("GET /api/events/{id}/comps/{name}", getCompHandler(reader, characters, events, logger))
	apiMux.HandleFunc("PUT /api/events/{id}/comps/{name}", saveCompHandler(manual, characters, events, logger))
	apiMux.HandleFunc("PATCH /api/events/{id}/comps/{name}", renameCompHandler(manual, events, logger))
	apiMux.HandleFunc("PUT /api/events/{id}/comps/{name}/mode", setCompModeHandler(manual, events, logger))
	apiMux.HandleFunc("POST /api/events/{id}/comps/{name}/lock", lockCompHandler(locker, characters, events, logger))

	mux.Handle("/api/", requireAuth(apiMux, apiKey, signupStore, logger))

	// The outbox is the bot talking as itself rather than on a raider's behalf, so it
	// takes the shared key alone and no actor headers. Both patterns are more specific
	// than the "/api/" prefix above, which is what routes them here instead.
	mux.Handle("GET /api/notifications",
		requireServiceKey(listNotificationsHandler(outbox, logger), apiKey, logger))
	mux.Handle("POST /api/notifications/{id}/delivered",
		requireServiceKey(markNotificationDeliveredHandler(outbox, logger), apiKey, logger))
	mux.Handle("POST /api/notifications/{id}/failed",
		requireServiceKey(markNotificationFailedHandler(outbox, logger), apiKey, logger))
	mux.Handle("GET /api/notifications/stream",
		requireServiceKey(streamNotificationsHandler(queued, logger), apiKey, logger))

	// The catalog push is the bot reporting its own view of a guild's channels and
	// roles, not something done on a raider's behalf, so it takes the shared key alone,
	// same reasoning as the notifications block above. Guild-scoped, unlike the
	// notifications routes, since the bot pushes one guild's catalog at a time.
	mux.Handle("PUT /api/guilds/{gid}/discord-channels",
		requireServiceKey(putGuildChannelsHandler(catalog, logger), apiKey, logger))
	mux.Handle("PUT /api/guilds/{gid}/discord-roles",
		requireServiceKey(putGuildRolesHandler(catalog, logger), apiKey, logger))

	// Asked before a guild is chosen, so it cannot go through requireAuth: there is no
	// guild id to put in the headers yet. More specific than the "/api/" prefix above,
	// which is what routes it here instead.
	mux.Handle("GET /api/users/{did}/guilds",
		requireGuildlessAuth(listUserGuildsHandler(characters, logger), apiKey, logger))

	// The bot telling the service where it posted an event it was asked to announce.
	// Service key alone for the same reason as the catalog pushes above: a poller has
	// no member to speak as, so it cannot pass the raid-lead check PATCH makes.
	mux.Handle("PUT /api/events/{id}/message",
		requireServiceKey(putEventMessageHandler(events, logger), apiKey, logger))

	return recoverPanic(logRequests(mux, logger), logger)
}
