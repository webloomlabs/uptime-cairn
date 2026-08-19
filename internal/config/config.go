// Package config holds the process's runtime configuration.
//
// Deliberately small, and deliberately not a framework. Progressive disclosure
// (README.md, principle 3) applies to operators too: a solo install should need
// no configuration at all, so every field here has a default that works and the
// flag exists for the person who needs to move it.
package config

import (
	"errors"
	"fmt"
)

// Modes the binary can run in. Solo mode still runs the probe behind the
// ADR-001 gRPC seam, in-process over bufconn — the split is invisible to the
// user, not absent (ADR-005 decision 14).
const (
	ModeSolo  = "solo"
	ModeProbe = "probe"
)

// Config is the whole of it, for now. Phase 1 adds database, TLS, and
// notification-related fields; Phase 4 adds the control-plane list a probe
// dials, which is operator-owned on the probe host and can never be supplied by
// a control plane (ADR-005 decision 8).
type Config struct {
	// Mode is ModeSolo or ModeProbe.
	Mode string

	// DataDir holds the SQLite database and, in probe mode, the credential
	// file — which is identity, not state (ADR-005 decision 9).
	DataDir string

	// ListenAddr serves the REST API and the embedded UI. The same port serves
	// both because the dashboard is an ordinary API client and gets no
	// privileged channel (PHASE-1-PLAN.md §2).
	ListenAddr string

	// EncryptionKeyFile is the root key for encryption at rest: 32 bytes, raw or
	// base64. Empty falls back to CAIRN_ENCRYPTION_KEY_FILE, then
	// CAIRN_ENCRYPTION_KEY, then a key generated into DataDir on first start
	// (data model §12.3). The default path exists so that `docker run` needs no
	// key management to work, and the flag exists so that a real deployment can
	// keep the key somewhere other than beside the database it protects.
	EncryptionKeyFile string

	// InstanceName is what an authenticator app shows beside the account, and
	// what a status page will call this install.
	InstanceName string
}

// Default matches the published quick start in README.md — port 3000, /data as
// the volume mount. A default that disagrees with the documentation is a
// support ticket waiting to happen.
func Default() Config {
	return Config{
		Mode:         ModeSolo,
		DataDir:      "/data",
		ListenAddr:   ":3000",
		InstanceName: "Uptime Cairn",
	}
}

// Validate rejects a configuration before anything is opened or bound. Errors
// name the flag the operator typed, not the field this struct calls it.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeSolo:
	case ModeProbe:
		return errors.New("--mode=probe is Phase 4 work: remote probes, enrolment, and credentials are specified in docs/probe/protocol.md but not built")
	default:
		return fmt.Errorf("unknown --mode %q: want %q or %q", c.Mode, ModeSolo, ModeProbe)
	}
	if c.DataDir == "" {
		return errors.New("--data-dir must not be empty")
	}
	if c.ListenAddr == "" {
		return errors.New("--listen must not be empty")
	}
	return nil
}
