package roster

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// Character is a roster character, translated out of pgtype.
type Character struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Realm      string
	Region     string
	Class      *string
	Spec       *string
	Ilvl       *float64
	MplusScore *float64
	IsMain     bool
	Synced     bool

	// The three gear facts the sync derives. Nil throughout means "not established":
	// an unsynced character, or a season whose rules the worker has not been given.
	// Never confuse one with zero, in either direction.
	EnchantsMissing  *int
	EnchantsExpected *int
	TierPieces       *int
	// Progression in the raid the worker tracks, nil until it has been there.
	Progression *RaidProgress
}

// RaidProgress is a character's kill count in one raid, carrying the raid's slug so a
// client never has to guess which tier the numbers describe.
type RaidProgress struct {
	Slug         string
	Bosses       int
	NormalKilled int
	HeroicKilled int
	MythicKilled int
}

// RoleChoice is one role a character can play, in signup-menu priority order.
type RoleChoice struct {
	Role     db.RoleEnum
	Priority int16
}

// RegisterInput is what registering a character needs.
type RegisterInput struct {
	DiscordID      int64
	DiscordGuildID int64
	Name           string
	Realm          string
	Region         string
	IsMain         bool
}

// characterStore is the persistence Characters needs. Declared here, by the
// consumer.
type characterStore interface {
	RegisterCharacter(ctx context.Context, in RegisterInput) (Character, error)
	GetCharacterInGuild(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (Character, error)
	GetCharacterOwner(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (int64, error)
	DeleteCharacter(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (bool, error)
	SetCharacterMain(ctx context.Context, characterID uuid.UUID, discordGuildID int64, isMain bool) (bool, error)
	ListCharactersInGuild(ctx context.Context, discordGuildID int64) ([]Character, error)
	ListCharactersByDiscord(ctx context.Context, discordID, discordGuildID int64) ([]Character, error)
	ReplaceCharacterRoles(ctx context.Context, characterID uuid.UUID, roles []RoleChoice) error
	ListCharacterRoles(ctx context.Context, characterID uuid.UUID) ([]RoleChoice, error)
	ListRolesForCharacters(ctx context.Context, characterIDs []uuid.UUID) (map[uuid.UUID][]RoleChoice, error)
	ListGuildsForDiscordUser(ctx context.Context, discordID int64) ([]int64, error)
}

// Characters registers characters and manages their role menus. Roles live here,
// on the character, never on a signup (hard rule 2): the ephemeral role select in
// design.md section 4 is this write, entirely separate from the signup write it
// precedes.
type Characters struct {
	store characterStore
}

// NewCharacters builds a Characters.
func NewCharacters(store characterStore) *Characters {
	return &Characters{store: store}
}

// Register creates a character, upserting the owning user first. A new character
// has no ilvl until the next worker sync tick (hard rule 5 forbids calling
// Raider.IO from this request handler); Character.Synced reports that honestly.
//
// RegisterInput.IsMain is a request, not an instruction. It is granted only while the
// raider has no main yet, so registering an alt never takes the flag off the character
// that holds it. Moving it is SetMain. Registering the same name, realm and region
// twice returns ErrCharacterExists rather than a constraint error.
//
// Realm and region are canonicalised to the form Raider.IO's API takes before they are
// stored. A realm typed as a raider sees it in game ("Twisting Nether") is not a slug,
// and the syncer's fetch would fail on it forever without ever clearing last_synced.
func (c *Characters) Register(ctx context.Context, in RegisterInput) (Character, error) {
	in.Realm = slugifyRealm(in.Realm)
	in.Region = normalizeRegion(in.Region)

	character, err := c.store.RegisterCharacter(ctx, in)
	if err != nil {
		if errors.Is(err, ErrCharacterExists) {
			return Character{}, err
		}
		return Character{}, fmt.Errorf("registering character: %w", err)
	}
	return character, nil
}

// GetInGuild loads a character, scoped to a guild so a caller cannot fetch someone
// else's character through a foreign guild id.
func (c *Characters) GetInGuild(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (Character, error) {
	character, err := c.store.GetCharacterInGuild(ctx, characterID, discordGuildID)
	if err != nil {
		return Character{}, fmt.Errorf("loading character: %w", err)
	}
	return character, nil
}

// InGuild reports whether characterID exists in discordGuildID at all, regardless of
// who owns it. Ownership answers "is this mine"; this answers "is this ours", which
// is the question for a raid lead acting on someone else's character.
func (c *Characters) InGuild(ctx context.Context, characterID uuid.UUID, discordGuildID int64) (bool, error) {
	_, err := c.store.GetCharacterInGuild(ctx, characterID, discordGuildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading character: %w", err)
	}
	return true, nil
}

// OwnedByDiscord reports whether characterID belongs to discordID within
// discordGuildID. A character that does not exist, or lives in another guild, is
// simply not owned: that is a "no", not an infrastructure failure, and callers turn
// it into a 403 rather than a 500 and a spurious error log.
func (c *Characters) OwnedByDiscord(ctx context.Context, characterID uuid.UUID, discordGuildID, discordID int64) (bool, error) {
	owner, err := c.store.GetCharacterOwner(ctx, characterID, discordGuildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading character owner: %w", err)
	}
	return owner == discordID, nil
}

func (c *Characters) ListForGuild(ctx context.Context, discordGuildID int64) ([]Character, error) {
	characters, err := c.store.ListCharactersInGuild(ctx, discordGuildID)
	if err != nil {
		return nil, fmt.Errorf("listing characters: %w", err)
	}
	return characters, nil
}

func (c *Characters) ListForUser(ctx context.Context, discordID, discordGuildID int64) ([]Character, error) {
	characters, err := c.store.ListCharactersByDiscord(ctx, discordID, discordGuildID)
	if err != nil {
		return nil, fmt.Errorf("listing characters: %w", err)
	}
	return characters, nil
}

// ListGuildsForUser returns the guilds Raider Mate knows this person in, soonest use
// first being meaningless here, so ordered by id for a stable answer.
//
// Deliberately not scoped to a guild: it is the one question that cannot be, because it
// is asked precisely to find out which guild to work in. Callers must confirm the
// requester is asking about themselves.
func (c *Characters) ListGuildsForUser(ctx context.Context, discordID int64) ([]int64, error) {
	guilds, err := c.store.ListGuildsForDiscordUser(ctx, discordID)
	if err != nil {
		return nil, fmt.Errorf("listing guilds for user: %w", err)
	}
	return guilds, nil
}

// ErrCharacterNotFound means no such character exists in that guild. Whether it
// never existed or lives in another guild is deliberately not distinguished.
var ErrCharacterNotFound = errors.New("character not found")

// ErrCharacterExists means this raider already registered that name, realm and region.
// A caller retyping a character they already have is a mistake worth naming, not an
// infrastructure failure: the handler turns it into a 409 the raider can act on.
var ErrCharacterExists = errors.New("character already registered")

// Delete removes a character. character_roles, signups, comp_slots, snapshots and
// late requests all carry ON DELETE CASCADE, so this takes the raider's history with
// it: it is for a mistyped registration, not for a raider leaving.
func (c *Characters) Delete(ctx context.Context, characterID uuid.UUID, discordGuildID int64) error {
	deleted, err := c.store.DeleteCharacter(ctx, characterID, discordGuildID)
	if err != nil {
		return fmt.Errorf("deleting character: %w", err)
	}
	if !deleted {
		return ErrCharacterNotFound
	}
	return nil
}

// SetMain moves the main flag, and is how a raider switches mains from the dashboard.
// One main per raider is enforced by characters_one_main_per_user, so promoting demotes
// whoever held it; a raider with no main at all is still fine, since the index is
// partial. Registration cannot move the flag (see Register), which leaves this the only
// way it changes hands.
func (c *Characters) SetMain(ctx context.Context, characterID uuid.UUID, discordGuildID int64, isMain bool) error {
	updated, err := c.store.SetCharacterMain(ctx, characterID, discordGuildID, isMain)
	if err != nil {
		return fmt.Errorf("setting character main: %w", err)
	}
	if !updated {
		return ErrCharacterNotFound
	}
	return nil
}

// SetRoles replaces a character's whole role menu.
func (c *Characters) SetRoles(ctx context.Context, characterID uuid.UUID, roles []RoleChoice) error {
	if err := c.store.ReplaceCharacterRoles(ctx, characterID, roles); err != nil {
		return fmt.Errorf("setting roles: %w", err)
	}
	return nil
}

func (c *Characters) ListRoles(ctx context.Context, characterID uuid.UUID) ([]RoleChoice, error) {
	roles, err := c.store.ListCharacterRoles(ctx, characterID)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	return roles, nil
}

// ListRolesForMany reads the role menus of a whole roster in one query. A character
// with no menu is absent from the map rather than present with an empty slice.
func (c *Characters) ListRolesForMany(ctx context.Context, characterIDs []uuid.UUID) (map[uuid.UUID][]RoleChoice, error) {
	if len(characterIDs) == 0 {
		return map[uuid.UUID][]RoleChoice{}, nil
	}
	roles, err := c.store.ListRolesForCharacters(ctx, characterIDs)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	return roles, nil
}
