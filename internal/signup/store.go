package signup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// Store implements eventStore, signupStore, lateStore, and reminderStore over
// Postgres.
type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

// NewStore builds a Store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, queries: db.New(pool)}
}

// begin opens a transaction and returns a Store bound to it. The three Transact
// methods below differ only in which consumer interface they hand fn, so the
// begin/commit knowledge lives here once; a shared generic version would have to
// assert *Store into the interface at runtime, trading a compile-time guarantee for
// three fewer lines.
func (s *Store) begin(ctx context.Context) (pgx.Tx, *Store, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("beginning tx: %w", err)
	}
	return tx, &Store{pool: s.pool, queries: s.queries.WithTx(tx)}, nil
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// Transact runs fn against a Store bound to one transaction. Committing or rolling
// back is Transact's job; fn only returns whether its work succeeded.
func (s *Store) Transact(ctx context.Context, fn func(ctx context.Context, tx reminderStore) error) error {
	tx, txStore, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := fn(ctx, txStore); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// TransactSignups is Transact for Signups. A signup write and the notification it
// produces have to land together: a row deleted out of a locked comp with no
// COMP_SLOT_DROPPED behind it is a hole nobody is told about.
func (s *Store) TransactSignups(ctx context.Context, fn func(ctx context.Context, tx signupStore) error) error {
	tx, txStore, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := fn(ctx, txStore); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// TransactLate is Transact for LateRequests. Same reason, plus one of its own:
// Approve reads a request's state and then decides it, and those two have to be one
// step or two raid leads racing the same button both get past the guard.
func (s *Store) TransactLate(ctx context.Context, fn func(ctx context.Context, tx lateStore) error) error {
	tx, txStore, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := fn(ctx, txStore); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// CreateEvent inserts the event and its initial job schedule in one transaction, so
// an event cannot end up posted with no reminders behind it.
func (s *Store) CreateEvent(ctx context.Context, in CreateEventInput) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	// Announce needs the events channel, and an unstated lead needs the guild default,
	// so the settings are read once for whichever of the two applies.
	var settings GuildSettings
	if in.ReminderLeadMinutes == nil || in.Announce {
		settings, err = guildSettings(ctx, q, in.DiscordGuildID)
		if err != nil {
			return Event{}, fmt.Errorf("reading guild settings: %w", err)
		}
	}

	// Refused before the insert rather than after: an announced event nobody can be
	// told about is worse than no event, and the caller can fix the setting and retry.
	if in.Announce && settings.EventsChannelID == nil {
		return Event{}, ErrNoEventsChannel
	}

	// Resolved here rather than at fire time: the effective lead is stored on the event,
	// so changing the guild default later cannot re-time a raid that is already posted.
	lead := in.ReminderLeadMinutes
	if lead == nil {
		resolved := settings.ReminderLead()
		lead = &resolved
	}

	row, err := q.CreateEvent(ctx, db.CreateEventParams{
		ID:                  db.NewID(),
		DiscordGuildID:      in.DiscordGuildID,
		Type:                in.Type,
		Title:               in.Title,
		StartsAt:            pgtype.Timestamptz{Time: in.StartsAt, Valid: true},
		SignupDeadline:      pgtype.Timestamptz{Time: in.SignupDeadline, Valid: true},
		CompTemplate:        in.CompTemplate,
		Difficulty:          in.Difficulty,
		ReminderLeadMinutes: lead,
	})
	if err != nil {
		return Event{}, fmt.Errorf("inserting event: %w", err)
	}

	if err := scheduleJobs(ctx, q, row.ID, in.StartsAt, in.SignupDeadline, *lead); err != nil {
		return Event{}, err
	}

	// In the same transaction as the event, so there is no window in which an announced
	// event exists with nothing queued to announce it.
	if in.Announce {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			ID:             db.NewID(),
			DiscordGuildID: in.DiscordGuildID,
			EventID:        row.ID,
			Kind:           db.NotificationKindEVENTCREATED,
			TargetKind:     db.NotificationTargetCHANNEL,
			ChannelID:      settings.EventsChannelID,
			Payload:        []byte("{}"),
		}); err != nil {
			return Event{}, fmt.Errorf("queueing event announcement: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("committing tx: %w", err)
	}
	return eventFromRow(row), nil
}

func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (Event, error) {
	row, err := s.queries.GetEvent(ctx, id)
	if err != nil {
		return Event{}, err
	}
	return eventFromRow(row), nil
}

func (s *Store) ListUpcomingEvents(ctx context.Context, discordGuildID int64) ([]Event, error) {
	rows, err := s.queries.ListUpcomingEvents(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	events := make([]Event, len(rows))
	for i, row := range rows {
		events[i] = eventFromRow(row)
	}
	return events, nil
}

// CountSignupsByStatus tallies signups for several events in one round trip. Grouped
// in SQL rather than by reading rows and counting in Go: a month of raid nights is a
// query per event otherwise, each one dragging back full signup rows to learn how many
// there are.
//
// An event with no signups at all is absent from the result. Seeding it, and seeding
// the statuses nobody chose, is the domain layer's job.
func (s *Store) CountSignupsByStatus(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]map[db.SignupStatus]int, error) {
	rows, err := s.queries.CountSignupsByStatusForEvents(ctx, eventIDs)
	if err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]map[db.SignupStatus]int, len(eventIDs))
	for _, row := range rows {
		if out[row.EventID] == nil {
			out[row.EventID] = map[db.SignupStatus]int{}
		}
		out[row.EventID][row.Status] = int(row.Total)
	}
	return out, nil
}

func (s *Store) ListPastEvents(ctx context.Context, discordGuildID int64) ([]Event, error) {
	rows, err := s.queries.ListPastEvents(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	events := make([]Event, len(rows))
	for i, row := range rows {
		events[i] = eventFromRow(row)
	}
	return events, nil
}

// UpdateEvent applies a partial edit and, whenever StartsAt or SignupDeadline moved,
// cancels every PENDING job for the event and reschedules from the new times, all in
// one transaction (design.md section 6: cancel on edit rather than validating at fire
// time). An edit to anything a reader sees also queues the redraw, in the same
// transaction, so the sheet in the channel cannot disagree with the stored event.
func (s *Store) UpdateEvent(ctx context.Context, in UpdateEventInput) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	params := db.UpdateEventParams{
		ID:                  in.ID,
		Title:               in.Title,
		CompTemplate:        in.CompTemplate,
		Difficulty:          in.Difficulty,
		MessageID:           in.MessageID,
		ChannelID:           in.ChannelID,
		ReminderLeadMinutes: in.ReminderLeadMinutes,
		WarcraftlogsUrl:     in.WarcraftLogsURL,
	}
	if in.StartsAt != nil {
		params.StartsAt = pgtype.Timestamptz{Time: *in.StartsAt, Valid: true}
	}
	if in.SignupDeadline != nil {
		params.SignupDeadline = pgtype.Timestamptz{Time: *in.SignupDeadline, Valid: true}
	}

	row, err := q.UpdateEvent(ctx, params)
	if err != nil {
		return Event{}, fmt.Errorf("updating event: %w", err)
	}

	if in.StartsAt != nil || in.SignupDeadline != nil || in.ReminderLeadMinutes != nil {
		if err := q.CancelJobsForEvent(ctx, row.ID); err != nil {
			return Event{}, fmt.Errorf("cancelling old jobs: %w", err)
		}
		// An event created before the lead time existed has no stored value. It gets the
		// default here rather than losing its pre-event reminder to a title edit.
		lead := DefaultReminderLeadMinutes
		if row.ReminderLeadMinutes != nil {
			lead = *row.ReminderLeadMinutes
		}
		if err := scheduleJobs(ctx, q, row.ID, row.StartsAt.Time, row.SignupDeadline.Time, lead); err != nil {
			return Event{}, err
		}
	}

	if in.changesWhatIsPosted() && row.MessageID != nil && row.ChannelID != nil {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			ID:             db.NewID(),
			DiscordGuildID: row.DiscordGuildID,
			EventID:        row.ID,
			Kind:           db.NotificationKindEVENTCHANGED,
			TargetKind:     db.NotificationTargetMESSAGE,
			ChannelID:      row.ChannelID,
			Payload:        []byte("{}"),
		}); err != nil {
			return Event{}, fmt.Errorf("queueing event redraw: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("committing tx: %w", err)
	}
	return eventFromRow(row), nil
}

func (s *Store) DeleteEvent(ctx context.Context, id uuid.UUID) error {
	return s.queries.DeleteEvent(ctx, id)
}

// scheduleJobs writes the reminder/deadline schedule jobsFor computes for an event.
func scheduleJobs(ctx context.Context, q *db.Queries, eventID uuid.UUID, startsAt, deadline time.Time, leadMinutes int32) error {
	for _, job := range jobsFor(startsAt, deadline, leadMinutes, time.Now()) {
		if err := q.ScheduleJob(ctx, db.ScheduleJobParams{
			ID:      db.NewID(),
			EventID: eventID, JobType: job.Kind, RunAt: pgtype.Timestamptz{Time: job.RunAt, Valid: true},
		}); err != nil {
			return fmt.Errorf("scheduling %s: %w", job.Kind, err)
		}
	}
	return nil
}

func eventFromRow(row db.Event) Event {
	return Event{
		ID:             row.ID,
		DiscordGuildID: row.DiscordGuildID,
		Type:           row.Type,
		Title:          row.Title,
		StartsAt:       row.StartsAt.Time,
		SignupDeadline: row.SignupDeadline.Time,
		CompTemplate:   row.CompTemplate,
		MessageID:      row.MessageID,
		ChannelID:      row.ChannelID,
		Difficulty:     row.Difficulty,

		ReminderLeadMinutes: row.ReminderLeadMinutes,
		WarcraftLogsURL:     row.WarcraftlogsUrl,
	}
}

// UpsertSignup writes the signup and, when the new status leaves the assignment pool,
// deletes whatever comp slots the character held. It returns the comps it emptied so
// the caller can tell the raid lead.
//
// No transaction of its own: both callers run inside TransactSignups or TransactLate,
// and opening one here would take a second connection and commit the write
// independently of the notification that reports it.
//
// The eviction lives here rather than in Signups.Write because LateRequests.Approve
// replays a stored status straight into the store: a request carrying DECLINED,
// approved after the comp locked, would otherwise leave a seat behind.
func (s *Store) UpsertSignup(ctx context.Context, in SignupWrite) (Signup, []string, error) {
	row, err := s.queries.UpsertSignup(ctx, db.UpsertSignupParams{
		ID:      db.NewID(),
		EventID: in.EventID, CharacterID: in.CharacterID, Status: in.Status, Note: in.Note,
		LateUntil: timestamptzFromPtr(in.LateUntil),
	})
	if err != nil {
		return Signup{}, nil, err
	}

	// Unconditional: the query itself decides, by reading back the status just
	// written, so the pool rule lives in one place (see its comment).
	droppedFrom, err := s.queries.DropCompSlotsForCharacter(ctx, db.DropCompSlotsForCharacterParams{
		EventID: in.EventID, CharacterID: in.CharacterID,
	})
	if err != nil {
		return Signup{}, nil, fmt.Errorf("dropping comp slots: %w", err)
	}
	return signupFromRow(row), droppedFrom, nil
}

// DeleteSignup removes the signup and the comp slots that went with it, returning the
// comps it emptied. With the row gone the pool test in DropCompSlotsForCharacter finds
// no signup at all, which is the strongest form of "not in the pool".
//
// No transaction of its own, for the same reason UpsertSignup has none: the caller owns
// the boundary, and the notification belongs inside it.
func (s *Store) DeleteSignup(ctx context.Context, eventID, characterID uuid.UUID) ([]string, error) {
	if err := s.queries.DeleteSignup(ctx, db.DeleteSignupParams{EventID: eventID, CharacterID: characterID}); err != nil {
		return nil, err
	}
	droppedFrom, err := s.queries.DropCompSlotsForCharacter(ctx, db.DropCompSlotsForCharacterParams{
		EventID: eventID, CharacterID: characterID,
	})
	if err != nil {
		return nil, fmt.Errorf("dropping comp slots: %w", err)
	}
	return droppedFrom, nil
}

func (s *Store) ListSignupsForEvent(ctx context.Context, eventID uuid.UUID) ([]Signup, error) {
	rows, err := s.queries.ListSignupsForEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	signups := make([]Signup, len(rows))
	for i, row := range rows {
		signups[i] = signupFromRow(row)
	}
	return signups, nil
}

func signupFromRow(row db.Signup) Signup {
	signup := Signup{
		ID:           row.ID,
		EventID:      row.EventID,
		CharacterID:  row.CharacterID,
		Status:       row.Status,
		AssignedRole: row.AssignedRole,
		Note:         row.Note,
		CreatedAt:    row.CreatedAt.Time,
	}
	if row.LateUntil.Valid {
		t := row.LateUntil.Time
		signup.LateUntil = &t
	}
	return signup
}

func (s *Store) UpsertLateRequest(ctx context.Context, in LateRequestWrite) (LateRequest, error) {
	row, err := s.queries.UpsertLateRequest(ctx, db.UpsertLateRequestParams{
		ID:      db.NewID(),
		EventID: in.EventID, CharacterID: in.CharacterID, Status: in.Status, Note: in.Note,
		LateUntil: timestamptzFromPtr(in.LateUntil),
	})
	if err != nil {
		return LateRequest{}, err
	}
	return lateRequestFromRow(row), nil
}

func (s *Store) GetLateRequest(ctx context.Context, id uuid.UUID) (LateRequest, error) {
	row, err := s.queries.GetLateRequest(ctx, id)
	if err != nil {
		return LateRequest{}, err
	}
	return lateRequestFromRow(row), nil
}

func (s *Store) ListLateRequests(ctx context.Context, eventID uuid.UUID) ([]LateRequest, error) {
	rows, err := s.queries.ListLateRequests(ctx, eventID)
	if err != nil {
		return nil, err
	}
	reqs := make([]LateRequest, len(rows))
	for i, row := range rows {
		reqs[i] = lateRequestFromRow(row)
	}
	return reqs, nil
}

func (s *Store) DecideLateRequest(ctx context.Context, id uuid.UUID, state db.RequestState) error {
	return s.queries.DecideLateRequest(ctx, db.DecideLateRequestParams{ID: id, State: state})
}

// timestamptzFromPtr maps a nil time to SQL NULL. Go's zero time is a real instant,
// so writing it unguarded would store year 1 rather than "unset".
func timestamptzFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func lateRequestFromRow(row db.LateSignupRequest) LateRequest {
	req := LateRequest{
		ID:          row.ID,
		EventID:     row.EventID,
		CharacterID: row.CharacterID,
		Status:      row.Status,
		Note:        row.Note,
		State:       row.State,
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.LateUntil.Valid {
		t := row.LateUntil.Time
		req.LateUntil = &t
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		req.DecidedAt = &t
	}
	return req
}

func (s *Store) RaidLeadRoleIDs(ctx context.Context, discordGuildID int64) ([]int64, error) {
	return s.queries.ListRaidLeadRoles(ctx, discordGuildID)
}

// ReplaceRaidLeadRoleIDs overwrites a guild's whole mapping in one transaction: a
// PUT that half-applies would leave a stale role granting the capability alongside
// the caller's intended set.
// HighestGuildRoleID reports the top of a guild's role hierarchy. No rows means the
// bot has not catalogued this guild's roles yet, which is not an error.
func (s *Store) HighestGuildRoleID(ctx context.Context, discordGuildID int64) (int64, bool, error) {
	id, err := s.queries.HighestGuildRole(ctx, discordGuildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) ReplaceRaidLeadRoleIDs(ctx context.Context, discordGuildID int64, roleIDs []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	if err := q.DeleteRaidLeadRoles(ctx, discordGuildID); err != nil {
		return fmt.Errorf("clearing existing roles: %w", err)
	}
	for _, roleID := range roleIDs {
		if err := q.InsertRaidLeadRole(ctx, db.InsertRaidLeadRoleParams{
			DiscordGuildID: discordGuildID, DiscordRoleID: roleID,
		}); err != nil {
			return fmt.Errorf("inserting role %d: %w", roleID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// GuildSettings reads a guild's configuration. A guild that has configured nothing has
// no row, which is not an error: the zero value is the correct answer, and every guild
// starts there.
func (s *Store) GuildSettings(ctx context.Context, discordGuildID int64) (GuildSettings, error) {
	return guildSettings(ctx, s.queries, discordGuildID)
}

// guildSettings takes the queries to read through, so a caller inside a transaction
// reads on the same connection rather than opening a second one mid-write.
func guildSettings(ctx context.Context, q *db.Queries, discordGuildID int64) (GuildSettings, error) {
	row, err := q.GetGuildSettings(ctx, discordGuildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return GuildSettings{DiscordGuildID: discordGuildID}, nil
	}
	if err != nil {
		return GuildSettings{}, err
	}
	return guildSettingsFromRow(row), nil
}

func (s *Store) UpsertGuildSettings(ctx context.Context, settings GuildSettings) (GuildSettings, error) {
	row, err := s.queries.UpsertGuildSettings(ctx, db.UpsertGuildSettingsParams{
		DiscordGuildID:      settings.DiscordGuildID,
		EventsChannelID:     settings.EventsChannelID,
		Timezone:            settings.Timezone,
		EventMentionRoleIds: settings.EventMentionRoleIDs,
		EventBannerUrl:      settings.EventBannerURL,
		ReminderLeadMinutes: settings.ReminderLeadMinutes,
		ReminderDelivery:    settings.ReminderDelivery,
	})
	if err != nil {
		return GuildSettings{}, err
	}
	return guildSettingsFromRow(row), nil
}

func guildSettingsFromRow(row db.GuildSetting) GuildSettings {
	return GuildSettings{
		DiscordGuildID:      row.DiscordGuildID,
		EventsChannelID:     row.EventsChannelID,
		Timezone:            row.Timezone,
		EventMentionRoleIDs: row.EventMentionRoleIds,
		EventBannerURL:      row.EventBannerUrl,
		ReminderLeadMinutes: row.ReminderLeadMinutes,
		ReminderDelivery:    row.ReminderDelivery,
	}
}

// GuildChannels reads a guild's channel catalog, as the bot last pushed it.
func (s *Store) GuildChannels(ctx context.Context, discordGuildID int64) ([]Channel, error) {
	rows, err := s.queries.ListGuildChannels(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, len(rows))
	for i, row := range rows {
		channels[i] = Channel{DiscordChannelID: row.DiscordChannelID, Name: row.Name, Type: row.Type}
	}
	return channels, nil
}

// ReplaceGuildChannels overwrites a guild's whole channel catalog in one transaction,
// same reasoning as ReplaceRaidLeadRoleIDs: a push that half-applies would leave a
// stale channel alongside the bot's current set.
func (s *Store) ReplaceGuildChannels(ctx context.Context, discordGuildID int64, channels []Channel) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	if err := q.DeleteGuildChannels(ctx, discordGuildID); err != nil {
		return fmt.Errorf("clearing existing channels: %w", err)
	}
	for _, channel := range channels {
		if err := q.InsertGuildChannel(ctx, db.InsertGuildChannelParams{
			DiscordGuildID: discordGuildID, DiscordChannelID: channel.DiscordChannelID,
			Name: channel.Name, Type: channel.Type,
		}); err != nil {
			return fmt.Errorf("inserting channel %d: %w", channel.DiscordChannelID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// GuildRoles reads a guild's role catalog, as the bot last pushed it.
func (s *Store) GuildRoles(ctx context.Context, discordGuildID int64) ([]Role, error) {
	rows, err := s.queries.ListGuildRoles(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	roles := make([]Role, len(rows))
	for i, row := range rows {
		roles[i] = Role{DiscordRoleID: row.DiscordRoleID, Name: row.Name, Color: row.Color, Position: row.Position}
	}
	return roles, nil
}

// ReplaceGuildRoles overwrites a guild's whole role catalog, same shape as
// ReplaceGuildChannels.
func (s *Store) ReplaceGuildRoles(ctx context.Context, discordGuildID int64, roles []Role) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := s.queries.WithTx(tx)

	if err := q.DeleteGuildRoles(ctx, discordGuildID); err != nil {
		return fmt.Errorf("clearing existing roles: %w", err)
	}
	for _, role := range roles {
		if err := q.InsertGuildRole(ctx, db.InsertGuildRoleParams{
			DiscordGuildID: discordGuildID, DiscordRoleID: role.DiscordRoleID,
			Name: role.Name, Color: role.Color, Position: role.Position,
		}); err != nil {
			return fmt.Errorf("inserting role %d: %w", role.DiscordRoleID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

func (s *Store) ClaimNotifications(ctx context.Context, guildID *int64, claimedBefore time.Time, limit int32) ([]StoredNotification, error) {
	rows, err := s.queries.ClaimNotifications(ctx, db.ClaimNotificationsParams{
		GuildID:       guildID,
		ClaimedBefore: pgtype.Timestamptz{Time: claimedBefore, Valid: true},
		RowLimit:      limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]StoredNotification, len(rows))
	for i, row := range rows {
		out[i] = StoredNotification{
			ID:             row.ID,
			DiscordGuildID: row.DiscordGuildID,
			EventID:        row.EventID,
			Kind:           row.Kind,
			TargetKind:     row.TargetKind,
			DiscordID:      row.DiscordID,
			RoleIDs:        row.RoleIds,
			DiscordIDs:     row.DiscordIds,
			ChannelID:      row.ChannelID,
			Payload:        row.Payload,
			CreatedAt:      row.CreatedAt.Time,
		}
	}
	return out, nil
}

func (s *Store) MarkNotificationDelivered(ctx context.Context, id uuid.UUID, discordGuildID *int64) error {
	rows, err := s.queries.MarkNotificationDelivered(ctx, db.MarkNotificationDeliveredParams{
		ID: id, GuildID: discordGuildID,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

func (s *Store) InsertNotification(ctx context.Context, n Notification) error {
	_, err := s.queries.InsertNotification(ctx, db.InsertNotificationParams{
		ID:             db.NewID(),
		DiscordGuildID: n.DiscordGuildID,
		EventID:        n.EventID,
		Kind:           n.Kind,
		TargetKind:     n.TargetKind,
		DiscordID:      n.DiscordID,
		RoleIds:        n.RoleIDs,
		DiscordIds:     n.DiscordIDs,
		ChannelID:      n.ChannelID,
		Payload:        n.Payload,
	})
	return err
}

func (s *Store) ClaimDueJobs(ctx context.Context, limit int32) ([]db.ScheduledJob, error) {
	return s.queries.ClaimDueJobs(ctx, limit)
}

func (s *Store) MarkJobSent(ctx context.Context, id uuid.UUID) error {
	return s.queries.MarkJobSent(ctx, id)
}

func (s *Store) MarkJobFailed(ctx context.Context, id uuid.UUID, status db.JobStatus) error {
	return s.queries.MarkJobFailed(ctx, db.MarkJobFailedParams{ID: id, Status: status})
}

func (s *Store) ListUndecidedForEvent(ctx context.Context, eventID uuid.UUID) ([]int64, error) {
	return s.queries.ListUndecidedForEvent(ctx, eventID)
}

func (s *Store) ListAttendingForEvent(ctx context.Context, eventID uuid.UUID) ([]AttendingSignup, error) {
	rows, err := s.queries.ListAttendingForEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]AttendingSignup, len(rows))
	for i, row := range rows {
		out[i] = AttendingSignup{DiscordID: row.DiscordID, AssignedRole: row.AssignedRole}
	}
	return out, nil
}
