// Package tokencrypt encrypts OAuth tokens at rest with AES-256-GCM
// (ADR-0002 §3, ADR-0004 §4).
//
// A sealed value is nonce‖ciphertext‖tag. Every key carries a version stored
// next to the ciphertext (provider_connections.token_key_version), so keys can
// be rotated: new values are always sealed with the current key, and older
// versions stay readable for as long as their keys are configured.
//
// Callers pass associated data naming where a value lives (connection, user,
// column). GCM authenticates it, so a ciphertext copied to another row or
// column fails to open instead of decrypting under the wrong identity.
package tokencrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

var (
	// ErrUnknownKeyVersion means a value was sealed with a key that is not
	// configured (for example, retired before every row was re-encrypted).
	ErrUnknownKeyVersion = errors.New("tokencrypt: unknown key version")
	// ErrDecrypt means a value failed authentication: wrong key, tampered
	// ciphertext, or associated data that does not match.
	ErrDecrypt = errors.New("tokencrypt: decryption failed")
)

// Keyring seals with its current key and opens with any configured key. It is
// safe for concurrent use.
type Keyring struct {
	current int16
	aeads   map[int16]cipher.AEAD
}

// NewKeyring builds a keyring from raw 32-byte keys by version. current must
// be one of them. Versions must be positive.
func NewKeyring(current int16, keys map[int16][]byte) (*Keyring, error) {
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("tokencrypt: current key version %d is not configured", current)
	}
	k := &Keyring{current: current, aeads: make(map[int16]cipher.AEAD, len(keys))}
	for v, key := range keys {
		if v < 1 {
			return nil, fmt.Errorf("tokencrypt: key version %d must be positive", v)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("tokencrypt: key version %d is %d bytes, want %d", v, len(key), KeySize)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("tokencrypt: key version %d: %w", v, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("tokencrypt: key version %d: %w", v, err)
		}
		k.aeads[v] = aead
	}
	return k, nil
}

// FromEnv builds a keyring from environment values:
//
//   - key: TOKEN_ENCRYPTION_KEY, the current key as standard base64 of 32 bytes
//     (generate with `openssl rand -base64 32`);
//   - version: TOKEN_ENCRYPTION_KEY_VERSION, a positive integer ("" means 1);
//   - previous: TOKEN_ENCRYPTION_PREVIOUS_KEYS, comma-separated
//     "version:base64key" pairs kept only to open values sealed before a
//     rotation ("" means none).
//
// Errors never include key material.
func FromEnv(key, version, previous string) (*Keyring, error) {
	current := int16(1)
	if version != "" {
		v, err := strconv.ParseInt(version, 10, 16)
		if err != nil || v < 1 {
			return nil, errors.New("tokencrypt: TOKEN_ENCRYPTION_KEY_VERSION must be a positive integer")
		}
		current = int16(v)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(raw) != KeySize {
		return nil, fmt.Errorf("tokencrypt: TOKEN_ENCRYPTION_KEY must be base64 of %d bytes", KeySize)
	}
	keys := map[int16][]byte{current: raw}
	for entry := range strings.SplitSeq(previous, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		vs, b64, ok := strings.Cut(entry, ":")
		v, verr := strconv.ParseInt(strings.TrimSpace(vs), 10, 16)
		if !ok || verr != nil || v < 1 {
			return nil, errors.New("tokencrypt: TOKEN_ENCRYPTION_PREVIOUS_KEYS entries must be version:base64key")
		}
		if _, dup := keys[int16(v)]; dup {
			return nil, fmt.Errorf("tokencrypt: key version %d configured twice", v)
		}
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(k) != KeySize {
			return nil, fmt.Errorf("tokencrypt: previous key version %d must be base64 of %d bytes", v, KeySize)
		}
		keys[int16(v)] = k
	}
	return NewKeyring(current, keys)
}

// CurrentVersion is the version Seal uses.
func (k *Keyring) CurrentVersion() int16 { return k.current }

// Seal encrypts plaintext with the current key, binding it to aad. It returns
// nonce‖ciphertext and the key version to store alongside it.
func (k *Keyring) Seal(plaintext, aad []byte) ([]byte, int16, error) {
	aead := k.aeads[k.current]
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, fmt.Errorf("tokencrypt: generating nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, aad), k.current, nil
}

// Open decrypts a value sealed with key version and the same aad.
func (k *Keyring) Open(version int16, sealed, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[version]
	if !ok {
		return nil, fmt.Errorf("%w %d", ErrUnknownKeyVersion, version)
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("%w: value too short", ErrDecrypt)
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt // GCM's error carries no detail worth wrapping
	}
	return plain, nil
}
