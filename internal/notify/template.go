package notify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Webhook payload templating (PHASE-1-PLAN.md §3.4).
//
// A deliberately tiny interpolator rather than text/template, for three reasons.
// The spec's syntax is {{monitor.name}} with no leading dot, which text/template
// cannot express. The variables a template may use are published as an endpoint
// so the UI's autocomplete cannot drift from the renderer — which requires the
// renderer and the catalogue to be the same list, not two lists that agree
// today. And a template is user input: an interpolator that can only substitute
// has no evaluation semantics to get wrong.
//
// The trade is real and worth stating: no conditionals, no loops. A user who
// wants "only mention response time when there is one" cannot express it. The
// default envelope covers that case by carrying nulls, and the alternative is
// shipping a language.

// EscapeMode says how a substituted value is quoted for the surrounding
// document.
type EscapeMode int

const (
	// EscapeNone substitutes verbatim. Correct for plain text and Markdown.
	EscapeNone EscapeMode = iota

	// EscapeJSON escapes values for a JSON string literal. Without it a monitor
	// named `He said "hi"` produces a payload the receiver rejects, and the user
	// discovers it during the outage rather than during setup.
	EscapeJSON
)

// RenderError is a template mistake, located. It is returned to the user as a
// 200 with ok:false rather than a 5xx: a broken template is their typo, not the
// server's fault.
type RenderError struct {
	Message string
	Line    int
	Column  int
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Message)
}

// Variable is one entry in the published catalogue.
type Variable struct {
	Name        string
	Type        string
	Description string
	Example     any
}

// variables is the single source of truth: the catalogue the endpoint publishes,
// the allow-list save-time validation checks against, and the key set Context
// fills. One list, so they cannot disagree.
var variables = []Variable{
	{"monitor.id", "string", "The monitor's UUID.", "018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70"},
	{"monitor.name", "string", "The monitor's display name.", "API gateway"},
	{"monitor.description", "string", "The monitor's description, empty when unset.", "Public edge, EU region"},
	{"monitor.type", "string", "The monitor type — http, tcp, icmp, and so on.", "http"},
	{"monitor.target", "string", "What is being checked: URL, host:port, domain, or container.", "https://api.example.com/health"},
	{"monitor.url", "string", "Alias of monitor.target, for templates written against other tools.", "https://api.example.com/health"},
	{"monitor.status", "string", "The monitor's status after this event: up, down, or pending.", "down"},
	{"status", "string", "Alias of monitor.status.", "down"},
	{"previous_status", "string", "The status before this event, empty when the event is not a transition.", "up"},
	{"event", "string", "The event type.", "monitor.down"},
	{"event_id", "string", "Unique per event and stable across retries, so a receiver can deduplicate.", "018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f71"},
	{"occurred_at", "timestamp", "When the event happened, RFC 3339 in UTC.", "2026-08-19T09:41:07Z"},
	{"timestamp", "timestamp", "Alias of occurred_at.", "2026-08-19T09:41:07Z"},
	{"message", "string", "The check's message — the provider's own words, not a summary.", "unexpected status 503"},
	{"code", "string", "The check's protocol-level code, where the type has one.", "503"},
	{"response_time", "number", "Response time in milliseconds, empty when the check measured none.", 412.7},
	{"attempt", "number", "Which attempt within the retry sequence produced this result.", 3},
	{"heartbeat.time", "timestamp", "When the check ran.", "2026-08-19T09:41:07Z"},
	{"heartbeat.status", "string", "The check's own outcome: up, down, unknown, or skipped.", "down"},
	{"instance.name", "string", "This install's name.", "Uptime Cairn"},
	{"instance.version", "string", "This install's version.", "0.1.0"},
	{"instance.base_url", "string", "This install's base URL, empty when not configured.", "https://cairn.example.com"},
}

// Variables returns the catalogue.
func Variables() []Variable {
	out := make([]Variable, len(variables))
	copy(out, variables)
	return out
}

var known = func() map[string]bool {
	m := make(map[string]bool, len(variables))
	for _, v := range variables {
		m[v.Name] = true
	}
	return m
}()

// Context builds the render context for an event. Every key in the catalogue is
// present — a variable that is merely empty renders as empty rather than
// failing, which is why an unset description cannot break a delivery at 3am.
func Context(e Event) map[string]any {
	ctx := map[string]any{
		"monitor.id":          e.Monitor.ID.String(),
		"monitor.name":        e.Monitor.Name,
		"monitor.description": e.Monitor.Description,
		"monitor.type":        e.Monitor.Type,
		"monitor.target":      e.Monitor.Target,
		"monitor.url":         e.Monitor.Target,
		"monitor.status":      e.Monitor.Status,
		"status":              e.Monitor.Status,
		"previous_status":     e.PreviousStatus,
		"event":               e.Type,
		"event_id":            e.ID.String(),
		"occurred_at":         e.OccurredAt.Format(time.RFC3339),
		"timestamp":           e.OccurredAt.Format(time.RFC3339),
		"message":             "",
		"code":                "",
		"response_time":       nil,
		"attempt":             nil,
		"heartbeat.time":      "",
		"heartbeat.status":    "",
		"instance.name":       e.Instance.Name,
		"instance.version":    e.Instance.Version,
		"instance.base_url":   e.Instance.BaseURL,
	}
	if hb := e.Heartbeat; hb != nil {
		ctx["message"] = hb.Message
		ctx["code"] = hb.Code
		ctx["attempt"] = hb.Attempt
		ctx["heartbeat.time"] = hb.Time.Format(time.RFC3339)
		ctx["heartbeat.status"] = hb.Status
		if hb.ResponseTimeMs != nil {
			ctx["response_time"] = *hb.ResponseTimeMs
		}
	}
	return ctx
}

// SampleContext is the synthetic event the preview renders against when the
// caller names no monitor. Built from the catalogue's own examples, so a new
// variable cannot be published without a sample value.
func SampleContext() map[string]any {
	ctx := make(map[string]any, len(variables))
	for _, v := range variables {
		ctx[v.Name] = v.Example
	}
	return ctx
}

// ValidateTemplate checks the syntax and that every variable named exists.
//
// Run when the channel is saved, not when it fires. A typo caught here is a 422
// on a form the user is looking at; the same typo caught at delivery time is a
// missed alert during an outage.
func ValidateTemplate(tmpl string) *RenderError {
	_, err := render(tmpl, nil, EscapeNone, true)
	return err
}

// Render substitutes the context into the template.
func Render(tmpl string, ctx map[string]any, mode EscapeMode) (string, *RenderError) {
	return render(tmpl, ctx, mode, false)
}

// render walks the template once. checkOnly resolves names against the catalogue
// instead of the context, which is what lets validation run without an event.
func render(tmpl string, ctx map[string]any, mode EscapeMode, checkOnly bool) (string, *RenderError) {
	var out strings.Builder
	out.Grow(len(tmpl))

	for i := 0; i < len(tmpl); {
		open := strings.Index(tmpl[i:], "{{")
		if open < 0 {
			out.WriteString(tmpl[i:])
			break
		}
		open += i
		out.WriteString(tmpl[i:open])

		closeAt := strings.Index(tmpl[open:], "}}")
		if closeAt < 0 {
			return "", locate(tmpl, open, "unclosed {{ — every placeholder needs a matching }}")
		}
		expr := tmpl[open+2 : open+closeAt]

		name, filter, err := parseExpr(expr)
		if err != nil {
			return "", locate(tmpl, open, err.Error())
		}
		if !known[name] {
			return "", locate(tmpl, open, fmt.Sprintf(
				"unknown variable %q — GET /api/v1/notification-channels/template-variables lists the ones that exist", name))
		}
		if !checkOnly {
			out.WriteString(substitute(ctx[name], filter, mode))
		}
		i = open + closeAt + 2
	}
	return out.String(), nil
}

// parseExpr splits `name` or `name | filter`. Two filters only: raw opts out of
// escaping, json opts in. Anything more is a language.
func parseExpr(expr string) (name, filter string, err error) {
	name, filter, found := strings.Cut(expr, "|")
	name = strings.TrimSpace(name)
	filter = strings.TrimSpace(filter)

	if name == "" {
		return "", "", fmt.Errorf("empty placeholder")
	}
	if found {
		switch filter {
		case "raw", "json":
		default:
			return "", "", fmt.Errorf("unknown filter %q — the filters are raw and json", filter)
		}
	}
	return name, filter, nil
}

func substitute(value any, filter string, mode EscapeMode) string {
	text := renderValue(value)

	switch {
	case filter == "raw":
		return text
	case filter == "json", mode == EscapeJSON:
		// The quotes are stripped because the placeholder is nearly always
		// already inside a JSON string literal in the user's template. Escaping
		// the contents is what makes that literal valid.
		encoded, err := json.Marshal(text)
		if err != nil {
			return text
		}
		return strings.TrimSuffix(strings.TrimPrefix(string(encoded), `"`), `"`)
	default:
		return text
	}
}

func renderValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case time.Time:
		return v.Format(time.RFC3339)
	case []string:
		return strings.Join(v, ", ")
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}

// locate turns a byte offset into a line and column, one-based, so the UI can
// put the cursor on the mistake.
func locate(tmpl string, offset int, message string) *RenderError {
	line, column := 1, 1
	for i := 0; i < offset && i < len(tmpl); i++ {
		if tmpl[i] == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return &RenderError{Message: message, Line: line, Column: column}
}

// SortedVariableNames is what the API's error messages and the docs quote.
func SortedVariableNames() []string {
	out := make([]string, 0, len(variables))
	for _, v := range variables {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}
