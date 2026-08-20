# Multi-peer routing design: making aom-lobby route a full 1v1 match

**Status (2026-08-10): implemented and confirmed working.** Originally
written 2026-08-08 as design-only. `lobby/main.go` now has the per-pair
relay (`pairRelay`/`newPairRelay`/`matchState`) and both rewrite
directions (`0x29` for the existing peer, `existingPeerBroadcast` for the
new peer - see the "asymmetric" correction below, that assumption turned
out to be wrong). Verified end to end through a real 2-client join
against the cluster: both clients connect, see each other, and exchange
chat. Durability (resign detection, scoped teardown, clean re-fill on a
dropped player, see that section below) is still not implemented - the
rest of this doc's content is otherwise accurate to what got built,
kept as-is below as the design record rather than rewritten after the
fact.

## The problem

`aom-lobby` (`lobby/main.go`) currently proxies exactly one relationship:
one real client ↔ the one real backend pod, treated as "the host." Every
client is told (via rewritten discovery/ping/handshake replies) that the
host is at the proxy's address; the backend is told (via the same
rewriting) that every client is at the proxy's address too. This worked -
confirmed with a real Direct-Connect join after fixing the session-port
handshake bug earlier the same day (see `docs/directplay8-protocol.md`).

It breaks down with a second real client. AoM's DirectPlay session turns
out to be genuinely peer-to-peer, not host-relayed: once two real clients
are in the same match, they open a **direct** UDP 2300 session with each
other, completely bypassing the host. `aom-lobby` has no way to see or
relay that traffic today - neither client is ever told the other's real
address, so there's nothing for the proxy to intercept. This was proven
with a from-scratch non-proxied 3-container capture; full writeup and
evidence in `docs/directplay8-protocol.md`'s "Sessions are genuinely
peer-to-peer, not host-relayed" section and
`archiving/sessions/20260808-234138-3client-p2p-check/NOTES.md`. Read
those first if picking this up cold - this doc assumes that context.

**Scope**: this project only ever hosts 1v1s (host in Observer Mode + two
real playing clients, per CLAUDE.md's Goals). So the ceiling is exactly
3 real peers and 3 pairwise relationships: host↔A, host↔B, A↔B. Nothing
here needs to generalize beyond that - no N-player lobbies, no
arbitrary-size mesh.

## The mechanism that makes this tractable

The proxy doesn't need to guess who a piece of traffic is for by parsing
DirectPlay connection IDs out of arbitrary packets. It controls the *one*
message that seeds every peer-to-peer connection in the first place: a
newly-documented settings-sync sub-message, type `0x29` (nested inside
the existing `03 00` wrapper), that the host sends to an *already-
connected* peer whenever a *new* peer joins, containing the new peer's
real `sockaddr_in`. Rewrite that message the same way `discoveryRewrite`
already rewrites `0x26`/`0x21`/`0x20`, and the proxy fully controls which
address each client tries to open a session with next. Exact byte
offsets are in `docs/directplay8-protocol.md`.

**Correction (2026-08-10): not asymmetric after all.** Originally
believed only the *existing* peer gets a broadcast and the *new* peer
just listens passively. Wrong - the new peer gets its own, different
(87-byte, not 51-byte) address-carrying message, found by re-examining
the archived genuine capture. Both directions needed rewriting, not one
- see `docs/directplay8-protocol.md`'s correction note and
`docs/directplay8-packet-classification.md` for the full byte-level
writeup. Implemented as two parallel code paths in `lobby/main.go`
(`newPeerBroadcast` for the existing-peer-facing one, `existingPeerBroadcast`
for the new-peer-facing one), both converging on the same lazily-created
`pairRelay` via `matchState.ensurePairRelay`'s idempotent creation.

## Proposed architecture: one dedicated relay port per pair

Instead of trying to demux a shared port by embedded connection ID,
**give every pairwise relationship in a match its own dedicated proxy
port**. For a live 1v1 (host, client A, client B):

- Host↔A → proxy port `P1` (this is exactly today's existing relay,
  unchanged - a client and "the backend")
- Host↔B → proxy port `P2` (same code, second instance)
- A↔B → proxy port `P3` (**new**, but the *same* relay logic again - it's
  just two clients instead of a client and the backend; nothing about
  today's `proxy` struct's rewrite logic actually assumes one side is
  special)

When the `0x29` broadcast tells A about B, rewrite the embedded address
to `PUBLIC_IP:P3` instead of B's real address. B, reached at that same
port by A's proactive `type 0x00`, works the address out from the
packet's real source the normal way. From there it's the identical
self/peer rewrite dance already built and tested for host↔client -
`sessionSelfOffset`/`sessionPeerOffset`, `isSessionHandshake`, the whole
existing session-proxy machinery - just instantiated per pair.

This sidesteps connection-ID-based demuxing entirely: each port only
ever has two real endpoints talking through it, so today's
address-keyed session map (`proxy.sessions map[string]*session`) is
sufficient again, unmodified.

### Rough shape (not final API)

- A `match` type: one active 1v1 lobby. Holds the host's real backend
  address and up to two client slots. Each slot holds: the real client
  address (or empty), and which pair-relay(s) currently serve it (its
  host-pair, and the other-slot pair once both slots are filled).
- Generalize today's `proxy` struct (or wrap it) so a pair-relay is
  parameterized by *which two real addresses it bridges*, rather than
  hardcoding "client side" vs "backend side." Port allocation: dynamic
  (`:0` bind, read back the OS-assigned port) rather than fixed, since
  there can be up to 3 of these per match and (later, per CLAUDE.md)
  potentially multiple concurrent matches.
- Extend the rewrite dispatch: today's `discoveryAddressOffsets` is keyed
  by a top-level type byte. The `0x29` rewrite needs its own lookup,
  keyed on (wrapper type `0x03`, sub-type `0x29`), since it's nested one
  level deeper - not a drop-in to the existing table without a small
  shape change there.

## Durability: a dropped player must not disturb the other one

Explicit requirement from this session: if slot 2's client drops while
slot 3's client is still connected and playing, slot 3's session must
keep working undisturbed, and a new player joining the vacated slot 2
should work like any fresh join.

**Isolation is structural, close to free**: because each pair has its
own relay/session state (separate maps, separate dialed sockets, no
shared mutable state across pairs), tearing down slot 2's pairings can't
corrupt slot 3's as long as teardown is scoped correctly (see below) -
this falls out of the per-pair design rather than needing new machinery.

**What does need building:**

1. **Departure detection**, two signals:
   - Fast path: the already-documented 3-byte `01 <connID>` resign/leave
     burst (`docs/directplay8-protocol.md`'s "Client resign/leave
     signature") - cheap to detect inline in the pair-relay's forward
     path (fixed shape, 3 bytes). React immediately on any pair involving
     the departing peer.
   - Slow/fallback path: the existing `idleTimeout`/`reapIdleSessions`
     mechanism already in `lobby/main.go` (currently 30s) - catches
     anything that doesn't cleanly resign (crash, network drop, killed
     container). Keep this as a safety net even once resign-detection
     exists - real captures haven't yet actually shown the resign burst
     firing between two *clients* (only client→host, in an older single-
     client capture, and even the recent 3-client capture never caught it
     - see `docs/directplay8-protocol.md`'s note), so it may not always
     fire between peers the way it does with the host.
2. **Scoped teardown**: closing a departed peer's pairings (host↔departed
   and survivor↔departed) must only touch that peer's own map entries and
   sockets - never the surviving pair's. Mostly a matter of making sure
   teardown code is written against the specific pair, not anything
   global.
3. **Clean re-fill**: a new Direct-Connect into the vacated slot is
   indistinguishable from any fresh join and should need no special-
   casing in the join path itself. The one ordering requirement: by the
   time the new joiner's connection reaches the point where the host
   emits its `0x29` broadcast to the survivor, the departed peer's old
   pairing state must already be fully torn down - so the survivor's
   rewritten address points at a clean, freshly (or newly re-)
   provisioned relay, not stale data from whoever left. This is a
   teardown-before-reuse ordering concern, not new protocol logic.

## Open questions / risks to verify while implementing

- **Port reuse vs. fresh allocation** after a pair tears down: reusing
  avoids port creep across a long-lived lobby but needs confidence the
  old socket/session state is fully drained first. Fresh allocation is
  simpler and safer to reason about at the cost of leaking a little OS
  port-space churn - probably the right starting choice, revisit if it
  matters in practice.
- **No reference capture yet** of "player drops mid-match, new player
  joins the empty slot" in a genuine non-proxied session. Same
  diff-driven approach that's worked well so far (extend
  `run-aom-verbose-3clients.sh`: get two joined, resign one, launch a
  throwaway 4th container to take the empty slot) would be worth doing
  *before* trusting the durability design above against a live proxied
  test, the same way the earlier handshake and multi-client bugs were
  each nailed down by diffing against a genuine capture first.
- **Does `0x29` ever fire more than once for the same peer** (e.g. a
  re-announce)? If so the rewrite needs to be safe to apply repeatedly,
  not just once.
- Voobly is still completely out of scope here - nothing in this design
  is assumed to transfer to it (see CLAUDE.md).

## Suggested order of work (2026-08-08 plan - superseded, kept for record)

1. ~~Reference capture of the drop/rejoin sequence~~ - skipped; durability
   still not built (see below), the base routing got proven with live
   traffic diffing instead once the second rewrite gap was found.
2. ~~Extend `lobby/main.go`'s rewrite dispatch to support the nested
   `0x03`/`0x29` lookup, log-only first~~ - done 2026-08-10.
3. ~~Build the per-pair relay type~~ - done 2026-08-10, but *not* as a
   generalization of the existing `proxy` struct as originally planned
   here - `proxy`'s asymmetric one-fixed-backend model doesn't fit a
   symmetric two-real-peer relationship (confirmed by a first attempt
   that crash-looped). Built as a new, dedicated `pairRelay` type
   instead - see the correction note above.
4. ~~Wire up A↔B using the `0x29` rewrite~~ - done, plus a second rewrite
   for the new-peer-facing direction that this plan didn't know existed
   yet (see the "not asymmetric after all" correction above).
5. Durability (resign detection, scoped teardown, re-fill) - **still not
   done**, see "Next steps" below.

## Next steps (2026-08-10)

Base P2P routing is done and confirmed working (real 2-client join,
both directions, chat verified). Queued for next session, roughly in
priority order:

1. ~~**Lobby-status endpoint for "fully staffed"**~~ - done 2026-08-11:
   `GET /full` added (`matchState.full()` in `lobby/main.go`), backed by
   the A<->B pair relay's existence rather than a new probe - see
   `docs/lobby-status-api.md`'s "How it decides full" section. Reset path
   landed as part of item 4's durability work below, confirmed live
   2026-08-14/15 - `/full` now correctly flips back once a departed
   player's slot is freed, not stuck `true` for the pod's life.
2. **Auto Observer Mode**: **done 2026-08-11**. The 2026-08-10 hypothesis
   (Observer Mode persists once set, even through a drop/rejoin) held up:
   confirmed again this session, both by kicking a real client and by
   watching one drop on its own - the host's row stayed "Observer" both
   times. What the 2026-08-10 pass didn't know yet: clicking Observer
   Mode at pure startup (both slots still "Open") doesn't work at all -
   AoM's lobby requires both slots filled first. The actual fix,
   calibrated live and now built into `host-game-kube.sh`: fill both
   "Open" slots with Standard AI, click Observer Mode, then kick both AI
   back out via a per-slot "shoe icon" control (confirmed to work
   identically on an AI or an already-connected real client) to leave the
   slots open again - all at pod startup, before any real client has
   joined, with zero manual clicks or log-polling. See
   `docs/host-flow.md`'s "Flow (continued)" section for the full
   calibrated coordinates and confirmation screenshots.
3. **Ready-up automation**: **done 2026-08-11**, end to end. The packet:
   a 22-byte client→host settings-sync message, found by packet-length
   frequency analysis against a live 2-real-client session (routine
   traffic is entirely 10/12/16 bytes; toggling Ready reliably produced a
   single 22-byte outlier) - see `docs/directplay8-protocol.md`'s
   "Ready-toggle sub-message" section. `lobby/main.go`'s
   `isReadyToggle()`/`readyToggleState()` detect it and log
   `client %s ready-toggle: %v` on every toggle in either direction -
   confirmed live against both real clients, both ready and un-ready.

   Driving the host's own response needed a new piece: rather than give
   `aom-lobby` k8s API/exec access (a real dependency jump for a UDP
   proxy), a small HTTP server (`input-agent/`) now runs *inside* the
   `aom-headless` container itself, alongside Xvfb/wine - see
   `dockerfile.k8s`'s new builder stage and `entrypoint.sh`. It exposes
   `POST /click?x=&y=` (shells out to `xdotool`, reachable at the
   `aom-headless-game` Service's stable DNS name, port 8082 - safe to use
   the Service rather than chase the pod's raw IP the way
   `SESSION_BACKEND_ADDR` has to, since this is a plain client-initiated
   HTTP call with no source-address matching requirement). `aom-lobby`'s
   new `matchState.setReady()`/`markStarted()`/`triggerMatchStart()`
   track both real clients' latest ready state and POST to input-agent
   exactly once when both flip ready.

   **The match-start trigger turned out to need no new UI exploration at
   all**: confirmed live that there's no separate "Start Game" button -
   the host has its own Ready crystal in the lobby's player list (same
   control real clients use, at (511, 89) in the 800x600 layout, visible
   even in Observer Mode), and clicking it once both other slots already
   read ready starts the match immediately. Verified end-to-end with both
   slots AI-filled (Standard) and the host's own crystal clicked via VNC -
   the lobby screen was replaced by the actual match in progress.

   **Still unconfirmed**: whether the host ever broadcasts a ready-state
   change back out to the *other* real client (see
   `docs/directplay8-protocol.md`'s "Not yet checked" note on this) - only
   client→host traffic was captured so far, since that's what this
   automation needed to react to.
4. **Durability**: **done 2026-08-11**, built exactly to the design in
   the original "Durability" section above - all three pieces:
   - **Departure detection**: the fast path (`isResignBurst()` matching
     the documented 3-byte `01 <connID>` shape) is built and detects in
     both directions, but **is deliberately not wired to act on
     anything** as of this writing - see `isResignBurst`'s doc comment's
     2026-08-11 correction. Live 2-real-client testing caught this exact
     shape firing as a precise, repeating burst exactly 2 minutes after
     every successful pairing, with no user action behind it - not a
     resign, some other periodic protocol message coincidentally matching
     the same shape. Acting on it (the original implementation) was
     closing every real session about 2 minutes into any match,
     reproducing the exact "Attempting to Connect" hang this whole pass
     was meant to fix. Both call sites now only log it (kept for forensic
     value against a future real capture). **The slow path is what's
     actually load-bearing today**: the existing `idleTimeout`/
     `reapIdleSessions` mechanism (30s, unchanged) is the only active
     departure-detection signal, wired through the same
     `proxy.onSessionRemoved` hook that the fast path would also use once
     it's trustworthy.
   - **Scoped teardown**: `matchState.onClientGone(addr)` only ever acts
     on the one address it's given - deletes its ready-state entry, and
     tears down `m.pair` *only if* that address was actually part of it.
     The surviving client's own session lives entirely in
     `sessionProxy.sessions`, never touched by this - isolation is
     structural, exactly as originally designed, not something the
     teardown code has to be careful about separately.
   - **Clean re-fill**: falls out for free from teardown being eager
     (fires the moment a departure is detected) rather than lazy -
     `ensurePairRelay` already sees `m.pair == nil` by the time a new
     joiner's connection reaches the point where the host would broadcast
     its address, so a fresh relay gets built with no ordering hacks
     needed.

   Found and fixed one real bug while building this: `pairRelay.run()`'s
   read-error handling was `continue`, not `return` - closing a relay's
   socket to tear it down would have busy-looped the goroutine forever
   instead of stopping it. Fixed as a prerequisite.

   Per item 2's confirmation that Observer Mode persists through a drop,
   no Observer Mode re-swap step was needed on top of this. The reference
   drop/rejoin capture this doc originally called for was never taken,
   but real testing this session (repeatedly killing and relaunching
   spoofed client containers mid-lobby) exercised the idle-timeout path
   directly and is what surfaced the original bug this item fixes.

## N-host round-robin matchmaking (2026-08-13)

The item this doc's "Next steps" never got to: multiple concurrent
matches across multiple `aom-headless` pods, not just one. Full design in
`lobby/packet-handling-design.md`'s "N-host round-robin matchmaking"
section; status as of this session:

- **Per-host scoping - done**: every place that used to assume "there is
  exactly one host" (`session`'s own dialed backend, `matchState`,
  `clientTracker`'s consumers, `otherSessionClientAddr`,
  `relayBackendInitiated`, `triggerMatchStart`) now threads a
  `*hostCandidate` through instead - see `lobby/main.go`'s `hostPool`/
  `hostCandidate` types. `otherSessionClientAddr`'s old "there's only
  ever one other session" assumption was the single biggest risk here:
  unscoped, it would have silently cross-wired two concurrent matches'
  pairings rather than erroring.
- **Dynamic discovery - done**: initially built as static comma-separated
  env vars (one Deployment+Service per host, hand-copied) - rejected as
  not real scalability. Replaced with `podLister`, a hand-rolled REST
  poller against the in-cluster Kubernetes API (no `k8s.io/client-go`
  dependency - this project has had zero external Go deps until now and
  that felt like too much for "poll a list-pods endpoint"), listing Pods
  by label every 3s and reconciling `hostPool` live. `kubectl scale
  deployment/aom-headless --replicas=N` is now the entire "add a host"
  operation. Granted read-only pod list/watch access via
  `k8s/lobby-rbac.yaml` - no create/update/delete/exec permission
  anywhere, matching `input-agent`'s own "no k8s API/exec access needed"
  posture for the *other* half of pod automation.
- **Self-hosting pods - done**: getting a newly-scaled pod into a
  joinable lobby used to require an external `kubectl exec
  host-game-kube.sh` run. Ported that click sequence into `auto-host.sh`,
  baked into the image and backgrounded by `entrypoint.sh` alongside
  `input-agent` - every pod hosts *itself* on startup now, no external
  trigger. `host-game-kube.sh` itself is kept only for manual/debug
  re-runs.
- **Selection policy - done**, same session: `hostPool.selectLocked`
  implements the real 3-tier priority (prefer a host already waiting for
  a second player, then an empty host, then ineligible), round-robining
  independently within whichever tier has candidates
  (`nextWaitingRR`/`nextEmptyRR`). Needed a way to tell "empty" from
  "waiting for a second player" apart that `hostProbe.hasWaitingGame()`
  alone can't provide (it's true for both states - a host keeps
  answering discovery queries whether 0 or 1 real clients are connected)
  - added `proxy.sessionCountForHost`, sessionProxy's own real per-host
  session count, wired into the pool after sessionProxy exists. Built
  and code-reviewed this session; the live 2-host/3-client test that
  should confirm client 1+2 land on the same host and client 3 lands on
  the second host once the first is full hasn't been run yet - next step.
- **Session-port stickiness - done, pulled forward**: originally
  designed as a later, separate concern; turned out matchmaking is
  unsafe without it (a client's discovery-port and session-port traffic
  independently round-robining to two different hosts would break
  everything), so it shipped as part of this same pass -
  `hostPool.assigned`, keyed by client IP.
- **Still open**: the claim/reservation race guard ("The race this needs
  to guard against" in `lobby/packet-handling-design.md`) and
  `synthesizeLobbyFullRejection` (case 3, blocked on a reference capture
  that's never been taken) - neither implemented, both still exactly as
  designed there.

## Regression investigation (2026-08-14): second client stuck on "Attempting to Connect" after LAN-sanitization

**Status: unresolved, root cause narrowed to the host's own DirectPlay
layer, not `aom-lobby`.** Surfaced immediately after landing the LAN/
Direct-IP sanitization work (client↔client iptables isolation +
broadcast suppression, see this repo's `deploy-minikube.sh` and
`k8s/aom-headless-netpol.yaml`) - a real second client (`aom-spoofed-pc2`)
consistently hung on "Attempting to Connect" against a host that already
had a first real client (`aom-spoofed-pc`) connected and waiting, even
though `aom-lobby` itself reported the pair as fully matched (`/full` ->
`true`, a genuine `pairRelay` created).

### The baseline: what a working handshake looks like

The archived `aom-lobby-live.log` from `archiving/sessions/20260812-215320-readyup-resign-quit-test/`
(a confirmed-successful ready-up/resign/quit run, same proxy code shape)
shows both rewrite directions firing at the *identical* timestamp, per
`newPeerBroadcast`'s own doc comment in `lobby/main.go` ("the host sends
this to already-connected peers at the SAME moment it responds to the
connecting peer"):

```
2026/08/12 11:12:56 [pair] new A<->B pair relay on 192.168.49.2:37894, bridging 192.168.49.4:2300 <-> 192.168.49.3:2300
2026/08/12 11:12:56 [session] rewrote existing-peer broadcast to client 192.168.49.4:2300: 192.168.49.2:2300 -> relay 192.168.49.2:37894
2026/08/12 11:12:56 [session] rewrote 0x29 new-peer broadcast to client 192.168.49.3:2300: 192.168.49.2:2300 -> relay 192.168.49.2:37894
```

That third line - the host telling the *already-connected* client (A,
`.3`) about the *new* one (B, `.4`) - is exactly what's missing in every
2026-08-14 reproduction below. `aom-lobby`'s own rewrite logic for this
message (`isNewPeerBroadcast`/`rewriteToClient` in `lobby/main.go`) is
unchanged since 2026-08-12 and logs unconditionally whenever it fires,
including the fallback case ("no other real client session on that host
after polling...") - across every attempt tonight, across the pod's
entire log history, **neither log line for the new-peer broadcast to
client A ever appeared once.** The host itself never sent it (or
`aom-lobby` never received anything shaped like it) - this isn't a
missed-rewrite bug, it's an absent message.

**The two messages on the wire, decoded** (pulled directly from that
session's own `clienta-capture.pcap`/`clientb-capture.pcap` via
`tshark`, post-rewrite, as actually delivered - both fired within 3ms of
each other):

Host -> client A (`0x29` new-peer broadcast, `clienta-capture.pcap`,
21:12:56.737532):

```
03 00 3e 00 ac 19 29 00 1c 01 00 00 00
02 00 94 06 c0 a8 31 02 18 a0 b4 02 20 a0 b4 02   <- addr block 1 @ offset 13
02 00 94 06 c0 a8 31 02 18 a0 b4 02 20 a0 b4 02   <- addr block 2 @ offset 29
03 00 00 00 17 00
```

Host -> client B (existing-peer broadcast, `clientb-capture.pcap`,
21:12:56.734716):

```
03 00 03 00 99 e4 4d 00 15 02 00 00 00 02 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
02 00 00 00 00 00 00 00 00 00 00 00 00 00 00 01 00 00 00
02 00 94 06 c0 a8 31 02 18 60 b4 02 20 60 b4 02   <- addr block 1 @ offset 49
02 00 94 06 c0 a8 31 02 18 60 b4 02 20 60 b4 02   <- addr block 2 @ offset 65
02 02 00 00 00 00
```

Decoding either address block as a `sockaddr_in` (`sin_family` /
`sin_port` big-endian / `sin_addr`, per `newPeerAddrOffset1`/
`existingPeerAddrOffset1` in `lobby/main.go`): **every block in both
messages carries the identical address `192.168.49.2:37894`** (`sin_port`
bytes `94 06` big-endian = `0x9406` = 37894; `sin_addr` bytes
`c0 a8 31 02` = `192.168.49.2`) - `aom-lobby`'s own pair-relay address
for that match, not either real client's address. Confirms
`rewriteNewPeerBroadcast`/`rewriteExistingPeerBroadcast` both did their
job correctly and consistently: same relay address written into both
directions, both clients told to talk to the relay rather than to each
other's real addresses, exactly the design intent. This is the reference
baseline a future capture of the 2026-08-14 failure should be compared
against - specifically, whether host->A ever appears on the wire at all
(not just whether `aom-lobby` logs it).

### Theories tested and ruled out

1. **Identical self-address confuses the host's player tracking.**
   `sessionProxy.rewriteToBackend` rewrites every real client's embedded
   "self" address to the same fixed `cfg.publicIP:cfg.publicPort` before
   it reaches the backend - theorized this could make the host's
   DirectPlay layer treat a second client's join as a re-announcement of
   the first rather than a genuinely new player. **Disproven**: the
   2026-08-12 baseline above used this exact same rewrite (unchanged
   code) and worked correctly. Identical self-addresses are not the
   problem.

2. **`aom-lobby` restart churn confusing the host mid-connection.**
   Tonight's first reproduction had client A sit connected through three
   separate `aom-lobby` pod restarts (each wiping `aom-lobby`'s own
   session state and creating a fresh outbound socket to the backend on
   the next packet) before client B joined - theorized the host's
   internal tracking of client A got left in an inconsistent state.
   **Disproven**: relaunched both clients fully fresh (client A a single,
   uninterrupted session, zero `aom-lobby` restarts behind it) and the
   exact same failure reproduced.

3. **Relay or network-layer bug (dropped/misrouted P2P traffic).**
   **Disproven** by direct evidence with `pairRelay.run()`'s raw payload
   logging added tonight (`VERBOSE=1`, see the `[pair] relaying ...`
   log lines): client B's P2P connection attempts genuinely reach the
   relay and get correctly forwarded to client A's real address, once
   every ~100ms, for the entire session. Decoded one such payload's
   embedded sockaddr fields directly: self = `192.168.49.4:2300` (client
   B's own real address, correctly unrewritten - it's not aware it's
   being proxied), peer = the relay's own address (correctly what it was
   told is "the existing player"). Client B is doing everything right.
   **100% of relay traffic across every reproduction was one-directional
   (B -> A) - client A never once replied on this channel.** Since the
   relay bridges both directions identically and B's traffic to A is
   confirmed delivered, client A's silence isn't a routing problem - it's
   that client A has no reason to respond to an unsolicited connection
   attempt it was never told to expect (see baseline above: the message
   that would have told it is the one that's missing).

### Wine-debug dead end (both available levers exhausted, not just untried)

Attempted to get host-side DirectPlay-level visibility (not just raw
winsock socket calls) to see what the host's own game logic does (or
doesn't do) at the moment client B joins:

- `k8s/aom-headless-deployment.yaml`'s `WINEDEBUG` already includes
  `+dpwsockx,+dplay,+dplaysvc,+dpnet` (added in an earlier session) and
  was confirmed active on the live pod - but **zero lines from any of
  those channels appear anywhere in the pod's log history**
  (`kubectl logs deployment/aom-headless | grep -c "dplay\|dpwsockx\|dpnet"`
  -> `0`). This Wine build's implementation of those DLLs apparently has
  no trace statements on whatever code path this game actually
  exercises - only `+winsock` is chatty. Redeploying with the same
  channels again would produce nothing new.
- The other lever, `+relay` (full Win32 API call tracing), was already
  tried on this exact host process in an earlier session (see that env
  var's own doc comment in the deployment manifest) and **saturated the
  pod with ~10,000 log lines/second, breaking discovery entirely within
  seconds** - a repeat of an already-observed failure, not a theoretical
  risk.

Getting further needs a genuinely different tool than blanket Wine debug
channels - the manifest's own comment suggests a narrower net.dll-only
channel or a live `strace` against the host's wine process instead.

### Spec cross-reference: who actually controls the address in this message

Checked the cached official `[MC-DPL8CS]` spec's closest analog to this
message, `DN_ADD_PLAYER` (§2.2.1.7) - it doesn't carry a raw address at
all, it carries a `url` field (`DN_ADDRESSING_URL`, §2.2.8 - DirectPlay
8's real addressing mechanism is a string like
`x-directplay:/provider=...;hostname=...;port=...`). AoM (2002) predates
that and uses its own simplified wire format instead - the raw 16-byte
`sockaddr_in` blocks this doc already reverse-engineered. Doesn't change
anything about the missing-message problem above, but it's worth being
explicit about for whoever designs the fix: whatever address the host
*originally* embeds in this field is irrelevant to us either way, since
`aom-lobby` already fully overwrites it (confirmed in the decoded packets
above - both clients end up with the relay's address, not each other's
real one, regardless of what the host itself chose). The fix doesn't
need to negotiate anything with the host's own addressing scheme - it
only needs the *message itself* to exist on the wire.

### What's actually still unknown

Every layer between the two real clients is now individually confirmed
correct except one: **why the host's own DirectPlay session logic
doesn't emit the new-peer notification to the first client, when the
identical proxy mechanism demonstrably did emit it on 2026-08-12.**
Candidate angles for a fresh session, roughly in order of how testable
they are without new tooling:

- **Diff what's different about the 2026-08-12 test's setup** (game
  settings, map/difficulty selection, how long the host sat idle before
  a second client joined, Observer Mode calibration state, `auto-host.sh`
  version) against tonight's - the archived session's own `NOTES.md`
  may have setup details worth comparing directly.
- **`strace -f` against the host pod's wine process** (`docker exec`/
  `kubectl exec`, matching the manifest comment's own suggestion) around
  the exact moment a second client's session-port handshake arrives -
  would show real syscall-level activity regardless of Wine's own debug
  channel support.
- **A genuinely non-proxied 3-container repro** (host + A + B all on the
  same LAN, no `aom-lobby` in between at all, same shape as
  `run-aom-verbose-3clients.sh`) to isolate whether this is proxy-shape
  specific at all, or reproduces even without `aom-lobby` in the loop -
  would be the cleanest possible signal, since it removes every rewrite
  path from the equation entirely.

### Proposed fix: reactive watchdog, not always-synthesize

Root cause is still unconfirmed, so this is a workaround, not a targeted
fix for a known cause - but it's robust to whatever the cause turns out
to be (host-side Wine bug, some state-desync specific to tonight's setup,
or something else entirely), because it stops depending on the host
reliably sending this message at all. **Deliberately reactive rather
than always-synthesizing**: the working 2026-08-12 case should be left
completely untouched, and this should only ever kick in as a repair when
the genuine message doesn't show up in time.

**Mechanism - a watchdog timer tied to relay creation, not to the
missing message itself** (there's nothing to react to if it never
arrives, so the trigger has to be absence-of-evidence within a deadline,
not presence-of-a-different-event):

1. `pairRelay` gets a small `notified map[string]bool` (keyed by
   `clientAddr.String()`, guarded by its existing `mu`), plus
   `markNotified`/`isNotified` methods.
2. `ensurePairRelay(selfAddr, otherAddr)` already tells us, on every
   call, which client is *definitely* about to be notified right now
   (`selfAddr` - the recipient of whichever real broadcast triggered
   this call) and which one isn't confirmed (`otherAddr`). Mark
   `selfAddr` notified on every call. On the call that actually *creates*
   the relay (first one in - the second call just reuses it), also start
   a watchdog for `otherAddr`.
3. Watchdog (`time.AfterFunc`, a few seconds - comfortably under the
   client's 15s connect budget, generous enough not to race a
   genuinely-just-slightly-delayed real message; the 2026-08-12 baseline
   shows the two firing within 3ms of each other under normal
   conditions, so this should essentially never come close to firing
   legitimately): if `otherAddr` still isn't marked notified when it
   fires, synthesize and send the equivalent broadcast directly. If the
   real message showed up in the meantime, `isNotified` is already true
   and this is a no-op.

**Synthesis needs no new rewrite logic** - store the byte-for-byte
constant parts of a real captured `0x29` packet as a static template
(everything that stayed identical between the raw and rewritten examples
decoded above: wrapper, sub-type, the `1c 01 00 00 00` header,
`sin_family`, `sin_zero`, trailer) and call the *existing*
`rewriteNewPeerBroadcast(template, cfg.publicIP, relay.selfPort)` on it
directly - the exact same function the reactive path already uses, just
given a stored template instead of a genuinely-arrived packet.

**The conn-ID field is not a guess** - `session.connID`/`connIDKnown`
(`lobby/main.go`, learned from real backend traffic, used since
2026-08-12 by `relayBackendInitiated`) already tracks the correct
per-client value. Look up `otherAddr`'s own session and patch its real
`connID` into the template before sending, rather than fabricating one.

**Still genuinely open**: the `seq` bytes (2-3) have no equivalent
existing tracking to borrow from - shipped as a fixed value taken from
the one real capture available, flagged as the one piece of the template
that isn't backed by a confirmed-safe mechanism the way conn-ID is. If
the synthesized packet gets silently ignored despite a correct
conn-ID, `seq` validation is the first thing to suspect.

**Validation**: exactly the same live 2-client reproduction used
throughout this investigation (`run-aom-spoofed-client.sh` x2,
`VERBOSE=1`) - watch for client A actually replying on the relay port
for the first time, and for the `[match] new-peer watchdog ...
synthesizing one` log line confirming the fallback fired.

## Update (2026-08-19): seq fix confirmed real, but not sufficient - core mystery now fully isolated

**Context correction first, found by checking git history directly**
(the user's own prompt this session - "check previous commits, pinpoint
where you got this idea to change this project's design"): **the entire
N-host pool (2026-08-13) and LAN-sanitization layer (2026-08-14,
including this doc's own "Regression investigation" section above) has
never been committed.** `git log -- lobby/main.go` stops at `07c6f98`
(2026-08-12) - the last actual commit, and the one matching this doc's
own confirmed-working baseline, used a completely different, simpler
architecture: one static host pinned via `SESSION_BACKEND_ADDR`, no
client isolation, no NetworkPolicy, no dynamic pod discovery. Everything
from N-host onward exists only in the working tree. This matters because
it means tonight's investigation was never chasing a code regression -
`pairRelay.run()`'s core rewrite logic (`isSessionHandshake`,
`rewriteSessionField`, the self/peer address swap) is byte-for-byte
identical to `07c6f98`. What changed is the *environment* the same code
runs in.

**Also confirmed by re-reading `k8s/aom-headless-netpol.yaml`'s own
comment closely**: the 2026-08-10/12 "working" baseline may never have
succeeded purely through `aom-lobby`'s own rewrite/relay logic in the
first place. That NetworkPolicy's own header states it closes "a real
bypass found live 2026-08-14: a pod's own `0x29`/`existingPeerBroadcast`
send could reach a client container directly, skipping `aom-lobby`'s
rewrite entirely, because both happened to share a network on this
single dev machine." I.e. the old baseline's success may have been
partly propped up by an accidental direct-reachability path that no
longer exists now that isolation is correctly enforced - consistent with
`startNewPeerWatchdog` (built specifically to patch the gap closing that
bypass exposed) never having a recorded "confirmed working" follow-up
entry in this doc, unlike every other feature here.

### Fix landed: `seq` continuity for the watchdog's synthesized broadcast

Exactly the gap flagged as "still genuinely open" just above. Added
`session.lastHostSeq`/`lastHostSeqKnown` (`lobby/main.go`), updated on
every `03 00`-wrapped packet `backendToClient` reads from the real
backend (unlike `connID`, which is learned once; `seq` updates on every
packet, since it's genuinely per-message). `sessionSeq` (parallel to the
existing `sessionConnID`) exposes it, threaded through `podLister`/
`matchState` the same way. `startNewPeerWatchdog` now patches
`lastHostSeq + 1` into the synthesized template instead of leaving it at
the hardcoded `0x3e` - continuing the client's real, already-observed seq
stream from that sender instead of a value with no relationship to it.

**Confirmed real, live-tested**: before this fix, every reproduction
showed the relay's traffic as 100% one-directional (only the
watchdog-notified client's peer ever sent anything; the other side, which
never got the address at all, had nothing to send). After the fix, the
relay is genuinely bidirectional for the first time - both real clients
correctly learn the relay's address and both send their own peer-open
toward it. This is a real, confirmed improvement, not a guess.

### What's still broken, now fully isolated

**Neither side ever acks the other's open.** Confirmed exhaustively
against a fresh capture this session: 7,075 packets on one relay
channel over ~8 minutes, zero `0x02` acks, zero `0x03` name-broadcasts,
in either direction - pure `0x00` open + `07ff` heartbeat retried
forever until each client's own ~2-minute patience window (see
`packet-handling-design.md`'s Timing model) expires and it resigns.
This rules out "just needs more time" completely.

Everything checkable from the server side is confirmed correct:

- **Content**: byte-perfect both directions, decoded field-by-field live
  this session. Each client's `sin_zero` is correctly echoed back
  describing itself; `self`/`peer` fields correctly show the relay's
  address and the receiving client's own real address respectively,
  matching `isSessionHandshake`'s documented self-consistency-check
  semantics exactly.
- **Delivery**: confirmed bidirectional, zero `WriteToUDP` errors, zero
  "packet from unexpected sender" drops.
- **Isolation**: `DOCKER-USER` `DROP` rule for the client subnet
  confirmed present and correctly ordered (checked live via `sudo
  iptables -L DOCKER-USER -n -v --line-numbers` mid-session).
- **Timing reference established**: a genuine non-proxied capture
  (`3client-p2p-check`, cross-referenced by absolute timestamp across
  all three vantage points for the first time) shows a real ack landing
  **1.66ms** after the peer's open is received on one side, **12.68ms**
  on the other - both sides fire their own open independently, ~137-139ms
  after receiving their respective address broadcast from the host, not
  reactively waiting on each other (disproves an asymmetric-roles
  hypothesis raised and tested this session). This gives a concrete "how
  fast should this be" reference: single-digit-to-low-double-digit
  milliseconds, not "never."

**Conclusion**: a byte-perfect packet reaches the client and the
client's own logic never acts on it. That gap is not visible from packet
captures or lobby logs - genuinely needs client-side instrumentation
(the `gdb`/`strace` route - see `lobby/packet-handling-design.md`'s
"Next investigation" section for the exact same class of wall hit
earlier this project) or static disassembly of the specific code path
deciding whether to ack a received peer-open, to go further.

### Two smaller bugs found along the way, not yet fixed

1. **`pairRelay.run()`'s verbose logging is misleading**: `log.Printf("[%s]
   relaying %s -> %s ... %s", ..., hex.EncodeToString(payload))` logs
   `payload` (the raw, pre-rewrite bytes as received) not `out` (what's
   actually rewritten and sent) - anyone reading `[pair] relaying ...`
   log lines is looking at input, not output. Caused real confusion this
   session before being caught by cross-checking against a client's own
   capture instead. One-line fix (`payload` -> `out` in that log call),
   not yet applied.
2. **`hostPool.assigned` never expires or re-validates capacity.** Found
   live this session: a client IP that was ever assigned to a host stays
   assigned forever (`assignForClient`'s fast path returns immediately on
   a map hit, no `hostFull` re-check). A third real client landed on an
   already-2-real-client host because it happened to reuse a Docker IP
   an earlier test had assigned to that same host - its discovery-port
   ping correctly got "no eligible host" (fresh `peekHost` call, not
   sticky), but its session-port connect one second later hit the stale
   sticky entry and got a real backend session anyway, with no pairing
   ever formed for it. Low real-world risk (real players don't get
   reused IPs the way ephemeral test containers do) but a genuine gap -
   `assigned` has no TTL and no path that ever calls `delete` on it.

## Update (2026-08-20): root cause found - resolved, not a host reliability issue

**The P2P ack mystery above is resolved. It was never the host.** Full
investigation trail, in order:

1. **Two real bugs fixed first, both real improvements but not the root
   cause**: `startNewPeerWatchdog`'s synthesized fallback broadcast baked
   in one fixed `sin_zero` value captured from a single historical
   session (`newPeerBroadcastTemplate`'s literal bytes), regardless of
   which real peer it was describing - fixed by learning each client's
   own real `sin_zero` from its genuine session-open (`session.selfSinZero`)
   and splicing it in dynamically. Separately, the watchdog only ever had
   a template for the `0x29` shape, so if the *new* peer (not the
   existing one) was the side missing its broadcast, the watchdog sent it
   the wrong-shaped packet - fixed by adding `existingPeerBroadcastTemplate`
   (byte-exact from a real capture) and threading which shape a given
   recipient needs (`peerBroadcastKind`) from whichever call site
   triggered `ensurePairRelay`. A live test with both fixes showed a
   real pairing complete further than ever before - open, ack, CD-key
   exchange, one-directional player-announce - before stalling on a
   *second*, newly-discovered host message (a player-roster-count
   broadcast, `tag(0x10) + count(u32 LE) + count×4B entries + f800f678`,
   sent alongside the address broadcast and never previously catalogued).
2. **The user pushed back hard on "the host is the problem"** - correctly.
   Discovering a second missing companion message made clear the
   watchdog-synthesis approach was structurally a losing game: no way to
   know the list of things the host "should" send is complete.
3. **Bypass hypothesis systematically ruled out.** Scanned every packet on
   both ports (2299, 2300) in both the current failing test and the full
   08-12 working reference for traffic sourced from anywhere other than
   `aom-lobby`'s own address - zero hits on port 2300 in either session.
   Also scanned the *entire* 08-12 reference match (22,752 packets) for
   the real host pod IP or the real peer's IP appearing anywhere in a
   payload sent to a client - zero leaks. The existing rewrite path was
   already proven complete and leak-free; the NetworkPolicy was confirmed
   correctly scoped (right IP, no port restriction on the punch-back
   rule). None of this was the cause.
4. **Rejoin-state hypothesis ruled out.** Rebuilt a genuinely fresh host
   pod and had both clients join once, cleanly, no resigns beforehand
   (confirmed via zero resign-burst log lines for the affected client).
   The genuine `0x29` still didn't arrive within the watchdog's 3s
   window. Not a test-methodology artifact.
5. **Root cause found via a node-level `tcpdump` capture filtered to the
   host pod's own IP**, catching a real client rejoin live. Found a
   byte-perfect, genuine `0x29` broadcast (subtype, offsets, content all
   correct) sent by the real host, arriving at `aom-lobby`'s own dialed
   socket for that client - confirmed independently via both the raw
   capture and `aom-lobby`'s own log entry for the identical instant,
   logged as `[session] backend->client non-handshake (51 bytes): 03 33
   25 05 70 03 29 00 ...`. **The host sent it. It arrived. Our own
   `isNewPeerBroadcast` didn't recognize it** - the packet's byte 1 is
   `0x33`, and that function (along with `isExistingPeerBroadcast`,
   `isReadyToggle`, `wrapperConnID`, `wrapperSeq`) has always required
   byte 1 to be exactly `0x00`. See `docs/directplay8-protocol.md`'s
   "Common wrapper" correction for the full byte-level evidence,
   including the same bug already present - and already silently
   dropping a genuine `0x29` - in the archived, confirmed-working
   2026-08-12 baseline itself (it only ever looked reliable because the
   host retries, and some retry usually happened to land with byte 1 =
   `0x00`).

**Conclusion**: this was a latent packet-classification bug in this
project's own code since its first working version, not a host
reliability issue and not a network-level bypass. The host has been
sending everything correctly and consistently the whole time. Fix:
loosen every wrapper detector to match on byte 0 (`0x03`) plus each
message's own subtype at its confirmed offset, not on byte 1 - see
`docs/directplay8-protocol.md` for the full reasoning. Once that lands,
the watchdog's synthesis machinery becomes unnecessary for real pairings
(the genuine broadcast will be recognized directly) and can be removed,
leaving it scoped to what it was always meant for - nothing, since even
the no-eligible-host synthesis path is separate code that never used it.
