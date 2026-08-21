# Your first monitor

Sixty seconds from a running install to a monitor checking something. Every
command below has been run; the output is real.

If you have not installed it yet, [install.md](install.md) is four ways in.

---

## 1. Create the administrator account

Open <http://localhost:3000>. The first visit asks for an email and a password,
and nothing else — no organisation name, no plan, no email verification.

The password is hashed with argon2id and there is no recovery path other than
the database. Use a password manager.

Through the API, if you prefer:

```sh
curl -s -c cookies.txt http://localhost:3000/api/v1/setup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse-battery"}'
```

The response carries a `csrf_token`. Keep it: every write echoes it in
`X-Cairn-CSRF-Token`, and the server issues it exactly once. `GET /auth/session`
deliberately does not reissue it.

## 2. Create a monitor

In the dashboard: **New**, pick HTTP, give it a URL, save.

Through the API:

```sh
curl -s -b cookies.txt http://localhost:3000/api/v1/monitors \
  -H 'Content-Type: application/json' \
  -H "X-Cairn-CSRF-Token: $CSRF" \
  -d '{
    "name": "Checkout",
    "type": "http",
    "config": { "url": "https://example.com/health" },
    "interval_seconds": 60
  }'
```

That is the whole minimum. Everything else has a default that works: a 30-second
timeout, one retry, no alerting yet.

## 3. Watch it check

The monitor starts as **pending**, not up and not down. A monitor that has never
reported has not earned a verdict either way, and rendering it as up would be a
claim nobody made.

Within one interval it flips. In the dashboard the row updates on its own — the
list holds a live channel scoped to exactly the monitors on screen, so a status
change appears within a second or two rather than on a refresh.

Through the API:

```sh
curl -s -b cookies.txt \
  "http://localhost:3000/api/v1/monitors/$ID/heartbeats?limit=5" | jq '.data[]'
```

```json
{
  "monitor_id": "01a02503-3d6a-72a3-a262-1145394c47d7",
  "time": "2026-08-21T15:49:44.574816Z",
  "status": "up",
  "response_time_ms": 84.2,
  "code": "200",
  "attempt": 1,
  "important": true
}
```

`important` marks a transition. It is what the activity feed and the alerting
path both key on, and it is why an install that has been green for a month has a
readable history rather than 43,200 identical rows to scroll past.

Do not wait for the interval if you are impatient: **Check now** on the monitor,
or `POST /api/v1/monitors/{id}/check`. It runs the same checker through the same
ingest path, so a manual check is indistinguishable from a scheduled one — same
transition table, same alerts, same certificate observation. A "test" that took a
different path would be testing the test. It is rate-limited to one every ten
seconds per monitor, because the thing being protected is somebody else's server.

## 4. Make it tell you

A monitor with no notification channel is a monitor that goes down quietly.

**Notifications → New**, pick a type, fill it in, and press **Send test**. The
test is a real delivery and it reports what the provider said, verbatim — not
"success", but the provider's own words, because "it says success and nothing
arrived" is the failure this exists to catch.

Thirteen channel types ship: email, generic webhook, Slack, Discord, Telegram,
Matrix, Gotify, ntfy, Microsoft Teams, PagerDuty, Opsgenie, Twilio/SMS, and
Apprise — which is roughly ninety more, if you have Apprise installed.

Two things worth knowing before you pick one:

- **Email needs a relay.** Either configure the instance relay in Settings →
  Mail relay, or give the channel its own SMTP settings. A channel set to use the
  instance relay when there is none is refused at save time with the alternative
  spelled out, rather than accepted and silently undeliverable.
- **A channel marked default is attached to every monitor created afterwards.**
  It does not retro-attach to monitors that already exist.

Then attach it: on the monitor form, or in bulk from the monitor list.

Leaving the channel list *absent* on a monitor attaches the defaults. Setting it
to an *empty list* means a deliberately silent monitor. The two are different and
the server keeps them apart, which is what lets you have a monitor you watch and
are not paged for.

## 5. Prove it works

The most valuable minute you will spend on a monitoring tool is the one where
you make it fire on purpose.

Point a throwaway monitor at something that will fail, on a short interval:

```sh
curl -s -b cookies.txt http://localhost:3000/api/v1/monitors \
  -H 'Content-Type: application/json' \
  -H "X-Cairn-CSRF-Token: $CSRF" \
  -d '{
    "name": "Deliberately broken",
    "type": "http",
    "config": { "url": "https://httpbin.org/status/500" },
    "interval_seconds": 20,
    "retries": 0,
    "notification_channel_ids": ["'"$CHANNEL_ID"'"]
  }'
```

Within twenty seconds it goes down and the alert fires. Delete it afterwards.

If nothing arrives, the channel's own row shows its last error — a channel that
has quietly stopped working is the failure mode this product cannot have, so the
error lives on the channel rather than only in the log.

---

## What next

- **[Monitor types](monitor-types.md)** — the nine types, what each one actually
  checks, and the config that is not obvious.
- **[Alerting](alerting.md)** — every channel, field by field, and webhook
  templating.
- **A status page** — Status pages → New. Pick monitors, publish, and the page
  is live at `/status/{slug}`.
- **[Coming from Uptime Kuma](migrating-from-uptime-kuma.md)** — do not rebuild
  by hand.
