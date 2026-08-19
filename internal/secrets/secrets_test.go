package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func testKeeper(t *testing.T) *Keeper {
	t.Helper()

	key, err := NewDataKey()
	if err != nil {
		t.Fatalf("data key: %v", err)
	}
	k, err := NewKeeper(1, map[uint32][]byte{1: key})
	if err != nil {
		t.Fatalf("keeper: %v", err)
	}
	return k
}

func testAAD() AAD {
	return AAD{OrgID: []byte("org"), Table: "users", Column: "totp_secret", RowID: []byte("row-1")}
}

func TestEncryptRoundTrip(t *testing.T) {
	t.Parallel()

	keeper := testKeeper(t)
	plaintext := []byte("a totp secret")

	sealed, err := keeper.Encrypt(plaintext, testAAD())
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("the plaintext is present in the ciphertext")
	}
	if sealed[0] != formatVersion {
		t.Errorf("envelope starts with 0x%02x, want 0x%02x", sealed[0], formatVersion)
	}

	opened, err := keeper.Decrypt(sealed, testAAD())
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("decrypted %q, want %q", opened, plaintext)
	}
}

// The defence that is most often left out: an attacker who can write to the
// database must not be able to move a ciphertext onto another row — the
// administrator's TOTP secret onto their own user, say — without decrypting it.
func TestRelocatedCiphertextFailsToOpen(t *testing.T) {
	t.Parallel()

	keeper := testKeeper(t)
	sealed, err := keeper.Encrypt([]byte("secret"), testAAD())
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	moved := testAAD()
	moved.RowID = []byte("row-2")
	if _, err := keeper.Decrypt(sealed, moved); err == nil {
		t.Error("a ciphertext moved to another row still opened")
	}

	otherColumn := testAAD()
	otherColumn.Column = "password_hash"
	if _, err := keeper.Decrypt(sealed, otherColumn); err == nil {
		t.Error("a ciphertext moved to another column still opened")
	}
}

func TestTamperedCiphertextFailsToOpen(t *testing.T) {
	t.Parallel()

	keeper := testKeeper(t)
	sealed, _ := keeper.Encrypt([]byte("secret"), testAAD())

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := keeper.Decrypt(tampered, testAAD()); err == nil {
		t.Error("a modified ciphertext opened; the AEAD tag is not being checked")
	}
}

// Two encryptions of one value must differ, or equal plaintexts become
// detectable by comparing rows.
func TestNoncesAreUnique(t *testing.T) {
	t.Parallel()

	keeper := testKeeper(t)
	first, _ := keeper.Encrypt([]byte("same"), testAAD())
	second, _ := keeper.Encrypt([]byte("same"), testAAD())
	if bytes.Equal(first, second) {
		t.Error("two encryptions of the same value are byte-identical")
	}
}

// Rotation depends on old and new ciphertexts coexisting, which is why the
// envelope names its own key version.
func TestOlderKeyVersionsStillOpen(t *testing.T) {
	t.Parallel()

	old, _ := NewDataKey()
	oldKeeper, _ := NewKeeper(1, map[uint32][]byte{1: old})
	sealed, _ := oldKeeper.Encrypt([]byte("written under v1"), testAAD())

	next, _ := NewDataKey()
	keeper, err := NewKeeper(2, map[uint32][]byte{1: old, 2: next})
	if err != nil {
		t.Fatalf("keeper: %v", err)
	}

	opened, err := keeper.Decrypt(sealed, testAAD())
	if err != nil {
		t.Fatalf("decrypt v1 ciphertext under v2 keeper: %v", err)
	}
	if string(opened) != "written under v1" {
		t.Errorf("got %q", opened)
	}

	fresh, _ := keeper.Encrypt([]byte("new"), testAAD())
	if fresh[4] != 2 {
		t.Errorf("new ciphertext records key version %d, want 2", fresh[4])
	}
}

func TestWrapUnwrap(t *testing.T) {
	t.Parallel()

	root, _ := NewDataKey()
	dek, _ := NewDataKey()

	wrapped, err := Wrap(root, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("the data key is present in its wrapped form")
	}

	opened, err := Unwrap(root, wrapped)
	if err != nil || !bytes.Equal(opened, dek) {
		t.Fatalf("unwrap = %x, %v; want the data key back", opened, err)
	}

	wrongRoot, _ := NewDataKey()
	if _, err := Unwrap(wrongRoot, wrapped); err == nil {
		t.Error("a data key unwrapped under the wrong root key")
	}
}

func TestLoadRootKeyGeneratesOnFirstStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source, err := LoadRootKey("", dir, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !source.Generated || len(source.Key) != keyLen {
		t.Fatalf("got %d bytes, generated=%v", len(source.Key), source.Generated)
	}

	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode is %04o, want 0600", info.Mode().Perm())
	}

	// The second start must read the same key back, not generate another.
	again, err := LoadRootKey("", dir, true)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !bytes.Equal(again.Key, source.Key) || again.Generated {
		t.Error("the second start did not reuse the key it wrote")
	}
}

// The dangerous case, spelled out in data model §12.3: a missing key with
// encrypted data present must stop the process. Generating a replacement would
// make every stored secret permanently unreadable while appearing to work.
func TestLoadRootKeyRefusesToReplaceAMissingKey(t *testing.T) {
	t.Parallel()

	_, err := LoadRootKey("", t.TempDir(), true)
	if err == nil {
		t.Fatal("a missing key was silently replaced while encrypted data existed")
	}
}

func TestLoadRootKeyRejectsLooseFilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "loose.key")
	key, _ := NewDataKey()
	if err := os.WriteFile(path, key, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadRootKey(path, dir, false); err == nil {
		t.Error("a world-readable key file was accepted")
	}
}
