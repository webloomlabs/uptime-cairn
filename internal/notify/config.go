package notify

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"sort"
	"strings"
)

// The channel config schemas, transcribed from docs/api/openapi.yaml.
//
// Declarative rather than thirteen hand-written validators, because the same
// table has to answer three questions that must never disagree: is this config
// valid, which of its fields are secrets, and which of those may be read back.
// Three hand-maintained lists is how a bot token ends up in a GET response.

type kind int

const (
	kString kind = iota
	kStringArray
	kInt
	kBool
	kStringMap
	kObjectArray
)

// field is one property of one channel type.
type field struct {
	name string
	kind kind

	required bool

	// secret puts the value in the encrypted envelope rather than in config.
	secret bool

	// redact additionally keeps it out of every read response. Every writeOnly
	// field in the spec is both; headers on a generic webhook are secret without
	// being redacted, because a header value routinely carries an Authorization
	// token and the UI still has to be able to edit the header list.
	redact bool

	// template marks a value the interpolator parses, so a typo is a 422 on the
	// form rather than a missed alert during an outage.
	template bool

	enum     []string
	format   string // "uri", "email"
	min, max int    // integers, inclusive; ignored when both zero
	minItems int
	nested   []field // kObjectArray
}

// Problem is one invalid field. Mirrors the spec's ValidationItem without this
// package having to know about HTTP.
type Problem struct {
	Pointer string
	Code    string
	Message string
}

var schemas = map[string][]field{
	"email": {
		{name: "to", kind: kStringArray, required: true, minItems: 1, format: "email"},
		{name: "cc", kind: kStringArray, format: "email"},
		{name: "use_instance_smtp", kind: kBool},
		{name: "smtp_host", kind: kString},
		{name: "smtp_port", kind: kInt, min: 1, max: 65535},
		{name: "smtp_username", kind: kString},
		{name: "smtp_password", kind: kString, secret: true, redact: true},
		{name: "smtp_encryption", kind: kString, enum: []string{"none", "starttls", "tls"}},
		{name: "from_address", kind: kString, format: "email"},
		{name: "from_name", kind: kString},
	},
	"webhook": {
		{name: "url", kind: kString, required: true, format: "uri"},
		{name: "method", kind: kString, enum: []string{"POST", "PUT", "PATCH", "GET"}},
		{name: "headers", kind: kStringMap, secret: true},
		{name: "body_template", kind: kString, template: true},
		{name: "content_type", kind: kString},
		{name: "verify_tls", kind: kBool},
		{name: "timeout_seconds", kind: kInt, min: 1, max: 60},
	},
	"slack": {
		{name: "webhook_url", kind: kString, required: true, format: "uri", secret: true, redact: true},
		{name: "channel", kind: kString},
		{name: "username", kind: kString},
		{name: "icon_emoji", kind: kString},
		{name: "message_template", kind: kString, template: true},
	},
	"discord": {
		{name: "webhook_url", kind: kString, required: true, format: "uri", secret: true, redact: true},
		{name: "username", kind: kString},
		{name: "avatar_url", kind: kString, format: "uri"},
		{name: "message_template", kind: kString, template: true},
	},
	"telegram": {
		{name: "bot_token", kind: kString, required: true, secret: true, redact: true},
		{name: "chat_id", kind: kString, required: true},
		{name: "message_thread_id", kind: kString},
		{name: "parse_mode", kind: kString, enum: []string{"none", "Markdown", "MarkdownV2", "HTML"}},
		{name: "disable_notification", kind: kBool},
		{name: "message_template", kind: kString, template: true},
	},
	"matrix": {
		{name: "homeserver_url", kind: kString, required: true, format: "uri"},
		{name: "room_id", kind: kString, required: true},
		{name: "access_token", kind: kString, required: true, secret: true, redact: true},
		{name: "message_template", kind: kString, template: true},
	},
	"gotify": {
		{name: "server_url", kind: kString, required: true, format: "uri"},
		{name: "application_token", kind: kString, required: true, secret: true, redact: true},
		{name: "priority", kind: kInt, min: 0, max: 10},
		{name: "message_template", kind: kString, template: true},
	},
	"ntfy": {
		{name: "server_url", kind: kString, format: "uri"},
		{name: "topic", kind: kString, required: true},
		{name: "priority", kind: kInt, min: 1, max: 5},
		{name: "tags", kind: kStringArray},
		{name: "auth_type", kind: kString, enum: []string{"none", "basic", "token"}},
		{name: "username", kind: kString},
		{name: "password", kind: kString, secret: true, redact: true},
		{name: "token", kind: kString, secret: true, redact: true},
		{name: "message_template", kind: kString, template: true},
	},
	"msteams": {
		{name: "webhook_url", kind: kString, required: true, format: "uri", secret: true, redact: true},
		{name: "message_template", kind: kString, template: true},
	},
	"pagerduty": {
		{name: "integration_key", kind: kString, required: true, secret: true, redact: true},
		{name: "severity", kind: kString, enum: []string{"critical", "error", "warning", "info"}},
		{name: "auto_resolve", kind: kBool},
		{name: "region", kind: kString, enum: []string{"us", "eu"}},
	},
	"opsgenie": {
		{name: "api_key", kind: kString, required: true, secret: true, redact: true},
		{name: "region", kind: kString, enum: []string{"us", "eu"}},
		{name: "priority", kind: kString, enum: []string{"P1", "P2", "P3", "P4", "P5"}},
		{name: "auto_close", kind: kBool},
		{name: "responders", kind: kObjectArray, nested: []field{
			{name: "type", kind: kString, required: true, enum: []string{"team", "user", "escalation", "schedule"}},
			{name: "value", kind: kString, required: true},
		}},
	},
	"twilio": {
		{name: "account_sid", kind: kString, required: true},
		{name: "auth_token", kind: kString, required: true, secret: true, redact: true},
		{name: "from_number", kind: kString, required: true},
		{name: "to_numbers", kind: kStringArray, required: true, minItems: 1},
		{name: "message_template", kind: kString, template: true},
	},
	"apprise": {
		{name: "urls", kind: kStringArray, required: true, minItems: 1, secret: true, redact: true},
		{name: "title_template", kind: kString, template: true},
		{name: "body_template", kind: kString, template: true},
	},
}

// Redacted is what a read response shows in place of a secret. A marker rather
// than an omission so the UI can tell "set" from "not set", and PATCH treats a
// value equal to it as "leave this alone" — which is what stops a form that
// round-trips its own GET from overwriting a bot token with asterisks.
const Redacted = "__redacted__"

// KnownType reports whether the spec defines this channel type.
func KnownType(t string) bool { _, ok := schemas[t]; return ok }

// Types lists every channel type, sorted.
func Types() []string {
	out := make([]string, 0, len(schemas))
	for t := range schemas {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Validate checks a whole config object against its type's schema and returns
// one problem per bad field — the same rule the monitor endpoint follows, for
// the same reason: a form resubmitted five times to learn five mistakes is a
// form nobody enjoys.
func Validate(channelType string, config map[string]any) []Problem {
	fields, ok := schemas[channelType]
	if !ok {
		return []Problem{{Pointer: "/type", Code: "invalid",
			Message: fmt.Sprintf("type %q is not one the spec defines: want %s", channelType, strings.Join(Types(), ", "))}}
	}

	var problems []Problem
	byName := make(map[string]field, len(fields))
	for _, f := range fields {
		byName[f.name] = f
	}

	for name := range config {
		if _, ok := byName[name]; !ok {
			problems = append(problems, Problem{
				Pointer: "/config/" + name, Code: "unknown_field",
				Message: fmt.Sprintf("%s channels have no %q setting", channelType, name)})
		}
	}

	for _, f := range fields {
		value, present := config[f.name]
		if !present || value == nil {
			if f.required {
				problems = append(problems, Problem{Pointer: "/config/" + f.name, Code: "required",
					Message: f.name + " is required"})
			}
			continue
		}
		problems = append(problems, validateField("/config/"+f.name, f, value)...)
	}

	// Cross-field rules the per-field pass cannot see.
	problems = append(problems, validateCombinations(channelType, config)...)
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Pointer < problems[j].Pointer })
	return problems
}

func validateField(pointer string, f field, value any) []Problem {
	bad := func(code, message string) []Problem {
		return []Problem{{Pointer: pointer, Code: code, Message: message}}
	}

	switch f.kind {
	case kString:
		text, ok := value.(string)
		if !ok {
			return bad("invalid_type", f.name+" must be a string")
		}
		if text == Redacted {
			// The caller echoed a redacted read straight back. Left alone here
			// and dropped before the write; see MergeSecrets.
			return nil
		}
		if len(f.enum) > 0 && !contains(f.enum, text) {
			return bad("invalid", fmt.Sprintf("%s must be one of %s", f.name, strings.Join(f.enum, ", ")))
		}
		if problem := checkFormat(pointer, f, text); problem != nil {
			return []Problem{*problem}
		}
		if f.template {
			if err := ValidateTemplate(text); err != nil {
				return bad("invalid_template", err.Error())
			}
		}

	case kStringArray:
		items, ok := value.([]any)
		if !ok {
			return bad("invalid_type", f.name+" must be an array of strings")
		}
		if len(items) < f.minItems {
			return bad("too_few_items", fmt.Sprintf("%s needs at least %d entr%s", f.name, f.minItems,
				map[bool]string{true: "y", false: "ies"}[f.minItems == 1]))
		}
		var problems []Problem
		for i, item := range items {
			text, ok := item.(string)
			if !ok {
				problems = append(problems, Problem{Pointer: fmt.Sprintf("%s/%d", pointer, i),
					Code: "invalid_type", Message: f.name + " entries must be strings"})
				continue
			}
			if text == Redacted {
				continue
			}
			if problem := checkFormat(fmt.Sprintf("%s/%d", pointer, i), f, text); problem != nil {
				problems = append(problems, *problem)
			}
		}
		return problems

	case kInt:
		number, ok := value.(float64)
		if !ok || number != float64(int(number)) {
			return bad("invalid_type", f.name+" must be a whole number")
		}
		if f.min != 0 || f.max != 0 {
			if int(number) < f.min || int(number) > f.max {
				return bad("out_of_range", fmt.Sprintf("%s must be between %d and %d", f.name, f.min, f.max))
			}
		}

	case kBool:
		if _, ok := value.(bool); !ok {
			return bad("invalid_type", f.name+" must be true or false")
		}

	case kStringMap:
		entries, ok := value.(map[string]any)
		if !ok {
			return bad("invalid_type", f.name+" must be an object of string values")
		}
		var problems []Problem
		for key, item := range entries {
			text, ok := item.(string)
			if !ok {
				problems = append(problems, Problem{Pointer: pointer + "/" + key,
					Code: "invalid_type", Message: "header values must be strings"})
				continue
			}
			// Header values interpolate too, which is the difference between
			// integrating with an endpoint that wants a per-alert token and not.
			if err := ValidateTemplate(text); err != nil {
				problems = append(problems, Problem{Pointer: pointer + "/" + key,
					Code: "invalid_template", Message: err.Error()})
			}
		}
		return problems

	case kObjectArray:
		items, ok := value.([]any)
		if !ok {
			return bad("invalid_type", f.name+" must be an array of objects")
		}
		var problems []Problem
		for i, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				problems = append(problems, Problem{Pointer: fmt.Sprintf("%s/%d", pointer, i),
					Code: "invalid_type", Message: f.name + " entries must be objects"})
				continue
			}
			for _, sub := range f.nested {
				subValue, present := entry[sub.name]
				subPointer := fmt.Sprintf("%s/%d/%s", pointer, i, sub.name)
				if !present || subValue == nil {
					if sub.required {
						problems = append(problems, Problem{Pointer: subPointer, Code: "required",
							Message: sub.name + " is required"})
					}
					continue
				}
				problems = append(problems, validateField(subPointer, sub, subValue)...)
			}
		}
		return problems
	}
	return nil
}

func checkFormat(pointer string, f field, text string) *Problem {
	switch f.format {
	case "email":
		if _, err := mail.ParseAddress(text); err != nil {
			return &Problem{Pointer: pointer, Code: "invalid", Message: fmt.Sprintf("%q is not an email address", text)}
		}
	case "uri":
		u, err := url.Parse(text)
		switch {
		case err != nil || u.Host == "":
			return &Problem{Pointer: pointer, Code: "invalid", Message: fmt.Sprintf("%q is not an absolute URL", text)}
		case u.Scheme != "http" && u.Scheme != "https":
			return &Problem{Pointer: pointer, Code: "invalid",
				Message: fmt.Sprintf("%q must be an http or https URL", text)}
		}
	case "":
		// A template may legitimately be cleared to the empty string, which means
		// "use the default rendering" rather than "send nothing".
		if !f.template && strings.TrimSpace(text) == "" {
			return &Problem{Pointer: pointer, Code: "required", Message: f.name + " must not be blank"}
		}
	}
	return nil
}

// validateCombinations catches the configurations that are individually valid
// and jointly useless — the ones that would otherwise fail silently at 3am.
func validateCombinations(channelType string, config map[string]any) []Problem {
	var problems []Problem

	switch channelType {
	case "email":
		// use_instance_smtp defaults to true, and a channel that asks for the
		// instance relay when none is configured is refused at save time rather
		// than accepted and silently undeliverable. The failure mode this avoids
		// is a channel that looks configured and delivers nothing, discovered
		// during the outage it was supposed to report.
		useInstance := true
		if v, ok := config["use_instance_smtp"].(bool); ok {
			useInstance = v
		}
		if useInstance {
			if !InstanceSMTPConfigured() {
				problems = append(problems, Problem{Pointer: "/config/use_instance_smtp", Code: "unconfigured",
					Message: "this instance has no SMTP relay configured; set one under /api/v1/settings, or set use_instance_smtp to false and give this channel its own smtp_host, smtp_port and from_address"})
			}
			break
		}
		if _, ok := config["smtp_host"].(string); !ok {
			problems = append(problems, Problem{Pointer: "/config/smtp_host", Code: "required",
				Message: "smtp_host is required when use_instance_smtp is false"})
		}
		if _, ok := config["from_address"].(string); !ok {
			problems = append(problems, Problem{Pointer: "/config/from_address", Code: "required",
				Message: "from_address is required when use_instance_smtp is false"})
		}

	case "ntfy":
		switch config["auth_type"] {
		case "basic":
			if _, ok := config["username"].(string); !ok {
				problems = append(problems, Problem{Pointer: "/config/username", Code: "required",
					Message: "username is required when auth_type is basic"})
			}
			if _, ok := config["password"].(string); !ok {
				problems = append(problems, Problem{Pointer: "/config/password", Code: "required",
					Message: "password is required when auth_type is basic"})
			}
		case "token":
			if _, ok := config["token"].(string); !ok {
				problems = append(problems, Problem{Pointer: "/config/token", Code: "required",
					Message: "token is required when auth_type is token"})
			}
		}

	case "webhook":
		// A GET with a body is not a request most servers read, and templating a
		// body the transport discards is a trap worth closing here.
		if config["method"] == "GET" {
			if body, ok := config["body_template"].(string); ok && body != "" {
				problems = append(problems, Problem{Pointer: "/config/body_template", Code: "conflict",
					Message: "a GET webhook sends no body; use POST, PUT or PATCH, or drop body_template"})
			}
		}
	}
	return problems
}

// Split separates a validated config into the part stored as JSON and the part
// stored in the encrypted envelope.
//
// The separation happens here, at the storage boundary, and not at the API
// boundary. That is deliberate: a read path serialising config cannot leak a bot
// token by accident, because the token is not in config to leak (data model
// §4.4).
func Split(channelType string, config map[string]any) (public, secret map[string]any) {
	public = make(map[string]any, len(config))
	secret = make(map[string]any)

	for _, f := range schemas[channelType] {
		value, ok := config[f.name]
		if !ok || value == nil {
			continue
		}
		if f.secret {
			secret[f.name] = value
			continue
		}
		public[f.name] = value
	}
	return public, secret
}

// Merge recombines them for delivery. The only place the two halves are ever
// whole is in memory, inside a send.
func Merge(public, secret map[string]any) map[string]any {
	out := make(map[string]any, len(public)+len(secret))
	for k, v := range public {
		out[k] = v
	}
	for k, v := range secret {
		out[k] = v
	}
	return out
}

// Redact replaces every redacted field with the marker, so a read says whether a
// secret is set without saying what it is.
func Redact(channelType string, public, secret map[string]any) map[string]any {
	out := make(map[string]any, len(public)+len(secret))
	for k, v := range public {
		out[k] = v
	}
	for _, f := range schemas[channelType] {
		value, ok := secret[f.name]
		if !ok || value == nil {
			continue
		}
		if !f.redact {
			out[f.name] = value
			continue
		}
		if items, ok := value.([]any); ok {
			masked := make([]any, len(items))
			for i := range items {
				masked[i] = Redacted
			}
			out[f.name] = masked
			continue
		}
		out[f.name] = Redacted
	}
	return out
}

// StripRedacted removes any field whose incoming value is the marker this server
// handed out, so a client that PATCHes back its own GET leaves the stored secret
// untouched rather than overwriting it with asterisks.
func StripRedacted(config map[string]any) {
	for name, value := range config {
		switch v := value.(type) {
		case string:
			if v == Redacted {
				delete(config, name)
			}
		case []any:
			allMasked := len(v) > 0
			for _, item := range v {
				if item != Redacted {
					allMasked = false
					break
				}
			}
			if allMasked {
				delete(config, name)
			}
		}
	}
}

// DecodeConfig parses stored JSON into the map the rest of this package speaks.
func DecodeConfig(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode channel config: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
