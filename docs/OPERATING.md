# Operating substrate as a service

What to do beyond running the binary: provisioning tenants, responding to
abuse, backups, and the decisions that are yours rather than the software's.

Config reference: [`CONFIGURATION.md`](CONFIGURATION.md).
Metrics and alerts: [`OBSERVABILITY.md`](OBSERVABILITY.md).

---

## Before the first customer

**Run it on its own machine.** Multi-tenant execution of untrusted code belongs
on a host where a sandbox escape costs you a rebuild, not your email, your
database, or your VPN. Do not co-locate it with anything you would miss.

**Pick a real isolation tier.** `driver.name: gvisor` on hosts without
`/dev/kvm`, `firecracker` where you have it. Set `driver.allow_process: false`
and `gateway.allow_process_tier_previews: false`: the `process` tier is not a
boundary, and those two flags are what stop a misconfiguration from silently
landing there.

**Size the network block.** A /30 per sandbox means a block addresses a
**quarter** of its size. `/19` = 2,046 sandboxes. Undersizing fails at
allocation time, in production, when you are busy.

**Set the secrets out of band.** `SUBSTRATE_JWT_SECRET` and
`SUBSTRATE_ADMIN_KEY` are environment-only and never in YAML, so the config can
be readable and version-controlled while these are not.

Deployment artifacts: [`deploy/systemd/`](../deploy/systemd/).

---

## Onboarding a tenant

The admin API is off unless `SUBSTRATE_ADMIN_KEY` is set. Generate one with
`printf 'ak_%s\n' "$(openssl rand -hex 24)"`.

```sh
A="Authorization: Bearer $SUBSTRATE_ADMIN_KEY"

# 1. the organisation
curl -sX POST localhost:8080/v1/admin/orgs -H "$A" \
     -d '{"name":"Acme Inc"}'

# 2. a project inside it
curl -sX POST localhost:8080/v1/admin/projects -H "$A" \
     -d '{"org_id":"org_...","name":"production","max_sandboxes":200}'

# 3. their API key -- SHOWN EXACTLY ONCE
curl -sX POST localhost:8080/v1/admin/keys -H "$A" \
     -d '{"org_id":"org_..."}'
```

Only the hash is stored. There is no "show me the key again"; re-issue and
revoke instead:

```sh
curl -sX DELETE localhost:8080/v1/admin/keys/key_... -H "$A"
```

Revocation takes effect on the next request, not on a cache expiry.

**Rotating the admin key** is editing `noded.env` and restarting. It is not in
the database, so a database compromise cannot mint tenants and a database
restore cannot resurrect a rotated key.

---

## Responding to abuse

You are renting arbitrary code execution to strangers. Expect crypto mining,
outbound port scanning, spam relay attempts, and someone using you to attack a
third party. Plan the response before you need it.

### What the platform already blocks

- **Cloud metadata (IMDS)** — `169.254.0.0/16` and the IPv6 equivalents,
  dropped as forward rule #1. This is the one that turns a sandbox into your
  cloud account.
- **Peer isolation** — sandboxes cannot reach each other.
- **RFC1918** — your own private networks are unreachable from a sandbox.
- **Outbound SMTP (25)** — dropped, so you are not a spam relay.
- **Rate limit** — 500 packets/second per sandbox, so one tenant cannot
  saturate the uplink.
- **Resource ceilings** — memory, vCPU, TTLs, per-project and per-end-user
  sandbox counts, a 512-process cap, and a per-sandbox disk write cap.

### The lever: suspension

```sh
curl -sX POST localhost:8080/v1/admin/orgs/org_.../suspend -H "$A" \
     -d '{"suspended":true,"reason":"outbound port scanning"}'
```

This does two things, and the second is the one that matters:

1. New sandbox creation returns `403 org_suspended`, carrying the reason, so
   the tenant knows why without opening a ticket.
2. **Running sandboxes are destroyed** by the reconciler on its next pass.
   Blocking creation while leaving the miner running is not a control.

Suspension is a reversible state, not a deletion: the audit trail and usage
records survive, so a wrongly-suspended tenant can be restored and a disputed
bill can still be reconstructed.

```sh
curl -sX POST localhost:8080/v1/admin/orgs/org_.../suspend -H "$A" -d '{"suspended":false}'
```

### What to watch

From [`OBSERVABILITY.md`](OBSERVABILITY.md):

- egress bytes per org (`GET /v1/usage`) — mining and exfiltration both show up
  as sustained egress from a small number of sandboxes
- `substrate_driver_errors_total{op="agent_auth_failed"}` — someone probing the
  agent boundary from inside a sandbox
- `substrate_cell_cross_tenant_denied_total` — someone enumerating sandbox ids

### What is NOT built

Be honest with yourself about the gaps:

- **No automatic abuse detection.** Nothing watches egress and suspends on a
  threshold. Suspension is a manual lever with an API; wire your own alerting
  to it.
- **No egress byte quota.** Usage is measured per sandbox, and nothing enforces
  a ceiling on it. A tenant can run up a large bandwidth bill before anyone
  notices.
- **`network.egress_policy: allowlist` is not implemented.** Only `open` works.

---

## Serving a site from a sandbox

A tenant runs a server in their sandbox; the gateway publishes it at
`{port}-{sandbox_id}.{base_domain}`.

```sh
# inside the sandbox
python3 -m http.server 8080     # or any server, on any port
```

The agent discovers listening sockets from the guest's own `/proc/net/tcp` and
announces them within ~2s — the tenant does not declare anything. `GET
/v1/sandboxes/{id}` lists the discovered ports.

**Listeners bound to `127.0.0.1` are deliberately NOT published.** Binding
loopback is how a tenant keeps a service private; bind `0.0.0.0` to expose it.

Requirements on your side:

- a **wildcard DNS record** `*.run.example.dev` pointing at the gateway
- a **wildcard TLS certificate** for the same (there is no ACME integration;
  terminate in front or set `gateway.tls_cert`/`tls_key`)
- `gateway.base_domain` set to match

Previews are **default-private**: they require the sandbox-scoped bearer token
unless the tenant marks them public. On the `process` tier previews are refused
unless explicitly enabled, because that tier has no per-sandbox address to route
to and every preview would point at the node itself.

## Backups

Enabled by default: hourly, verified, 48 retained.

```yaml
backup:
  enabled: true
  dir: /var/backups/substrate   # a DIFFERENT volume from node.data_dir
  interval_mins: 60
  keep: 48
```

Each snapshot is taken with `VACUUM INTO` (not a file copy — the store runs in
WAL mode, and copying the `.db` alone silently restores to an older state),
then **opened and read back** before it counts as a success. "The backup ran"
and "the backup works" are different claims; this collapses them.

Watch the logs for `backup wrote but DOES NOT VERIFY` — that means you have a
file that is not a backup.

**Restoring** is stopping noded, putting the snapshot at `node.data_dir/…`, and
starting it. Practise this before you need it.

For Postgres (`mode: cell`), in-process backup is not implemented: use
`pg_dump` against `cell.database_url` on your own schedule.

---

## Billing

Usage is metered continuously and priced by
[`internal/billing`](../internal/billing). Set prices in **nano-cents** per unit
(1e-9 of a cent) so a per-second price does not round to zero:

```yaml
billing:
  currency: usd
  sandbox_second_nano_cents: 300           # residency
  cpu_second_nano_cents: 2000
  gib_hour_nano_cents: 1200000000          # $0.012 / GiB-hour
  egress_gb_nano_cents: 9000000000         # $0.09 / GB
  included_cents: 0                        # free tier
  minimum_cents: 0
```

Tenants read their own invoice at `GET /v1/usage/invoice?from=&to=`, scoped to
their own org — never to a query parameter.

**Unpriced deployments invoice zero.** The software does not guess a price.

**No payment processor is wired.** `billing.PaymentProvider` is an interface
with a no-op default; charging real money is an explicit decision requiring
live credentials, and the interface requires idempotency on retry because a
retry that charges twice is the worst bug in that package. Per-org plans need a
schema migration; today the plan is node-wide.

---

## What is still your decision

- Provisioning the separate host, and its firewall
- TLS certificates (there is no ACME integration; terminate in front)
- Choosing and wiring a payment processor
- Legal review of [`ACCEPTABLE-USE.md`](ACCEPTABLE-USE.md) and your terms
- Whether your jurisdiction and your upstream provider's AUP permit what you
  are about to sell
