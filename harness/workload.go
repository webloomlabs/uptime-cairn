package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"
)

// Workload is a deterministic synthetic install: one organisation, one embedded
// probe, and N monitors spread across types, groups, tags, and statuses.
//
// Determinism matters. A load-test gate that generates a different shape on every
// run cannot tell a regression from a reshuffle, so everything here derives from
// a seed and a fixed base time.
type Workload struct {
	OrgID    []byte
	ProbeID  []byte
	Groups   [][]byte
	Tags     [][]byte
	Monitors []Monitor
	BaseTime time.Time

	// DeepCursor positions roughly halfway through the collection, filled in by
	// Setup because the two targets obtain it differently: one computes it from
	// the index, the other has to page to it because the API's cursor is opaque
	// by design. The scenario reads it and does not care which.
	DeepCursor *Cursor

	// HistoryFrom and HistoryTo bound the range the history scenario reads, and
	// are set by the target rather than fixed here for the same reason: the
	// SQLite target seeded rollups at a known synthetic time, and the HTTP
	// target's history is whatever the engine produced during this run. A fixed
	// window would have one of them reading an empty range and reporting a fast
	// query over nothing.
	HistoryFrom time.Time
	HistoryTo   time.Time
}

// baseTime is fixed rather than time.Now() so two runs at different wall-clock
// times produce byte-identical workloads.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// monitorTypes is weighted towards HTTP, which is what real installs look like.
var monitorTypes = []string{
	"http", "http", "http", "http", "http", "http",
	"tcp", "tcp",
	"icmp",
	"dns",
	"tls_expiry",
	"domain_expiry",
	"push",
	"docker",
	"grpc",
}

// GenerateWorkload builds n monitors. groups and tags scale with n so that
// filtering by one stays proportionally selective as the workload grows —
// otherwise a tag filter at 5,000 monitors would return a different fraction of
// the set than at 500, and the scaling comparison would be measuring the change
// in selectivity rather than the change in size.
func GenerateWorkload(n int, seed int64) *Workload {
	r := rand.New(rand.NewSource(seed))

	w := &Workload{BaseTime: baseTime}
	w.OrgID = uuidv7(baseTime.UnixMilli(), r)
	w.ProbeID = uuidv7(baseTime.UnixMilli(), r)

	groupCount := max(1, n/50)
	tagCount := max(1, n/250)
	for i := 0; i < groupCount; i++ {
		w.Groups = append(w.Groups, uuidv7(baseTime.UnixMilli()+int64(i), r))
	}
	for i := 0; i < tagCount; i++ {
		w.Tags = append(w.Tags, uuidv7(baseTime.UnixMilli()+int64(i), r))
	}

	w.Monitors = make([]Monitor, 0, n)
	for i := 0; i < n; i++ {
		// Spread updated_at across the range so the (updated_at, id) cursor has
		// a realistic distribution to page through rather than a single hot value.
		updated := baseTime.Add(time.Duration(i) * time.Second)
		id := uuidv7(updated.UnixMilli(), r)

		typ := monitorTypes[r.Intn(len(monitorTypes))]
		host := fmt.Sprintf("host-%04d.example.test", i%997)

		m := Monitor{
			ID:        id,
			Name:      fmt.Sprintf("monitor-%05d", i),
			Type:      typ,
			Target:    host,
			Config:    configFor(typ, host),
			GroupID:   w.Groups[i%len(w.Groups)],
			Status:    statusFor(r),
			Interval:  60,
			Timeout:   30,
			CreatedAt: updated,
			UpdatedAt: updated,
		}
		// Two tags each: enough for the reverse-index lookup to matter, few
		// enough that the join table stays a realistic size.
		m.TagIDs = append(m.TagIDs, w.Tags[i%len(w.Tags)])
		if len(w.Tags) > 1 {
			m.TagIDs = append(m.TagIDs, w.Tags[(i+1)%len(w.Tags)])
		}
		w.Monitors = append(w.Monitors, m)
	}
	return w
}

// statusFor mirrors a healthy install: most monitors up, a few down, a few
// pending. The proportions matter — the status filter is only worth an index if
// `down` is selective, and §6.2 of the data model rests on exactly that.
func statusFor(r *rand.Rand) string {
	switch v := r.Intn(100); {
	case v < 95:
		return "up"
	case v < 98:
		return "down"
	default:
		return "pending"
	}
}

func configFor(typ, host string) string {
	switch typ {
	case "http":
		return fmt.Sprintf(`{"url":"https://%s/health","method":"GET","accepted_status_codes":["200-299"]}`, host)
	case "tcp":
		return fmt.Sprintf(`{"hostname":"%s","port":443}`, host)
	case "icmp":
		return fmt.Sprintf(`{"hostname":"%s","packet_size":56}`, host)
	case "dns":
		return fmt.Sprintf(`{"hostname":"%s","record_type":"A"}`, host)
	case "tls_expiry":
		return fmt.Sprintf(`{"hostname":"%s","port":443,"days_remaining_threshold":14}`, host)
	case "domain_expiry":
		return fmt.Sprintf(`{"domain":"%s","days_remaining_threshold":30}`, host)
	case "push":
		return `{"expected_interval_seconds":60,"grace_period_seconds":30}`
	case "docker":
		return fmt.Sprintf(`{"container":"%s","docker_host":"unix:///var/run/docker.sock"}`, host)
	case "grpc":
		return fmt.Sprintf(`{"address":"%s:443","use_tls":true}`, host)
	default:
		return `{}`
	}
}

// HeartbeatBatch builds one scheduler tick's worth of results: one heartbeat per
// monitor in the slice, at the given time.
func (w *Workload) HeartbeatBatch(at time.Time, from, to int, r *rand.Rand) []Heartbeat {
	if to > len(w.Monitors) {
		to = len(w.Monitors)
	}
	batch := make([]Heartbeat, 0, to-from)
	for i := from; i < to; i++ {
		status := 1
		if r.Intn(1000) < 20 {
			status = 0
		}
		batch = append(batch, Heartbeat{
			Time:       at,
			MonitorID:  w.Monitors[i].ID,
			Status:     status,
			ResponseMS: 20 + r.Float64()*180,
			// Roughly one in a thousand results is a state change. Only those
			// carry a message, per §5.2 — storing "OK" 21M times a day is waste.
			Important: r.Intn(1000) == 0,
		})
	}
	return batch
}

// uuidv7 builds a time-ordered UUID: 48-bit millisecond timestamp, version 7,
// then random bits. Time-ordered ids append to the right-hand edge of the B-tree
// instead of scattering across it, which is why the data model (§11.3) chose v7.
func uuidv7(ms int64, r *rand.Rand) []byte {
	b := make([]byte, 16)
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	binary.BigEndian.PutUint64(b[6:14], r.Uint64())
	binary.BigEndian.PutUint16(b[14:16], uint16(r.Uint64()))
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return b
}
