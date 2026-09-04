# Reporting

Scheduled, branded reports over your monitoring history: what a template is, how
a schedule fires, where the files live, what arrives in a client's inbox, and how
to publish one at a link.

If a figure in a report is disputed, the page you want is
[sla-methodology.md](sla-methodology.md) — it states exactly what counts as
downtime and what leaves the denominator, and it is the page to send somebody who
disagrees with a number.

---

## Three things, kept apart

| | What it is |
|---|---|
| **Template** | The definition: what to report on, over what window, under whose branding, in which formats. |
| **Schedule** | When a template runs, in which timezone, and who receives it. |
| **Run** | One execution, with the artifacts it produced and the deliveries it attempted. |

They are three things rather than one because three different questions get asked
of them. "Re-send last month's" is a delivery over an existing run. "Regenerate it
after we corrected the incident record" is a new run of the same template over the
same window. "The PDF failed but the HTML went out" is a run in the `partial`
state with one failed artifact and three good ones. A single object could express
none of the three.

**Deleting a template does not delete its runs.** The template is soft-deleted and
disappears from the list; every report it ever produced stays, still able to say
what definition and what schedule produced it. A run is a record of what a client
was sent, and it outlives the arrangement that sent it — including the client
leaving.

---

## Report types

| Type | What it puts on the page |
|---|---|
| `uptime` | Uptime, the daily availability strip, and response time. **No SLO vocabulary at all.** |
| `sla` | The above plus target versus actual, error budget consumed and remaining, burn rate, and a breach log with timestamps. |
| `post_mortem` | The incident log with time to detect, acknowledge and resolve, and the means across them. |
| `comparative` | Two or more series side by side: this period against the last, or named monitors or groups against each other. |
| `custom` | Everything, with `sections` choosing which blocks appear. |

`uptime` is the default and it is the one a solo user should stay on. It carries
no SLA block **even when the monitors in scope have targets set**, and that is
deliberate twice over: it keeps error-budget vocabulary off the screen of somebody
who has not asked for it, and it means an agency running an uptime summary for a
client does not publish the internal target it set on that client's monitors.
Choosing the type is the choice.

### The comparative type

```jsonc
"type": "comparative",
"comparison": { "mode": "previous_period" }
```

Three modes: `previous_period`, `monitors`, and `groups`. The entity modes need at
least two things to compare and are refused at save time otherwise.

**The previous period is the same length placed immediately before**, not the
previous calendar period. A March report compares against 29 January – 1 March
rather than against February, because 28 days beside 31 makes every count differ
for reasons that are about the calendar rather than about the service. The ratios
would still compare correctly and the counts would not, and the table shows both.

Region against region is Phase 4. The mode does not exist rather than existing and
returning nothing.

---

## Scope

Selection is **by rule, resolved when the report runs** — not a list flattened
when you saved it.

```jsonc
"scope": {
  "monitor_ids": ["…"],
  "group_ids":   ["…"],
  "tag_ids":     ["…"]
}
```

The three combine as a union, and a monitor selected three ways appears once. Add
a monitor to a client's tag and it is in that client's next report without anybody
editing the report.

Paused monitors are included: they still have history in the window, and a report
that silently dropped them would understate the estate.

A scope that resolves to nothing produces a report saying so rather than a failed
run. A client whose monitors were all deleted still gets a document; a failed run
is something nobody looks at until the invoice goes out.

---

## Windows and timezones

`period` is `day`, `week`, `month`, `quarter`, `year` or `custom`, and
`period_style` decides how it is cut:

- **`calendar`** cuts the last complete period in the schedule's own timezone —
  what a client invoiced monthly expects.
- **`rolling`** counts back from the moment the run started.

The zone matters more than it looks. A monthly report for a Sydney agency starts
at `2026-02-28T13:00Z`, not at midnight UTC, and the difference is the best part
of a working day at both ends. The zone the boundaries were cut in is recorded on
the run and printed on the document, because there is no way to recover it
afterwards.

Weeks start Monday.

---

## Resolution, and what a report over last March will contain

Reports read the rollup tiers, never raw heartbeats. Which tier answers depends on
how far back the window reaches and on your retention settings:

| Tier | Default retention | A report over a window this old reads at |
|---|---|---|
| raw | 7 days | never — reports do not read raw |
| `1m` | 30 days | under 30 days |
| `5m` | 90 days | 30–90 days |
| `1h` | 365 days | 90–365 days |
| `1d` | indefinite | over a year, and any window the tiers above no longer cover |

The document says which tier actually answered, whether that was a downgrade from
what was asked for, and — where retention truncated the start of the window —
the earliest point the data reaches. **The figures are computed over the covered
window rather than the requested one**, so a truncated range cannot return the
same rows under a period the data does not reach.

Retention is read fresh on every run. Shortening it this morning changes the tier
this afternoon's report reads at, without a restart.

### The one percentile, and its three gates

There is exactly one percentile in the product: a real nearest-rank p95 over the
**trailing seven days of the reported period**. Not of today — a March report
generated in April describes March.

It is withheld, with the reason on the page, when:

- the scope is over 25 monitors (`scope_too_large`), because a rank statistic
  cannot be batched and 5,000 of them is fifty million rows;
- `raw_days` is under seven (`insufficient_raw_retention`);
- raw rows for that monitor have already been pruned behind the daily tier that
  summarised them (`insufficient_raw_retention` again).

There is **no percentile over the whole window at any tier**, and there cannot be:
a quantile is a rank statistic and does not merge, so no coarser tier holds one.
The daily average series is the primary latency exhibit instead.

---

## Formats

Four, and they are siblings over one computed document — the PDF is never a
converted web page.

| | |
|---|---|
| `html` | The canonical output. Self-contained: inline styles, inline SVG charts, a data-URI logo, no network. |
| `pdf` | Three A4 pages or more, written by hand with an embedded Roboto family. |
| `csv` | One well-formed file with a `row_type` discriminator. **A null is an empty field, never a zero.** |
| `json` | The `ReportDocument` verbatim, with `meta.schema_version`. What a BI tool binds to. |

**A format that fails does not fail the run.** The run becomes `partial`, the
failure is recorded against that format with its reason, and the formats that did
render are still delivered. Asking to download the one that failed is a `409`
naming the cause rather than a `404`.

---

## Branding

A brand profile carries a logo, a primary and accent colour, footer and cover
text, and the client name that appears as "Prepared for". A template names one, or
takes the default.

**An install with no brand profile is not unbranded.** The report falls back to
the instance name and the dashboard's primary colour from
`settings.appearance`, so the first report a new install produces looks
configured. Nothing else is inferred: there is no logo, footer or client name in
that settings section, and inventing a footer would put words on a client's
document that nobody wrote.

The colour appears in exactly two places — the rule under the cover title and the
accent bar beside each figure — and deliberately nowhere else. The uptime strip's
green, red and grey are the *legend* its caption names by colour, so recolouring
them would produce a figure that contradicts its own caption.

**Logos must be raster.** PNG or JPEG, refused at upload with the reason if not,
and the format is decided from the bytes rather than from the declared
`Content-Type`. See [brand-profiles.md](brand-profiles.md).

The branding is **copied onto each document when it runs**. Rebrand in June and
every January report still says what it said when it was sent — which is the one
thing an artifact exists to guarantee.

---

## Schedules

```jsonc
{
  "report_template_id": "…",
  "frequency": "monthly",
  "timezone": "Australia/Sydney",
  "send_at": "07:30",
  "deliveries": [ … ]
}
```

`frequency` is `daily`, `weekly`, `monthly`, `quarterly` or `cron`. The named ones
are cron expressions underneath, which is why they behave correctly across
daylight saving:

| Frequency | Fires |
|---|---|
| `daily` | every day at `send_at` |
| `weekly` | Monday |
| `monthly` | the 1st |
| `quarterly` | the 1st of January, April, July and October |

Weekly fires on Monday because weeks are cut from Monday, so the report arriving
Monday morning covers the week that just ended. Monthly and quarterly fire on the
first, for the period that just closed. Neither is configurable — an operator who
wants Thursdays writes a `cron`, which is what cron is for.

**A schedule that will never fire is refused when you save it.** `0 0 30 2 *`
parses cleanly and matches nothing; a timezone that does not exist is refused by
name rather than falling back to UTC. Discovering either by its silence a month
later is the failure this prevents.

### Late, and missed periods

A run that starts more than fifteen minutes after it was due is marked `late` and
says how far behind it was. Fifteen minutes because ticks are a minute apart and a
busy pool adds a few more; marking ordinary jitter late would make the flag
meaningless on the screen where it matters.

**A missed schedule fires once, not once per missed period.** An instance down for
three days owes a daily client one report, not three copies of yesterday's
arriving as an apology.

---

## Delivery

A run produces artifacts and finishes; handing them over is a separate step. **A
delivery failure is not a run failure** — the report exists, is on disk with a
digest beside it, and can be downloaded and re-sent.

| Target | What arrives |
|---|---|
| `email` | The report **attached**, through the instance SMTP relay, with a short covering note. |
| `slack` | A message announcing the report and what it covers. Not the file — an incoming webhook cannot carry an upload. |
| `webhook` | A JSON description of the run and its artifacts, each with its SHA-256, so a receiver can fetch and verify. |
| `s3` | The run's files, dropped into a bucket under a readable key. See [The drop, and the mirror](#the-drop-and-the-mirror) — they are not the same thing. |

Set `formats` on a target to narrow what it receives: an auditor gets the PDF and
a BI pipeline gets the CSV, from one schedule.

**Name a notification channel instead of restating its configuration.** A target
with `notification_channel_id` reads that channel's URL, recipients and secrets at
delivery time, so a rotated Slack token is rotated once. A target that keeps its
own copy would keep working after the rotation and then fail, months later, in a
way nobody connects to it.

### The delivery log

Every attempt is a row, including the ones that were retried, so "it took three
goes tonight and two last month" is visible. Three attempts, then it stops — a
monthly report's value does not decay in minutes the way an alert's does, and you
can re-send it tomorrow.

A **skipped** delivery is recorded as loudly as a failed one and is not a failure:

- no SMTP relay configured;
- nothing rendered in a format this target takes;
- the notification channel it delivers through is disabled.

Silence with no row behind it is indistinguishable from a system that is not
running, which is the whole reason the log exists.

Webhook and Slack URLs are **redacted before they are stored** in the log — the
path of a Slack incoming webhook *is* the credential, and this log gets pasted into
support conversations.

---

## Where the files live

`<data-dir>/reports/<yyyy>/<mm>/<artifact-id>.<ext>`, directories `0750` and files
`0600`. The name comes from the artifact id and the format and never from the
template title, so there is no sanitisation step to get wrong.

Every artifact carries a SHA-256 and a size, and the digest is offered to the
downloader as `X-Cairn-SHA256`. That is what makes an artifact evidence rather
than a file: "is this the document we sent?" is answerable, and a truncated write
from a full disk is detectable rather than served silently.

**The reports directory is part of your backup.** See
[operations/backup-restore.md](../operations/backup-restore.md) — snapshot the
database first, then the directory, because a file is written before its row and
the reverse order can produce a row whose bytes are not in the backup.

### Retention

`settings.retention.report_artifact_days` (default 365, `0` keeps indefinitely).
When it passes, the bytes are reclaimed and **the row stays as a tombstone**, so a
bookmarked link answers `410` — "this existed and is gone" — rather than `404`.

It is deliberately not tied to the rollup tiers. An artifact is expected to
outlive the data it was computed from; that is the point of keeping one.

An hourly sweep also reclaims orphan files — bytes written by a run that was
killed before it committed the row. It leaves anything written in the last hour
alone, because a report being produced right now looks exactly like an orphan.

---

## The drop, and the mirror

Two features that both put report files in an S3-compatible bucket, and they are
**not the same thing**. Configuring one when you meant the other is the mistake
this section exists to prevent, because both look like success from the outside.

| | The mirror | The drop |
|---|---|---|
| What it is | A durability copy of **every artifact this install produces** | A **delivery**: one schedule's output, for a recipient |
| Configured in | `settings.report_storage`, once | On a schedule, as an `s3` delivery target |
| Key layout | The on-disk path: `<prefix>/<yyyy>/<mm>/<artifact-id>.<ext>` | Readable: `<prefix>/<template>/<period>/<template>-<period>.<ext>` |
| Read by | A restore, which wants the tree reproduced exactly | A person, or somebody's data pipeline |
| On failure | Recorded on the artifact; **the run still succeeds** | A failed delivery, retried like any other |

**A drop is not a backup.** It sends the formats that target takes, for the
schedules that have one, to wherever a client asked for them. If you want the
guarantee that every report this install ever produced exists somewhere other
than this disk, that is the mirror.

They share one client — a few hundred lines of SigV4 over the standard library,
rather than a vendor SDK — and nothing else.

### Configuring the mirror

Settings → **Report artifact mirror**, or `PATCH /api/v1/settings`:

```jsonc
{
  "report_storage": {
    "mirror_enabled": true,
    "bucket": "cairn-artifacts",
    "prefix": "cairn/reports",
    "region": "us-east-1",
    "endpoint": "https://minio.example.com:9000",
    "path_style": true,
    "access_key_id": "…",
    "secret_access_key": "…",
    "server_side_encryption": "AES256"
  }
}
```

Three fields are worth explaining because they are the ones that go wrong:

- **`region` is required even where your provider ignores it.** It is an input to
  the signing key, not to routing, so a wrong or missing one produces a `403` that
  explains nothing. If you are on MinIO, Garage or Ceph and have never needed a
  region, `us-east-1` is the conventional answer.
- **`path_style` puts the bucket in the path rather than the hostname.** MinIO,
  Garage and Ceph commonly need it; AWS prefers the alternative. Without it, the
  request goes to `<bucket>.<your-endpoint>`, which needs DNS and a certificate
  that a self-hosted server generally does not have — and the failure is a TLS
  error naming a hostname you never typed.
- **`endpoint` is empty for AWS** and set for everything else.

Only static credentials are supported. No instance profiles, no STS, no credential
chain — those are AWS-specific paths with their own refresh and failure modes.

The secret is **sealed at rest** exactly as the SMTP password is, and no read ever
returns it. `secret_access_key_set` tells you one is stored. Leaving the field
empty on a later save keeps the stored one; sending `null` clears it.

Changes take effect on the **next report**, not the next restart.

### The bucket must not be public

> A public bucket holding client reports is a data breach with no code defect
> behind it. Nothing in this product can detect that you have made one.

Artifacts are **not encrypted before upload**. That is a decision on the record
rather than an omission: monitor names, uptime figures and incident narratives are
already plaintext in `cairn.db`, so encrypting the report that renders them would
be inconsistent without being protective. `server_side_encryption` is passed
through to the provider as `x-amz-server-side-encryption` when set, and `aws:kms`
requires KMS to actually be configured at the far end — MinIO without it refuses
the upload with `NotImplemented`.

**Do not put your encryption key in the same bucket as a database backup.** A
backup that puts the key beside the database it protects has encrypted nothing
against the threat that actually happens, which is somebody walking off with the
backup. Artifacts and a database backup may share a bucket; the key needs a
different trust boundary.

### When the mirror fails

The upload happens after the local file is written and after its row is committed,
so a failure has somewhere to be recorded and the artifact is already readable when
it is attempted. **A failed upload does not fail the run.** Local storage is the
source of truth and the only read path in every configuration, so a report that
rendered, filed and delivered has not failed because a bucket was unreachable.

Each artifact carries the outcome, with the provider's own message:

```jsonc
"mirror": {
  "state": "failed",
  "uploaded_at": null,
  "error": "s3: not found: put cairn/reports/2026/04/…csv: NoSuchBucket: The specified bucket does not exist"
}
```

`null` rather than `pending` means no mirror was configured when that artifact was
written, which is a different fact from "an upload has not happened yet".

**Nothing retries it.** A failed upload stays failed on that artifact; fixing the
configuration affects the next report, not the backlog, and nothing compares the
bucket against the database. If you need the gap filled, copy `<data-dir>/reports/`
into the bucket yourself — the layouts are identical, which is why they are
identical.

### Configuring a drop

On a schedule's delivery target:

```jsonc
{
  "type": "s3",
  "formats": ["csv", "json"],
  "s3": {
    "bucket": "client-drop",
    "prefix": "acme",
    "region": "us-east-1",
    "endpoint": "https://minio.example.com:9000",
    "path_style": true,
    "access_key_id": "…",
    "secret_access_key": "…"
  }
}
```

The secret is sealed on the delivery row and never returned by a read of the
schedule; the bucket, prefix, region and endpoint come back, and the key id does
not.

---

## Share links

A report published to anyone holding a URL:

```
POST   /api/v1/report-runs/{id}/share     → { "url": "…", "expires_at": null }
DELETE /api/v1/report-runs/{id}/share     → 204
```

The URL is **shown once**. The token is stored hashed for the lookup and sealed
for replay, so no read path can produce it again — a run afterwards reports that a
link exists, when it was created, when it expires and whether the recipient has
opened it, and never what it is. If you lose it, revoke and create another; a link
you cannot produce is a link you have lost control of.

**Anyone with the URL can read the report.** There is no password and no second
factor: the token is the whole of the authorisation. It is 256 bits from
`crypto/rand`, looked up against a unique index so a guess costs one probe.

What the public path does:

- Serves the **stored artifact, never a re-render**. The figures a client
  bookmarked do not change when retention drops a tier — and a public URL that
  triggered a full report computation would be a denial-of-service primitive
  pointed at your instance.
- Answers on a **separate public projection**, not a filtered view of the run.
  There is no run id, no template id, no schedule, no delivery log and no monitor
  identifier in the response, and no `target` anywhere in the document — a field
  cannot leak through a shape that has no place to put it.
- Carries `X-Robots-Tag: noindex, nofollow` and `Referrer-Policy: no-referrer`, on
  the refusals as well as the successes.
- Is rate limited per token.

Three answers, not two. `404` for no such link, **`410` for one that was revoked,
has expired, or whose files retention reclaimed**, and `429` for too many
requests. "It is gone" and "it was never here" are different facts, and only one of
them is true for somebody holding a bookmark.

One live link per run, enforced by the database. Creating a second is a `409`
rather than a silent replacement — quietly revoking a link a colleague already
sent to a client is a support call that starts with "the report link you sent me
stopped working". Revoke first, then create.

Revocation is immediate and **leaves the artifacts untouched**.

---

## Generating one now

```
POST /api/v1/report-templates/{id}/generate
```

Answers `202` with a run to poll, not the document — rendering fifty PDFs inside a
request is how the first of the month takes the monitoring down with it. Poll the
run, then download:

```
GET /api/v1/report-runs/{id}/download?format=pdf
```

An optional body sets an explicit window, which is what makes a re-run exact:

```jsonc
{ "period_start": "2026-03-01T00:00:00Z", "period_end": "2026-04-01T00:00:00Z" }
```

**The run's recorded window wins over the template's period.** Regenerating a
report after correcting an incident covers the same window as the one it replaces,
not whatever "last month" resolves to on the day you press the button.

---

## Not in this release

- **`logo_url`.** The field is defined and no operation serves the bytes, so it is
  emitted as `null` rather than naming an endpoint that answers `405`.
- **Automatic mirror reconciliation.** A failed upload stays `failed` on the
  artifact row until the next report is generated. Nothing sweeps the backlog and
  retries it, and nothing compares the bucket against the database. That is
  deliberate for this phase rather than an oversight — see
  [When the mirror fails](#when-the-mirror-fails).
