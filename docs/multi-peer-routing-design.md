# Multi-peer routing design: making aom-lobby route a full 1v1 match

**Status: design only, not yet implemented.** Written 2026-08-08 end of
session, for picking back up fresh. Nothing in this doc is code - it's
the plan, the evidence behind it, and what to verify along the way.

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

Confirmed asymmetric: only the *existing* peer gets this broadcast. The
*new* peer gets a different, address-free message instead, and simply
learns the existing peer's address passively (from the source address of
that peer's incoming, proactively-sent `type 0x00`) - the same pattern
already implemented for the client↔host handshake. So there's exactly one
new rewrite rule needed to seed the whole thing, not two.

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

## Suggested order of work (next session)

1. Reference capture of the drop/rejoin sequence (see above) - cheap
   insurance against building teardown logic against assumptions instead
   of evidence.
2. Extend `lobby/main.go`'s rewrite dispatch to support the nested
   `0x03`/`0x29` lookup, still just logging what it *would* rewrite to
   (mirrors how `VERBOSE` logging was used to validate earlier fixes
   before wiring them live).
3. Build the per-pair relay type by generalizing today's `proxy` struct,
   prove it against the existing host↔client relationship first (should
   be a no-op refactor, easy to verify against the already-working
   single-client path).
4. Wire up A↔B using the `0x29` rewrite to seed it, test against the
   real 2-client scenario from earlier today.
5. Layer in durability (resign detection, scoped teardown, re-fill) once
   the base 3-pair routing is confirmed working.
