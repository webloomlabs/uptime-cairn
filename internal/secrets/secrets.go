// Package secrets implements encryption at rest, per data model §12.
//
// The rule the whole package follows: hash what you verify, encrypt what you
// replay. Passwords, session tokens, API keys, and recovery codes are verified
// by comparison and are therefore hashed and never recoverable. A TOTP secret
// must be presented to the algorithm on every login, so it is encrypted.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Envelope layout (§12.2). Self-describing because rotation needs old and new
// ciphertexts to coexist, and a bare blob leaves no way to tell them apart:
//
//	byte  0      format version (0x01)
//	bytes 1-4    key version, uint32 big-endian
//	bytes 5-16   nonce, 96 bits, unique per encryption
//	bytes 17-    ciphertext || GCM tag
const (
	formatVersion = 0x01
	headerLen     = 1 + 4 + 12
	keyLen        = 32

	// Algorithm is recorded alongside each wrapped data key so a future change
	// is a new value rather than a silent reinterpretation of old bytes.
	Algorithm = "aes-256-gcm"
)

// ErrWrongKey means the ciphertext did not authenticate. Either the key is wrong
// or the value was tampered with — GCM cannot tell you which, and neither can we.
var ErrWrongKey = errors.New("secrets: cannot decrypt (wrong key, or the value was altered)")

// Keeper holds the current data key and can open ciphertexts written under
// earlier ones.
type Keeper struct {
	current uint32
	keys    map[uint32][]byte
}

// NewKeeper builds a keeper from unwrapped data keys.
func NewKeeper(current uint32, keys map[uint32][]byte) (*Keeper, error) {
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("secrets: no data key for current version %d", current)
	}
	for version, key := range keys {
		if len(key) != keyLen {
			return nil, fmt.Errorf("secrets: data key %d is %d bytes, want %d", version, len(key), keyLen)
		}
	}
	return &Keeper{current: current, keys: keys}, nil
}

// AAD binds a ciphertext to exactly where it lives (§12.2).
//
// Without it, an attacker who can write to the database can relocate a blob
// without ever decrypting it — moving the administrator's TOTP secret onto their
// own user row. GCM authenticates this, so a relocated ciphertext fails to open.
// It costs nothing and is the most commonly omitted part of a scheme like this.
type AAD struct {
	OrgID  []byte
	Table  string
	Column string
	RowID  []byte
}

func (a AAD) bytes(keyVersion uint32) []byte {
	out := make([]byte, 0, len(a.OrgID)+len(a.Table)+len(a.Column)+len(a.RowID)+16)
	out = append(out, a.OrgID...)
	out = append(out, a.Table...)
	out = append(out, a.Column...)
	out = append(out, a.RowID...)
	return binary.BigEndian.AppendUint32(out, keyVersion)
}

// Encrypt seals plaintext under the current data key.
func (k *Keeper) Encrypt(plaintext []byte, aad AAD) ([]byte, error) {
	gcm, err := aead(k.keys[k.current])
	if err != nil {
		return nil, err
	}

	envelope := make([]byte, headerLen, headerLen+len(plaintext)+gcm.Overhead())
	envelope[0] = formatVersion
	binary.BigEndian.PutUint32(envelope[1:5], k.current)
	if _, err := rand.Read(envelope[5:headerLen]); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}

	return gcm.Seal(envelope, envelope[5:headerLen], plaintext, aad.bytes(k.current)), nil
}

// Decrypt opens a ciphertext written under any key version this keeper holds.
func (k *Keeper) Decrypt(envelope []byte, aad AAD) ([]byte, error) {
	if len(envelope) < headerLen {
		return nil, errors.New("secrets: ciphertext is too short to be an envelope")
	}
	if envelope[0] != formatVersion {
		return nil, fmt.Errorf("secrets: unknown envelope format 0x%02x", envelope[0])
	}

	version := binary.BigEndian.Uint32(envelope[1:5])
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("secrets: no data key for version %d — it may have been retired while rows still referenced it", version)
	}

	gcm, err := aead(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, envelope[5:headerLen], envelope[headerLen:], aad.bytes(version))
	if err != nil {
		return nil, ErrWrongKey
	}
	return plaintext, nil
}

// CurrentVersion reports which data key new writes use.
func (k *Keeper) CurrentVersion() uint32 { return k.current }

// NewDataKey returns 32 random bytes for a new data key version.
func NewDataKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate data key: %w", err)
	}
	return key, nil
}

// Wrap seals a data key with the root key. Data keys live in the database
// wrapped; the root key never does.
func Wrap(rootKey, dataKey []byte) ([]byte, error) {
	gcm, err := aead(rootKey)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, dataKey, []byte("cairn-dek")), nil
}

// Unwrap opens a wrapped data key.
func Unwrap(rootKey, wrapped []byte) ([]byte, error) {
	gcm, err := aead(rootKey)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize() {
		return nil, errors.New("secrets: wrapped key is too short")
	}

	dataKey, err := gcm.Open(nil, wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():], []byte("cairn-dek"))
	if err != nil {
		return nil, ErrWrongKey
	}
	return dataKey, nil
}

func aead(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secrets: key is %d bytes, want %d", len(key), keyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return gcm, nil
}
