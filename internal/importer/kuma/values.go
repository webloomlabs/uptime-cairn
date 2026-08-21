package kuma

import (
	"strconv"
	"strings"
	"time"
)

// Coercion, in one place.
//
// A column read through database/sql's `any` arrives as int64, float64, string,
// []byte, bool, or nil, and which one depends on the column's declared affinity
// — which in Kuma's schema is inconsistent enough that guessing per call site
// would be wrong somewhere. `active BOOLEAN default 1` reads as an int64;
// `timeout DOUBLE` reads as a float64 except when it was written as an integer.
// These functions take whatever arrived and answer the question that was asked.

func text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func number(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64)
		return n
	default:
		return 0
	}
}

func decimal(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(t)), 64)
		return f
	default:
		return 0
	}
}

// truthy reads Kuma's booleans, which are 0/1 integers in SQLite and true/false
// in the JSON blobs its notification configs are stored as.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	case []byte:
		return truthy(string(t))
	default:
		return false
	}
}

// present reports whether a column held anything at all. Distinct from an empty
// string, because Kuma stores "no keyword configured" as NULL and "keyword
// configured as empty" is not a thing — so a NULL check is how an optional
// feature is recognised as switched off.
func present(v any) bool {
	if v == nil {
		return false
	}
	return text(v) != ""
}

// kumaTimeLayouts are the shapes Kuma's DATETIME columns come back as.
//
// It stores local time without a zone in 1.x and ISO-8601 in places in 2.x, and
// SQLite will hand back whatever string was written. Parsed as UTC when there is
// no zone, which is a documented approximation rather than a silent one: an
// imported heartbeat's timestamp may be off by the source host's offset, and the
// import report says so.
var kumaTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999Z",
	"2006-01-02",
}

func timestamp(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), true
	case int64:
		// Kuma's stat_* tables key on a unix timestamp in seconds. Anything
		// large enough to be milliseconds is treated as milliseconds, which is
		// the only ambiguity here and resolves at the year 33658.
		if t > 1e12 {
			return time.UnixMilli(t).UTC(), true
		}
		if t > 0 {
			return time.Unix(t, 0).UTC(), true
		}
		return time.Time{}, false
	case float64:
		return timestamp(int64(t))
	}

	raw := strings.TrimSpace(text(v))
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range kumaTimeLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
