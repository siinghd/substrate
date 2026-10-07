# Agent-Facing API Design + Sandbox Network Security — as of 2026-08-13

*Research report 6 of 6 for the Substrate architecture plan. Produced 2026-08-13.*

---

## (A) Agent Interface

### A1. MCP spec state — **major redesign landed 2026-07-28**

**Current revision: `2026-07-28`** (final; schema at `schema/2026-07-28/schema.ts`). Predecessor stable was `2025-11-25`. Source: https://modelcontextprotocol.io/specification/ (fetched 2026-08-13); changelog: https://modelcontextprotocol.io/specification/2026-07-28/changelog (fetched 2026-08-13). RC was published 2026-05-21 per https://blog.modelcontextprotocol.io/posts/2026-07-28-release-candidate/.

**This revision matters enormously for a sandbox platform** — it explicitly moves toward the model you'd build anyway:

- **Protocol-level sessions REMOVED.** `Mcp-Session-Id` header is gone from Streamable HTTP. *"Servers that need cross-call state use explicit, server-minted handles passed as ordinary tool arguments"* (SEP-2567). A direct endorsement of the sandbox-handle pattern: `create_sandbox` → returns `sandbox_id` → passed as a plain tool arg thereafter.
- **MCP is now stateless.** `initialize`/`notifications/initialized` handshake removed; every request carries `io.modelcontextprotocol/protocolVersion` + `clientCapabilities` in `_meta`. New mandatory `server/discover` RPC advertises versions/capabilities/identity (SEP-2575).
- **Transports:** Streamable HTTP is the transport (single endpoint, POST; `Content-Type: application/json` or `text/event-stream`). HTTP+SSE is formally **Deprecated** (SEP-2596). The HTTP **GET** endpoint was removed and replaced by `subscriptions/listen` (one long-lived POST-response stream, opt-in notification types). **SSE resumability removed** — no `Last-Event-ID`, no event IDs; a broken stream loses the in-flight request and the client MUST re-issue with a new request ID. **Implication: long-running sandbox ops must NOT rely on a held stream.**
- **Long-running ops → `io.modelcontextprotocol/tasks` extension** (moved out of core, SEP-2663). Polling via `tasks/get`, client→server input via `tasks/update`, `tasks/list` removed; servers may return task handles *unsolicited*. Described in spec overview as "durable handles." The canonical answer for `exec` calls that outlive an HTTP request.
- **Auth: OAuth 2.1 resource server, now mandatory-ish.** MCP server = OAuth 2.1 resource server; MUST implement RFC 9728 Protected Resource Metadata; clients MUST implement RFC 8707 `resource` indicator in *both* authorization and token requests; servers MUST validate token audience and MUST NOT accept or transit other tokens. RFC 9207 `iss` validation added. **Dynamic Client Registration (RFC 7591) is DEPRECATED** in favor of **Client ID Metadata Documents** (`draft-ietf-oauth-client-id-metadata-document-00`) — https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization (fetched 2026-08-13). Step-up auth via `403 + WWW-Authenticate: error="insufficient_scope", scope="..."`.
- **Caching, new and load-bearing for token cost:** `CacheableResult` — `ttlMs` + `cacheScope` (`public`/`private`) now **required** on `tools/list`, `prompts/list`, `resources/list`, `resources/read`, `resources/templates/list`. Servers SHOULD return `tools/list` in **deterministic order** to improve LLM prompt-cache hit rates.
- **Deprecated:** Roots, Sampling, Logging (SEP-2577, 12-month minimum window). Don't build on them.
- **Tool schemas loosened:** full JSON Schema 2020-12 keywords in `inputSchema`/`outputSchema`; `structuredContent` may be any JSON value (SEP-2106).
- **No spec-level tool-output size limit found.** Size discipline is client-side convention (see A3).

**Adoption:** vendor support well-attested (OpenAI Agents SDK/Responses API/ChatGPT Mar 2025; Google Gemini/Vertex Apr 2025; Microsoft Windows/GitHub/Azure AI Foundry at Build 2025). Quantitative figures circulating (~97M monthly SDK downloads by Mar 2026; ~9,652 servers in the official Registry on 2026-05-24; 41% of orgs in production per "Stacklok 2026") come only from SEO-aggregator sites — **[UNVERIFIED]**, directional only.

### A2. How sandbox providers expose agent interfaces

- **E2B**: SDK-first (Python/JS) + native MCP *inside* the sandbox. Docker partnership brings 200+ Docker MCP-catalog tools into every sandbox via an MCP gateway reachable from inside (localhost) and outside (sandbox URL) — https://e2b.dev/blog/docker-e2b-partner-to-introduce-mcp-support-in-e2b-sandbox. The community/official e2b MCP *server* is minimal: `create_sandbox`, `execute_code_sandbox`, `kill_sandbox` (https://playbooks.com/mcp/e2b-dev-mcp-server-python). Network controls first-class: `allowInternetAccess` (false ≡ `network.denyOut: ['0.0.0.0/0']`), CIDR + **domain-based** allow/deny for HTTP/TLS incl. `*.wildcard`, and `updateNetwork()` to mutate a *running* sandbox's egress — https://docs.e2b.dev/sandbox/internet-access (fetched 2026-08-13).
- **Daytona MCP**: filesystem ops, shell exec, git clone, and **public URL tunneling for web previews** — https://www.daytona.io/docs/en/mcp/ and https://www.daytona.io/dotfiles/introducing-daytona-mcp-server.
- **Cloudflare Sandbox SDK**: V8-isolate/container hybrid. `exposePort()` → preview URL `https://{port}-{sandbox-id}-{token}.yourdomain.com` (crypto token embedded in the hostname, wildcard-subdomain routing); router called at top of the Worker `fetch` handler. Added 2026-05-29: `sandbox.tunnels.get(port)` → zero-config `*.trycloudflare.com` quick tunnel via bundled `cloudflared` — https://developers.cloudflare.com/changelog/post/2026-05-29-sandbox-named-tunnels/ and https://developers.cloudflare.com/sandbox/concepts/preview-urls.
- **Modal**: gVisor-based Sandboxes, GPU-capable, positioned as an OpenAI Agents SDK sandbox provider; claims 50,000+ concurrent sessions — https://modal.com/resources/best-code-execution-sandboxes-ai-agents (vendor marketing, 2026) **[partly UNVERIFIED]**.
- **Anthropic**: as of ~May 2026, **self-hosted sandboxes for Claude Managed Agents (public beta)** and **MCP tunnels (research preview)** — outbound-only encrypted connections letting Claude reach private MCP servers with no inbound firewall rules. Only found via third-party writeups (dev.to) — **[UNVERIFIED against Anthropic docs]**, confirm directly before relying on it.

### A3. Published agent-ergonomic API design guidance

- **Anthropic, "Writing tools for agents", 2025-09-11** — https://www.anthropic.com/engineering/writing-tools-for-agents. Concrete: prefix namespacing by service+resource (`asana_projects_search`); unambiguous param names (`user_id` not `user`); **Claude Code truncates tool responses at 25,000 tokens by default**; build in pagination/range/filter/truncation with sane defaults; a `ResponseFormat` enum (`"concise" | "detailed"`) where concise ≈ ⅓ the tokens; **prefer natural-language names over UUIDs**, return raw IDs only when needed downstream; errors must be specific and actionable with examples.
- **Anthropic, "Code execution with MCP", 2025-11-04** — https://www.anthropic.com/engineering/code-execution-with-mcp. Tools as a *filesystem of functions* (`./servers/google-drive/getDocument.ts`), progressive disclosure, agent writes code to chain calls so intermediates never traverse the model. Headline: **150,000 → 2,000 tokens (98.7% reduction)** on their example.
- **Cloudflare, "Code Mode", 2025-09-26** — https://blog.cloudflare.com/code-mode/. Convert MCP tools to a TypeScript API and have the model write code against it: *"LLMs have seen a lot of code. They have not seen a lot of 'tool calls'."* Runs in fresh-per-snippet V8 isolates via the **Worker Loader API**; isolates have **no general network access**, only bindings to MCP servers, keeping API keys out of generated code. No token-savings numbers given.
- **Idempotency for agent retries**: **no dedicated published spec/post found** on idempotency keys for agent tool retries. Closest normative material: the MCP `tasks` extension (durable handles + poll) and SSE-resumability removal, which together *force* clients to re-issue requests with new IDs — meaning **server-side idempotency is effectively required, but unspecified by MCP**. Searched: MCP changelog, spec index, Anthropic engineering blog. **Gap flagged in the architecture plan (§6.1: Idempotency-Key header, mandatory).**

---

## (B) Network Security

### B4. Metadata endpoint (169.254.169.254)

- **Canonical incident: Capital One (2019)** — SSRF against EC2 IMDSv1 → IAM role creds → ~100M records. Origin of IMDSv2.
- **Ongoing, current:** MITRE ATT&CK **T1552.005** "Cloud Instance Metadata API" — https://attack.mitre.org/techniques/T1552/005/. F5 Labs documented an active **campaign scanning for SSRF-to-EC2-IMDS** — https://www.f5.com/labs/articles/campaign-targets-amazon-ec2-instance-metadata-via-ssrf. 2025: **CVE-2025-51591** (Pandoc SSRF via crafted `<iframe>` → EC2 IMDS creds), exploited in the wild — https://thehackernews.com/2025/09/hackers-exploit-pandoc-cve-2025-51591.html (Sept 2025). Pandoc is *exactly* the class of document-conversion tool a sandbox platform runs on user input. 2026: **CVE-2026-14646** (Sonatype Nexus applied SSRF protections to direct proxy requests but **not to HTTP redirect targets**) — the redirect-bypass pattern is the recurring failure mode.
- **Mitigations, documented:**
  - IMDSv2 (PUT-issued session token) + **hop limit**. AWS: default PUT response hop limit is 1; EKS managed node groups default to **2** because pods run in a separate netns — https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-metadata-limiting-access.html.
  - **Critical caveat**: hop-limit=1 only blocks bridge-mode containers. `awsvpc` and `host` networking modes remain 1 hop from IMDS and **can still reach it** — https://www.latacora.com/blog/2025/10/02/ecs-on-ec2-covering-gaps-in-imds-hardening/ (2025-10-02). **Do not rely on hop limit alone for a multi-tenant sandbox.**
  - Explicit drop from the guest: `iptables --insert DOCKER-USER 1 --in-interface docker+ --destination 169.254.169.254/32 --jump DROP` (AWS re:Post guidance). Per-VM netns + explicit DROP is the belt-and-braces answer.

### B5. Abuse / cryptomining in free-tier compute

- **PurpleUrchin "freejacking" (Unit 42, Jan 2023)**: automated abuse across GitHub (~300 accts), Heroku (~2,000), Buddy.works (~900); >1M function calls/day — https://www.bleepingcomputer.com/news/security/massive-cryptomining-campaign-abuses-free-tier-cloud-dev-resources/ and https://thenewstack.io/purpleurchin-github-actions-hijacked-for-crypto-mining/.
- **Sysdig TRT, GitHub Actions mining op** — https://www.sysdig.com/blog/massive-cryptomining-operation-github-actions.
- **GitLab.com policy response (2021-05-17)**: required a **valid credit card** for shared runners on free tier; abusers reused one card across many accounts to defeat it — https://forum.gitlab.com/t/preventing-crypto-mining-abuse-on-gitlab-com-saas/52911 and https://www.theregister.com/2021/05/19/gitlab_crypto/. **Lesson: payment-instrument gating is a weak signal; velocity/reuse detection matters more.**
- **GitHub response**: changed PR handling so a bare PR from a fork can't trigger workflow execution on upstream.
- **Affected-platform list (broad)**: GitHub, GitLab, Azure, TravisCI, LayerCI, CircleCI, Render, CloudBees CodeShip, Sourcehut, Okteto — https://therecord.media/crypto-mining-gangs-are-running-amok-on-free-cloud-computing-platforms.
- **Egress policy**: **no published postmortem** from Fly.io / Val Town / Replit detailing stratum-protocol blocking or default-deny egress. What exists is policy-level: Replit ToS bans miners outright and ships **DNS-level blocking** (https://blog.replit.com/family-friendly-dns); Fly.io **blocks outbound SMTP by default and unblocks on request** (evidenced by repeated community threads, e.g. https://community.fly.io/t/request-to-unblock-outbound-smtp-port-587-for-app-rialma-api/27164) and has a network-policies doc (https://fly.io/docs/machines/guides-examples/network-policies/). Port-25 blocking is near-universal cloud practice. **Stratum-specific blocking: not found in any published provider doc.** [UNVERIFIED — the practical prior is CPU-pattern + egress-destination detection, not protocol blocking.]

### B6. Preview URL / per-sandbox ingress

- **E2B**: `https://{PORT}-{SANDBOX_ID}.e2b.app`. Wildcard DNS + port-as-leftmost-label. Docs mention "Restricting public access" and a `maskRequestHost` option but **do not document token auth on the URL itself** — https://docs.e2b.dev/network/public-url (fetched 2026-08-13). Treat E2B preview URLs as capability-URLs at best.
- **Cloudflare Sandbox SDK**: `https://{port}-{sandbox-id}-{token}.yourdomain.com` — **unguessable token embedded in the subdomain**, i.e. capability URL by construction. Plus optional named/quick Cloudflare Tunnels.
- **GitHub Codespaces**: `https://{CODESPACENAME}-{PORT}.app.github.dev`. Ports default to **private**; three visibility levels (private / org / public). **Private ports on the remote domain require an `X-Github-Token: TOKEN` header** (real auth, not just an unguessable URL). Org owners can forbid public port sharing. Switching a public port to HTTPS auto-reverts visibility to private — https://docs.github.com/en/codespaces/developing-in-a-codespace/forwarding-ports-in-your-codespace (fetched 2026-08-13).
- Pattern summary: everyone uses **wildcard DNS + `{port}-{id}` leftmost label + SNI/Host routing**. The differentiator is auth: Codespaces = real bearer token; Cloudflare = capability token in hostname; E2B = effectively public-if-known. **For a multi-tenant agent platform, Codespaces' default-private + token model is the defensible one; agents handle bearer headers fine.**

### B7. eBPF vs nftables for per-VM egress at density

- The well-attested claim is architectural: iptables/nftables does **O(n) sequential rule traversal** while eBPF does **O(1) hash-map lookups**, and eBPF rule count doesn't scale linearly with endpoint count. Cited breakpoint: ~100 services fine, ~1,000 services → ~10,000+ rules → significant impact; ~5,000 services → tens of thousands of rules with linearly growing connection-setup latency.
- **Sourcing caveat: this was only found in 2026 SEO/tutorial content** (codingprotocols.com, k8s.info, tutorials.technology), **not in a primary Cilium/Isovalent benchmark**. **Mark as [UNVERIFIED]; pull the Cilium docs / Isovalent benchmark posts directly before it goes in a design.** Note also that modern **nftables** (sets/maps, unlike legacy iptables) does *not* have the same O(n) problem — the SEO articles conflate iptables and nftables. That conflation is the single biggest risk in this section.
- Counterweight for the isolation layer generally: **Firecracker's first two escape-class CVEs landed in 2026** — CVE-2026-5747 (OOB write in virtio-pci, Firecracker 1.13.0–1.14.3 and 1.15.0, x86_64+aarch64; guest root → VMM crash or potential host code execution; fixed in 1.14.4 / 1.15.1; reported via AWS VDP 2026-04-07) — https://github.com/firecracker-microvm/firecracker/security/advisories/GHSA-776c-mpj7-jm3r and https://aws.amazon.com/security/security-bulletins/2026-015-aws. Also CVE-2026-1386 (jailer symlink host-write) **[UNVERIFIED]**. **Defense in depth on the network layer must assume the hypervisor boundary can fail.**

---

## Load-bearing sources

1. https://modelcontextprotocol.io/specification/2026-07-28/changelog — MCP 2026-07-28 key changes (sessions removed, stateless, tasks extension, SSE resumability removed)
2. https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization — OAuth 2.1 RS, RFC 9728/8707/9207, DCR deprecated → Client ID Metadata Documents
3. https://modelcontextprotocol.io/specification/ — confirms 2026-07-28 is current; tasks/skills/apps extensions
4. https://blog.modelcontextprotocol.io/posts/2026-07-28-release-candidate/ — RC published 2026-05-21
5. https://www.anthropic.com/engineering/writing-tools-for-agents (2025-09-11) — 25k-token cap, namespacing, ResponseFormat, ID/name guidance
6. https://www.anthropic.com/engineering/code-execution-with-mcp (2025-11-04) — filesystem-as-API, 150k→2k tokens
7. https://blog.cloudflare.com/code-mode/ (2025-09-26) — code-over-tool-calls, V8 isolates, no general network access
8. https://developers.cloudflare.com/sandbox/concepts/preview-urls + https://developers.cloudflare.com/changelog/post/2026-05-29-sandbox-named-tunnels/ — `{port}-{id}-{token}` wildcard URLs; tunnels
9. https://docs.github.com/en/codespaces/developing-in-a-codespace/forwarding-ports-in-your-codespace — private-by-default ports, `X-Github-Token` auth
10. https://docs.e2b.dev/sandbox/internet-access + https://docs.e2b.dev/network/public-url — `allowInternetAccess`, domain/CIDR deny lists, `updateNetwork()`, `{port}-{id}.e2b.app`
11. https://e2b.dev/blog/docker-e2b-partner-to-introduce-mcp-support-in-e2b-sandbox — MCP gateway inside sandboxes
12. https://www.daytona.io/docs/en/mcp/ — Daytona MCP toolset incl. public preview tunneling
13. https://www.latacora.com/blog/2025/10/02/ecs-on-ec2-covering-gaps-in-imds-hardening/ — hop-limit=1 only covers bridge mode; awsvpc/host still reach IMDS
14. https://thehackernews.com/2025/09/hackers-exploit-pandoc-cve-2025-51591.html — in-the-wild SSRF→IMDS via a document-conversion tool
15. https://www.sysdig.com/blog/massive-cryptomining-operation-github-actions + https://forum.gitlab.com/t/preventing-crypto-mining-abuse-on-gitlab-com-saas/52911 — freejacking scale; credit-card gating and its bypass
16. https://github.com/firecracker-microvm/firecracker/security/advisories/GHSA-776c-mpj7-jm3r — CVE-2026-5747 Firecracker escape-class advisory

**Top three things to verify before the plan hardens:** (a) Anthropic self-hosted sandboxes / MCP tunnels — confirm from Anthropic docs, not DEV.to; (b) eBPF-vs-**nftables** scaling — get a primary benchmark; (c) MCP adoption numbers — all quantitative figures came from aggregator blogs.
