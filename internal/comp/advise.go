package comp

import (
	"fmt"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// Advise is what a board says about itself: the template's own departures from the
// suggestion, and every group the slots leave short.
//
// Derived on read rather than stored at lock time, for two reasons. A stored advisory
// describes the pool the assigner saw, and the board it was printed beside can be
// hand-edited afterwards, so the two drift apart with nothing saying which is current.
// And a MANUAL comp never runs the assigner at all, so a stored advisory would be the
// one kind of board that never has any, when "HEALER: 1 seated, the comp asks for 4" is
// exactly what a raid lead wants to see before pulling a hand-built raid.
//
// Turnout is the whole board, bench included, which is the pool the last lock placed.
//
// Advisories are information, never errors, and never a reason to refuse a save.
func Advise(slots []Assignment, tmpl Template, mode Mode) []Advisory {
	// A comp row with no slots has never been locked or saved. It has nothing to say
	// about itself yet, and resolving a template against a turnout of zero would answer
	// with the minimum raid size and call every seat missing.
	if len(slots) == 0 {
		return nil
	}

	needs, advisories := tmpl.Resolve(mode, len(slots))

	seated := make(map[string]int, 3)
	for _, slot := range slots {
		// The bench is on the board but not in the raid, so it counts toward turnout
		// and not toward a filled seat.
		if slot.IsBench {
			continue
		}
		seated[groupForRole(slot.Role)]++
	}

	// Deliberately not the assigner's "not enough signups": that sentence is about the
	// pool at lock time, and a seat can be empty here because a raid lead left it that
	// way. This one states the gap without claiming a cause.
	need := map[string]int{groupTank: needs.Tanks, groupHealer: needs.Healers, groupDPS: needs.DPS}
	for _, g := range []string{groupTank, groupHealer, groupDPS} {
		if seated[g] < need[g] {
			advisories = append(advisories, Advisory{
				Role:    roleForGroup(g),
				Message: fmt.Sprintf("%s: %d seated, the comp asks for %d", g, seated[g], need[g]),
			})
		}
	}

	return advisories
}

// groupForRole buckets a slot's role the way the assigner's needs are counted. Melee
// and ranged are separate roles but one group: the template caps each side, while the
// count a comp needs is a single DPS number.
func groupForRole(role db.RoleEnum) string {
	switch role {
	case db.RoleEnumTANK:
		return groupTank
	case db.RoleEnumHEALER:
		return groupHealer
	default:
		return groupDPS
	}
}
