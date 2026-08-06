# Uptime Cairn API v1

The REST API specification lives in [openapi.yaml](openapi.yaml). It is written
in OpenAPI 3.1 and validates clean against `openapi-spec-validator`.

**Status: draft, not frozen.** Phase 0 exits when this spec is
"frozen, published, and reviewed publicly — detailed enough that a stranger
could implement a conforming server or client against it"
([PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §5). This draft exists to be argued
with. Nothing in it is settled until it is published for comment, revised, and
frozen at the end of week 4.

---

## What is in it

| | Count |
|---|---|
| Operations | 125 |
| Schemas | 124 |
| Phase 1 operations | 91 |
| Phase 2 operations | 14 |
| Phase 3 operations | 20 |

Entity coverage follows [PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §3.3:
monitors, tags, groups, notifications, status pages, incidents, maintenance
windows, users, teams, schedules, and reports — plus API keys, outbound
webhooks, the Kuma importer, settings, system metadata, and the public status
page and push-ingest paths.

The probe-facing protocol is **not** here. That is gRPC + Protobuf per
[ADR-001](../adr/001-probe-and-control-plane-split.md), and it is a separate
Phase 0 deliverable (§3.5).

## Conventions

**Versioning.** Everything lives under `/api/v1`, except `/healthz`, `/readyz`,
and `/metrics`, which are operational endpoints outside the versioned surface.

**Authentication.** Two schemes, either sufficient: a scoped bearer API key
(`Authorization: Bearer cairn_<key>`), or the browser session cookie the
dashboard uses, which additionally requires `X-Cairn-CSRF-Token` on writes.
Operations tagged `Public` require neither.

**Scopes.** Every operation declares the scope it needs in `x-cairn-scopes`.
Scopes are `<resource>:<read|write>`, and `write` implies `read` on the same
resource. A key cannot be granted a scope its creator does not hold.

**Pagination.** One model, everywhere: an opaque `cursor` keyed on
`(updated_at, id)`, per [ADR-004](../adr/004-ui-state-synchronisation.md).
Applied uniformly — there is no small-install exception where the full set is
sent because it happens to fit today.

Cursor responses carry **no total count**, because producing one costs a scan of
the filtered set on every page fetch. A count comes from
`GET /api/v1/monitors/membership`, which a client tracking a live view is
already polling.

**Live updates.** A client subscribes to exactly the monitor IDs on its screen
and receives `MonitorStatusDiff` messages for those alone, so push volume is
bounded by viewport size rather than by monitor count. Membership of *filtered*
views is reconciled by polling, not by the server evaluating live predicates per
client. See [Live updates](#live-updates) below.

**Errors.** RFC 9457 problem documents (`application/problem+json`). Clients
branch on the `type` URI, never on `title` or `detail`, both of which are prose.
Validation failures add an `errors` array of JSON pointers.

**Nullability.** OpenAPI 3.1, so nullable fields are `type: [string, "null"]`.
There is no `nullable` keyword.

**Read-only fields.** Server-managed fields are marked `readOnly` and are always
present on reads. The `required` lists describe what a client must send when
writing. This is why `MonitorWrite` is an alias of `Monitor` rather than a
parallel schema — one shape, with `readOnly` doing the work.

**Polymorphism.** Monitors and notification channels are `oneOf` with a `type`
discriminator, so `oapi-codegen` and `openapi-typescript` generate usable
tagged unions rather than a bag of optional fields.

**Phase gating.** Every operation carries `x-cairn-phase`. Phase 1 contract
tests select `x-cairn-phase: 1`; the later-phase operations are specified now so
that shipping them in Phase 2 and 3 is additive rather than a v2.

## Compatibility promise

Within `/api/v1`:

- Fields may be **added**. Fields are never removed or retyped.
- Enum values may be **added**. Clients must tolerate values they do not
  recognise — `MonitorType`, `NotificationChannelType`, `EventType`, and
  `ApiKeyScope` are all expected to grow.
- Endpoints may be added. Existing endpoints do not change semantics.
- Anything breaking goes to `/api/v2`.

**Deprecation.** A deprecated operation or field is marked `deprecated: true` in
the spec, announced in the release notes of the version that deprecates it, and
kept working for **no less than two minor releases or six months, whichever is
longer**. Responses from deprecated endpoints carry a `Deprecation` header and a
`Sunset` header naming the removal date.

## Working with the spec

```bash
# Validate
python3 -m pip install openapi-spec-validator
python3 -c "from openapi_spec_validator import validate; \
  from openapi_spec_validator.readers import read_from_filename; \
  validate(read_from_filename('docs/api/openapi.yaml')[0])"

# Browse locally
npx @redocly/cli preview-docs docs/api/openapi.yaml
```

Generated Go and TypeScript clients are published from this file per
[PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §3.6. Contract tests in CI verify
the server against it — the spec is the source of truth, and a server that
disagrees with it is the thing that is wrong.

---

## Live updates

[ADR-004](../adr/004-ui-state-synchronisation.md)'s client-facing contract. It is
not REST, so OpenAPI cannot express it directly; the message shapes live in the
spec as documented-only schemas (`MonitorStatusDiff`, `OverviewDiff`) that no
endpoint returns, the same pattern `EventEnvelope` uses for outbound webhooks.

**Transport.** NATS in scaled mode, an in-process bus in solo mode. Both present
identical message shapes and identical subscribe semantics, so a frontend never
knows which it is talking to — the ADR's open follow-up, resolved here by stating
it as a requirement of the contract rather than an implementation detail.

**Subjects.**

| Subject | Carries |
|---|---|
| `updates.{org_id}.{monitor_id}.status` | `MonitorStatusDiff` for one monitor |
| `updates.{org_id}.summary` | `OverviewDiff`, the global header counts |

`org_id` appears in the subject while the REST surface exposes no tenancy field
at all, per ADR-003. That is consistent rather than contradictory — the segment
is a broker-level isolation backstop, not an API concept — and ADR-003's
compliance note about there being no tenancy API surface still holds.

**The client's loop, which is the whole design in four steps:**

1. Fetch a page of monitors with a cursor and filters.
2. Subscribe to exactly those monitor IDs. Unsubscribe on paginate or unmount.
3. Apply `MonitorStatusDiff` messages in place. Push volume is bounded by
   viewport size, never by total monitor count — this is what makes the Kuma
   fan-out failure structurally impossible rather than merely tuned around.
4. Poll `GET /api/v1/monitors/membership` every ~5s with the same filters. If
   `version` or `count` changed, re-fetch the affected page boundary.

Step 4 exists because steps 2–3 cannot see monitors that are off-screen. A
monitor that goes down elsewhere and now matches a `status=down` filter has no
subscription telling the server anyone cares — so filtered views are eventually
consistent, bounded by the poll interval. That staleness is the trade ADR-004
accepted deliberately, in exchange for not running a predicate-evaluation service
that scales with connected clients.

**A client must not** derive the global summary by summing what it is subscribed
to. That would couple a global number to viewport state and make the header
disagree with itself as the user paginates. Use `OverviewDiff` or
`GET /api/v1/overview`.

---

## Open questions — these need a human decision before freeze

Per [AGENTS.md](../../AGENTS.md) §3, the API contract is an architectural
decision requiring a human-authored ADR. The following were settled in this
draft only so that it would be coherent enough to review. Each is a real
decision, and none of them is mine to make:

1. ~~**Pagination model.**~~ **Resolved 2026-08-06** — reconciled with ADR-004.
   All 17 collection endpoints now use `(updated_at, id)` cursors;
   `page`/`per_page` and the `PagePagination` schema are gone. See the new
   consequence in question 8, which this created.

2. **Error format.** RFC 9457 problem documents with stable `type` URIs. The
   alternative is a simpler bespoke `{error: {code, message}}` envelope. RFC
   9457 is more work to implement and better for the generated clients.

3. ~~**ADR-004's surface is missing.**~~ **Resolved 2026-08-06.**
   `GET /api/v1/monitors/membership` returns the `MembershipSignal`; the channel
   contract, subjects, and `MonitorStatusDiff` / `OverviewDiff` shapes are
   specified under [Live updates](#live-updates).

   The signal returns **both** a version and a count, rather than the ADR's
   "version counter, *or* count+hash". Either alone is insufficient: a monitor
   leaving a `status=down` filter as another enters keeps the count identical
   while the view is stale, and a version that changes on every heartbeat would
   fire on essentially every poll at 250 writes/second. The pair is the smallest
   thing that actually works, and it matches the mechanism chosen in the data
   model's §6.5.

4. **The `include` parameter.** `include=last_heartbeat,uptime` on monitor
   lists is a per-row cost multiplier at 5,000 monitors, and it is exactly the
   sort of convenience that quietly fails a load-test gate. Worth deciding
   deliberately: keep it, cap it, or drop it in favour of the dedicated
   endpoints.

5. **Bulk operation ceiling.** 1,000 monitors per bulk call, with partial
   success reported per identifier rather than an all-or-nothing transaction.
   An agency at 1,000 monitors will hit this on day one.

6. **Retention defaults.** The per-tier defaults in `Settings.retention` (7 days
   raw, 30 at 1m, 90 at 5m, 365 at 1h, indefinite at 1d) are placeholders that
   look plausible. They should come from the ADR-002 rollup design and the
   load-test harness's actual disk numbers, not from me.

7. **Authentication surface.** [AGENTS.md](../../AGENTS.md) §8 says security
   work is human-led. The login, TOTP, session, CSRF, API-key, and status-page
   password endpoints are specified here because Phase 0 §3.3 requires scoped
   API keys to be specified — but the whole of `Setup`, `Authentication`, and
   `API Keys` should get a deliberate security review before freeze, not a
   skim.

8. **Sorting monitors by anything other than `updated_at` is now impossible —
   and that is a UX regression worth your explicit decision.**

   A keyset cursor can only paginate the ordering it is keyed on, and ADR-004
   fixes that key as `(updated_at, id)`. So the `sort` parameter on
   `listMonitors`, which previously offered name, status, `last_check_at`, and
   `uptime_24h`, is now restricted to `updated_at` ascending or descending. A
   dashboard listing 5,000 monitors cannot be sorted alphabetically.

   I kept the spec strictly ADR-compliant rather than quietly widening an
   accepted, immutable decision. Three ways forward, and this is yours to pick:

   - **Accept it.** Filter and search replace sorting. Defensible — the ADR's
     ordering is "most recently changed first", which is arguably what a
     monitoring dashboard wants anyway.
   - **Generalise the cursor to `(sort_field, id)`.** Standard keyset
     pagination, and it satisfies ADR-004's stated *reason* — sorting by name is
     stable precisely because names do not change in real time, so it does not
     reintroduce the reordering problem the ADR was avoiding. Costs one index
     per sortable field (the data model's §6.2 index budget already feels the
     strain) and needs a superseding ADR, since the current one names
     `(updated_at, id)` specifically.
   - **Sort client-side within a page.** Do not — it orders 25 rows out of
     5,000 and looks like a bug to the user.

   My read is that the second option is what you actually want and that ADR-004
   would have said so had the question come up, but extending an accepted ADR is
   not mine to do.

## Discrepancies found in the existing plans

Both of these are small, and both mean a published number does not match a
published list. Worth resolving before either document is quoted at anyone:

- **Monitor types: 10 or 9?** [PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §3.1
  is headed "Monitor Types (10)" and lists nine: HTTP/HTTPS, TCP, ICMP, DNS,
  SSL/TLS expiry, domain expiry, push, Docker, gRPC.
  [ROADMAP.md](../../ROADMAP.md) lists the same nine. The spec has nine.

- **Native alert channels: 13 or 12?** Both plans say "13 native channels +
  Apprise" and then list twelve: email, webhook, Slack, Discord, Telegram,
  Matrix, Gotify, ntfy, Microsoft Teams, PagerDuty, Opsgenie, Twilio. The spec
  has those twelve plus Apprise, which makes thirteen channel types in total —
  which may well be what was meant.

## Deliberately absent

- **Tenancy.** No `org_id` field and no organisation endpoints, per
  [ADR-003](../adr/003-tenancy-model.md): the column is inert schema
  infrastructure in Phase 1, and the ADR's compliance checklist states that no
  tenancy-specific API surface exists yet. Phase 3 adds it.

- **The probe protocol.** gRPC, separate deliverable, [ADR-001](../adr/001-probe-and-control-plane-split.md).

- **A REST transport for live updates.** The channel is specified under
  [Live updates](#live-updates), but it is a message bus, not HTTP — there is no
  WebSocket or SSE endpoint in this spec, by design. `MonitorStatusDiff` and
  `OverviewDiff` are carried by NATS or the in-process bus, and appear here only
  as documented schemas.

- **OpenTelemetry export.** Named in Phase 0 §3.3 and Phase 1 §3.6. It is
  configuration and an outbound exporter rather than a REST surface, so it
  belongs in the settings design and the ops documentation. Say so explicitly if
  it should instead appear here.
