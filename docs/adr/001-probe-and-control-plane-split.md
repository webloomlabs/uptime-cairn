# ADR-001: Probe and Control-Plane Split

- **Status:** Accepted
- **Date:** 2026-07-31
- **Deciders:** [Shakil Ilham](https://github.com/silham)

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

## Context

The category leader, Uptime Kuma, lacks distributed probing capabilities; monitors execute from a single location, meaning if that server dies, alerting dies with it. User research indicates that High Availability (HA) is frequently requested but explicitly unavailable and not planned for the upstream Kuma project. Furthermore, serving the enterprise and MSP segments requires the capability to monitor private, internal services without punching inbound holes through strict corporate firewalls. 

## Decision

We are implementing a strict structural split between the control plane and check-execution probes. Probes will operate as completely stateless agents that register with the control plane over gRPC (with NATS remaining an option for fan-out) to pull their monitoring assignments. Communication is strictly outbound-only; the agent dials out to the control plane, requiring absolutely no inbound ports on the probe's host. 

To satisfy the onboarding requirements of solo users, the probe will be compiled directly into the single binary and run in-process during solo mode. However, even in-process, it must execute behind the defined gRPC interface, and the scheduler must never assume same-process execution.

## Consequences

**What this makes easy.**
This single architectural split simultaneously unlocks multi-region monitoring, private probes for VPCs/intranets, horizontal scaling by simply adding more probe agents, High Availability (probes buffer and replay data if the control plane drops), and N-of-M consensus checking to eliminate false positives.

**What this makes hard, or forecloses.** 
This permanently forecloses taking shortcuts in the check execution engine. The codebase is forced to serialize and deserialize all check configurations and results across a gRPC boundary, even when running in "solo mode" inside a single binary[cite: 2]. It increases the upfront complexity of the scheduler.

**What becomes expensive to reverse later**, and roughly when the point of no return is.
This decision cannot be retrofitted or reversed later. The point of no return is week one of Phase 0. Reversing this after Phase 1 would require throwing away and rewriting the entire execution engine, network topology, and security model.

## Alternatives considered

*   **Monolithic execution engine (the Uptime Kuma model):** We considered keeping check execution tightly coupled to the control plane database to save development time. This was rejected because it imposes a hard ceiling on High Availability and geographical distribution, which are core requirements for enterprise users.
*   **Push-based agent orchestration (Control Plane dials Probes):** We considered a model where the control plane reaches out to probes via a REST API to trigger checks. This was rejected because it requires inbound firewall ports to be opened on the agent side, completely defeating the ability to securely deploy private probes inside restricted VPCs.

## Compliance with the product principles

Confirm the decision holds the lines that are not negotiable, or explain why an
exception is warranted:

- [x] Sixty seconds to first monitor is preserved *(the probe is compiled in-process for the default solo mode, maintaining the one-command docker run)*
- [x] Nothing is paywalled in the open source build
- [x] API-first — no privileged endpoints the dashboard uses and users cannot
- [x] Progressive disclosure — no new complexity imposed on the solo user *(the solo user never knows the control-plane/probe split exists)*
- [x] The client is never sent full state; the UI stays fast at 5,000 monitors
- [x] Solo mode keeps zero required external dependencies *(runs entirely inside the single Go binary)*
- [x] Dependency surface stays minimal

## References

*   Uptime Cairn Project Plan, Draft v1, Sections 1.1, 1.4(d), and 5.2
*   Phase 0 Plan: Foundations, Section 3.2 (ADR-001)
*   Phase 1 Plan: Solid Core, Section 2 (Architecture in Phase 1)