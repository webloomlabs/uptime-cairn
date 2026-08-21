# Self-monitoring

Who watches the watchman. A monitoring tool that fails quietly is worse than no
monitoring tool, because the silence reads as good news — so the process reports
on itself, and the numbers worth alerting on are the ones that reveal quiet
failure rather than the ones that look impressive on a dashboard.

Three endpoints, none of them under `/api/v1`:

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness. `{"status":"ok","version":"v0.1.0"}` |
| `GET /readyz` | Readiness. Same handler today |
| `GET /metrics` | Prometheus text format |

`/healthz` and `/readyz` are the same handler in this build. They are separate
endpoints because they answer different questions and will diverge — readiness
should fail while migrations run, and today it does not — so wire your
orchestrator to `/readyz` now and get the behaviour for free when it lands.

## Scraping

`/metrics` needs an API key with `metrics:read`, **except from loopback**, where
it is served without one so a Prometheus on the same box needs no credential:

```console
$ curl -s localhost:3000/metrics | head -3
# HELP cairn_build_info Build information for the running instance.
# TYPE cairn_build_info gauge
cairn_build_info{version="dev",mode="solo"} 1
```

The exemption is on the connection's remote address. Behind a same-host reverse
proxy every connection arrives from `127.0.0.1`, which applies the local-scraper
exemption to the whole internet — so the proxy has to deny `/metrics`. That is
not optional and it is covered in
[reverse-proxy.md](reverse-proxy.md#block-metrics-at-the-proxy).

From another host, mint a key with `metrics:read` and nothing else:

```yaml
scrape_configs:
  - job_name: uptime-cairn
    static_configs: [{targets: ["127.0.0.1:3000"]}]
    # Only needed off-box; drop it when scraping loopback directly.
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/cairn.key
```

## The numbers that matter

Most of the surface is what you would expect — monitors by status, per-monitor
status, response times. These five are the ones to alert on, because each is a
way for the product to be broken while appearing to work.

**`cairn_alerts_dropped_total`** — events shed because the notification queue
was full. The dispatcher sheds rather than blocking, on purpose: alerting must
never become backpressure on ingest. The cost of that choice is that a full
queue is silent, and this counter is the only place it is visible. Any increase
means somebody did not get paged.

```yaml
- alert: CairnAlertsDropped
  expr: increase(cairn_alerts_dropped_total[15m]) > 0
  annotations:
    summary: "Cairn shed {{ $value }} alerts — someone was not paged"
```

**`cairn_probe_shed_results_total`** — the same failure one layer earlier. The
probe buffers results when it cannot reach the control plane and sheds when the
buffer fills. Shed results are heartbeats that never happened as far as history
is concerned.

**`cairn_results_rejected_total`** — results that could not be attributed to a
live monitor. A few around a deletion are normal: a check was in flight when the
monitor went away. A sustained rate is a probe bug, and rejections are dropped
rather than retried, so this counter is the only trace they leave.

**`cairn_heartbeats_written_total`** — counted *after* the write returns, never
on intent. A counter that moves on intent is a counter that says the system is
fine while it is losing data. Alert on it going flat, which is the shape of "the
scheduler stopped" and does not raise any error anywhere:

```yaml
- alert: CairnHeartbeatsStalled
  expr: rate(cairn_heartbeats_written_total[10m]) == 0 and cairn_monitors > 0
  for: 10m
```

Compare it against `cairn_results_ingested_total`: ingested exceeding written is
a probe redelivering, which the natural key absorbs correctly and is still worth
seeing — one counter for both would make "the probe is resending" and "the
system is doing twice the work" indistinguishable.

**`cairn_db_pool_wait_total`** and `cairn_db_pool_wait_seconds_total` — waits for
a database connection, per pool. The writer pool has a ceiling of one, by
design. This is the number that tells you whether contention has moved from the
queue in front of the writer to the writer itself, and it is the first thing to
look at when creation slows down on a large install.

## Probe health

A probe has no inbound port, so it cannot be scraped. It reports its own health
on the result stream instead, and the control plane republishes it here labelled
by probe — `cairn_probe_uptime_seconds`, `cairn_probe_due_queue_depth`,
`cairn_probe_checks_in_flight`, `cairn_probe_buffered_results`,
`cairn_probe_clock_offset_seconds`, and the shed and skipped counters.

In solo mode there is exactly one probe, `embedded`, and it is behind the same
gRPC seam as a remote one would be — so these series exist and are populated
today, and mean the same thing in Phase 4 when the probe is on another
continent.

`cairn_probe_due_queue_depth` growing without bound means the probe cannot keep
up with its own schedule; `cairn_probe_clock_offset_seconds` drifting means
heartbeat timestamps are drifting with it.

## Cardinality

`cairn_monitor_status`, `cairn_monitor_response_time_seconds`, and
`cairn_monitor_last_check_timestamp_seconds` carry one series per monitor,
labelled with `monitor_id`, `monitor` (the name), and `type`.

At the install size this product is built for — 5,000 monitors — that is 15,000
series from one target, and the `monitor` label churns a fresh series every time
somebody renames a monitor. That is a real cost in your Prometheus, not in
Cairn. If it bites, drop the per-monitor series at scrape time and keep the
aggregate `cairn_monitors{status=…}`:

```yaml
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: 'cairn_monitor_(status|response_time_seconds|last_check_timestamp_seconds)'
        action: drop
```

You lose nothing about the health of Cairn itself by doing that. Per-monitor
history is what the product stores and what its own API answers; the metrics
endpoint is for the health of the watchman.
