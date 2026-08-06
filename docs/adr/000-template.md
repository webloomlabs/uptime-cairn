# ADR-000: <short, decisive title>

- **Status:** Proposed | Accepted | Superseded by ADR-XXX (link it as `[ADR-XXX](XXX-slug.md)`)
- **Date:** YYYY-MM-DD
- **Deciders:** <who agreed>

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

## Context

What forces are at play? What constraint, evidence, or user report prompted
this? Link the issue, the benchmark, or the quote. Be specific about numbers —
"the UI stalls above ~300 monitors" is useful; "it gets slow" is not.

## Decision

What we are doing, stated plainly and in the active voice. One paragraph if
possible.

## Consequences

**What this makes easy.**

**What this makes hard, or forecloses.** Be honest here; an ADR that lists no
downsides has not been thought through.

**What becomes expensive to reverse later**, and roughly when the point of no
return is.

## Alternatives considered

For each: what it was, and the specific reason it lost. "We considered X but
chose Y because Y was better" is not a reason.

## Compliance with the product principles

Confirm the decision holds the lines that are not negotiable, or explain why an
exception is warranted:

- [ ] Sixty seconds to first monitor is preserved
- [ ] Nothing is paywalled in the open source build
- [ ] API-first — no privileged endpoints the dashboard uses and users cannot
- [ ] Progressive disclosure — no new complexity imposed on the solo user
- [ ] The client is never sent full state; the UI stays fast at 5,000 monitors
- [ ] Solo mode keeps zero required external dependencies
- [ ] Dependency surface stays minimal

## References

Issues, discussions, benchmarks, prior art, upstream conversations.
