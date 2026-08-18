package model

// The fixed identities migration 0001 seeds (data model §3.1, §4.11).
//
// Solo mode has exactly one organisation and one probe, and both ids are
// well-known constants so nothing has to look them up or invent them. They are
// shaped as valid UUIDv7 values — version nibble 7, variant bits 10 — so no code
// downstream has to special-case them.
//
//	org:   00000000-0000-7000-8000-000000000001
//	probe: 00000000-0000-7000-8000-000000000002
var (
	SentinelOrgID   = ID{0, 0, 0, 0, 0, 0, 0x70, 0x00, 0x80, 0, 0, 0, 0, 0, 0, 1}
	EmbeddedProbeID = ID{0, 0, 0, 0, 0, 0, 0x70, 0x00, 0x80, 0, 0, 0, 0, 0, 0, 2}
)
