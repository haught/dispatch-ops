// Package secrets encrypts credential values at rest with AES-256-GCM.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of an encryption key in bytes.
const KeySize = 32

// PublishedDevKey is the example key earlier versions shipped in
// .env.example ("dev-only-dispatch-ops-key-32byte"). It is public, so it
// must never encrypt anything: see CheckCurrentKey.
const PublishedDevKey = "ZGV2LW9ubHktZGlzcGF0Y2gtb3BzLWtleS0zMmJ5dGU="

// CheckCurrentKey refuses a key that is public knowledge as the key new
// values are encrypted with. It may still be given as an old key, so values
// encrypted with it can be read and re-encrypted.
func CheckCurrentKey(key string) error {
	if strings.TrimSpace(key) == PublishedDevKey {
		return errors.New("DISPATCH_ENCRYPTION_KEY is the example key published in .env.example, so anyone can decrypt what it encrypts. " +
			"Generate your own with `dispatch-ops gen-key`, and move this one to DISPATCH_OLD_ENCRYPTION_KEYS so values already encrypted with it can still be read")
	}
	return nil
}

// Keyring encrypts with the current key and decrypts with any known key, so
// the key can be rotated: add the old key to the decrypt-only list, set a new
// current key, and values are re-encrypted with it as they are next saved.
type Keyring struct {
	currentID string
	aeads     map[string]cipher.AEAD
	// keys are the raw keys by ID, for Derive.
	keys map[string][]byte
}

// NewKeyring builds a keyring from base64-encoded 32-byte keys.
func NewKeyring(current string, old ...string) (*Keyring, error) {
	k := &Keyring{aeads: map[string]cipher.AEAD{}, keys: map[string][]byte{}}
	id, err := k.add(current)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	k.currentID = id
	for i, o := range old {
		if strings.TrimSpace(o) == "" {
			continue
		}
		if _, err := k.add(o); err != nil {
			return nil, fmt.Errorf("old encryption key %d: %w", i+1, err)
		}
	}
	return k, nil
}

func (k *Keyring) add(encoded string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("not valid base64")
	}
	if len(key) != KeySize {
		return "", fmt.Errorf("must be %d bytes, got %d", KeySize, len(key))
	}
	return k.addRaw(key)
}

func (k *Keyring) addRaw(key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	id := KeyID(key)
	k.aeads[id] = aead
	k.keys[id] = key
	return id, nil
}

// Derive returns a keyring of keys derived from each of k's with
// HKDF-SHA256 and info, which names their one purpose. Encrypting with it
// gives no one a way to make ciphertexts under k's own keys, and old keys
// still decrypt, so rotation keeps working.
func (k *Keyring) Derive(info string) (*Keyring, error) {
	d := &Keyring{aeads: map[string]cipher.AEAD{}, keys: map[string][]byte{}}
	for id, key := range k.keys {
		dk, err := hkdf.Key(sha256.New, key, nil, info, KeySize)
		if err != nil {
			return nil, err
		}
		did, err := d.addRaw(dk)
		if err != nil {
			return nil, err
		}
		if id == k.currentID {
			d.currentID = did
		}
	}
	return d, nil
}

// KeyID derives a short, non-secret identifier for a key.
func KeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("dispatch-ops key id\x00"), key...))
	return hex.EncodeToString(sum[:8])
}

// CurrentKeyID identifies the key new values are encrypted with.
func (k *Keyring) CurrentKeyID() string { return k.currentID }

// Encrypt seals plaintext with the current key. aad binds the ciphertext to
// its context (e.g. the credential ID) so it can't be moved to another row.
func (k *Keyring) Encrypt(plaintext, aad []byte) (ciphertext []byte, keyID string, err error) {
	aead := k.aeads[k.currentID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), k.currentID, nil
}

// Decrypt opens a value sealed by Encrypt.
func (k *Keyring) Decrypt(ciphertext []byte, keyID string, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("value was encrypted with key %s, which is not configured", keyID)
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, sealed := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, errors.New("decryption failed: wrong key or corrupted value")
	}
	return plaintext, nil
}

// GenerateKey returns a new random key, base64-encoded.
func GenerateKey() string {
	key := make([]byte, KeySize)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	return base64.StdEncoding.EncodeToString(key)
}
