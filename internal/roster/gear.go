package roster

import (
	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
)

// GearRules is the game data that moves with a patch. The syncer holds one, built once
// at startup, because none of it can be derived from a Raider.IO response: the payload
// says an item sits in a slot, never that the slot ought to carry an enchant or that
// the item belongs to this season's class set.
type GearRules struct {
	// TierSetItemIDs is every item id in the current season's class sets, across all
	// classes. A tier item id is unique to one class and one slot, so a flat set needs
	// no per-class breakdown. Empty means no season is configured, and tier counting
	// answers "not established" rather than zero.
	TierSetItemIDs map[int64]struct{}
	// CurrentRaidSlug is Raider.IO's slug for the raid the roster is measured on.
	// Empty means progression is not tracked.
	CurrentRaidSlug string
}

// enchantableSlots are the slots that take a permanent enchant, in Raider.IO's own
// slot vocabulary. Game data, and it moves between expansions: a patch that makes
// bracers enchantable again is an edit here and nowhere else.
//
// offhand is deliberately out. It holds a shield for one spec and a second weapon for
// another, and counting it would report every two-handed spec as permanently one
// enchant short of compliant.
var enchantableSlots = map[string]struct{}{
	"back":     {},
	"chest":    {},
	"wrist":    {},
	"legs":     {},
	"feet":     {},
	"finger1":  {},
	"finger2":  {},
	"mainhand": {},
}

// EnchantCompliance counts the equipped slots that take an enchant, and how many of
// those have none.
//
// Raider.IO reports an unenchanted slot as enchant 0 rather than by leaving the key
// out, so "equipped but bare" and "nothing equipped" stay different facts: only slots
// the character actually fills are counted, which keeps a raider with an empty ring
// slot from reading as non-compliant in it.
func EnchantCompliance(gear []raiderio.GearItem) (missing, expected int) {
	for _, item := range gear {
		if _, ok := enchantableSlots[item.Slot]; !ok {
			continue
		}
		expected++
		if item.EnchantID == 0 {
			missing++
		}
	}
	return missing, expected
}

// TierPieces counts equipped items from the configured season's class sets. The second
// return is false when no season is configured: nothing has been established about
// this raider's tier, which is not the same as their wearing none of it.
func (r GearRules) TierPieces(gear []raiderio.GearItem) (int, bool) {
	if len(r.TierSetItemIDs) == 0 {
		return 0, false
	}

	pieces := 0
	for _, item := range gear {
		if _, ok := r.TierSetItemIDs[item.ItemID]; ok {
			pieces++
		}
	}
	return pieces, true
}

// CurrentRaid picks the configured raid out of a profile's progression.
//
// False when no raid is configured, and false when the character has never zoned into
// the one that is. Raider.IO omits a raid nobody has entered rather than sending it as
// zeros, and inventing those zeros here would report a raider as having walked in and
// killed nothing.
func (r GearRules) CurrentRaid(progression []raiderio.RaidProgress) (raiderio.RaidProgress, bool) {
	if r.CurrentRaidSlug == "" {
		return raiderio.RaidProgress{}, false
	}

	for _, raid := range progression {
		if raid.Slug == r.CurrentRaidSlug {
			return raid, true
		}
	}
	return raiderio.RaidProgress{}, false
}
