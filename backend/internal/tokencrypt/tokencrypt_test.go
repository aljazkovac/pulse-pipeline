package tokencrypt_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/tokencrypt"
)

// Fixed, obviously fake test keys.
var (
	key1 = bytes.Repeat([]byte{0x11}, tokencrypt.KeySize)
	key2 = bytes.Repeat([]byte{0x22}, tokencrypt.KeySize)
	aad  = []byte("provider_connections/fake-conn/fake-user/access_token")
)

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func mustKeyring(t *testing.T, current int16, keys map[int16][]byte) *tokencrypt.Keyring {
	t.Helper()
	k, err := tokencrypt.NewKeyring(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen_RoundTrip(t *testing.T) {
	k := mustKeyring(t, 1, map[int16][]byte{1: key1})
	for _, plain := range []string{"fake-access-token", "", strings.Repeat("x", 4096)} {
		sealed, v, err := k.Seal([]byte(plain), aad)
		if err != nil {
			t.Fatal(err)
		}
		if v != 1 {
			t.Errorf("version = %d, want 1", v)
		}
		if plain != "" && bytes.Contains(sealed, []byte(plain)) {
			t.Error("sealed value contains the plaintext")
		}
		got, err := k.Open(v, sealed, aad)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != plain {
			t.Errorf("Open = %q, want %q", got, plain)
		}
	}
}

func TestSeal_FreshNonceEachTime(t *testing.T) {
	k := mustKeyring(t, 1, map[int16][]byte{1: key1})
	a, _, _ := k.Seal([]byte("fake-token"), aad)
	b, _, _ := k.Seal([]byte("fake-token"), aad)
	if bytes.Equal(a, b) {
		t.Fatal("sealing the same plaintext twice gave identical output; nonce reused")
	}
	if len(a) != 12+len("fake-token")+16 {
		t.Errorf("sealed length = %d, want nonce(12)+plaintext+tag(16)", len(a))
	}
}

func TestOpen_WrongKey(t *testing.T) {
	sealed, v, _ := mustKeyring(t, 1, map[int16][]byte{1: key1}).Seal([]byte("fake-token"), aad)
	other := mustKeyring(t, 1, map[int16][]byte{1: key2})
	if _, err := other.Open(v, sealed, aad); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("Open with wrong key: err = %v, want ErrDecrypt", err)
	}
}

func TestOpen_Tampering(t *testing.T) {
	k := mustKeyring(t, 1, map[int16][]byte{1: key1})
	sealed, v, _ := k.Seal([]byte("fake-token"), aad)
	for i := range sealed {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0x01
		if _, err := k.Open(v, bad, aad); !errors.Is(err, tokencrypt.ErrDecrypt) {
			t.Fatalf("flipping byte %d: err = %v, want ErrDecrypt", i, err)
		}
	}
	if _, err := k.Open(v, sealed[:10], aad); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("truncated: err = %v, want ErrDecrypt", err)
	}
	if _, err := k.Open(v, sealed, []byte("provider_connections/other-conn/fake-user/access_token")); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("different associated data: err = %v, want ErrDecrypt", err)
	}
}

func TestKeyVersion_Rotation(t *testing.T) {
	old := mustKeyring(t, 1, map[int16][]byte{1: key1})
	sealedV1, v1, _ := old.Seal([]byte("fake-old-token"), aad)

	rotated := mustKeyring(t, 2, map[int16][]byte{1: key1, 2: key2})
	got, err := rotated.Open(v1, sealedV1, aad)
	if err != nil || string(got) != "fake-old-token" {
		t.Fatalf("opening v1 after rotation = %q, %v", got, err)
	}
	sealedV2, v2, _ := rotated.Seal([]byte("fake-new-token"), aad)
	if v2 != 2 {
		t.Fatalf("new values sealed with version %d, want 2", v2)
	}
	// The version selects the key: v2 ciphertext under v1's key must fail.
	if _, err := rotated.Open(1, sealedV2, aad); !errors.Is(err, tokencrypt.ErrDecrypt) {
		t.Fatalf("v2 value opened as v1: err = %v, want ErrDecrypt", err)
	}

	retired := mustKeyring(t, 2, map[int16][]byte{2: key2})
	if _, err := retired.Open(v1, sealedV1, aad); !errors.Is(err, tokencrypt.ErrUnknownKeyVersion) {
		t.Fatalf("opening v1 after retiring its key: err = %v, want ErrUnknownKeyVersion", err)
	}
}

func TestNewKeyring_Validation(t *testing.T) {
	for name, tc := range map[string]struct {
		current int16
		keys    map[int16][]byte
	}{
		"current missing":  {2, map[int16][]byte{1: key1}},
		"short key":        {1, map[int16][]byte{1: key1[:16]}},
		"zero version":     {1, map[int16][]byte{1: key1, 0: key2}},
		"negative version": {1, map[int16][]byte{1: key1, -1: key2}},
	} {
		if _, err := tokencrypt.NewKeyring(tc.current, tc.keys); err == nil {
			t.Errorf("%s: NewKeyring succeeded, want error", name)
		}
	}
}

func TestFromEnv(t *testing.T) {
	k, err := tokencrypt.FromEnv(b64(key2), "2", " 1:"+b64(key1)+" ")
	if err != nil {
		t.Fatal(err)
	}
	if k.CurrentVersion() != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", k.CurrentVersion())
	}
	sealedV1, _, _ := mustKeyring(t, 1, map[int16][]byte{1: key1}).Seal([]byte("fake-token"), aad)
	if _, err := k.Open(1, sealedV1, aad); err != nil {
		t.Fatalf("previous key not usable: %v", err)
	}

	def, err := tokencrypt.FromEnv(b64(key1), "", "")
	if err != nil || def.CurrentVersion() != 1 {
		t.Fatalf("defaults: version %v, err %v", def, err)
	}

	secret := b64(key1)
	for name, tc := range map[string][3]string{
		"missing key":         {"", "", ""},
		"not base64":          {"not-base64!!", "", ""},
		"wrong length":        {b64(key1[:31]), "", ""},
		"bad version":         {secret, "zero", ""},
		"zero version":        {secret, "0", ""},
		"bad previous entry":  {secret, "2", secret},
		"previous short key":  {secret, "2", "1:" + b64(key1[:8])},
		"duplicate version":   {secret, "1", "1:" + b64(key2)},
		"previous bad base64": {secret, "2", "1:%%%"},
	} {
		_, err := tokencrypt.FromEnv(tc[0], tc[1], tc[2])
		if err == nil {
			t.Errorf("%s: FromEnv succeeded, want error", name)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), b64(key2)) {
			t.Errorf("%s: error leaks key material: %v", name, err)
		}
	}
}
