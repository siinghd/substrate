# Observability

Every metric `substrate` exposes, what it answers, and the ones worth alerting
on. Prometheus text format on the node agent's metrics endpoint; all series are
namespaced `substrate_`. The standard Go runtime and process collectors are
also registered.

Percentile snapshots are additionally **persisted to the store** so latency can
be queried in SQL without a Prometheus deployment — see *Persisted ledger*.

## Label cardinality

Every label value in this system is a **fixed constant chosen by the code** —
tier names, driver operation names, HTTP methods, and matched route *patterns*
(never raw paths). Nothing a tenant controls reaches a label. That is a
deliberate constraint: a sandbox id or a raw URL in a label is how a metrics
backend gets taken down by ordinary traffic.

---

## Sandbox lifecycle

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_sandboxes_live` | gauge | — | How many machines are live on this node. |
| `substrate_sandbox_starts_total` | counter | `tier`, `outcome` | Start **error rate** per tier. Without the `outcome` split a node whose runtime is broken serves failures at full rate with a flat success counter. |
| `substrate_sandbox_stops_total` | counter | `tier`, `reason` | Did it stop because we killed it (`killed`) or did it die on its own (`exited`)? A rising `exited` is guests crashing or being OOM-killed. |
| `substrate_driver_errors_total` | counter | `tier`, `op` | Which driver operation is failing. `op` values are constants like `runsc_run`, `agent_connect`, `handshake`, `kill`, `prepare_template`, `shim_not_static`, `pid_file`, `agent_auth_failed`. |
| `substrate_agent_reconnects_total` | counter | `tier` | Guest agents that came back after a transport drop. **Recovery, not failure** — deliberately not in `driver_errors_total`, which is the series you alert on. A steady rate means connections are dropping; the sandboxes are surviving it. |
| `substrate_machine_starts_total` | counter | `kind` | VMM starts by `create` vs `restore`. |
| `substrate_restores_total` | counter | — | Total machine starts. |

Three `driver_errors_total` values are worth naming because they are silent
otherwise:

- **`pid_file`** — the sentry pid could not be read, so that sandbox reports
  **no usage for its entire life**. Billing looks healthy; a tenant is simply
  missing from it.
- **`shim_not_static`** — the guest agent was built with cgo and cannot run in a
  tenant rootfs. Every create on that node will fail.
- **`agent_auth_failed`** — something inside a sandbox tried to impersonate its
  agent. One or two during a reconnect is possible; a sustained rate is a tenant
  probing the boundary.

## Latency

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_restore_latency_ms` | histogram | — | End-to-end create/resume latency to a usable guest. This is the series the plan's **p50 ≤ 300 ms / p99 ≤ 1 s** target is measured against. All tiers report here, so they are comparable on one dashboard. |
| `substrate_stage_duration_seconds` | histogram | `operation`, `stage` | *Where* that time goes. For `operation="sandbox_create"` the stages are `stage_rootfs`, `vmm_spawn`, `agent_connect`, `guest_handshake`. |

The stage breakdown is the point of having it. Measured on the `kernel` tier,
`vmm_spawn` is ~80% of create — which says the next optimisation is the runtime
start, not the rootfs work anyone would have guessed at.

## API surface

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_http_requests_total` | counter | `method`, `route`, `status` | Throughput per route. `route` is the matched **pattern**, not the path. |
| `substrate_http_request_duration_seconds` | histogram | `method`, `route`, `status` | Request latency p50/p95/p99 per route. |
| `substrate_http_errors_total` | counter | — | 5xx rate. |
| `substrate_rate_limited_total` | counter | — | Requests rejected by the limiter. |
| `substrate_ratelimit_evictions_total` | counter | — | Limiter buckets evicted at the table cap. A sustained rise means **key churn** — someone cycling principals to dilute the table. |
| `substrate_idempotency_replays_total` | counter | — | Mutations served from the idempotency cache, i.e. client retries actually observed. |

## Reconciler

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_reconcile_queue_depth` | gauge | — | Sandboxes waiting to converge. Sustained growth means the worker pool cannot keep up. |
| `substrate_reconcile_enqueue_deferred_total` | counter | — | Enqueues pushed to a later tick (queue full, or already in flight). |

## Billing and usage

The usage path is the one place where a silent failure costs money, so it is
instrumented at every step where a sample can be lost.

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_usage_samples_pending` | gauge | — | Samples buffered in memory awaiting flush. |
| `substrate_usage_flush_errors_total` | counter | — | Failed ledger flushes — a store outage in progress. |
| `substrate_usage_samples_dropped_total` | counter | — | **Samples discarded because the buffer hit its cap.** Any non-zero value is unbilled usage. The buffer is bounded deliberately (an unbounded requeue OOM'd the node during a store outage, killing every sandbox on it over a database problem), so this counter is the cost of that trade and must be alerted on. |
| `substrate_metric_snapshots_persisted_total` | counter | — | Percentile rows written to the SQL ledger. |

## Storage and cell

| metric | type | labels | answers |
|---|---|---|---|
| `substrate_chunk_cache_bytes` | gauge | — | Snapshot chunk cache occupancy on this node. |
| `substrate_events_dropped_total` | counter | — | Lifecycle **audit** events discarded (ring overflow or failed persist). Non-zero means the audit trail has holes. |
| `substrate_cell_cross_tenant_denied_total` | counter | — | Cell proxy requests refused because the caller does not own the sandbox. Should be ~0; a rise is either a broken client or someone enumerating ids. |

## Process

| metric | type | answers |
|---|---|---|
| `substrate_runtime_goroutines` | gauge | Live goroutines. Monotonic growth is a leak — one per exec was a real defect. |
| `substrate_runtime_heap_bytes` | gauge | Heap in use. |

---

## Persisted ledger

Histogram percentiles are also written to the store on a schedule, so latency
trends are queryable in SQL without running Prometheus.

**Known limitation:** the persisted rows are **lifetime-cumulative** quantiles.
Cumulative percentiles are not differenceable — you cannot subtract two rows to
get "p95 during this window" — so the ledger shows the lifetime shape and will
be slow to reflect a regression. Prometheus is the accurate source for windowed
percentiles. This is recorded as an open finding (`OBS-5`) in
[`../.audit/STATUS.md`](../.audit/STATUS.md) rather than presented as working.

## Worth alerting on

Ordered by how quietly they fail:

1. `substrate_usage_samples_dropped_total > 0` — unbilled usage, silent.
2. `rate(substrate_driver_errors_total{op="pid_file"}[1h]) > 0` — a sandbox
   billing zero for its whole life, silent.
3. `substrate_sandbox_starts_total{outcome="error"}` rising — the node cannot
   start sandboxes; the API may still be answering.
4. `substrate_driver_errors_total{op="agent_auth_failed"}` sustained — a tenant
   probing the agent boundary.
5. `substrate_events_dropped_total > 0` — the audit trail has holes.
6. `histogram_quantile(0.5, substrate_restore_latency_ms) > 300` — the plan's
   own cold-start target is being missed.
