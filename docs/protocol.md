# SGP/1 — Substrate Guest Protocol v1

The only contract between `noded` (host) and the guest agent. Transport-agnostic:
the identical bytes run over a Firecracker **vsock**, a unix socket, or TCP.

There are **two** guest agents speaking it, and they must stay byte-compatible:

| agent | language | tiers | transport |
|---|---|---|---|
| `guestd` | Rust | `hv` (Firecracker) | vsock, guest CID port 5000 |
| `substrate-shim` | Go | `kernel` (gVisor), `process` | unix socket |

## Transports and who connects

Two directions, and the difference is not cosmetic:

**Guest listens, host connects** — vsock (`hv`) and `substrate-shim --unix`
(`process`). The agent binds and waits. It listens on nothing else: no IP, no
port, unreachable from guest network space.

**Host listens, guest dials out** — `substrate-shim --connect` (`kernel`).
Required under gVisor: a unix socket *bound inside* the sandbox lives in the
sentry's own VFS and the gofer does not propagate it, so the host sees an empty
directory while the sandbox sees its own socket. Verified directly:

```
host:    ls /…/sock/  -> (empty)
sandbox: ls /sock/    -> srw------- agent.sock
```

The host therefore creates the socket before the sandbox starts (which also
removes the "agent dialled too early" race) and the agent connects out.

### The agent survives its connection

On the dial-out transport the agent is the sandbox's **PID 1**. It serves a
connection, and when that connection ends it **reconnects** rather than exiting.
It used to exit, which meant any transport error — a mux desync, a slow reader,
an unclean host exit — destroyed the sandbox and everything written into it.

Execs still die with their connection (see *Reattach* below). What survives a
reconnect is the sandbox and its filesystem, not the processes inside it.

### Agent authentication (dial-out only)

Because the host keeps its listener open for the whole life of the sandbox, and
that socket is bind-mounted **into** the sandbox, anything running inside can
dial it. An accepted impostor answers on the sandbox's behalf — including
reporting the CPU and memory that usage is billed from.

So on this transport the agent proves its identity:

1. The host writes a 32-byte random token, hex encoded, mode `0600`, into the
   sandbox's own socket directory before the container starts.
2. The agent reads it and **unlinks it** before serving anything. It is PID 1 and
   the OCI spec starts nothing else, so no guest code can exist in that window.
   A failed unlink is fatal — a token the tenant can read is worse than no
   sandbox.
3. Every connection, initial and reconnect, carries it in `Hello.token`.
4. The host compares in constant time and drops the connection on mismatch. A
   rejected connection does **not** end the host's re-accept wait, or forging one
   would be a reliable way for a tenant to kill its own sandbox.

The token is **never** passed in argv or the environment: both are readable from
inside via `/proc/1`. Only the token's *path* appears there.

Agents on the listen transports do not send a token, and the host requires one
only when it issued one — so `guestd` is unaffected.

## Mux framing

Little-endian. Header is 13 bytes:

```
offset size field
0      4    stream_id  u32   (0 reserved; initiator: host uses odd ids, guest even)
4      1    flags      u8    bit0 SYN (open stream), bit1 FIN (close write side),
                             bit2 BINARY (payload is raw bytes, not JSON)
5      1    op         u8    message type (below)
6      4    length     u32   payload length; hard cap 1 MiB (1_048_576)
10     3    reserved   must be zero
```

Header is 13 bytes total.

Payload follows when length > 0.

Stream lifecycle: the first frame on a stream MUST carry SYN and the request op.
Responses reuse the stream id. A peer closes its write side with FIN. A stream is
reaped when both sides have FIN'd, or immediately after an error response.

## Ops

| op | name        | direction | payload |
|----|-------------|-----------|---------|
| 0  | PING        | both      | empty; reply PONG |
| 1  | PONG        | both      | empty |
| 2  | HELLO       | g→h       | JSON Hello — first frame on every connection |
| 3  | EXEC_REQ    | h→g       | JSON ExecRequest |
| 4  | EXEC_DATA   | both      | BINARY: raw stdout/stderr/stdin bytes. Non-binary from g→h: JSON ExecDataMeta `{exec_id, fd:"out"\|"err", eof:true}` announcing EOF for that fd |
| 5  | EXEC_EXIT   | g→h       | JSON ExecExit `{exec_id, code, killed}` then FIN |
| 6  | EXEC_SIGNAL | h→g       | JSON ExecSignal `{exec_id, signal}` (kill = signal "KILL") |
| 7  | FS_REQ      | h→g       | JSON FsRequest (+ optional BINARY FS_DATA frames from host for writes) |
| 8  | FS_DATA     | both      | BINARY raw file bytes |
| 9  | FS_RESP     | g→h       | JSON FsResponse (+ optional BINARY FS_DATA frames for reads), then FIN |
| 10 | PORTS       | g→h       | JSON PortsAnnounce `{ports:[{port,proto}]}` — re-announced after every resume |
| 11 | METRICS     | g→h       | JSON GuestMetrics (advisory only; billing uses host cgroups) |
| 12 | RESEED      | h→g       | JSON Reseed `{vmgenid}` — sent once after every restore before traffic |
| 13 | RESEED_ACK  | g→h       | JSON ReseedAck `{machine_id, ssh_fingerprint}` |
| 14 | EXEC_RESIZE | h→g       | JSON ExecResize `{exec_id, rows, cols}` |

## Message schemas (JSON)

```json
Hello         {"proto":1,"agent":"guestd","version":"1.0.0","boot_id":"...","features":["exec","fs","pty"]}
              // optional: "token":"<64 hex chars>" — REQUIRED on the dial-out
              // transport, omitted entirely elsewhere. See Agent authentication.
ExecRequest   {"exec_id":"e-...","argv":["bash","-lc","make"],"cwd":"/workspace","env":{"K":"v"},
               "tty":false,"rows":24,"cols":80,"user":"root"}
ExecExit      {"exec_id":"e-...","code":0,"killed":false}
FsRequest     {"op":"read|write|list|remove|mkdir|stat","path":"/abs/path","offset":0,
               "length":0,"mode":420,"append":false}
FsResponse    {"ok":true,"error":"","size":123,"entries":[{"name":"x","size":1,"dir":false}]}
PortsAnnounce {"ports":[{"port":3000,"proto":"tcp"}]}
GuestMetrics  {"mem_total_kb":512000,"mem_avail_kb":210000,"load1":13,"pids":42,"uptime_s":3600}
Reseed        {"vmgenid":"8f3b..."}
ReseedAck     {"machine_id":"...","ssh_fingerprint":"SHA256:..."}
```

## Rules

- Every frame from the guest is length-checked and schema-validated by the host;
  the guest is hostile the moment user code runs.
- Bulk data never rides inside JSON; it uses BINARY frames (no base64 tax).
- One logical operation per stream; no cross-stream ordering guarantees except
  that a stream's response follows its request.
- Either side may PING; missing PONG within 10s marks the connection dead.
