# Backend-facing source port: making the payload's claim literally true

**Status: proposed, not yet implemented.** Written 2026-08-26 chasing the
still-open AKS connection-never-completes investigation (see
`docs/directplay8-protocol.md`'s ack-tail-byte correction and
`clientSessionPort`'s own doc comment in `lobby/main.go` for the
client-facing half of this same bug class, already fixed and deployed).
This doc covers the other half - the backend-facing direction - which
is a genuine redesign, not a one-line patch, because of a real conflict
with this project's own 1v1-per-host architecture. Not yet built or
tested; this is the plan, written before touching any code, matching
this project's own established practice for changes of this size (see
`lobby/session-cleanup-design.md`, `lobby/join-cushion-design.md`).

## Why this matters

`clientSessionPort` (2026-08-25) fixed a real, live-verified bug: the
client-facing rewrite told the client's own DirectPlay engine "you are
at port 2300" (matching the client's own genuine local bind), instead
of the literal NAT-remapped source port `aom-lobby` actually observed
on the wire. That fix shipped and is confirmed correct at the addressing
level - but the AKS connection **still** never completes: the backend
still never sends a real handshake ack (`0x02`), across every attempt
tested since, with every other explanation (proxy fidelity, game binary
version, Wine version, the intermittent `0x0230` message, the port-
remap mechanism itself) ruled out via direct live testing (see this
project's own session history around 2026-08-25/26 for the full trail -
ingress-level packet captures confirmed byte-for-byte identical traffic
between what arrives at `aom-lobby` and what gets forwarded to the
backend, so the proxy itself is not dropping, corrupting, or duplicating
anything).

That leaves one more place the exact same class of bug could be hiding,
found by asking the same question in the other direction: **does the
backend-facing payload's own claim about its "self" address match what
the backend literally observes on the wire?**

## The mismatch, found directly in the code

`rewriteToBackend`'s handshake rewrite (`lobby/main.go`):

```go
out := rewriteSessionField(payload, sessionSelfOffset, cfg.internalIP, cfg.publicPort)
```

This tells the backend, inside the DirectPlay payload: *"the client
(really `aom-lobby`, standing in for it) is at `cfg.internalIP:cfg.publicPort`"*
- and `cfg.publicPort` is `2300`.

But the actual UDP socket `aom-lobby` uses to talk to the backend
(`forwardToBackend`, `lobby/main.go`):

```go
backendConn, err := net.DialUDP("udp", nil, backendUDPAddr)
```

`nil` for the local address means the OS picks a **random ephemeral
port** for this socket - confirmed directly via a live ingress-level
packet capture on the `aom-lobby` pod's own network namespace
(`kubectl debug --target=aom-lobby`, 2026-08-26): the backend was
observed replying to ports like `46282` and `41328`, never `2300`.

So the payload claims port `2300`; the literal wire-level source port
is something else entirely. This is the exact same shape as the bug
`clientSessionPort` fixed - a payload-level lie the receiving DirectPlay
engine may be validating against literal wire reality and rejecting.
**Not yet confirmed to be the actual root cause of the stuck handshake**
- only confirmed to exist, and to be a live, real mismatch worth closing
regardless, on the same reasoning basis that motivated the client-facing
fix.

## Why this isn't a one-line fix: the two-clients-per-host conflict

The naive fix - explicitly bind local port 2300 when dialing the backend,
same idiom as `clientSessionPort`'s reasoning:

```go
backendConn, err := net.DialUDP("udp", &net.UDPAddr{Port: int(cfg.publicPort)}, backendUDPAddr)
```

works fine as long as every session's own dial targets a **different**
backend pod - a UDP "connected" socket's identity is its full 4-tuple
(local IP:port + remote IP:port), so two different remote pod addresses
naturally produce two different 4-tuples even with the same local port.

It breaks the moment two real clients are matched onto the **same**
host pod - which isn't an edge case here, it's the normal shape of a
finished match (see `CLAUDE.md`'s Goals: "this project only ever hosts
1v1s"). Both sessions' backend-dial sockets would need
`(aom-lobby pod IP : 2300) -> (that one pod's IP : 2300)` - an identical
4-tuple. That's a genuine `EADDRINUSE` conflict, not a cosmetic one, and
`SO_REUSEPORT` doesn't solve it: that mechanism load-balances *incoming*
connections across multiple listeners, but two independent app-level
sessions both owning the *same* connected 4-tuple leaves the kernel with
no way to know which socket a given backend reply belongs to - traffic
would split unpredictably between the two sessions, corrupting both.

## The design: one shared backend-dial socket per host, not per session

Since the conflict only exists between sessions sharing a single host
pod, and a pod is architecturally capped at 2 real sessions, the fix is
to dial the backend **once per host**, lazily, and have every session
assigned to that host reuse the one socket - not one dial per session.

- New field on `hostCandidate` (not `session`): `backendConn *net.UDPConn`,
  created the first time a session is assigned to that host:
  ```go
  net.DialUDP("udp", &net.UDPAddr{Port: int(cfg.publicPort)}, backendUDPAddr)
  ```
- Because the dial happens once per *host*, never once per *client*,
  there is at most one such socket per pod at any time, regardless of
  how many of that pod's (at most 2) sessions are currently active - no
  conflict, by construction.

### What sharing breaks, and how each piece gets fixed

**Demuxing incoming replies.** Today, each session's own dedicated
`backendConn` has its own read goroutine, so "which session does this
packet belong to" is answered for free, by which socket it arrived on.
Once two sessions share one socket, that free answer disappears -
replies need to be attributed by *packet content* instead. This project
already has precedent for exactly this problem: `relayBackendInitiated`
(existing code) already attributes an unprompted backend broadcast on
the 0x03-wrapped settings-sync layer to the right client by content,
via `wrapperConnID` - matching the sender's own 2-byte connection ID
against `session.connID`, learned the first time it's seen. The same
pattern needs to extend to the raw 0x00/0x02/0x07ff handshake layer,
which doesn't currently need this kind of lookup because each session
owns its own socket today.

**The genuinely open risk, stated plainly rather than assumed away:**
connID-based demuxing only works once a connID has actually been
*learned* for a session - and the failure this whole investigation is
chasing lives in the *earliest* part of the handshake, before either
side has necessarily settled into a stable, attributable identity. If
two sessions' very first opens to the same host land close together in
time, it's a real open question whether the demux logic can tell them
apart yet, or whether the first packet of a second session could get
misattributed to the first session's own state before its own connID is
known. In practice, this project's existing pairing flow has the second
real client join *after* the first is already established (not
genuinely simultaneous), which should make the tightest version of this
race rare rather than routine - but "should be rare" is a hypothesis to
verify with a deliberate two-client test (see Testing below), not
something to ship on the assumption alone.

**Socket lifecycle and teardown.** Today, ending a session calls
`sess.backendConn.Close()` as its own teardown signal - this is load-
bearing, not incidental: it's what triggers `backendToClient`'s own
read-error cleanup path, which every departure mechanism in this
project already routes through (resign-burst teardown, session-cleanup-
design.md's Phase 3, the lonely-player-kick feature's explicit
teardown, etc.). With a shared per-host socket, naively closing it on
any *one* session's teardown would incorrectly kill the *other* session
still using that same host. This needs reference counting on
`hostCandidate.backendConn`: increment when a session starts using it,
decrement on that session's teardown, and only actually call
`.Close()` once the count reaches zero - the same "own the resource,
tear down on last user, not first" shape `lobby/session-cleanup-design.md`
already established for other per-host state (`pairRelay`, `hostProbe`).

## Recommended sequencing

1. **Single-client-per-host case first.** Since a lone session on a host
   never hits the sharing conflict at all (there's nothing to share
   with), this can ship as a much smaller change initially: bind local
   port `cfg.publicPort` on that session's own dial, no sharing/refcount/
   demux machinery yet. This is also the exact case the current stuck-
   connection investigation needs - it directly tests whether this
   mismatch is actually contributing to the backend never acking, before
   investing in the harder two-client design.
2. **Only then** build the sharing/demux/refcounting work described
   above, and test it *specifically* for the two-client case - get two
   real clients matched onto one host deliberately and confirm neither
   session's traffic leaks into the other's, rather than inferring
   correctness from the single-client case alone.
3. Given this touches core socket-ownership and session-teardown
   assumptions used throughout `lobby/main.go` (not a self-contained
   addition), treat this as review-worthy in the same way
   `session-cleanup-design.md`'s Phase 2/3 work was - a real design
   change to load-bearing lifecycle code, not a quick patch.

## Explicitly out of scope for this pass

- Any change to the **client-facing** direction - `clientSessionPort`
  already covers that, and is unaffected by anything in this doc.
- Handling more than 2 real sessions per host pod - not a real scenario
  under this project's own 1v1-only architecture (see `CLAUDE.md`'s
  Goals), so the refcounted single-shared-socket design doesn't need to
  generalize beyond that.
- Confirming this is *the* remaining root cause of the stuck AKS
  handshake before implementing step 1. It's a real, live-verified
  mismatch worth closing on its own merits (same reasoning as
  `clientSessionPort`), but whether closing it actually produces a real
  ack from the backend is an open empirical question step 1's own
  testing needs to answer, not something this doc claims in advance.

## Verifying the fix (once implemented)

1. **Step 1 (single-client) verification**: redeploy with the explicit-
   local-port dial, retest a single real client attempt against AKS,
   confirm via `aom-lobby`'s own verbose logs and/or an ingress-level
   packet capture that the backend now observes `aom-lobby`'s traffic
   arriving from literal port 2300 (not a random ephemeral one) -
   independent of whether the ack itself starts arriving, since the
   addressing correctness and the handshake-completion question are
   separate things to confirm separately, same discipline this
   project's other NAT-adjacent fixes have followed.
2. **Step 2 (two-client) verification**: deliberately get two real
   clients matched onto the same host, confirm both complete their own
   independent handshakes correctly with no cross-session traffic
   leakage, and confirm normal match teardown (one client leaving)
   doesn't disturb the other's still-active session - directly
   exercising the refcounted-close logic, not just the happy path where
   both sessions end at the same time.
