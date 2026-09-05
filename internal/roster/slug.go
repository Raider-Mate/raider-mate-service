package roster

import (
	"strings"
)

// deaccent maps the accented Latin letters that appear in WoW realm names to their
// unaccented form. Blizzard's realm slugs strip diacritics rather than dropping the
// letter, so "Marécage de Zangar" is "marecage-de-zangar" and not "mar-cage-de-zangar".
// A table beats pulling in golang.org/x/text/unicode/norm for twenty-seven runes, and
// migration 00004 carries the same pairs as a Postgres translate().
var deaccent = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y', 'ÿ': 'y',
}

// RealmSlug turns a realm as a raider typed it into the slug Raider.IO's API
// expects: "Twisting Nether" becomes "twisting-nether", "Kil'jaeden" becomes
// "kiljaeden", "Aggra (Português)" becomes "aggra-portugues". Anything that is not a
// letter or digit collapses to a single hyphen, except the apostrophe, which
// disappears entirely rather than splitting the word.
//
// Unlike the JVM habit of validating on the way out, this normalises on the way in:
// the canonical form is what gets stored, so the unique index also stops the same
// character being registered twice under two spellings of its realm.
//
// Exported because internal/raidlog has to put a WarcraftLogs actor's server through
// exactly this transformation to match it against a stored realm. Two copies would
// drift, and the drift would be invisible: a French guild would simply stop matching.
func RealmSlug(realm string) string {
	var b strings.Builder
	b.Grow(len(realm))

	pendingHyphen := false
	for _, r := range strings.ToLower(realm) {
		if r == '\'' || r == '’' {
			continue
		}
		if folded, ok := deaccent[r]; ok {
			r = folded
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			// Held back rather than written eagerly, so a trailing run of punctuation
			// never leaves a hyphen on the end.
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
			continue
		}
		pendingHyphen = true
	}

	return b.String()
}

// normalizeRegion lowercases the region. Raider.IO rejects "EU" with a 400, and the
// syncer cannot tell that apart from a transient failure, so a raider who typed their
// region in caps would never sync at all.
func normalizeRegion(region string) string {
	return strings.ToLower(strings.TrimSpace(region))
}
