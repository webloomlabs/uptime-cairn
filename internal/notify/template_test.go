package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func sampleEvent() Event {
	rt := 412.7
	at := time.Date(2026, 8, 19, 9, 41, 7, 0, time.UTC)
	return Event{
		ID:         model.ID{1},
		Type:       model.EventMonitorDown,
		OccurredAt: at,
		Instance:   Instance{Name: "Uptime Cairn", Version: "0.1.0", BaseURL: "https://cairn.example.com"},
		Monitor: Monitor{
			ID:     model.ID{2},
			Name:   `API "edge"`,
			Type:   "http",
			Target: "https://api.example.com/health",
			Status: "down",
		},
		PreviousStatus: "up",
		Heartbeat: &Heartbeat{
			Time:           at,
			Status:         "down",
			ResponseTimeMs: &rt,
			Message:        "unexpected status 503",
			Code:           "503",
			Attempt:        3,
		},
	}
}

// The catalogue is published as an endpoint so the UI's autocomplete cannot
// drift from the renderer. That promise is only true if the two are the same
// list, which is what this asserts.
func TestEveryPublishedVariableResolves(t *testing.T) {
	t.Parallel()

	ctx := Context(sampleEvent())
	for _, v := range Variables() {
		if _, ok := ctx[v.Name]; !ok {
			t.Errorf("%s is published but Context does not fill it", v.Name)
		}
		if _, problem := Render("{{"+v.Name+"}}", ctx, EscapeNone); problem != nil {
			t.Errorf("%s is published but does not render: %s", v.Name, problem)
		}
		if v.Example == nil {
			t.Errorf("%s has no example value, so the preview would render it blank", v.Name)
		}
	}

	// And the other direction: nothing renders that is not published, because
	// a variable the UI cannot offer is a variable nobody finds.
	sample := SampleContext()
	if len(sample) != len(Variables()) {
		t.Errorf("sample context has %d keys, catalogue has %d", len(sample), len(Variables()))
	}
}

func TestRenderSubstitutes(t *testing.T) {
	t.Parallel()

	got, problem := Render("{{monitor.name}} is {{status}} ({{response_time}} ms)", Context(sampleEvent()), EscapeNone)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	want := `API "edge" is down (412.7 ms)`
	if got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

func TestRenderTolerateSpacesAndRepeats(t *testing.T) {
	t.Parallel()

	got, problem := Render("{{ status }}/{{status}}/{{  status  }}", Context(sampleEvent()), EscapeNone)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	if got != "down/down/down" {
		t.Errorf("rendered %q", got)
	}
}

// The failure this prevents: a monitor named with a quote in it produces a
// payload the receiver rejects, and the user finds out during the outage.
func TestJSONEscapingProducesValidJSON(t *testing.T) {
	t.Parallel()

	tmpl := `{"text": "{{monitor.name}} is {{status}}"}`
	got, problem := Render(tmpl, Context(sampleEvent()), EscapeJSON)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}

	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("rendered payload is not valid JSON: %v\n%s", err, got)
	}
	if decoded["text"] != `API "edge" is down` {
		t.Errorf("text = %q", decoded["text"])
	}
}

func TestRawFilterOptsOutOfEscaping(t *testing.T) {
	t.Parallel()

	got, problem := Render(`{{monitor.name | raw}}`, Context(sampleEvent()), EscapeJSON)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	if got != `API "edge"` {
		t.Errorf("raw filter escaped anyway: %q", got)
	}
}

func TestJSONFilterOptsIn(t *testing.T) {
	t.Parallel()

	got, problem := Render(`{{monitor.name | json}}`, Context(sampleEvent()), EscapeNone)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	if got != `API \"edge\"` {
		t.Errorf("json filter did not escape: %q", got)
	}
}

// A variable that exists but has no value renders empty rather than failing.
// The difference matters at 3am: an unset description must not stop the alert.
func TestKnownButEmptyVariableRendersEmpty(t *testing.T) {
	t.Parallel()

	ev := sampleEvent()
	ev.Heartbeat = nil
	got, problem := Render("[{{message}}][{{response_time}}]", Context(ev), EscapeNone)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	if got != "[][]" {
		t.Errorf("rendered %q, want [][]", got)
	}
}

func TestUnknownVariableIsRejectedWithItsPosition(t *testing.T) {
	t.Parallel()

	problem := ValidateTemplate("line one\nline two {{monitor.nmae}}")
	if problem == nil {
		t.Fatal("a misspelled variable was accepted")
	}
	if problem.Line != 2 {
		t.Errorf("line = %d, want 2", problem.Line)
	}
	if problem.Column != 10 {
		t.Errorf("column = %d, want 10", problem.Column)
	}
	if !strings.Contains(problem.Message, "monitor.nmae") {
		t.Errorf("message does not name the variable: %s", problem.Message)
	}
}

func TestUnclosedPlaceholderIsRejected(t *testing.T) {
	t.Parallel()

	if problem := ValidateTemplate("{{status"); problem == nil {
		t.Error("an unclosed placeholder was accepted")
	}
}

func TestUnknownFilterIsRejected(t *testing.T) {
	t.Parallel()

	problem := ValidateTemplate("{{status | shout}}")
	if problem == nil {
		t.Fatal("an unknown filter was accepted")
	}
	if !strings.Contains(problem.Message, "shout") {
		t.Errorf("message does not name the filter: %s", problem.Message)
	}
}

func TestTemplateWithNoPlaceholdersIsUnchanged(t *testing.T) {
	t.Parallel()

	const text = "a plain body with { one brace } and no placeholders"
	got, problem := Render(text, Context(sampleEvent()), EscapeJSON)
	if problem != nil {
		t.Fatalf("render: %s", problem)
	}
	if got != text {
		t.Errorf("rendered %q", got)
	}
}

// Validation runs when the channel is saved, without an event to render
// against. A template that validates must then render.
func TestValidationNeedsNoContext(t *testing.T) {
	t.Parallel()

	const tmpl = "{{monitor.name}} {{status}} {{occurred_at}}"
	if problem := ValidateTemplate(tmpl); problem != nil {
		t.Fatalf("valid template rejected: %s", problem)
	}
	if _, problem := Render(tmpl, Context(sampleEvent()), EscapeNone); problem != nil {
		t.Fatalf("validated template failed to render: %s", problem)
	}
}
