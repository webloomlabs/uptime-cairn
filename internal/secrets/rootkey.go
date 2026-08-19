package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RootKeySource says where a root key came from, so an error can name the path
// the operator has to go and look at.
type RootKeySource struct {
	Key         []byte
	Description string
	Generated   bool
}

// KeyFileName is where an auto-generated root key is written inside the data
// directory.
const KeyFileName = "cairn.key"

// LoadRootKey resolves the root key in the precedence data model §12.3 fixes:
//
//  1. an explicit key file — the right choice for Docker secrets, systemd
//     LoadCredential, or a KMS-mounted path;
//  2. CAIRN_ENCRYPTION_KEY, base64 — convenient, but readable through
//     docker inspect and /proc/<pid>/environ, and it lands in shell history;
//  3. auto-generated on first start, written to <data-dir>/cairn.key at 0600.
//
// Option 3 exists to protect the sixty-seconds-to-first-monitor promise:
// requiring a key before the first run would put a setup step in front of
// docker run. It is progressive disclosure applied to key management — zero
// configuration by default, fully configurable when it matters.
//
// hasEncryptedData decides the one dangerous case. If the key is missing and
// encrypted rows exist, this refuses to start rather than generating a
// replacement: generating one would render every stored credential permanently
// unreadable while appearing to work.
func LoadRootKey(keyFile, dataDir string, hasEncryptedData bool) (RootKeySource, error) {
	if keyFile == "" {
		keyFile = os.Getenv("CAIRN_ENCRYPTION_KEY_FILE")
	}
	if keyFile != "" {
		key, err := readKeyFile(keyFile)
		if err != nil {
			return RootKeySource{}, err
		}
		return RootKeySource{Key: key, Description: keyFile}, nil
	}

	if encoded := os.Getenv("CAIRN_ENCRYPTION_KEY"); encoded != "" {
		key, err := decodeKey([]byte(encoded))
		if err != nil {
			return RootKeySource{}, fmt.Errorf("CAIRN_ENCRYPTION_KEY: %w", err)
		}
		return RootKeySource{Key: key, Description: "CAIRN_ENCRYPTION_KEY"}, nil
	}

	path := filepath.Join(dataDir, KeyFileName)
	switch key, err := readKeyFile(path); {
	case err == nil:
		return RootKeySource{Key: key, Description: path}, nil
	case !errors.Is(err, os.ErrNotExist):
		return RootKeySource{}, err
	}

	if hasEncryptedData {
		return RootKeySource{}, fmt.Errorf(
			"encryption key not found at %s, but this database holds encrypted data. "+
				"Refusing to start: generating a new key would make every stored secret permanently unreadable "+
				"while appearing to work. Restore the key file from backup, or pass --encryption-key-file", path)
	}

	key, err := NewDataKey()
	if err != nil {
		return RootKeySource{}, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return RootKeySource{}, fmt.Errorf("write encryption key: %w", err)
	}
	return RootKeySource{Key: key, Description: path, Generated: true}, nil
}

// readKeyFile accepts 32 raw bytes or a base64 encoding of them. No passphrase
// derivation: a passphrase invites a weak one and adds a KDF choice to get
// wrong, and `openssl rand -base64 32` is one command the docs can quote.
func readKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	info, statErr := os.Stat(path)
	if statErr == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("encryption key %s is readable by other users (mode %04o): chmod 600 it", path, info.Mode().Perm())
	}

	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("encryption key %s: %w", path, err)
	}
	return key, nil
}

func decodeKey(raw []byte) ([]byte, error) {
	if len(raw) == keyLen {
		return raw, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(trimmed); err == nil && len(key) == keyLen {
			return key, nil
		}
	}
	return nil, fmt.Errorf("want %d raw bytes or a base64 encoding of them, got %d bytes", keyLen, len(raw))
}
