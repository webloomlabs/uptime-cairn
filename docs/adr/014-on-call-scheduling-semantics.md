# ADR-014: On-Call Scheduling Semantics — Local Wall-Clock Handoffs, Overrides Above All Layers, a Named Fallback for Gaps, and Durable Escalation

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Completes [ADR-013](013-incident-automation-and-alert-routing.md)**, which routes an incident to an escalation policy and leaves to this ADR the question of who a step resolves to. Assumes [ADR-010](010-tenancy-isolation-and-membership.md) and [ADR-011](011-authorisation-roles-and-grants.md).

> ADRs are **immutable once accepted** — if we later change our minds, write a
> new ADR that supersedes this one and update the Status line above.

> **How this ADR was written.** [AGENTS.md](../../AGENTS.md) rule 3 was waived by
> the maintainer on 2026-09-22 (precedent: [data model
> §11.6](../data-model/README.md#116-secrets-at-rest)). The decisions are the
> maintainer's, each put to them as a question with alternatives; the prose and
> the evidence are an agent's.

## Context

Phase 3's exit condition is that a 50-person engineering organisation can run
production on-call from this and nothing else. Nothing of on-call exists yet:
no schedules, rotations, overrides, holidays or escalation. What exists is the
spec draft and three pieces of prior art worth copying rather than
re-deciding.

**The spec already drafts the shape**, tagged `x-cairn-phase: 3` and unshipped,
therefore still editable: `OnCallSchedule` with rotations
(daily/weekly/biweekly/custom length, `handoff_time`, and a `restriction` for
business-hours-only routing), overrides, `OnCallShift`, `getWhoIsOnCall`, and
`EscalationPolicy` with ordered steps carrying `delay_minutes`, targets of type
user/team/schedule/notification_channel, and `repeat_count`.

**Time is the hard part, and the project has already been here.** Phase 2 fixed
that calendar report windows are cut in a stated timezone from
`GeneralSettings.Timezone`, on the explicit grounds that a monthly window
starting at midnight UTC "is wrong by a working day for an Australian agency and
will be reported as a bug". The same reasoning governs a handoff. There is also
a hand-written five-field cron parser in `internal/cron` with the
day-of-month/day-of-week union rule correct and tested, shared since Phase 2.

**Durability is a settled pattern, not an open question.** Phase 2's report
scheduler established that a missed schedule is **late, not lost**, that it is
visible as late, and that a run interrupted by a restart is finished on start-up
rather than forgotten. An escalation step pending in ten minutes is state of
exactly that kind.

**Delivery plumbing exists.** Thirteen native notification providers plus
Apprise, with per-attempt rows, retries and `last_error` surfaced in the UI.
A page is a payload, not a new transport.

## Decision

**1. Handoffs are local wall-clock times in the schedule's timezone.** A rotation
handing off at 09:00 in `Australia/Sydney` hands off at 09:00 local, always, so a
shift spanning a clock change is 23 or 25 hours long. Two edge cases are fixed
rather than left to the library:

- A handoff time that **does not exist** on a spring-forward night fires at the
  first valid instant after the skipped interval.
- A handoff time that **occurs twice** on a fall-back night fires **once**, on
  the first occurrence.

Both are asserted by table-driven tests across at least three zones, including a
zone with a half-hour offset, because that is where off-by-thirty-minutes bugs
live.

**2. Resolution order: override, then layers from highest priority down.** A
schedule has one or more layered rotations and any number of overrides.
`who_is_on_call(T)` is:

1. an override covering `T` → that person;
2. otherwise the highest-priority layer with coverage at `T`;
3. otherwise the next layer down, and so on;
4. otherwise nobody, which decision 3 handles.

**An override is an explicit human statement and nothing outranks it.** The
failure people least forgive is being paged for a shift they swapped away.
**Overlapping overrides are refused at creation**, with the conflict named,
rather than resolved by a precedence rule nobody will remember at 3am.

**3. A coverage gap pages the escalation policy's fallback target, and gaps are
surfaced in advance.** Every escalation policy names a fallback (a user, a team
or a notification channel) used whenever a step's schedule resolves to nobody —
a hole in the rota, or a holiday that removed the day. The page goes to the
fallback and the timeline records that it did, naming the time and the empty
schedule. Separately, the schedule editor **warns about gaps in the coming 28
days**, so a hole is found while someone is looking at a screen rather than
during an incident. A policy with no fallback is refused.

**4. Holiday calendars are manually entered dates.** An organisation creates a
named calendar and adds dated entries, each optionally repeating yearly on the
same date; a schedule may reference one calendar and skips its dates, falling
through to the next layer or to the fallback. **No iCalendar import and no
`RRULE` engine in this phase.** Dated entries meet the requirement, carry no
dependency, and add no outbound fetch.

**5. Escalation state is rows, not timers.** Each pending step is a row with the
instant it is due. A scheduler fires due steps, and on start-up it picks up steps
whose time passed while the process was down — firing them **late and marked
late in the timeline**, never silently dropping them, exactly as the report
scheduler treats a missed run. Each step fires at most once per incident per
repeat, enforced in the database rather than by in-process bookkeeping, so a
restart mid-escalation cannot double-page. Acknowledgement
([ADR-013](013-incident-automation-and-alert-routing.md)) cancels the remaining
steps.

**6. Escalation runs off the check path**, on a bounded worker pool, as report
generation does. Paging fifty people must not delay a single check, and the load
gate says so rather than the author.

**7. A step resolves through to people, and people choose their own channels.**
A target of type user resolves to that user; team to its members; schedule to
whoever decision 2 returns; notification channel to that channel directly. For a
person, delivery uses their **personal notification rules**: which of their
contact methods receive pages and which receive lower-priority updates, built on
the existing providers. Every user can **send a test page to themselves**,
because the first time somebody discovers their phone number is wrong must not be
during an incident.

**8. Business-hours routing uses the drafted rotation `restriction`** (weekdays,
start and end time), evaluated in the schedule's timezone under the same
wall-clock rule as decision 1. A layer restricted to business hours simply has no
coverage outside them, which is what makes it compose with decision 2 instead of
needing a rule of its own.

**9. Quiet hours hold, they do not drop.** A per-user quiet-hours window
withholds non-urgent notifications and delivers them afterwards as a digest.
**Pages for an incident are never held** — a quiet hour that silences the pager
is a pager that does not work — and the policy states which notifications count
as urgent, visibly, in the UI.

## Consequences

**What this makes easy.**

- **The rota means what it says on the wall.** 09:00 is 09:00 all year, which is
  how every team describes its own handoff.
- **A swapped shift is honoured**, unconditionally, which is the property that
  determines whether people trust the system at all.
- **A gap is loud, twice**: visible in the editor weeks ahead, and named in the
  timeline if it is ever reached. Nothing is silently lost.
- **A restart is survivable.** Escalation resumes, late steps are visible as
  late, and nobody is double-paged. Copied from a pattern this project has
  already demonstrated under test.
- **No new dependency and no new infrastructure.** Dates, rows, the existing
  providers, the existing cron parser.

**What this makes hard, or forecloses.**

- **Unequal shift lengths.** A 23-hour and a 25-hour shift occur twice a year.
  Anyone computing fairness from shift duration will find it uneven, and the
  calendar view must show real local times rather than durations.
- **Overlapping overrides are refused**, so "Dana covers Saturday, and actually
  Sam covers Saturday afternoon" needs the first override edited rather than a
  second added. Simpler to reason about, and more clicks.
- **Planned exceptions cannot outrank an individual override.** An organisation
  wanting a release-weekend layer to beat everybody's personal swaps cannot
  express that; it must edit the overrides.
- **Holiday calendars are manual.** Somebody enters next year's public holidays
  once a year, and a calendar left un-updated silently stops skipping anything.
  The editor's gap warning is the mitigation, and it is partial.
- **A fallback target is mandatory**, which is one more thing to configure before
  a policy can be saved. Chosen over the alternative of a page with nowhere to
  go.
- **Quiet hours add a held-notification state** that must be visible, or a user
  will believe a notification was lost.
- **No mobile push acknowledgement** in this phase; acknowledgement arrives
  through the signed link of ADR-013 on existing channels.

**What becomes expensive to reverse later.**

- **Wall-clock handoffs, from the first release with a rotation.** Switching to
  fixed-interval handoffs later would move every existing rota's handoff time
  twice a year.
- **Override-above-layers, from the first release.** Reversing it would page
  people for shifts they swapped away — the one failure that loses trust
  permanently.
- **Escalation rows and their at-most-once guarantee**, once installs have
  escalation histories.
- **Manual holiday entries are cheap to extend**: adding an iCalendar importer
  later writes the same rows. That is why the bounded version is safe to ship
  first.

## Alternatives considered

Recorded as they were put to the maintainer.

**Fixed UTC-offset handoffs (every handoff exactly 168 hours after the last).**
Shifts are always equal length and the arithmetic is trivial, with no DST edge
cases at all. It lost because the handoff would drift to 08:00 or 10:00 local
twice a year, which every user would read as a bug — the same reasoning Phase 2
used for calendar report windows.

**Layers beat overrides.** More expressive for a planned event: a release-weekend
layer would take precedence over individual swaps. It lost because a person who
swapped a shift could still be paged, which is the failure people least forgive
and the fastest way to lose trust in an on-call tool.

**Escalate immediately to the next step on an empty schedule.** No new concept to
configure, and an empty step simply skipped. It lost because if no step resolves
to anybody, the page is silently lost — the single worst outcome available to a
paging system.

**Page every organisation admin on a gap.** A guaranteed recipient with nothing
to configure. It lost because it pages people who may have no context for the
service, and an organisation with twenty admins would send twenty pages.

**iCalendar (.ics) import for holidays.** Real public-holiday feeds, and the
obvious route. It lost on scope: full `RRULE` support is far larger than it
appears, and a fetched URL is an outbound dependency on a third party inside the
paging path. Deferred rather than rejected — the data model is the same rows.

**No holiday calendars in Phase 3.** Smallest surface; overrides express a
holiday by hand. It lost because the roadmap lists holiday calendars explicitly
and manual dated entries are cheap enough that cutting them buys little.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved. Nothing here touches the
      first-run path.
- [x] Nothing is paywalled in the open source build. Schedules, rotations,
      overrides, holidays and escalation ship in the open source build.
- [x] API-first. Schedules, overrides, holiday calendars, policies, personal
      notification rules and the test page are API operations, specified in the
      Phase 3 spec draft before any handler.
- [x] Progressive disclosure. No on-call UI until an organisation creates a
      schedule or a policy.
- [ ] The client is never sent full state; the UI stays fast at 5,000 monitors.
      **Not yet demonstrated.** `who_is_on_call` is a bounded computation over
      one schedule's layers and overrides, and the calendar view renders a
      bounded window — but escalation during the harness's total partition is
      the real test, and the gate must show check lateness within the same
      tolerance the report burst uses.
- [x] Solo mode keeps zero required external dependencies. Escalation state is
      rows in the existing database; no broker and no external scheduler.
- [x] Dependency surface stays minimal. Nothing new is introduced; the existing
      cron parser and notification providers are reused, and the `RRULE` engine
      an iCalendar importer would have needed is deliberately avoided.

## References

- [ADR-013](013-incident-automation-and-alert-routing.md): what delivers an
  incident to a policy, and the acknowledgement that cancels the remaining
  steps.
- [`docs/api/openapi.yaml`](../api/openapi.yaml): the drafted `OnCallSchedule`
  (rotation lengths, `handoff_time`, `restriction`), `OnCallShift`,
  `EscalationPolicy` (`delay_minutes`, target types, `repeat_count`) and
  `getWhoIsOnCall` — unshipped and therefore editable, and reconciled against
  this ADR in the Phase 3 spec draft.
- [PHASE-2-PLAN.md](../plans/PHASE-2-PLAN.md) §4.2 and §5: calendar windows cut
  in a stated timezone, the missed-run-is-late rule, and the bounded worker pool
  off the check path — the three patterns decisions 1, 5 and 6 copy.
- `internal/cron`: the shared five-field parser available to schedules.
- `internal/notify`: the thirteen providers, per-attempt rows, retries and
  `last_error` that decision 7 reuses rather than reinventing.
- [PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md) §3.5, §4.8, §4.9 and §8: the
  questions as posed, and the gate the escalation work must pass.
- Deferred, named so they are refused consistently: iCalendar import, computed
  (`RRULE`) holidays, mobile push acknowledgement, phone-call escalation beyond
  the existing Twilio provider, and per-recipient timezones for digests.
