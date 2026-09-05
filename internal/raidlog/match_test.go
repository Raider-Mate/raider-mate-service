package raidlog

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

func character(name, realm string) RosterCharacter {
	return RosterCharacter{ID: uuid.New(), Name: name, Realm: realm}
}

func actor(name, server string) warcraftlogs.Actor {
	return warcraftlogs.Actor{Name: name, Server: server}
}

func TestMatch(t *testing.T) {
	runeweaver := character("Runeweaver", "twisting-nether")
	marecage := character("Soothwyrm", "marecage-de-zangar")

	tests := []struct {
		name  string
		actor warcraftlogs.Actor
		want  *RosterCharacter
	}{
		{
			name:  "exact name and realm",
			actor: actor("Runeweaver", "Twisting Nether"),
			want:  &runeweaver,
		},
		{
			// Blizzard's slugs strip diacritics rather than dropping the letter. If the
			// two sides do not agree on that, a French guild matches nobody.
			name:  "accented server against a slugged realm",
			actor: actor("Soothwyrm", "Marécage de Zangar"),
			want:  &marecage,
		},
		{
			name:  "case-insensitive name",
			actor: actor("RUNEWEAVER", "Twisting Nether"),
			want:  &runeweaver,
		},
		{
			// The log did not say, or said a realm the roster does not carry. A unique
			// name is still an answer.
			name:  "empty server with a unique name",
			actor: actor("Runeweaver", ""),
			want:  &runeweaver,
		},
		{
			name:  "unknown realm falls back to a unique name",
			actor: actor("Runeweaver", "Some Other Realm"),
			want:  &runeweaver,
		},
		{
			name:  "nobody on the roster",
			actor: actor("Pugsley", "Ravencrest"),
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match([]warcraftlogs.Actor{tt.actor}, []RosterCharacter{runeweaver, marecage})
			if len(got) != 1 {
				t.Fatalf("matched %d actors, want 1", len(got))
			}
			switch {
			case tt.want == nil && got[0].CharacterID != nil:
				t.Errorf("matched %v, want no character", *got[0].CharacterID)
			case tt.want != nil && got[0].CharacterID == nil:
				t.Errorf("matched nothing, want %v", tt.want.Name)
			case tt.want != nil && *got[0].CharacterID != tt.want.ID:
				t.Errorf("matched the wrong character")
			}
		})
	}
}

// Attributing a night's damage to the wrong person is worse than attributing it to
// nobody, so an ambiguous name is refused rather than guessed at.
func TestMatchRefusesAnAmbiguousName(t *testing.T) {
	one := character("Thrall", "twisting-nether")
	two := character("Thrall", "ravencrest")

	got := Match([]warcraftlogs.Actor{actor("Thrall", "")}, []RosterCharacter{one, two})
	if got[0].CharacterID != nil {
		t.Errorf("matched %v, want nothing: two characters answer to that name", *got[0].CharacterID)
	}
}

// The realm is what tells them apart, and it still does when there are two.
func TestMatchUsesTheRealmToDisambiguate(t *testing.T) {
	one := character("Thrall", "twisting-nether")
	two := character("Thrall", "ravencrest")

	got := Match([]warcraftlogs.Actor{actor("Thrall", "Ravencrest")}, []RosterCharacter{one, two})
	if got[0].CharacterID == nil || *got[0].CharacterID != two.ID {
		t.Error("did not match the Thrall on the realm the log named")
	}
}

func TestReckon(t *testing.T) {
	present := character("Runeweaver", "twisting-nether")
	absent := character("Gravechill", "twisting-nether")

	matched := Match(
		[]warcraftlogs.Actor{actor("Runeweaver", "Twisting Nether"), actor("Pugsley", "Ravencrest")},
		[]RosterCharacter{present, absent},
	)

	turnout := Reckon(matched, []uuid.UUID{present.ID, absent.ID})

	if len(turnout.Attended) != 1 || turnout.Attended[0] != present.ID {
		t.Errorf("attended = %v, want just the raider in the log", turnout.Attended)
	}
	if len(turnout.Missing) != 1 || turnout.Missing[0] != absent.ID {
		t.Errorf("missing = %v, want the raider who said yes and did not appear", turnout.Missing)
	}
	if turnout.Unknown != 1 {
		t.Errorf("unknown = %d, want the one pug", turnout.Unknown)
	}
	if turnout.Overlap != 0.5 {
		t.Errorf("overlap = %v, want 0.5", turnout.Overlap)
	}
}

// A tentative signup never reaches expected, so this asserts what Reckon does with the
// list it is given: nobody outside it is ever reported missing.
func TestReckonReportsOnlyWhoWasExpected(t *testing.T) {
	expected := character("Runeweaver", "twisting-nether")
	maybe := character("Gravechill", "twisting-nether")

	matched := Match([]warcraftlogs.Actor{actor("Nobody", "Ravencrest")}, []RosterCharacter{expected, maybe})
	turnout := Reckon(matched, []uuid.UUID{expected.ID})

	if len(turnout.Missing) != 1 || turnout.Missing[0] != expected.ID {
		t.Errorf("missing = %v, want only the raider who was expected", turnout.Missing)
	}
}

// A report pasted onto the wrong event shows up as almost everybody unknown, which says
// "wrong log" better than a rejection would.
func TestReckonOverlapIsZeroOnAForeignReport(t *testing.T) {
	ours := character("Runeweaver", "twisting-nether")

	matched := Match(
		[]warcraftlogs.Actor{actor("Stranger", "Ravencrest"), actor("Another", "Ravencrest")},
		[]RosterCharacter{ours},
	)
	turnout := Reckon(matched, []uuid.UUID{ours.ID})

	if turnout.Overlap != 0 {
		t.Errorf("overlap = %v, want 0", turnout.Overlap)
	}
	if len(turnout.Attended) != 0 {
		t.Errorf("attended = %v, want nobody", turnout.Attended)
	}
}

// An empty log is not a divide by zero.
func TestReckonHandlesAnEmptyLog(t *testing.T) {
	turnout := Reckon(nil, nil)
	if turnout.Overlap != 0 || turnout.Unknown != 0 || len(turnout.Attended) != 0 {
		t.Errorf("turnout = %+v, want an empty one", turnout)
	}
}

// One raider on two rows of the log is one person who turned up.
func TestReckonCountsARaiderOnce(t *testing.T) {
	raider := character("Runeweaver", "twisting-nether")

	matched := Match(
		[]warcraftlogs.Actor{actor("Runeweaver", "Twisting Nether"), actor("Runeweaver", "Twisting Nether")},
		[]RosterCharacter{raider},
	)
	turnout := Reckon(matched, []uuid.UUID{raider.ID})

	if len(turnout.Attended) != 1 {
		t.Errorf("attended = %v, want one entry", turnout.Attended)
	}
}
