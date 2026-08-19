package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Splitting a monitor's configuration into the half that is stored in the clear
// and the half that is encrypted (data model §12.1).
//
// Notification channels do the same thing over a flat map. A monitor's config is
// nested — auth.password, tls.client_key — so the paths here are dotted and the
// walk is recursive. Everything else about the arrangement is deliberately
// identical, because two ways of handling a credential is one more than anybody
// can keep straight.
//
// The functions take the path list rather than deciding it. Which fields are
// secret is a property of the monitor type, and the checker for that type is the
// only thing that knows — see check.Confidential.

// Redacted is what a read shows in place of a secret.
//
// A marker rather than an omission, so a client can tell "set" from "not set",
// and so a form that round-trips its own GET is answerable: the write path can
// see the marker come back and know it means "leave this alone" rather than
// "the password is literally this".
const Redacted = "__redacted__"

// SplitConfig moves every named path out of config and into a second document
// with the same shape.
//
// Shape-preserving on purpose: merging is then a deep merge with no path list
// required, so the reader of an encrypted blob does not have to be told what is
// in it before it can be put back.
func SplitConfig(config json.RawMessage, paths []string) (public, secret json.RawMessage, err error) {
	if len(config) == 0 || len(paths) == 0 {
		return config, nil, nil
	}

	root, err := decodeObject(config)
	if err != nil {
		return nil, nil, err
	}
	held := map[string]any{}

	for _, path := range paths {
		value, ok := takePath(root, strings.Split(path, "."))
		if !ok {
			continue
		}
		putPath(held, strings.Split(path, "."), value)
	}

	public, err = json.Marshal(root)
	if err != nil {
		return nil, nil, fmt.Errorf("encode monitor config: %w", err)
	}
	if len(held) == 0 {
		return public, nil, nil
	}
	secret, err = json.Marshal(held)
	if err != nil {
		return nil, nil, fmt.Errorf("encode monitor credentials: %w", err)
	}
	return public, secret, nil
}

// MergeConfig puts them back together. The only place a monitor's configuration
// is whole is in memory: in the API while it is being validated, and on the way
// to the probe that will use it.
func MergeConfig(public, secret json.RawMessage) (json.RawMessage, error) {
	if len(secret) == 0 {
		return public, nil
	}
	root, err := decodeObject(public)
	if err != nil {
		return nil, err
	}
	held, err := decodeObject(secret)
	if err != nil {
		return nil, err
	}

	mergeInto(root, held)
	merged, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode monitor config: %w", err)
	}
	return merged, nil
}

// RedactConfig renders the read shape: the public half, plus a marker wherever
// the encrypted half holds something.
//
// A map is redacted value by value with its keys intact, because "which gRPC
// metadata headers are set" is configuration the user needs to see and their
// values are the part that is not.
func RedactConfig(public, secret json.RawMessage, paths []string) (json.RawMessage, error) {
	if len(secret) == 0 {
		return public, nil
	}
	root, err := decodeObject(public)
	if err != nil {
		return nil, err
	}
	held, err := decodeObject(secret)
	if err != nil {
		return nil, err
	}

	for _, path := range paths {
		segments := strings.Split(path, ".")
		value, ok := readPath(held, segments)
		if !ok {
			continue
		}
		putPath(root, segments, mask(value))
	}

	redacted, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode monitor config: %w", err)
	}
	return redacted, nil
}

// FindRedacted reports which of the named paths in config hold the marker this
// server hands out.
//
// A create carrying one is refused rather than stored: silently accepting
// "__redacted__" as a password produces a monitor that looks configured and
// authenticates as nobody, and the failure surfaces as a 401 from the target
// hours later, attributed to the target.
func FindRedacted(config json.RawMessage, paths []string) []string {
	if len(config) == 0 {
		return nil
	}
	root, err := decodeObject(config)
	if err != nil {
		return nil
	}

	var found []string
	for _, path := range paths {
		value, ok := readPath(root, strings.Split(path, "."))
		if ok && holdsMarker(value) {
			found = append(found, path)
		}
	}
	sort.Strings(found)
	return found
}

// StripRedacted removes every path whose incoming value is the marker, so a
// partial update that echoes a redacted read leaves the stored credential alone.
func StripRedacted(config json.RawMessage, paths []string) (json.RawMessage, error) {
	if len(config) == 0 {
		return config, nil
	}
	root, err := decodeObject(config)
	if err != nil {
		return nil, err
	}

	changed := false
	for _, path := range paths {
		segments := strings.Split(path, ".")
		value, ok := readPath(root, segments)
		if ok && holdsMarker(value) {
			takePath(root, segments)
			changed = true
		}
	}
	if !changed {
		return config, nil
	}

	stripped, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode monitor config: %w", err)
	}
	return stripped, nil
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode monitor config: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// readPath returns the value at a dotted path without removing it.
func readPath(root map[string]any, segments []string) (any, bool) {
	current := root
	for i, segment := range segments {
		value, ok := current[segment]
		if !ok || value == nil {
			return nil, false
		}
		if i == len(segments)-1 {
			return value, true
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

// takePath removes and returns it, leaving the intermediate objects in place. An
// auth object stripped of its password still has its type and username, and
// deleting the empty husk would change what the config means.
func takePath(root map[string]any, segments []string) (any, bool) {
	value, ok := readPath(root, segments)
	if !ok {
		return nil, false
	}

	current := root
	for _, segment := range segments[:len(segments)-1] {
		// readPath has already walked this chain and found objects all the way
		// down, so the assertion holds; the comma-ok form is here so that a
		// future caller reaching takePath directly cannot panic.
		next, ok := current[segment].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	delete(current, segments[len(segments)-1])
	return value, true
}

func putPath(root map[string]any, segments []string, value any) {
	current := root
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
	current[segments[len(segments)-1]] = value
}

// mergeInto is a deep merge: objects recurse, everything else replaces. The
// secret half never holds a value the public half also holds, so no conflict
// resolution beyond that is needed.
func mergeInto(dst, src map[string]any) {
	for key, value := range src {
		nested, isObject := value.(map[string]any)
		existing, alsoObject := dst[key].(map[string]any)
		if isObject && alsoObject {
			mergeInto(existing, nested)
			continue
		}
		dst[key] = value
	}
}

func mask(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key := range v {
			out[key] = Redacted
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = Redacted
		}
		return out
	default:
		return Redacted
	}
}

func holdsMarker(value any) bool {
	switch v := value.(type) {
	case string:
		return v == Redacted
	case map[string]any:
		if len(v) == 0 {
			return false
		}
		for _, item := range v {
			if item != Redacted {
				return false
			}
		}
		return true
	case []any:
		if len(v) == 0 {
			return false
		}
		for _, item := range v {
			if item != Redacted {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// PreserveRedacted substitutes stored values back into an incoming config
// wherever it carries the redaction marker.
//
// This is what makes a form that round-trips its own GET work. A read renders
// "__redacted__" in place of every credential; a client that edits the timeout
// and submits the whole object back is saying "leave the password alone", and
// without this the marker would either be stored as the literal password or the
// credential would be silently dropped. Both failures surface hours later as an
// authentication error attributed to the target.
//
// A path the stored config does not hold is removed rather than left as a
// marker: the caller echoed a marker for something that was never set, and
// writing the marker itself is the one outcome that must not happen.
func PreserveRedacted(stored, incoming json.RawMessage, paths []string) (json.RawMessage, error) {
	if len(incoming) == 0 || len(paths) == 0 {
		return incoming, nil
	}

	target, err := decodeObject(incoming)
	if err != nil {
		return nil, err
	}
	source, err := decodeObject(stored)
	if err != nil {
		return nil, err
	}

	changed := false
	for _, path := range paths {
		segments := strings.Split(path, ".")
		value, ok := readPath(target, segments)
		if !ok || !holdsMarker(value) {
			continue
		}
		changed = true
		if kept, ok := readPath(source, segments); ok {
			putPath(target, segments, kept)
		} else {
			takePath(target, segments)
		}
	}
	if !changed {
		return incoming, nil
	}
	return json.Marshal(target)
}
