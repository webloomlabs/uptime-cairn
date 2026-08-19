package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// The five chat destinations. Each one is somebody else's JSON shape and
// nothing more interesting than that — which is the point of writing them
// natively rather than routing everything through the meta-provider: no Python
// runtime in the container, and an error message that came from Slack.

// Colours, used wherever the transport has a notion of one. Deliberately not
// pure red and green: the two most common forms of colour blindness make those
// hard to tell apart, and this is a signal somebody reads at 3am.
const (
	colourDown    = 0xD64545
	colourUp      = 0x2E9E5B
	colourPending = 0xD98E04
	colourInfo    = 0x4A6D8C
)

func colourFor(ev Event) int {
	switch ev.Type {
	case "monitor.down":
		return colourDown
	case "monitor.up":
		return colourUp
	case "monitor.pending":
		return colourPending
	default:
		return colourInfo
	}
}

func sendSlack(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	payload := map[string]any{
		"text": Title(ev),
		"attachments": []any{map[string]any{
			"color":     fmt.Sprintf("#%06X", colourFor(ev)),
			"text":      text,
			"fallback":  Title(ev),
			"ts":        ev.OccurredAt.Unix(),
			"footer":    ev.Instance.Name,
			"mrkdwn_in": []string{"text"},
		}},
	}
	// Overriding the incoming webhook's own channel only works on legacy
	// webhooks; on modern ones Slack ignores it. Sent when asked for rather than
	// refused, because "ignored by the other end" is not this program's call.
	if v := c.str("channel", ""); v != "" {
		payload["channel"] = v
	}
	if v := c.str("username", ""); v != "" {
		payload["username"] = v
	}
	if v := c.str("icon_emoji", ""); v != "" {
		payload["icon_emoji"] = v
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	return s.postJSON(ctx, c.str("webhook_url", ""), body, nil)
}

func sendDiscord(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	// Discord rejects an embed description over 4,096 characters outright, so a
	// long template would fail the whole delivery rather than arrive clipped.
	payload := map[string]any{
		"embeds": []any{map[string]any{
			"title":       Title(ev),
			"description": truncate(text, 4000),
			"color":       colourFor(ev),
			"timestamp":   ev.OccurredAt.Format("2006-01-02T15:04:05.000Z07:00"),
			"footer":      map[string]any{"text": ev.Instance.Name},
		}},
	}
	if v := c.str("username", ""); v != "" {
		payload["username"] = v
	}
	if v := c.str("avatar_url", ""); v != "" {
		payload["avatar_url"] = v
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	return s.postJSON(ctx, c.str("webhook_url", ""), body, nil)
}

func sendMSTeams(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	// MessageCard rather than Adaptive Card: it is what an Incoming Webhook
	// connector accepts, and it renders in every Teams client rather than only
	// the current one.
	payload := map[string]any{
		"@type":      "MessageCard",
		"@context":   "https://schema.org/extensions",
		"themeColor": fmt.Sprintf("%06X", colourFor(ev)),
		"summary":    Title(ev),
		"title":      Title(ev),
		// Teams collapses newlines in text; two spaces before one is its
		// Markdown line break.
		"text": strings.ReplaceAll(text, "\n", "  \n"),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	return s.postJSON(ctx, c.str("webhook_url", ""), body, nil)
}

func sendTelegram(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	payload := map[string]any{
		"chat_id": c.str("chat_id", ""),
		"text":    Title(ev) + "\n\n" + text,
	}
	if mode := c.str("parse_mode", "none"); mode != "none" {
		// Only when asked for. Defaulting to Markdown would mean an underscore
		// in a monitor name becomes a formatting error from Telegram rather
		// than an alert.
		payload["parse_mode"] = mode
	}
	if c.flag("disable_notification", false) {
		payload["disable_notification"] = true
	}
	if v := c.str("message_thread_id", ""); v != "" {
		payload["message_thread_id"] = v
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}

	// The bot token is a path segment, so the URL is a secret and the delivery
	// log records the payload rather than the endpoint.
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(c.str("bot_token", "")) + "/sendMessage"
	return s.postJSON(ctx, endpoint, body, nil)
}

func sendMatrix(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	payload := map[string]any{
		"msgtype": "m.text",
		"body":    Title(ev) + "\n\n" + text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}

	// The transaction id is the event id, which makes the send idempotent: a
	// retry after a timeout that actually succeeded posts the same message
	// rather than a second copy of it. Matrix is the one provider here that
	// offers that, and taking it is free.
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(c.str("homeserver_url", ""), "/"),
		url.PathEscape(c.str("room_id", "")),
		url.PathEscape("cairn-"+ev.ID.String()))

	return s.do(ctx, request{
		method:      "PUT",
		url:         endpoint,
		contentType: "application/json",
		body:        body,
		headers:     map[string]string{"Authorization": "Bearer " + c.str("access_token", "")},
		verifyTLS:   true,
	})
}
