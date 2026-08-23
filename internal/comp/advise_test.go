package comp

import (
	"strings"
	"testing"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// boardOf builds a board with count seated raiders in role, all non-bench.
func boardOf(role db.RoleEnum, count int) []Assignment {
	slots := make([]Assignment, count)
	for i := range slots {
		slots[i] = Assignment{CharacterID: charID(byte(i + 1)), Role: role, SlotIndex: int16(i)}
	}
	return slots
}

func messages(advisories []Advisory) string {
	parts := make([]string, len(advisories))
	for i, a := range advisories {
		parts[i] = a.Message
	}
	return strings.Join(parts, " | ")
}

// The case docs/design.md names: a hand-built board a healer short. The assigner never
// ran on it, so this is the only way that sentence reaches a raid lead.
func TestAdviseReportsTheGapBetweenTheBoardAndTheTemplate(t *testing.T) {
	var slots []Assignment
	slots = append(slots, boardOf(db.RoleEnumTANK, 2)...)
	slots = append(slots, boardOf(db.RoleEnumHEALER, 1)...)
	slots = append(slots, boardOf(db.RoleEnumMDPS, 17)...)

	got := Advise(slots, Template{Tanks: intPtr(2), Healers: intPtr(4)}, ModeRaidMythic)

	if !strings.Contains(messages(got), "HEALER: 1 seated, the comp asks for 4") {
		t.Errorf("advisories = %q, want the healer gap named", messages(got))
	}
	if strings.Contains(messages(got), "TANK:") {
		t.Errorf("advisories = %q, want nothing about tanks: the board fills them", messages(got))
	}
}

// Melee and ranged are separate roles and one group, because the template caps each
// side while the count a comp needs is a single DPS number.
func TestAdviseCountsMeleeAndRangedAsOneGroup(t *testing.T) {
	var slots []Assignment
	slots = append(slots, boardOf(db.RoleEnumTANK, 2)...)
	slots = append(slots, boardOf(db.RoleEnumHEALER, 4)...)
	slots = append(slots, boardOf(db.RoleEnumMDPS, 7)...)
	slots = append(slots, boardOf(db.RoleEnumRDPS, 7)...)

	if got := Advise(slots, Template{}, ModeRaidMythic); messages(got) != "" {
		t.Errorf("advisories = %q, want none: 14 DPS fills a 20-raider comp", messages(got))
	}
}

// The bench is on the board but not in the raid. It counts toward turnout, which is
// what the comp is sized from, and never toward a filled seat.
func TestAdviseCountsTheBenchAsTurnoutAndNotAsASeat(t *testing.T) {
	var slots []Assignment
	slots = append(slots, boardOf(db.RoleEnumTANK, 2)...)
	slots = append(slots, boardOf(db.RoleEnumHEALER, 4)...)
	slots = append(slots, boardOf(db.RoleEnumMDPS, 14)...)
	benched := Assignment{CharacterID: charID(200), Role: db.RoleEnumHEALER, IsBench: true}

	seated := Advise(slots, Template{}, ModeRaidMythic)
	withBench := Advise(append(slots, benched), Template{}, ModeRaidMythic)

	if messages(seated) != "" {
		t.Fatalf("advisories = %q, want none before the bench row", messages(seated))
	}
	if messages(withBench) != "" {
		t.Errorf("advisories = %q, want none: a benched healer is not a missing seat", messages(withBench))
	}
}

// A comp row exists before anything is locked into it. Resolving a template against a
// turnout of zero would answer with the minimum raid size and call every seat missing,
// which is a screen full of advisories about a board nobody has built.
func TestAdviseSaysNothingAboutAnEmptyBoard(t *testing.T) {
	if got := Advise(nil, Template{Tanks: intPtr(2)}, ModeRaidFlex); got != nil {
		t.Errorf("advisories = %q, want none for a board with no slots", messages(got))
	}
}

// Template departures are the assigner's own wording, from Resolve, so a board and the
// lock that built it say the same thing about the same override.
func TestAdviseCarriesTheTemplateAdvisoriesResolveProduces(t *testing.T) {
	slots := boardOf(db.RoleEnumTANK, 5)

	got := Advise(slots, Template{Tanks: intPtr(5)}, ModeMythicPlus)
	_, want := Template{Tanks: intPtr(5)}.Resolve(ModeMythicPlus, len(slots))

	if len(want) == 0 {
		t.Fatal("Resolve produced no advisory for a five-tank Mythic+ template, test proves nothing")
	}
	if !strings.Contains(messages(got), want[0].Message) {
		t.Errorf("advisories = %q, want them to carry %q", messages(got), want[0].Message)
	}
}
