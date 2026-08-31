package roster

import (
	"testing"

	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
)

func item(slot string, itemID, enchant int64) raiderio.GearItem {
	return raiderio.GearItem{Slot: slot, ItemID: itemID, ItemLevel: 600, EnchantID: enchant}
}

func TestEnchantCompliance(t *testing.T) {
	tests := []struct {
		name              string
		gear              []raiderio.GearItem
		missing, expected int
	}{
		{
			name: "counts only enchantable slots",
			gear: []raiderio.GearItem{
				item("head", 1, 0),
				item("neck", 2, 0),
				item("back", 3, 500),
				item("chest", 4, 501),
			},
			missing: 0, expected: 2,
		},
		{
			name: "enchant 0 is a bare slot, not an absent one",
			gear: []raiderio.GearItem{
				item("back", 3, 0),
				item("chest", 4, 501),
				item("feet", 5, 0),
			},
			missing: 2, expected: 3,
		},
		{
			// A two-handed spec has no off-hand and is not one enchant short for it.
			name: "offhand never counts",
			gear: []raiderio.GearItem{
				item("mainhand", 6, 502),
				item("offhand", 7, 0),
			},
			missing: 0, expected: 1,
		},
		{
			// An empty ring slot is a raider who has not replaced a ring, not a raider
			// who failed to enchant one.
			name: "an unequipped slot is not counted against anyone",
			gear: []raiderio.GearItem{
				item("finger1", 8, 503),
			},
			missing: 0, expected: 1,
		},
		{
			name:    "no gear establishes nothing",
			gear:    nil,
			missing: 0, expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missing, expected := EnchantCompliance(tt.gear)
			if missing != tt.missing || expected != tt.expected {
				t.Errorf("EnchantCompliance() = (%d, %d), want (%d, %d)",
					missing, expected, tt.missing, tt.expected)
			}
		})
	}
}

func TestTierPieces(t *testing.T) {
	rules := GearRules{TierSetItemIDs: map[int64]struct{}{101: {}, 102: {}, 103: {}}}

	gear := []raiderio.GearItem{
		item("head", 101, 0),
		item("shoulder", 102, 0),
		item("chest", 999, 0),
	}
	pieces, ok := rules.TierPieces(gear)
	if !ok {
		t.Fatal("TierPieces reported no configured season, want one")
	}
	if pieces != 2 {
		t.Errorf("pieces = %d, want 2", pieces)
	}
}

func TestTierPiecesWithoutAConfiguredSeason(t *testing.T) {
	// Zero pieces and "we do not know" are different answers, and the API renders them
	// differently: an absent field against a raider wearing none of the set.
	var rules GearRules
	if _, ok := rules.TierPieces([]raiderio.GearItem{item("head", 101, 0)}); ok {
		t.Error("TierPieces reported a season, want none configured")
	}
}

func TestCurrentRaid(t *testing.T) {
	rules := GearRules{CurrentRaidSlug: "liberation-of-undermine"}
	progression := []raiderio.RaidProgress{
		{Slug: "nerubar-palace", Bosses: 8, NormalKilled: 8},
		{Slug: "liberation-of-undermine", Bosses: 8, NormalKilled: 8, HeroicKilled: 6, MythicKilled: 2},
	}

	raid, ok := rules.CurrentRaid(progression)
	if !ok {
		t.Fatal("CurrentRaid found nothing, want the configured raid")
	}
	if raid.Slug != "liberation-of-undermine" || raid.HeroicKilled != 6 {
		t.Errorf("raid = %+v, want liberation-of-undermine with 6 heroic", raid)
	}
}

func TestCurrentRaidNotEntered(t *testing.T) {
	// Raider.IO omits a raid nobody has set foot in. Reporting that as all-zero would
	// claim the raider walked in and killed nothing.
	rules := GearRules{CurrentRaidSlug: "liberation-of-undermine"}
	progression := []raiderio.RaidProgress{{Slug: "nerubar-palace", Bosses: 8, NormalKilled: 8}}

	if _, ok := rules.CurrentRaid(progression); ok {
		t.Error("CurrentRaid found a raid the character has never entered")
	}
}

func TestCurrentRaidNotConfigured(t *testing.T) {
	var rules GearRules
	progression := []raiderio.RaidProgress{{Slug: "nerubar-palace", Bosses: 8}}
	if _, ok := rules.CurrentRaid(progression); ok {
		t.Error("CurrentRaid answered with no raid configured")
	}
}
