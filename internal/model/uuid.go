package model

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// uuidState makes generated ids monotonic within a millisecond.
//
// UUIDv7 is time-ordered, which is what gives the heartbeat table its index
// locality and what lets the probe protocol express acknowledgement as a
// high-water mark over result ids. Neither survives two ids generated in the
// same millisecond coming back in random order, so within a millisecond the
// counter increments instead.
var uuidState struct {
	sync.Mutex
	lastMillis int64
	counter    uint16
}

// NewID returns a UUIDv7 (RFC 9562): 48 bits of Unix milliseconds, then the
// version and variant bits, then a counter and randomness.
func NewID() ID {
	var id ID

	now := time.Now().UnixMilli()

	uuidState.Lock()
	if now == uuidState.lastMillis {
		uuidState.counter++
	} else {
		uuidState.lastMillis = now
		uuidState.counter = randomCounter()
	}
	counter := uuidState.counter
	uuidState.Unlock()

	binary.BigEndian.PutUint16(id[0:2], uint16(now>>32))
	binary.BigEndian.PutUint32(id[2:6], uint32(now))

	// Version 7 in the high nibble of byte 6, then 12 bits of counter.
	binary.BigEndian.PutUint16(id[6:8], counter&0x0fff)
	id[6] = (id[6] & 0x0f) | 0x70

	// 62 bits of randomness, with the RFC 4122 variant bits on top.
	_, _ = rand.Read(id[8:16])
	id[8] = (id[8] & 0x3f) | 0x80

	return id
}

// randomCounter seeds the intra-millisecond counter low enough that a burst
// cannot roll it over into the next millisecond's space.
func randomCounter() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:]) & 0x03ff
}

// ParseID reads the canonical dashed form. Used at the API boundary, where ids
// arrive as text.
func ParseID(s string) (ID, bool) {
	var id ID

	clean := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			continue
		}
		clean = append(clean, s[i])
	}
	if len(clean) != 32 {
		return id, false
	}
	for i := range 16 {
		hi, ok1 := hexNibble(clean[i*2])
		lo, ok2 := hexNibble(clean[i*2+1])
		if !ok1 || !ok2 {
			return ID{}, false
		}
		id[i] = hi<<4 | lo
	}
	return id, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
