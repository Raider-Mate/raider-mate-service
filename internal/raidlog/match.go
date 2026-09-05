package raidlog

import (
	"strings"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/roster"
	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

// RosterCharacter is one character on the guild's roster, as much of it as matching
// needs.
type RosterCharacter struct {
	ID    uuid.UUID
	Name  string
	Realm string
}

// Matched is one WarcraftLogs actor with the roster character it turned out to be, or
// no character at all.
type Matched struct {
	Actor warcraftlogs.Actor
	// CharacterID is nil when nobody on the roster answers to this name. That is not an
	// error and not a hole to fill: it is a pug, a trial who never registered, or the
	// wrong report on the event.
	CharacterID *uuid.UUID
}

// Match pairs the log's actors with the guild's characters.
//
// The rule, in order:
//
//  1. The actor's server through roster.RealmSlug, the same transformation a stored
//     realm went through. Blizzard's slugs strip diacritics, so this has to be one
//     function or a French guild silently matches nobody.
//  2. Name and realm slug together, case-insensitive. Exactly one hit wins.
//  3. Failing that, name alone, and only when exactly one character answers to it. Two
//     raiders called Thrall on different realms is genuinely ambiguous, and attributing
//     a night's damage to the wrong person is worse than attributing it to nobody.
//
// Region is not used: a WarcraftLogs actor carries no region, and the report's region is
// the report's rather than each actor's.
func Match(actors []warcraftlogs.Actor, characters []RosterCharacter) []Matched {
	byNameRealm := map[string][]RosterCharacter{}
	byName := map[string][]RosterCharacter{}

	for _, character := range characters {
		name := strings.ToLower(character.Name)
		realm := roster.RealmSlug(character.Realm)
		byNameRealm[name+"\x00"+realm] = append(byNameRealm[name+"\x00"+realm], character)
		byName[name] = append(byName[name], character)
	}

	matched := make([]Matched, 0, len(actors))
	for _, actor := range actors {
		name := strings.ToLower(actor.Name)
		realm := roster.RealmSlug(actor.Server)

		found := only(byNameRealm[name+"\x00"+realm])
		if found == nil {
			// The log did not say which realm, or said one the roster does not carry.
			// A unique name is still an answer; an ambiguous one is not.
			found = only(byName[name])
		}

		entry := Matched{Actor: actor}
		if found != nil {
			id := found.ID
			entry.CharacterID = &id
		}
		matched = append(matched, entry)
	}

	return matched
}

// only returns the single candidate, or nil where there is none or more than one.
func only(candidates []RosterCharacter) *RosterCharacter {
	if len(candidates) != 1 {
		return nil
	}
	return &candidates[0]
}

// Turnout is the log checked against the signup sheet: who said yes and zoned in, who
// zoned in without being on the roster, and who said yes and never appeared.
//
// This is not attendance validation. It is the answer to "did the twenty people who said
// yes actually zone in", which is the question a raid lead asks on Thursday morning.
type Turnout struct {
	// Attended is roster character ids the log saw.
	Attended []uuid.UUID
	// Missing is character ids that were confirmed on the sheet and are not in the log.
	Missing []uuid.UUID
	// Unknown counts actors in the log with no character on this roster. The actors
	// themselves are stored, so the read side lists them; this is the count the overlap
	// is worked out from.
	Unknown int
	// Overlap is matched actors over total actors, 0 to 1. Near zero on a full raid
	// means the report is not this guild's night, which is the cheapest wrong-log
	// detector there is and better than refusing the report outright.
	Overlap float64
}

// Reckon works out the turnout from a matched log and the characters that were expected.
//
// expected is the characters whose signup says they were coming. Tentative signups are
// deliberately not in it: "maybe" was never a promise, and listing a maybe as missing
// would put somebody on a no-show list for answering honestly.
func Reckon(matched []Matched, expected []uuid.UUID) Turnout {
	turnout := Turnout{}

	seen := map[uuid.UUID]bool{}
	for _, entry := range matched {
		if entry.CharacterID == nil {
			turnout.Unknown++
			continue
		}
		if !seen[*entry.CharacterID] {
			seen[*entry.CharacterID] = true
			turnout.Attended = append(turnout.Attended, *entry.CharacterID)
		}
	}

	for _, id := range expected {
		if !seen[id] {
			turnout.Missing = append(turnout.Missing, id)
		}
	}

	if len(matched) > 0 {
		turnout.Overlap = float64(len(matched)-turnout.Unknown) / float64(len(matched))
	}

	return turnout
}
