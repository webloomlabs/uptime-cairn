package secrets

import "fmt"

// Vault is a Keeper bound to one table and column.
//
// It exists so that the additional authenticated data — the part of §12.2 that
// is easiest to omit and hardest to notice missing — is written down once per
// column rather than once per call site. Without AAD, an attacker who can write
// to the database can relocate a blob without ever decrypting it: move the
// administrator's TOTP secret onto their own user row, or a privileged monitor's
// bearer token onto a monitor pointed at a host they control. GCM authenticates
// it, so a relocated ciphertext fails to open.
type Vault struct {
	keeper *Keeper
	table  string
	column string
}

// NewVault binds a keeper to one column. A nil keeper is not valid; the caller
// is the composition root and always has one by the time this is called.
func NewVault(keeper *Keeper, table, column string) *Vault {
	return &Vault{keeper: keeper, table: table, column: column}
}

// Seal encrypts a value for one row. An empty plaintext seals to nil rather than
// to an envelope around nothing, so "does this row hold a secret?" is answerable
// without decrypting anything.
func (v *Vault) Seal(orgID, rowID, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	sealed, err := v.keeper.Encrypt(plaintext, v.aad(orgID, rowID))
	if err != nil {
		return nil, fmt.Errorf("seal %s.%s: %w", v.table, v.column, err)
	}
	return sealed, nil
}

// Open decrypts it. A nil envelope opens to nil, which is the same statement in
// the other direction.
func (v *Vault) Open(orgID, rowID, envelope []byte) ([]byte, error) {
	if len(envelope) == 0 {
		return nil, nil
	}
	plaintext, err := v.keeper.Decrypt(envelope, v.aad(orgID, rowID))
	if err != nil {
		return nil, fmt.Errorf("open %s.%s: %w", v.table, v.column, err)
	}
	return plaintext, nil
}

func (v *Vault) aad(orgID, rowID []byte) AAD {
	return AAD{OrgID: orgID, Table: v.table, Column: v.column, RowID: rowID}
}
