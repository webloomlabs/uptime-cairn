# Alerting

Thirteen channel types, what each one needs, and the two things about the system
as a whole that are worth knowing before you configure any of them.

---

## Two rules that apply to every channel

**Test-fire is a real delivery, and it reports what the provider said.** Not
"success" — the provider's own words. "It says success and nothing arrived" is
the failure this exists to catch, and a green tick that means "we posted the
request" catches nothing.

**A channel's last error lives on the channel.** A channel that has quietly
stopped working — a rotated bot token, a revoked webhook — is the failure mode
this feature cannot have, so the error is on the row in the channel list rather
than only in the log.

---

## Attaching channels to monitors

Three states, and they are genuinely different:

| On the monitor | Means |
|---|---|
| `notification_channel_ids` absent | Attach whatever channels are marked **default**. |
| `notification_channel_ids: []` | A deliberately silent monitor. |
| `notification_channel_ids: [...]` | Exactly those. |

The first two are kept apart on purpose. Collapsing them would make it
impossible to have a monitor you watch on the dashboard and are not paged about.

Marking a channel **default** attaches it to monitors created *afterwards*. It
does not retro-attach.

## Events

A channel with no events selected receives every monitor state change, which is
what somebody who never opens the control gets and is almost always right.

The events this build emits:

| Event | When |
|---|---|
| `monitor.down` | A monitor transitions to down. |
| `monitor.up` | It recovers, if `notify_on_recovery` is on. |
| `monitor.pending` | It enters the pending state. |
| `monitor.certificate_expiring` | A TLS certificate crosses the monitor's `days_remaining_threshold`. Fires again when the certificate is replaced by one still inside the threshold, and once a day after that. |
| `monitor.domain_expiring` | The same, for a domain registration. |
| `incident.opened` / `incident.updated` / `incident.resolved` | An incident advances. |
| `maintenance.started` / `maintenance.ended` | A maintenance window opens or closes. |

The expiry pair is deduplicated against the *stored* observation rather than
against process memory, so restarting the binary does not re-page you for a
certificate you already knew about.

---

## The channels

### Email (SMTP)

| Field | Notes |
|---|---|
| `to` | At least one address. |
| `cc` | Optional. |
| `use_instance_smtp` | Default true. Reads Settings → Mail relay. |
| `smtp_host`, `smtp_port`, `smtp_username`, `smtp_password` | Per-channel relay, when `use_instance_smtp` is false. |
| `smtp_encryption` | `none`, `starttls`, or `tls`. |
| `from_address`, `from_name` | |

**A channel set to use the instance relay when there is none is refused at save
time**, with the alternative spelled out. That is deliberate: accepting it and
being silently undeliverable is the worse outcome by a wide margin.

Bodies are base64-encoded, so a long template stays inside SMTP's line-length
limit rather than being wrapped into something a client renders wrong. Threading
headers are set, so an outage and its recovery arrive as one conversation.

### Generic webhook

| Field | Notes |
|---|---|
| `url` | Required. |
| `method` | `POST` (default), `PUT`, `PATCH`, `GET`. |
| `headers` | Encrypted at rest — a header value is routinely an `Authorization` token. |
| `body_template` | See [templating](#webhook-templating) below. |
| `content_type` | Drives JSON escaping in the template. |
| `verify_tls`, `timeout_seconds` | |

With no `body_template`, the default event envelope is sent.

### Slack

`webhook_url` (encrypted), plus optional `channel`, `username`, `icon_emoji`, and
`message_template`.

### Discord

`webhook_url` (encrypted), plus optional `username`, `avatar_url`,
`message_template`.

### Telegram

`bot_token` (encrypted) and `chat_id`. Optional `message_thread_id` for a forum
topic, `parse_mode` (`none`, `Markdown`, `MarkdownV2`, `HTML`), and
`disable_notification`.

### Matrix

`homeserver_url`, `room_id`, `access_token` (encrypted).

Messages are sent with **the event id as the transaction id**. A retry after a
timeout that actually succeeded therefore posts the same message rather than a
second copy — which matters more than it sounds, because a timeout that
succeeded is exactly what happens when a homeserver is under the load an outage
generates.

### Gotify

`server_url` and `application_token` (encrypted). Optional `priority` (0–10).

### ntfy

`topic` is the only required field; `server_url` defaults to the public
instance. `priority` (1–5), `tags`, and an `auth_type` of `none`, `basic`
(`username`/`password`), or `token`.

### Microsoft Teams

`webhook_url` (encrypted) and an optional `message_template`.

### PagerDuty

`integration_key` (encrypted), `severity` (`critical`, `error`, `warning`,
`info`), `region` (`us`/`eu`), `auto_resolve`.

**An outage and its recovery are two edges of one incident**, keyed by monitor.
The recovery closes the alert the failure opened rather than creating a second
one somebody has to resolve by hand.

### Opsgenie

`api_key` (encrypted), `region`, `priority` (`P1`–`P5`), `auto_close`, and
`responders` — a list of `{type, value}` where type is `team`, `user`,
`escalation`, or `schedule`.

Same incident-pairing behaviour as PagerDuty.

### Twilio / SMS

`account_sid`, `auth_token` (encrypted), `from_number`, `to_numbers`.

SMS is 160 characters. Write a `message_template`; the default envelope is not
designed for it.

### Apprise

`urls` — a list, encrypted at rest — plus optional `title_template` and
`body_template`.

This is the one that buys roughly ninety more destinations for the effort of one.

Two things about how it is run:

- **The URLs go to a mode-0600 file, not to the argument vector.** An Apprise URL
  embeds its own credentials, and an argument vector is readable through `ps` by
  anyone on the host.
- **It reports itself unavailable when the binary is not installed**, rather than
  being offered and failing on first use. Install `apprise` on the host running
  Cairn.

---

## Webhook templating

Every string field marked "template" above is interpolated with `{{variable}}`.

The syntax is deliberately not Go's `text/template` or Liquid: it is
`{{name}}` with no leading dot, no conditionals, and no loops. A notification
template that can branch is a template that can fail at 3am, and the failure
mode is a missed alert.

### The variables

The catalogue is published at
`GET /api/v1/notification-channels/template-variables`, and it is the same list
the renderer resolves against — a preview that rendered through different code
than delivery would be a preview that lies at the moment somebody is trusting it.

| Variable | |
|---|---|
| `{{monitor.id}}` | UUID |
| `{{monitor.name}}` | Display name |
| `{{monitor.description}}` | Empty when unset |
| `{{monitor.type}}` | `http`, `tcp`, … |
| `{{monitor.target}}` | URL, host:port, domain, or container |
| `{{monitor.url}}` | Alias of `monitor.target` |
| `{{monitor.status}}` / `{{status}}` | Status *after* this event |
| `{{previous_status}}` | Empty when the event is not a transition |
| `{{event}}` | `monitor.down`, … |
| `{{event_id}}` | Stable across retries. Deduplicate on this. |
| `{{occurred_at}}` / `{{timestamp}}` | RFC 3339, UTC |
| `{{message}}` | The check's own message |
| `{{code}}` | Protocol-level code, where the type has one |
| `{{response_time}}` | Milliseconds; empty when nothing was measured |
| `{{attempt}}` | Which retry produced this |
| `{{heartbeat.time}}`, `{{heartbeat.status}}` | |
| `{{instance.name}}`, `{{instance.version}}`, `{{instance.base_url}}` | |

A variable that is merely empty renders empty. A variable that does not exist is
a **422 at save time**, naming it — so a typo is caught on the form rather than
discovered as a missed alert.

### Escaping

Escaping follows the declared `content_type`. With `application/json`, values are
JSON-escaped — so a monitor named `He said "hi"` produces a payload the receiver
accepts rather than one it rejects with a parse error nobody sees.

### Example

```json
{
  "text": "{{monitor.name}} is {{status}}",
  "attachments": [{
    "color": "danger",
    "fields": [
      { "title": "Target", "value": "{{monitor.target}}" },
      { "title": "Message", "value": "{{message}}" },
      { "title": "Response time", "value": "{{response_time}}ms" }
    ],
    "footer": "{{instance.name}} · {{occurred_at}}"
  }]
}
```

### Preview

The channel form previews the rendered output live, **through the server's own
renderer**. It is not a second implementation in the browser, for the reason
above.

---

## Outbound webhooks are a different thing

Notification channels render a sentence for a person. Outbound webhooks
(Settings → Webhooks, or `/api/v1/webhooks`) deliver an envelope to a program,
and they are a separate subsystem on purpose.

- Signed with an HMAC over **the exact bytes sent**, so a receiver can verify
  without re-serialising and getting a different answer.
- `event_id` is stable across retries and manual redelivery, so a receiver can
  deduplicate.
- Self-disabling after a sustained run of failures, because a dead endpoint
  should not consume the delivery queue forever.

Use a notification channel to tell a person. Use an outbound webhook to tell a
system.

---

## Maintenance windows

An alert nobody wanted is worse than an alert nobody got, and a deploy window is
where that happens.

Windows are single, daily, weekly, monthly, or cron, and each is evaluated **in
its own IANA time zone** with the zone database compiled into the binary. "02:00
every Sunday" survives a daylight-saving transition still meaning 02:00.

Targets resolve by query through monitors, groups, and tags — so a window
covering a tag keeps covering monitors added to that tag later, which is the
whole reason to express it that way.

A schedule that will never fire is refused at write time rather than discovered
by its silence.
