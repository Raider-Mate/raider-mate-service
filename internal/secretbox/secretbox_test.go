package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const testKey = "8Xy2wMbQ0lJ4nR7vTgKfC1dZs5hUeA3pYo6iL9BxNqE="

func newBox(t *testing.T) *Box {
	t.Helper()
	box, err := New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return box
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	box := newBox(t)
	const secret = "oG2IlSfEjHKx2OTwVg8D6Hp9YTUlxzQZTU5OLIdt"

	sealed, err := box.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("the sealed bytes contain the plaintext")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != secret {
		t.Errorf("Open = %q, want the secret back", opened)
	}
}

// A fixed nonce would leak that two guilds pasted the same key, and worse with GCM.
func TestSealUsesARandomNonce(t *testing.T) {
	box := newBox(t)

	first, err := box.Seal("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := box.Seal("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("two seals of the same plaintext produced the same bytes")
	}
}

func TestOpenRejectsTheWrongKey(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Open(sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

func TestOpenRejectsATamperedValue(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := box.Open(tampered); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

func TestOpenRejectsATruncatedValue(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := box.Open(sealed[:4]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
	if _, err := box.Open(nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err on empty = %v, want ErrCorrupt", err)
	}
}

// An instance that never intends to hold a guild's key is a supported configuration.
// What it must never do is store one anyway.
func TestNoKeyIsConfiguredButRefusesToSeal(t *testing.T) {
	box, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if box.Configured() {
		t.Error("Configured = true with no key")
	}
	if _, err := box.Seal("secret"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Seal err = %v, want ErrNoKey", err)
	}
	if _, err := box.Open([]byte("whatever")); !errors.Is(err, ErrNoKey) {
		t.Errorf("Open err = %v, want ErrNoKey", err)
	}
}

// Non-empty but wrong is a typo, and a typo is a startup error rather than a silent
// downgrade to no encryption.
func TestNewRejectsABadKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want error
	}{
		{"too short", base64.StdEncoding.EncodeToString([]byte("short")), ErrKeyLength},
		{"too long", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)), ErrKeyLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.key); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}

	if _, err := New("not base64 at all!!"); err == nil {
		t.Error("New accepted a key that is not base64")
	}
}

// The key never belongs in an error, because errors get logged.
func TestErrorsDoNotCarryTheKey(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0x01

	_, err = box.Open(tampered)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "secret") {
		t.Errorf("error carries the secret or the key: %v", err)
	}
}
