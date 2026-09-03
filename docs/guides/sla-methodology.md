# SLA methodology

**This is the page to read when a figure is disputed.** It states exactly what
counts as downtime, what leaves the denominator, what the maintenance default is,
and which of those choices a report prints on its own face.

Everything here is what the product actually does. Where a figure cannot be
computed honestly it is absent with a reason beside it, and the reasons are
listed too.

---

## Uptime

> **Uptime is the share of observed checks that succeeded.**

```
uptime = up / (up + down)
```

That denominator is the whole of the argument, so it is worth being slow about.

### What counts as down

A check counts as **down** when it failed: no answer, a wrong answer, or an answer
slower than the monitor's configured `response_time_threshold_ms`.

Those three are not distinguishable in stored history. A threshold breach is
recorded as `down` with nothing beside it to say which rule it broke, so **a
report never claims a service "was slow"** — it says whether a response-time
target was met, which is a claim about a rule somebody agreed to rather than about
an experience nothing measured. The report's methodology note says so on its own
face wherever a response-time target is set.

### What leaves the denominator entirely

| State | Meaning | Effect on uptime |
|---|---|---|
| `up` | The check succeeded. | Numerator and denominator. |
| `down` | The check failed. | Denominator only. |
| `unknown` | **The probe could not perform the check.** | Neither. |
| `skipped` | **The scheduler shed the check under overload.** | Neither. |
| `pending` | Inside the retry window; not yet a verdict. | Neither. |
| `maintenance` | Inside a declared window. | Depends — see below. |

`unknown` and `skipped` are statements about *this system*, never about the
service being watched. A probe that could not look has not observed an outage, and
counting it as one would let a network problem at the monitoring end manufacture
downtime at the client end.

Worked example. A monitor scheduled 200 checks over a window: 90 succeeded, 10
failed, and 100 were never made because the probe was offline.

| Reading | Figure | Why it is wrong |
|---|---|---|
| 90 / 200 | 45% | Counts unmade checks as failures. Invents an outage. |
| 190 / 200 | 95% | Counts unmade checks as successes. Invents health. |
| **90 / 100** | **90%** | The share of what was actually observed. |

The report gives 90%, and prints **`unobserved_share`** beside it — here 50% —
so a reader can see how much to trust it. An SLA computed over 60% observation is
not an SLA, and the figure that says so travels with it.

### When there is no figure at all

A window in which nothing was observed has **no uptime percentage**. The report
prints a blank with "nothing was observed in this period" beside it, and the JSON
and CSV carry a null and an empty field.

Neither zero nor 100% is available here. Zero would claim total downtime; 100%
would claim perfect service from a probe that never ran.

---

## Maintenance

Declared maintenance windows are **excluded by default**: checks inside them leave
the denominator, exactly as `unknown` does.

Three settings, and the same window yields three different lawful percentages:

| `maintenance_handling` | 80 up, 10 down, 10 in maintenance |
|---|---|
| `exclude` *(default)* | 80 / 90 = **88.9%** |
| `count_as_up` | 90 / 100 = **90%** |
| `count_as_down` | 80 / 100 = **80%** |

Because the answer depends on the setting, **the setting is printed on the report**
rather than living only in the template. A figure without its policy cannot be
checked by the person it is handed to.

One deliberate asymmetry: excluding maintenance does **not** improve
`unobserved_share`. That figure is taken over everything scheduled, so an
exclusion cannot flatter the quality of the observation as well as the uptime.

---

## Error budgets

Given a target — say 99.9% over a 31-day month:

```
error budget          = (1 − target) × window length
                      = 0.001 × 31 days = 2,678 seconds

budget consumed       = the sum of the observed breach durations
budget remaining      = budget − consumed
burn rate             = consumed / budget
```

Four things about this are decisions rather than arithmetic.

**The window length is the difference between the two instants**, not 30 × 24
hours. April in Sydney is 30 days and one hour, so the budget follows the clocks
back — otherwise it is an hour wrong twice a year, in a number people check.

**Consumed is the sum of the breach durations**, the same durations the breach
table on the report lists. The two therefore agree by construction rather than by
arithmetic coincidence. An earlier version projected the window-level down
proportion onto the whole window and produced "budget used 3h 36m" above a breach
table summing to 2h 24m — the two side by side on one page is the only thing that
would have caught it.

**Remaining goes negative rather than flooring at zero.** "41 minutes past" is the
number somebody wants; a floor at zero hides how far past.

**A target of exactly 100% is refused**, at the API and in the schema. Its budget
is zero seconds, which makes the burn rate undefined and every report a breach
report.

A window that observed nothing consumes *nothing*: a probe that never looked has
not spent budget.

A target met exactly is met. 99.9% arrives as 8991/9000, which is not exactly
99.9 in binary, and a naive comparison turns a met SLA into a breach.

---

## The breach log

Breaches are read from the **daily tier**, and the report says so.

The alternatives were considered and are worse. Raw heartbeats would give real
minutes but are bounded by `raw_days` — seven by default — so a breach log built
on them would be empty for exactly the completed months an SLA report covers.
Incidents carry real timestamps but are human-declared, so the log would omit most
outages.

The daily tier is kept indefinitely and always answers. The boundaries are days;
**the duration inside a day is the projected downtime rather than the span**, so a
day containing four minutes of downtime is a four-minute breach and not a
24-hour one.

---

## Where a target comes from

First match wins:

1. the report template's `sla_target`;
2. the monitor's `slo_target_percent`;
3. the monitor's group's `slo_target_percent`;
4. none.

**The report prints which of the four answered.** "99.9%" means something
different when nobody set it on this monitor, and a silent inheritance is
invisible to whoever reads the report.

Resolution stops at the monitor's own group. Groups nest, the order has no fifth
step, and climbing further would print "inherited from group" for a number set two
levels up.

**A monitor with no target at any level gets no SLA block at all** — not one
computed against a default nobody chose.

---

## Response time

| Figure | How |
|---|---|
| Window average | `SUM(response_time_sum) / SUM(response_time_count)` — exact at any tier. |
| Daily average series | From the daily tier. The primary exhibit. |
| Best and worst day | From that series, **over observed days only**. |
| Days over target | Present only when `response_time_target_ms` is set. |
| p95 | Nearest rank, over the trailing seven days of the period, when it can be computed at all. |

The window average is never the mean of the daily averages. 1,000 checks at 100 ms
and 10 at 1,000 ms is 108.9 ms, not 550 ms.

A day with no successful checks stays on the series with a **null** average, so a
chart breaks rather than dipping to zero. A day the probe could not run is not the
fastest day the service ever had, and it is excluded from best-and-worst.

Days over target is **null rather than zero** when no target is set. An absence of
a rule and a clean sheet are different claims, and only one of them is a
compliment. A day exactly at the target is not over it.

There is **no window-level minimum or maximum, anywhere**. Over a month they are
extreme-value statistics — the single slowest successful check out of tens of
thousands — which reads as alarming and carries no signal.

### Why there is only one percentile

A quantile is a rank statistic. It does not merge: you cannot combine the p95 of
Monday with the p95 of Tuesday and get the p95 of both days, by any arithmetic.
The rollup tiers therefore hold no usable percentile, and the coarse-tier column
that looks like one is a maximum-of-p95 — which the product declines to serve at
all rather than serving with a caveat the API schema has nowhere to put.

The one real percentile is computed from raw heartbeats over seven days, and it is
withheld with a stated reason wherever those rows are not there to compute it
from. The reasons are `scope_too_large`, `insufficient_raw_retention` and
`no_successful_checks`, and one of them is always given: a figure absent without a
reason reads as a defect in the product rather than as a decision about honesty.

---

## Post-mortem figures

| Figure | From | Null when |
|---|---|---|
| Time to detect | `detected_at − started_at` | No detection was recorded. |
| Time to acknowledge | `acknowledged_at − started_at` | Nobody acknowledged. |
| Time to resolve | `resolved_at − started_at` | Still open. |

**Time to detect is usually unknown, and that is the common case rather than the
edge one.** Nothing raises incidents automatically before Phase 3, so an incident
recorded by hand has no detection time at all. The report prints "unknown", not a
dash and not zero: a zero would claim the outage was noticed the moment it began.

The means are taken over the incidents that **have** each figure, and the count
travels with the mean — "22 minutes, from one incident of nine" is a very
different claim from "22 minutes". Averaging an unknown as zero would drag every
mean towards zero in proportion to how much is unknown, which is the opposite of
what a reader should conclude from missing data.

`alerts_fired` distinguishes **zero** — the delivery log covers this incident and
holds nothing, which reads as *nobody was told* — from **null**, meaning the rows
have been swept. Retention must not be able to manufacture that finding.

---

## Resolution, and what it does not change

A report reads the coarsest tier that covers its window (see the table in
[reporting.md](reporting.md)) and says which one answered. Resolution bounds
*detail*, never *existence*: an older window is not less true, it is coarser.

Where retention has truncated the start of a window, the figures are computed over
the **covered** window and the document states the range it actually covered
rather than the range that was asked for.

---

## The short version, for the top of an email

> Uptime is the share of observed checks that succeeded. Checks the probe could
> not make are excluded from the figure and reported separately: a gap in
> observation is not an outage. Declared maintenance is excluded. Figures are read
> at daily resolution.

That paragraph is printed on every report, before any figure, because a
denominator explained after the number has already been misread.
