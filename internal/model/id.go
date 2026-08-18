// Package model holds the domain types every other package speaks in.
//
// It imports nothing from this repository, on purpose. A domain type that knows
// about storage, HTTP, or gRPC drags one of them into every package that touches
// it, and this is the package everything touches.
package model

import "encoding/hex"

// ID is a UUIDv7 in its storage form: 16 raw bytes, never a string.
//
// The data model (§11.3) chose v7 for index locality on the heartbeat table and
// raw bytes because a 36-character text UUID costs 20 bytes a row and a parse on
// both sides. The probe protocol carries the same 16 bytes.
type ID [16]byte

// String renders the canonical dashed form, for logs, errors, and the API. It is
// the only place this type is expensive, and it is never on the write path.
func (id ID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf[:])
}

// IsZero reports whether the ID is unset. A zero ID reaching storage is a bug,
// not a valid value.
func (id ID) IsZero() bool {
	return id == ID{}
}
