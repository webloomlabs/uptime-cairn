package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Problem is RFC 9457, the error shape the OpenAPI spec fixes for every failure.
//
// Clients branch on Type, never on Title or Detail: those are prose and may be
// translated. That is why every problem below has a stable URI even when the
// status code alone would seem to say enough.
type Problem struct {
	Type     string           `json:"type"`
	Title    string           `json:"title"`
	Status   int              `json:"status"`
	Detail   string           `json:"detail,omitempty"`
	Instance string           `json:"instance,omitempty"`
	Errors   []ValidationItem `json:"errors,omitempty"`
}

// ValidationItem is one invalid field, pointed at by an RFC 6901 JSON pointer so
// a form can highlight it without string-matching a sentence.
type ValidationItem struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

const errorBase = "https://uptimecairn.dev/errors/"

func writeProblem(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, kind, title, detail string, items ...ValidationItem) {
	p := Problem{
		Type:     errorBase + kind,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		Errors:   items,
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		log.Error("write problem response", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Too late for a problem response: the status line is already sent.
		// Logging it is all that is left, and silence here would turn a
		// truncated body into a mystery.
		log.Error("write response", "error", err)
	}
}
