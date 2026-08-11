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
   `docs/lobby-status-api.md`'s "How it decides full" section. No reset
   path yet (stays `true` for the pod's life even through a later drop),
   which is fine for now but will need revisiting alongside item 4's
   durability work.
2. **Auto Observer Mode**: `host-game-kube.sh` currently requires a
   manual click on Observer Mode once both real clients have joined (see
   `docs/host-flow.md`) - automate that transition (probably driven by
   the same "2 real clients connected" signal as item 1, now available
   via `GET /full`).
   **Simplified by a 2026-08-10 manual-testing observation**: once a host
   fills both slots (in whatever order/fashion) it appears to *stay* in
   Observer Mode from then on, including through a later drop/rejoin -
   i.e. this doesn't need to be a dynamic "detect 2 clients, click
   Observer Mode, detect a drop, click it again" state machine.

   **Complicated by a 2026-08-11 finding**: clicking Observer Mode was
   confirmed working (coordinates verified, checkbox toggles, host row
   switches to "Observer") with 2 real clients already joined - but doing
   it at pod startup, before any client has joined, is expected to need
   an extra precursor step, not just an earlier click. AoM's lobby is
   believed to require both "Open" (waiting-for-human) slots to be filled
   with an AI player before Observer Mode can be set at all, with a
   separate per-slot "shoe icon" control to kick an AI back out once a
   real client is ready to Direct-Connect into that slot. None of the
   AI-fill-in or shoe-icon-kick coordinates are calibrated yet - see
   `docs/host-flow.md`'s "Still to do". Until that's done,
   `host-game-kube.sh` keeps the original wait-for-join approach.
3. **Ready-up automation**: **packet found and detection wired up
   2026-08-11** - see `docs/directplay8-protocol.md`'s "Ready-toggle
   sub-message" section. A 22-byte client→host settings-sync message,
   found by packet-length frequency analysis against a live 2-real-client
   session (routine traffic is entirely 10/12/16 bytes; toggling Ready
   reliably produced a single 22-byte outlier). `lobby/main.go`'s
   `isReadyToggle()`/`readyToggleState()` detect it and log
   `client %s ready-toggle: %v` on every toggle in either direction -
   confirmed live against both real clients, both ready and un-ready.
   **Still to do**: driving the host's own response (starting the match)
   once both real clients are ready - not built yet, and needs a way for
   `aom-lobby` to actually command the pod (`kubectl exec ... xdotool`,
   same mechanism `host-game-kube.sh` uses, but `aom-lobby` doesn't have
   k8s API access today) once it knows both are ready. Also unconfirmed:
   whether the host ever broadcasts a ready-state change back out to the
   *other* real client (see the doc section's "Not yet checked" note) -
   only client→host traffic was captured so far, since that's what a
   host-side automation needs to react to.
4. **Durability**: a client drops mid-match, a new client joins the
   vacated slot - the original "Durability" section above still describes
   the design (resign-signal detection + existing idle-timeout reap,
   scoped teardown, clean re-fill), none of it built yet. Needs the
   reference drop/rejoin capture this doc originally called for in step 1
   above, still not taken. Per item 2's observation, may **not** need an
   Observer Mode re-swap step if Observer Mode really does persist through
   a drop/rejoin - re-verify that assumption before building any re-swap
   logic on the strength of it.
