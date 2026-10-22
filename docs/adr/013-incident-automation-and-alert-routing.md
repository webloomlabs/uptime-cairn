# ADR-013: Incident Automation and Alert Routing — Policy-Gated Auto-Opening, Cause-Based Correlation, and Escalation as a Second Delivery Path

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** Independent of [ADR-010](010-tenancy-isolation-and-membership.md) and [ADR-011](011-authorisation-roles-and-grants.md), which it assumes. Constrains [ADR-014](014-on-call-scheduling-semantics.md), which decides who an escalation step resolves to.

> ADRs are **immutable once accepted** — if we later change our minds, write a
> new ADR that supersedes this one and update the Status line above.

> **How this ADR was written.** [AGENTS.md](../../AGENTS.md) rule 3 was waived by
> the maintainer on 2026-09-22 (precedent: [data model
> §11.6](../data-model/README.md#116-secrets-at-rest)). The decisions are the
> maintainer's, each put to them as a question with alternatives; the prose and
> the evidence are an agent's. Alternatives records the options as presented.

## Context

The roadmap requires incidents that "open **automatically** from failing checks,
with acknowledgement, assignment, a threaded timeline, status page linkage, and
auto-resolve", plus "alert deduplication and grouping". What exists today is
precisely the half that can be built without deciding anything:

- **The incident record is ready.** `model.Incident` carries `AutoOpened`,
  `DetectedAt`, `AcknowledgedAt`, `AcknowledgedBy` and `AssignedTo`, with
  MTTD/MTTA/MTTR derived on read. The comment on `AutoOpened` says "Phase 3 sets
  it; Phase 1 never does". **Nothing sets any of those five fields today**, and
  `POST /incidents/{id}/acknowledge` is in the spec with no route behind it.
- **The state decision lives in one function.** `apply()` in
  [`internal/controlplane/ingest.go`](../../internal/controlplane/ingest.go)
  decides pending, down and up, already applies retries
  (`ConsecutiveFailures > Retries`), maintenance suppression via
  `monitor_state.suppressed_by`, and dependency suppression by walking
  `ParentMonitorID` up to a depth of 10. Auto-opening hangs off this one place.
- **Incident events reach nobody.** The notify dispatcher resolves channels only
  through `ChannelsForMonitor(ev.Monitor.ID)`, and an incident event deliberately
  carries no monitor — the comment in `internal/notify/event.go` explains why:
  an incident is not about one monitor, and picking one would make
  `{{monitor.name}}` render something arbitrary. So `incident.*` events reach
  outbound webhooks and **no notification channel at all**.
- **Deduplication exists only at the provider.** `DedupKey()` is
  `"cairn-monitor-<id>"`, used as PagerDuty's `dedup_key` and Opsgenie's alias,
  with auto-resolve/auto-close already wired (`internal/notify/oncall.go`).
  There is no grouping, no escalation and no quiet hours.

Two external facts bound the design. **The load harness already simulates a
total partition** at 5,000 monitors and asserts that every monitor is marked down
within two intervals, all recover, and no alert is dropped — so whatever is
decided here is measured, not asserted. And **v1.1.0 is tagged**, so every
existing alerting behaviour is shipped surface that an upgrade must not change.

## Decision

**1. Automatic opening is off by default and enabled by policy.** An
*incident policy* selects monitors (explicit ids, a group, or a tag — the same
selector shape report templates already use) and says what to do when one goes
down: open an incident, with a stated impact, optional status-page linkage, and
an optional escalation policy. **An install that upgrades behaves exactly as
1.1.x until somebody creates a policy.** Incidents stay human-declared by
default, which is what the Phase 2 reporting guide already tells users.

**2. The trigger is the existing `down` transition.** No second threshold is
introduced. A monitor is down when `apply()` says it is — after its retries are
exhausted and after maintenance and dependency suppression have been applied — and
that is the moment a policy may open an incident. Retries already are the
blip filter; a second one next to it would be two mechanisms for one job and
would inflate MTTD by its own length.

**3. `detected_at` is the first failing check, not the transition.** The
transition happens after retries, which can be minutes later. Time-to-detect is
the one figure that cannot be reconstructed afterwards (the comment in
`internal/model/incident.go` says so), and recording the transition instant
would silently understate it by the retry budget. `auto_opened` is set true.
Phase 2's post-mortem renderer then shows real MTTD with no change to
`internal/report`.

**4. Correlation is by cause, in a fixed order: dependency root, then group,
then the policy's selection.** When a monitor goes down and a policy applies:

- If any ancestor in its `ParentMonitorID` chain is also down, the failure joins
  (or creates) the incident rooted at the **highest** such ancestor. This reuses
  the chain `apply()` already walks.
- Otherwise, if an open auto-opened incident exists for the same group within
  the correlation window, the monitor joins it.
- Otherwise, if one exists for the same policy selection within the window, the
  monitor joins it.
- Otherwise a new incident is opened.

Joining an incident adds the monitor to `incident_monitors` and a system entry
to the timeline. **It does not page again** (decision 6). The correlation window
is a policy setting with a short default.

**5. Auto-resolve requires every linked monitor up, plus a quiet period.** When
all monitors linked to an auto-opened incident are up and stay up for the quiet
period (a policy setting, defaulting to a few minutes), the incident resolves
with the reason in the timeline. A monitor flapping down resets the timer rather
than resolving and reopening. **An incident whose state a human has changed is
never auto-resolved** — a human's judgement outranks the scheduler's, and an
incident someone is actively working is not closed underneath them.

**6. Incidents route through escalation policies. Monitor alerting is
unchanged.** Two delivery paths with two purposes:

- `monitor.up` / `monitor.down` / `monitor.pending` continue to go to the
  monitor's own notification channels, exactly as they do today. **No shipped
  behaviour changes.**
- `incident.*` events are delivered through the incident's escalation policy:
  policy → step → target (user, team, schedule or notification channel, as the
  spec already drafts) → for a person, their own notification rules
  ([ADR-014](014-on-call-scheduling-semantics.md)). This is the second
  resolution path the notify dispatcher needs, and it is keyed on the incident
  rather than on a monitor, which is why the empty monitor envelope stays empty.

One page per incident per escalation step. Not one per failing check, not one per
retry, and not one per monitor that joins.

**7. Native escalation does not silently displace an external pager.** Where a
monitor is covered both by a PagerDuty or Opsgenie channel and by a native
escalation policy, **both keep working** and the UI warns at save time, naming
the overlap. An install that upgrades has no policies, so it has no overlap. The
operator decides; the product does not quietly switch off an integration a team
depends on. The existing per-monitor `DedupKey()` is unchanged, so an
organisation that stays on PagerDuty is unaffected by this ADR entirely.

**8. Acknowledgement stops escalation.** `POST /incidents/{id}/acknowledge`
stamps `acknowledged_at` and `acknowledged_by`, halts pending escalation steps,
and writes a timeline entry. It is granted to `responder` and above
([ADR-011](011-authorisation-roles-and-grants.md)). Acknowledgement is also
possible from a notification through a signed, single-use, expiring link, so
nobody has to find a laptop at 3am — **that link is authentication-adjacent and
is human-led work under [AGENTS.md](../../AGENTS.md) rule 8.**

**9. Status page linkage is opt-in per policy.** An automatic incident links to a
public status page only when its policy says so. A customer-facing page must
never announce an internal check failing because somebody enabled automation.

**10. The partition bound is the acceptance test.** With a policy covering every
monitor, the harness's total-partition scenario must produce **one incident per
dependency root or group**, not one per monitor, and **one page per incident per
step**. The number is asserted in the load gate, and `cairn_alerts_dropped_total`
stays at zero. A design that reduces noise in prose and not in the gate has not
reduced noise.

## Consequences

**What this makes easy.**

- **The upgrade is silent.** No policies means no incidents, no escalation, no
  behaviour change — which is what lets this ship in a phase that is already
  changing tenancy underneath everything.
- **MTTD becomes real** for auto-opened incidents, and Phase 2's post-mortems
  improve without being touched.
- **One incident per cause.** A parent and forty children produce one incident
  whose timeline names all forty, which is the shape a post-mortem wants.
- **Monitor alerting is untouched**, so every existing install, template and
  integration keeps working, and the risky new path is additive.
- **Grouping reuses the dependency chain** that already exists and is already
  tested, rather than inventing a correlation engine.

**What this makes hard, or forecloses.**

- **Automation is invisible until configured**, so the roadmap's "incidents open
  automatically" is true only of an install that asked for it. Defensible, and it
  must be said plainly in the docs rather than implied away.
- **Correlation depends on the dependency graph being modelled.** An install that
  never set `ParentMonitorID` and puts everything in one group gets one incident
  per group, which may be too coarse; one that uses neither gets one per
  monitor, which is the noise case. The schedule editor cannot fix this; the
  documentation must explain that dependencies are what make grouping work.
- **A joined monitor does not page.** If a second, unrelated-feeling failure
  joins an existing incident, the on-call person is not paged again and will
  only see it in the timeline. That is the intended trade (noise), and it is
  the most likely complaint after a real incident.
- **Double paging is possible by configuration**, for a team that has both a
  PagerDuty channel and a native policy. A warning is not prevention. Chosen
  over silently suppressing an integration.
- **The quiet period delays resolution**, so an incident stays open for minutes
  after recovery and MTTR reads slightly long. Stated on the incident.
- **Two delivery paths is more code than one**, and a reader of the dispatcher
  now has to know which path an event takes.

**What becomes expensive to reverse later.**

- **The correlation order, from the first release that opens incidents
  automatically.** Installs will have incident histories whose shape depends on
  it, and post-mortems already written cite them.
- **`detected_at` meaning first-failure, from the first auto-opened incident.**
  Changing it later makes MTTD incomparable across time, which is worse than
  either definition.
- **Escalation as the incident path, from the first policy.** Moving incidents
  back onto monitor channels later would double-notify every existing install.
- Policy-gated opening is cheap to reverse — a default can be changed — and
  turning it on by default later is additive. That asymmetry is why off is the
  right default to ship.

## Alternatives considered

Recorded as they were put to the maintainer.

**Auto-open on by default for every down transition.** Nothing to configure, and
MTTD becomes real everywhere immediately. It lost because an upgrade would
suddenly produce an incident for every blip on every install, and because the
worst case is the 5,000-monitor partition the harness already simulates.

**Auto-open above a sustained-failure threshold (N minutes or N checks beyond
retries).** Fewer incidents from blips with no configuration at all. It lost on
two counts: it is a second retry mechanism beside the existing one, and it
inflates MTTD by exactly its own length, which corrupts the one metric this
feature exists to make honest.

**One incident per monitor.** Simplest and most predictable, and each timeline is
exactly one monitor's story. It lost because a partition would open hundreds of
incidents and the roadmap's grouping requirement would go unmet.

**One incident per policy per correlation window.** The strongest noise
reduction: everything a policy covers that fails in the window is one incident.
It lost because one incident could then mix unrelated causes, which makes the
post-mortem it feeds misleading — and a misleading post-mortem is worse than a
noisy pager.

**Escalation replaces monitor-bound alerting where a policy exists.** One path per
monitor, so double notification is structurally impossible. It lost because
adding a policy would change a shipped behaviour — a monitor's channels would
stop firing — which is precisely the kind of surprise an upgrade must not
contain.

**Incidents also notify every affected monitor's channels.** Nobody could miss
anything. It lost because it guarantees duplicate notifications, which is the
problem this phase is meant to reduce.

**Immediate auto-resolve when all monitors recover.** No quiet period, fastest
and simplest. It lost because a flapping monitor would produce a stream of
opened-and-resolved incidents, each with its own pages.

**Never auto-resolve; a human closes.** A forcing function for post-mortems. It
lost because the roadmap lists auto-resolve explicitly and an unattended install
would accumulate stale incidents.

**An incident policy suppresses external pager channels.** Guarantees one page
per incident. It lost for the same reason as the escalation-replaces option: it
silently disables an integration a team relies on.

**No special handling of external pagers.** Least code. It lost because the first
real incident after an upgrade would page everybody twice.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved. Nothing here is on the
      first-run path; a new install has no policies.
- [x] Nothing is paywalled in the open source build. Automatic incidents,
      grouping and escalation ship in the open source build.
- [x] API-first. Policies, acknowledgement and the timeline are API operations,
      specified in the Phase 3 spec draft before any handler.
- [x] Progressive disclosure. No incident-policy or escalation UI until an
      organisation creates one; the solo user's incident screen is unchanged.
- [ ] The client is never sent full state; the UI stays fast at 5,000 monitors.
      **Not yet demonstrated, and this is the decision most able to break it.**
      Correlation runs on the ingest path, which is the hottest path in the
      product. Ancestor-chain lookup is already bounded at depth 10 and
      already happens for suppression; the correlation lookup must be bounded
      and indexed, and the load gate with a policy covering 5,000 monitors is
      what proves it. **Escalation work runs off the check path**, like report
      generation.
- [x] Solo mode keeps zero required external dependencies. No broker, no queue,
      no external scheduler; escalation state is rows.
- [x] Dependency surface stays minimal. Nothing new is introduced.

## References

- [`internal/controlplane/ingest.go`](../../internal/controlplane/ingest.go):
  `apply()`, the retry rule, maintenance suppression via `suppressed_by`, and the
  depth-10 `ParentMonitorID` walk this ADR reuses for correlation.
- [`internal/model/incident.go`](../../internal/model/incident.go): the five
  fields nothing currently sets, and the comment on time-to-detect that
  decision 3 follows.
- `internal/notify/event.go`: why an incident event carries no monitor, and the
  per-monitor `DedupKey()` decision 7 leaves alone.
- `internal/notify/oncall.go`: the existing PagerDuty/Opsgenie dedup and
  auto-resolve behaviour.
- `internal/report/postmortem.go`: the Phase 2 consumer of `AutoOpened` and
  `DetectedAt`, which decisions 3 improves without modifying.
- [`docs/api/openapi.yaml`](../api/openapi.yaml): the drafted
  `acknowledgeIncident` operation and the `EscalationPolicy` schema whose target
  types decision 6 adopts.
- [docs/guides/sla-methodology.md](../guides/sla-methodology.md): states that
  nothing raises incidents automatically before Phase 3, and must be updated with
  this.
- `harness/` and `.github/workflows/load-test.yml`: the total-partition scenario
  that decision 10 turns into the acceptance test.
- [PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md) §3.4, §4.7, §4.8 and §8: the
  questions as posed and the gate they feed.
