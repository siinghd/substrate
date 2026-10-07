# Acceptable Use Policy — DRAFT

> **This is a starting point written by an engineer, not a lawyer, and it is not
> legal advice.** It exists so the technical controls in this repository have a
> stated policy behind them, and so you have something concrete to hand to a
> lawyer rather than a blank page. Have it reviewed before you publish it or
> rely on it. Your jurisdiction, your company structure, and your upstream
> provider's own AUP all change what belongs here.

---

## Why this matters more than usual

You are selling arbitrary code execution to strangers. Three consequences
follow, and they are the reason an AUP is load-bearing rather than decorative:

1. **Your IP addresses will be used to attack third parties.** Not
   hypothetically. You need the stated right to cut a customer off immediately.
2. **Your upstream provider's AUP applies to you.** If a tenant relays spam or
   scans a network from your host, your provider suspends *you*, not them. Read
   their AUP before writing yours; you cannot grant permissions you do not have.
3. **Abuse complaints arrive addressed to you.** You need a published contact
   and a response process, or your netblock's reputation degrades.

---

## 1. Scope

This policy governs all use of the Service, including by the customer's own end
users. **The customer is responsible for their end users' conduct.** This is a
two-level tenancy: our customer's users run code we never see, and the contract
has to place that responsibility somewhere explicit.

## 2. Prohibited uses

You may not use the Service to:

**Attack or interfere with others**
- port scanning, vulnerability scanning, or penetration testing of systems you
  do not own or have written authorisation to test
- denial-of-service traffic of any kind
- unauthorised access attempts, credential stuffing, or password cracking
- distributing malware, ransomware, or botnet command-and-control

**Abuse network resources**
- unsolicited bulk email, or any spam relay or list-washing activity
- open proxies, open relays, or anonymising services for third parties
- traffic that violates our upstream providers' acceptable use policies

**Consume resources in bad faith**
- cryptocurrency mining, or any workload whose primary purpose is converting
  our compute into a commodity, unless separately agreed in writing
- deliberately evading resource limits, quotas, or billing
- attempts to escape the sandbox boundary, escalate privilege on the host, or
  access another tenant's data

**Break the law or hurt people**
- content that is illegal in the jurisdictions where the Service operates
- child sexual abuse material — reported to authorities, account terminated
  immediately, no notice
- infringing others' intellectual property

## 3. Security testing of the Service itself

Security research **on your own sandboxes** is welcome. Report findings to
[SECURITY CONTACT]. Do not test against other tenants, do not exfiltrate data
that is not yours, and do not run availability-affecting tests. Good-faith
research reported responsibly will not be pursued.

## 4. Enforcement

We may suspend an organisation immediately and without notice where use appears
to violate this policy or threatens the Service or third parties. Suspension
stops new sandboxes and terminates running ones.

Suspension is reversible. Usage records and audit history are preserved so a
suspension can be reviewed and, where wrong, undone.

Where circumstances allow, we will state the reason and give an opportunity to
respond. Where they do not — an attack in progress, a legal obligation — we act
first.

## 5. Data, retention and deletion

- Sandbox filesystems are **ephemeral**. On the `kernel` tier a sandbox's
  filesystem does not survive its lifetime. Do not treat a sandbox as storage.
- Usage records are retained for billing and dispute resolution.
- On account closure, [RETENTION PERIOD — decide this] then deletion.

*Engineering note: `limits.retention_days` is declared in configuration and
**nothing enforces it**. Do not publish a retention promise this software does
not yet keep.*

## 6. Availability

State your actual availability commitment here, or state explicitly that there
is none. Do not imply an SLA you have not built for: this is currently a
single-node deployment with no failover.

## 7. Reporting abuse

Abuse reports: **[ABUSE CONTACT — publish this and monitor it]**

Include timestamps with timezone, source and destination addresses, and
protocol. We aim to acknowledge within [TIMEFRAME].

## 8. Changes

We may update this policy. Material changes will be announced [HOW] with
[NOTICE PERIOD].

---

## Checklist before you publish

- [ ] Reviewed by a lawyer in your jurisdiction
- [ ] Reconciled against your upstream provider's AUP
- [ ] Abuse and security contacts exist and are monitored
- [ ] Retention period decided **and actually implemented**
- [ ] Availability language matches what you have built
- [ ] Terms of Service and Privacy Policy written to match
- [ ] A process exists for handling law-enforcement requests
