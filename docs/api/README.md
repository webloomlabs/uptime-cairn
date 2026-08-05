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
| Operations | 124 |
| Schemas | 122 |
| Phase 1 operations | 90 |
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

**Pagination.** Two models, chosen by the shape of the data:

- Collections a UI renders page controls for use `page` / `per_page` and return
  `pagination` with a `total`.
- Append-only time series — heartbeats, webhook deliveries — use an opaque
  `cursor`, because an offset into a table being written to continuously is not
  a stable position.

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

## Open questions — these need a human decision before freeze

Per [AGENTS.md](../../AGENTS.md) §3, the API contract is an architectural
decision requiring a human-authored ADR. The following were settled in this
draft only so that it would be coherent enough to review. Each is a real
decision, and none of them is mine to make:

1. **Pagination model.** Page-number for collections, cursor for time series.
   The alternative — cursor everywhere — is more correct under concurrent
   writes but costs the dashboard its page controls and result counts. This
   interacts directly with ADR-004 and belongs in it or alongside it.

2. **Error format.** RFC 9457 problem documents with stable `type` URIs. The
   alternative is a simpler bespoke `{error: {code, message}}` envelope. RFC
   9457 is more work to implement and better for the generated clients.

3. **ADR-004 does not exist yet, and this spec has a hole where it goes.**
   The spec covers server-side pagination, filtering, and search — but nothing
   about **live updates**. The dashboard needs scoped incremental subscriptions
   (the whole point of ADR-004, and the specific thing Uptime Kuma got wrong),
   and no transport for them is specified here: no WebSocket, no SSE, no
   long-poll. I deliberately did not invent one. Whatever ADR-004 decides needs
   its own section in this spec before freeze, and it is the single largest gap
   in this draft.

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

- **Live updates.** See open question 3 — this is a gap, not a decision.

- **OpenTelemetry export.** Named in Phase 0 §3.3 and Phase 1 §3.6. It is
  configuration and an outbound exporter rather than a REST surface, so it
  belongs in the settings design and the ops documentation. Say so explicitly if
  it should instead appear here.
