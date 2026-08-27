# ADR-007: Report Rendering — A Pure-Go PDF Writer over the Report Model, Not an HTML Conversion

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** Independent. Constrained by the packaging properties [ADR-002](002-storage-engine.md) assumes for solo mode — one binary, no external services — but it does not modify or extend any prior decision.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

**Recorded for the repository's own history:** [AGENTS.md](../../AGENTS.md) §3 places
ADRs with a human and describes that restriction as absolute. It was explicitly
waived for this document by the project maintainer on 2026-08-27, on the same
terms as the waiver recorded in [ADR-006](006-report-latency-statistics.md) and
following the precedent at
[data model §11.6](../data-model/README.md#116-secrets-at-rest). The decision below
is the maintainer's; the drafting is not.

## Context

Phase 2 ships PDF reports. It is a roadmap commitment in two places — the Phase 2
format list, and the "deliberately *not* on this roadmap" section, which names PDF
reports among the things that ship in the open source build rather than behind a
paid tier. The exit condition is an agency sending fifty branded client reports on
the first of the month without touching anything, and a PDF that a human has to
save by hand does not satisfy it.

[AGENTS.md](../../AGENTS.md) names "Typst for PDF reports" in its stack summary.
That line was written in Phase 0 as an expectation, not a decision; no ADR
supports it, and this is the first time the constraints have been checked against
it. **This ADR settles the question, and that line must be updated in the same
pull request** or the repository contradicts itself in the file that tells
contributors what the stack is.

### The constraints are narrower than they look

Four properties of the build eliminate most of the option space before any
comparison of rendering quality.

**`CGO_ENABLED=0`, across five targets.** Set explicitly in both the
[Dockerfile](../../Dockerfile) and
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), and argued
for in the Dockerfile's own comments: the Go build is pure Go because
`modernc.org/sqlite` needs no cgo, so `GOARCH=arm64` is "a flag rather than a
toolchain," and building under emulation instead would take "roughly an order of
magnitude longer." The release matrix is `linux/amd64`, `linux/arm64`,
`linux/arm/v7`, `darwin/amd64` and `darwin/arm64` — and the darwin builds
cross-compile from an ubuntu runner, which is possible *only* because there is no
cgo. Any cgo-linked renderer — Cairo, HarfBuzz bindings, wkhtmltopdf as a
library — does not cost more here. It breaks the release matrix structurally.

**The binary distribution is one file.** The release workflow builds
`cairn_<version>_<os>_<arch>.tar.gz` containing the binary plus `README.md`,
`LICENSE`, `NOTICE` and `SECURITY.md`. A container image can bundle a helper
binary; **the tarball has no mechanism for one.** Any design that ships a second
executable solves Docker and abandons every binary install — including the 32-bit
Raspberry Pi that the matrix carries deliberately, with a comment noting that
"runs on a Raspberry Pi" is a claim in the README and that a Pi predating the
64-bit images is the one that needs a binary.

**Image size has already been argued at eight-megabyte granularity.** The
Dockerfile justifies `alpine` over distroless at length — about 8 MB, against
`sqlite3` for the documented online backup path, something that can make an HTTP
request for `HEALTHCHECK`, and a shell, because "a monitoring tool that cannot be
shelled into at 3am is a monitoring tool nobody trusts." A browser engine is
300 MB and upward. That is roughly a tenfold image increase set against a decision
taken over eight megabytes.

**Licence.** The project has just moved from AGPL 3.0 to Apache 2.0 across the
whole repository. A PDF dependency under AGPL-or-commercial terms would reimport
exactly what was deliberately removed.

### The framing error worth naming

The question was originally posed as "how do we render HTML to PDF," and that
question is what makes this expensive. HTML needs a browser engine because HTML is
an arbitrary layout language.

**A report is not an arbitrary web page.** It is a cover, headings, key–value
blocks, a table, a chart, and a footer. The Phase 2 plan already puts a computed,
backend-independent report model at the centre of the subsystem, with renderers
turning it into HTML, PDF, CSV and JSON. Once the PDF renderer consumes *that
model* rather than another renderer's output, the browser question does not arise.

## Decision

**Uptime Cairn renders PDF with a pure-Go writer, in-tree, driven by the computed
report model. No cgo, no subprocess, no sidecar, and no third-party PDF library in
Phase 2.**

Specifically:

1. **Renderers are siblings, not a chain.** `internal/report` computes the model;
   HTML, PDF, CSV and JSON renderers each consume the model. No renderer consumes
   another's output. A PDF is never produced by converting HTML.

2. **One drawing primitive set, two backends.** A small interface — text run,
   rectangle, line, path, image — with an SVG backend for the HTML report and a
   PDF content-stream backend for the PDF. Charts are written once against the
   primitives and drawn by both. This is the same discipline
   [`internal/rollup`](../../internal/rollup/rollup.go) applies to SQLite and
   Timescale, one layer up: the contract lives in our code and the backend supplies
   only the emission.

3. **The document model is bounded and enumerated**: cover block, heading,
   paragraph, key–value block, table with page breaking, chart, footer. Anything
   not on that list is not a report element in Phase 2. The list may grow by
   decision; it may not grow by accident.

4. **Fonts: one embedded TrueType family**, regular and bold at minimum, embedded
   whole rather than subset in the first cut. The fourteen standard PDF base fonts
   are rejected — they cost no bytes but lock encoding to WinAnsi and produce a
   document that looks generic, which defeats a white-label feature whose entire
   purpose is that the client believes their agency made it.

5. **Non-Latin scripts are out of scope for Phase 2, and the design does not
   foreclose them.** The text primitive accepts a shaped run rather than a string
   with an implied shaping step, so a shaping layer can be introduced later without
   changing a single caller. Today this costs nothing to honour:
   [`web/src/lib/i18n`](../../web/src/lib/i18n) holds `en.json` and nothing else.

6. **Output is deterministic.** The same model rendered twice produces
   byte-identical output. PDF's creation date and `/ID` array are derived from the
   report run — its window and identifier — never from the wall clock or a random
   source. This mirrors the reproducibility rule the Dockerfile already states for
   `BUILD_DATE`: "a wall-clock timestamp defeats reproducible builds on its own."
   It is also what makes the plan's re-runnable-generation property real rather than
   nominal.

7. **The failure path is binding.** A PDF render failure degrades the run to the
   other requested formats, records the reason against the run, delivers what
   succeeded, and surfaces the failure where an operator sees it — the discipline
   [`internal/notify`](../../internal/notify) already applies to delivery. A report
   run never fails silently, and never fails wholly because one format could not be
   produced.

8. **Print CSS ships on the HTML report**, as a complement. It is not a substitute:
   a PDF saved by hand fifty times is not the exit condition.

9. **The Chromium/Playwright path is explicitly out of scope.** If the Phase 4
   browser-check sidecar arrives, a higher-fidelity PDF renderer may be offered as
   an option at that point, under its own ADR. It is not designed for here.

## Consequences

**What this makes easy.**

Nothing about packaging changes. The release matrix, the Dockerfile, the image
size, `CGO_ENABLED=0`, the cross-compilation story, and the single-file tarball are
all untouched. The armv7 Raspberry Pi gets byte-for-byte the same reporting
capability as an amd64 container, which no other option on the table delivers.

No new CVE surface, nothing additional for the existing Trivy gate to scan, and no
upstream release cadence to track. No licence question to resolve.

Reports work in an airgapped install, which is a real constituency for a
self-hosted monitoring tool and one that a sidecar or a font-fetching renderer
would quietly exclude.

Determinism comes free rather than being engineered around. A browser-rendered PDF
would not be byte-reproducible, and the plan's "a definition plus a window plus a
data snapshot yields the same artifact" would have been an aspiration rather than a
property.

Charts arrive on the HTML side as a by-product of the primitive interface rather
than as separate work.

**What this makes hard, or forecloses.**

*We own a layout engine.* Bounded, but ours. **Table page-breaking is the known
time-sink** — widow and orphan handling, header repetition, a row taller than the
remaining page — and it should be budgeted explicitly rather than discovered in
month 6.

*There is a fidelity ceiling, and two layouts that can drift.* The PDF will not be
the HTML; they are two renderings of one model. A visual regression test over a
fixed golden report is therefore **required, not optional**, and it is what stops
the two drifting apart release by release.

*User-supplied branded text is plain text.* Cover text and footer text cannot carry
HTML or markdown, because the PDF renderer cannot render it and a field that works
in one format and not another is worse than a field that works nowhere. This is a
product constraint and belongs in the brand-profile UI, not only in a comment.

*Logo embedding is narrower than it looks.* The PDF writer can embed raster images;
it cannot embed SVG without either a path translator or a rasteriser. Status pages
today accept an arbitrary `LogoURL`
([`internal/model/statuspage.go`](../../internal/model/statuspage.go)) and the
project's own mark is `web/static/logo.svg`, so **SVG logos are the expected case,
not the exotic one.** Brand profiles must either accept PNG and JPEG only — stated
in the UI at upload time, not discovered at render time — or the writer needs a
minimal SVG-path translator. This is the single most likely source of a month-6
surprise and is called out here so that it is a decision rather than an incident.

*Complex chart types are limited to what the primitives draw.* The daily average
series and uptime bars of [ADR-006](006-report-latency-statistics.md) are within
reach; anything wanting curve fitting, dense scatter or fine typography inside the
plot is not.

*Non-Latin reports do not work in Phase 2.* Accepted knowingly. The most likely way
this surfaces is an agency with a client whose report must be in Japanese or
Arabic, and the answer then is a shaping layer behind the existing text primitive —
work, but not rework.

**What becomes expensive to reverse later.**

Little of it. The writer sits behind the renderer interface, so adding a
higher-fidelity backend later is additive and nothing written now has to be
unwritten.

Two things are sticky. **The drawing primitive interface** is what charts and
layout are written against, so it should be kept small — every primitive added is a
primitive both backends must implement forever. And **the embedded font** is both a
binary-size commitment, on the order of a few hundred kilobytes to a megabyte, and
a visual identity commitment: changing it reflows every future report, though not
artifacts already rendered and stored.

The point of no return for the primitive interface is the first release in which
chart code is written against it. There is no point of no return for the renderer
choice itself.

**On the estimate.** The bounded document above is on the order of one to one and a
half thousand lines: object and cross-reference writer, page tree, text with an
embedded TrueType face, vector primitives, raster image embedding, and a layout
pass. The known unknowns are table page-breaking and the SVG logo question above.
That is an estimate offered for planning, and the plan's month-7 valve — PDF is the
cuttable feature, reporting is not the cuttable phase — remains the response if it
proves optimistic.

## Alternatives considered

**Typst, wkhtmltopdf or WeasyPrint as a bundled subprocess.** The stack line in
AGENTS.md, and the reason this ADR exists. It lost on the tarball, not on the
image: a second executable can be baked into a container and cannot be shipped in a
single-file archive, so it would deliver PDF to Docker users and withhold it from
binary and Raspberry Pi installs. Three further costs, any one of which would give
pause on its own: per-architecture bundling for five targets, where upstream may
not publish an `armv7` build at all; a second CVE surface with an independent
release cadence, under a Trivy gate already set to fail on fixable HIGH and
CRITICAL; and, for Typst specifically, that it is a typesetting *language*, so
branded reports mean maintaining `.typ` templates with user-supplied footer and
cover text interpolated into source files — a template-injection surface introduced
for cosmetic gain.

The "use it when present on `PATH`, fall back otherwise" variant was considered and
is worse than either branch: two renderers to maintain indefinitely, and identical
report definitions producing visibly different documents on different installs.

**A headless Chromium sidecar.** The highest-fidelity option, and the lowest layout
effort, because the PDF is simply the HTML. It lost on the one-container promise, an
image roughly ten times larger, a sandbox story under the image's non-root
`USER 10001`, non-deterministic output, and the judgement that a browser engine —
the largest CVE surface in common software — does not belong inside a monitoring
tool by default. AGENTS.md does anticipate a Playwright sidecar for browser checks,
but that is Phase 4 and opt-in: a user who wants synthetic checks accepts the
weight. Requiring every install to carry Chromium so that reports can be PDFs
inverts that bargain.

**A third-party pure-Go PDF library.** Not rejected in principle, and a reasonable
future economy. Rejected for Phase 2 because adopting one requires verification
this decision should not wait on: licence compatibility with Apache 2.0 — at least
one prominent Go PDF library is dual AGPL-or-commercial, which would reimport the
licensing position just deliberately left — maintenance status, since at least one
widely-cited option is archived upstream, and confirmation that no transitive cgo
dependency enters the build. Any of these can be established later in a PR that
proposes a specific library against a working in-tree writer, which is a far better
position from which to judge than adopting one first.

**The fourteen standard PDF base fonts.** Zero embedded bytes and universal reader
support. It lost on encoding, which is WinAnsi and therefore Latin-1 only, closing
the door on the shaping path in item 5; and on appearance, since a white-label
report exists so that a client believes their agency produced it, and Helvetica on
an unstyled page does not clear that bar.

**HTML with a print stylesheet, and no PDF.** Genuinely useful, and adopted as a
complement in item 8. It lost as the answer because it fails the phase's exit
condition: fifty reports delivered on the first of the month without anybody
touching anything requires a server-generated attachment.

**Deferring PDF to Phase 3.** It lost because PDF is a stated roadmap commitment in
two places, and because the schedule valve the plan already carries — PDF is the
cuttable feature of the phase — is the right instrument for schedule pressure, used
if it arrives rather than assumed now.

**Rendering PDF from the generated HTML string with a Go HTML-to-PDF package.**
These either wrap a browser, returning us to the sidecar, or implement a subset of
CSS. The subset is exactly where the fidelity argument collapses: the appeal of
HTML-to-PDF is that the PDF *is* the HTML, and a partial CSS implementation gives a
document that is neither the HTML nor a design anybody chose, while still costing a
dependency.

## Compliance with the product principles

- [x] **Sixty seconds to first monitor is preserved.** No new prerequisite, service,
      or configuration on any install path.
- [x] **Nothing is paywalled in the open source build.** PDF ships to every user, on
      every one of the five release targets, which is a stronger form of this
      principle than the alternatives could hold.
- [x] **API-first.** Rendering sits behind the reports API; no privileged path is
      introduced and the dashboard uses what users can.
- [x] **Progressive disclosure.** A solo user asks for a PDF and receives one. Brand
      profiles, cover text and format selection are opt-in surfaces.
- [x] **The client is never sent full state; the UI stays fast at 5,000 monitors.**
      Rendering is server-side, off the check path, on the bounded worker pool the
      plan specifies, and is covered by the month-7 gate that runs report generation
      concurrently with the 5,000-monitor load test.
- [x] **Solo mode keeps zero required external dependencies.** This is the principle
      the decision turns on: no sidecar, no subprocess, no service.
- [x] **Dependency surface stays minimal.** Nothing is added to `go.mod`. The cost is
      paid in code we own and can explain, which is the trade [AGENTS.md](../../AGENTS.md)
      §5 asks for by name — "must not reach for a package when a hundred lines of our
      own code will do."

## References

- [ADR-002](002-storage-engine.md) — solo mode as one binary with no external
  services, the packaging property this decision protects.
- [ADR-006](006-report-latency-statistics.md) — the report figures this renderer
  draws, and the precedent for the waiver recorded above.
- [Dockerfile](../../Dockerfile) — `CGO_ENABLED=0`, the cross-compilation rationale,
  the alpine-over-distroless argument at 8 MB, non-root `USER 10001`, and the
  `BUILD_DATE`-from-commit reproducibility rule item 6 mirrors.
- [`.github/workflows/release.yml`](../../.github/workflows/release.yml) — the
  five-target matrix, `CGO_ENABLED=0`, and the single-file tarball that disqualifies
  the subprocess alternatives.
- [`internal/rollup/rollup.go`](../../internal/rollup/rollup.go) — the
  one-contract-two-backends discipline item 2 reuses.
- [`internal/model/statuspage.go`](../../internal/model/statuspage.go) — `LogoURL`,
  and the SVG embedding problem it implies for brand profiles.
- [`internal/notify`](../../internal/notify) — the recorded-and-visible failure
  discipline item 7 adopts.
- [`web/src/lib/i18n`](../../web/src/lib/i18n) — English only today, which is why
  item 5 costs nothing to honour now.
- [AGENTS.md](../../AGENTS.md) — the "Typst for PDF reports" stack line this ADR
  supersedes, to be updated in the same pull request; and §5 on preferring our own
  code to a dependency.
- [PHASE-2-PLAN.md §3.3, §4.7](../plans/PHASE-2-PLAN.md) — the problem statement this
  ADR closes and the format list it serves.
- Open follow-up: revisit if and when the Phase 4 Playwright sidecar lands, as a
  superseding or complementary ADR offering higher-fidelity rendering as an option —
  never as a requirement.
