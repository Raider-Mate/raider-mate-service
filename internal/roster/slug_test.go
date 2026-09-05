package roster

import "testing"

func TestSlugifyRealm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already a slug", "twisting-nether", "twisting-nether"},
		{"as typed in game", "Twisting Nether", "twisting-nether"},
		{"apostrophe closes up", "Kil'jaeden", "kiljaeden"},
		{"typographic apostrophe closes up", "Kil’jaeden", "kiljaeden"},
		{"digits survive", "Area 52", "area-52"},
		{"hyphen already present", "Area-52", "area-52"},
		{"diacritics fold", "Marécage de Zangar", "marecage-de-zangar"},
		{"parenthesised suffix", "Aggra (Português)", "aggra-portugues"},
		{"german umlaut", "Süd Ost", "sud-ost"},
		{"runs collapse to one hyphen", "Der  Rat   von Dalaran", "der-rat-von-dalaran"},
		{"leading and trailing punctuation dropped", "  Blackmoore. ", "blackmoore"},
		{"empty stays empty", "", ""},
		{"punctuation only", "---", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RealmSlug(tt.in); got != tt.want {
				t.Errorf("RealmSlug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Registering the same character twice must still collide on the unique index after
// normalisation, which is the point of storing the canonical form rather than
// slugifying at fetch time.
func TestSlugifyRealmIsIdempotent(t *testing.T) {
	for _, in := range []string{"Twisting Nether", "Kil'jaeden", "Aggra (Português)", "Area-52"} {
		once := RealmSlug(in)
		if twice := RealmSlug(once); twice != once {
			t.Errorf("RealmSlug(%q) not idempotent: %q then %q", in, once, twice)
		}
	}
}

func TestNormalizeRegion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"eu", "eu"},
		{"EU", "eu"},
		{" us ", "us"},
		{"Kr", "kr"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeRegion(tt.in); got != tt.want {
				t.Errorf("normalizeRegion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
