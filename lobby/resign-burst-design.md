# Resign burst: acting on it safely, scoped to pre-match departures

**Status: implemented and live-verified, pre-match case only, 2026-08-23.
In-match resign detection explicitly deferred.** Written 2026-08-23,
revised twice same day - first after live testing ruled out the original
silence-based design, then again after live testing caught the teardown
itself racing ahead of the packet it was reacting to - see "History" at
the end for both. Addresses the todo this project has carried since
2026-08-11: `isResignBurst` correctly *detects* a real "this peer just
left" signal, but has never been safe to *act* on, because the exact
same byte shape is also used for something else entirely.

Confirmed live: a player leaving the lobby now tears down within
`resignBurstDrain` (100ms), and - checked directly on the surviving
client's own screen, not just backend logs - that client actually sees
the departure. See `archiving/sessions/20260823-resign-burst-pre-match/`
for the full before/after evidence.

## Why this matters

Today, the only way a departed player's session/relay ever gets torn
down is the 30s idle timeout (`idleTimeout`, `reapIdleSessions`) - a
real player who leaves the lobby before a match starts leaves their
host slot and `pairRelay` occupied for up to half a minute before
anyone else can be routed there. `isResignBurst` already recognizes the
wire signal that would let this happen instantly instead - it's been
sitting in the codebase, detecting correctly, unused, since 2026-08-11.

**Scope of this pass, deliberately narrowed**: only the *pre-match*
case (a player leaving the lobby/ready-up screen before the match
starts) is handled here. Detecting a genuine resign *during* active
gameplay and telling it apart from the known periodic false positive
remains unsolved and is intentionally left for later - see "What's
explicitly deferred" below.

## The ambiguity, evidenced from real captures and live testing

The 3-byte `01 <connID>` burst (ten identical copies, sent in under a
millisecond) has been directly observed in four distinct situations:

| Event | Direction | Source | Trigger | Repeats? | What follows |
|---|---|---|---|---|---|
| 2026-08-03 | client→host | `archiving/sessions/20260803-3v3-win-loss/session.pcap`, confirmed against `clicks.log` | user resigns **mid-game** | once | **total silence** - confirmed directly: zero packets from that client for the remaining 419s of a 630s capture |
| 2026-08-08 | host→client | `docs/directplay8-protocol.md`'s "Post-handshake session handshake" section | client's own ~2-minute connect-retry timeout expires, gives up | once | client already back at LAN-browse; peer genuinely gone |
| 2026-08-11 | either | live 2-client testing (see `isResignBurst`'s own doc comment) | **no user action at all** | **every 2 minutes, indefinitely, for as long as the match runs** | match continues completely normally |
| **2026-08-23** | client→host, client→client | live testing this pass | user **leaves the lobby menu (pre-match)** | once (not yet confirmed whether it can also repeat) | **connection stays fully, normally active** - real heartbeats, real settings-sync, real relay traffic, on both channels, for the whole window checked |

The first three were already known to share one wire signal across
genuinely different triggers - confirmed bidirectional and trigger-
agnostic, not resign-specific. **The fourth is what invalidated this
design's first draft**: leaving the lobby produces the identical burst,
but - unlike the confirmed mid-game resign - is *not* followed by
silence. The connection keeps chattering normally, indistinguishable
from a healthy ongoing session by any activity-based check. Silence
cannot be the distinguishing feature for this case, because the case
that most needs fast handling (a pre-match leave) doesn't reliably
produce it.

## The design: gate on `matchState.hasStarted()`, not on what happens after

What actually separates the confirmed-safe case from the known false
positive isn't the burst's aftermath - it's *when* it happens. The
2026-08-11 false positive was only ever observed **during active
gameplay** ("killing every real match about 2 minutes in" - a match
that was already started and being played). A pre-match lobby-leave, by
definition, happens **before** `triggerMatchStart` has ever fired.
`matchState.started` already tracks exactly this boundary (set once,
by `markStarted()`, the moment both players ready up):

```go
// matchState gets a new read-only accessor:
func (m *matchState) hasStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}
```

A resign-shaped burst seen while `!hasStarted()` is confident enough to
act on - no confirmation window, no waiting for silence. A burst seen
after `hasStarted()` stays exactly as cautious as it's always been
(logged only), because that's the regime the known false positive
actually occurs in and this pass doesn't attempt to solve it.

**Not literally instant, though - `resignBurstDrain` (100ms) matters.**
Found live 2026-08-23: closing the connection in the *same call* that
detected the burst raced ahead of that packet's own delivery - the burst
is ten rapid, redundant copies specifically so it survives ordinary
packet loss, and closing the connection before any of them got written
meant neither the real host nor the surviving peer reliably received
it at all (the peer's game window kept showing the departed player as
still present). It also caused a self-inflicted reconnect loop: the
real client, seeing its connection reset, immediately retried, and each
retry's remaining burst copies looked like a fresh resign all over
again. Deferring the actual teardown by a short, fixed delay - just
long enough for the whole burst (confirmed ~1ms end-to-end in the
original 2026-08-03 capture) to flow through normally first - fixes
both: the full burst reaches its destination(s) intact, and the
`resignCheckPending` debounce absorbs the remaining nine copies on the
same still-open session instead of each one recreating it.

### Client↔host (`sessionProxy.rewriteToBackend`)

```go
if isResignBurst(payload) {
	if sess.host != nil && sess.host.match != nil && !sess.host.match.hasStarted() {
		sess.mu.Lock()
		alreadyActed := sess.resignCheckPending
		sess.resignCheckPending = true
		sess.mu.Unlock()
		if !alreadyActed {
			log.Printf("[session] client %s resign-shaped burst seen pre-match, tearing down in %s", sess.clientAddr, resignBurstDrain)
			clientAddr, host, backendConn := sess.clientAddr, sess.host, sess.backendConn
			time.AfterFunc(resignBurstDrain, func() {
				host.match.onClientGone(clientAddr)
				backendConn.Close() // triggers backendToClient's own
				                    // read-error cleanup - the same
				                    // teardown every other departure
				                    // already goes through, not a new one
			})
		}
	} else {
		log.Printf("[session] client %s resign-shaped burst seen in-match (not acted on, see resign-burst-design.md)", sess.clientAddr)
	}
}
```

`sess.resignCheckPending` debounces the burst's ten identical copies
down to one deferred action - correctness-critical now, not just tidy:
without it, each of the nine copies still arriving during
`resignBurstDrain` would arm its own redundant timer.

### Client↔client (`pairRelay`)

`pairRelay.run()` doesn't have `matchState` in scope, so it stays a
simple, debounced pass-through to `onResign` - the pre-match/in-match
decision lives in `ensurePairRelay`'s own wiring of that callback,
where `matchState` (and `hasStarted()`) is naturally available:

```go
relay.onResign = func(peerAddr *net.UDPAddr) {
	if m.hasStarted() {
		log.Printf("[pair] resign-shaped burst from %s in-match (not acted on, see resign-burst-design.md)", peerAddr)
		return
	}
	log.Printf("[pair] resign-shaped burst from %s pre-match, tearing down in %s", peerAddr, resignBurstDrain)
	// Deferred, not immediate - same resignBurstDrain reasoning as the
	// client<->host side above: closing m.pair.conn (this relay's own
	// r.conn) in the same call processing one of this burst's packets
	// would race ahead of run()'s own forward of that packet to the
	// other peer, right after onResign returns.
	time.AfterFunc(resignBurstDrain, func() {
		m.onClientGone(peerAddr)
	})
}
```

Both paths reuse `matchState.onClientGone` unchanged - this design only
changes *when* teardown gets triggered, not *how* it happens.

## What's explicitly deferred

- **Distinguishing a genuine in-match resign from the periodic false
  positive.** Still unsolved. The silence-based approach from this
  design's first draft might still be the right tool *for this specific
  case* (a real mid-game resign, per the 2026-08-03 capture, *is*
  followed by total silence - only the pre-match case turned out not to
  work that way), but that needs its own verification pass before
  trusting it, not an assumption carried over from a case that just
  failed for a different reason.
- **Whether the pre-match burst can itself repeat** (the way the
  false-positive does) wasn't confirmed either way in this pass's
  testing - if a pre-match leave *can* somehow recur on the same
  connection, the debounce above (one action per session/relay
  lifetime) already prevents acting twice, so this doesn't block
  shipping the current scope, just worth knowing if the same session
  object ever needs to legitimately re-arm.
- **Flagging which hosts are actively mid-match vs. just in-lobby** as
  its own, more general concept (useful beyond just this feature) -
  noted as a future direction, not built here. `matchState.hasStarted()`
  already provides exactly this signal per-host today, which is why
  nothing new needed building for *this* pass specifically.

## Verifying the fix

1. **Pre-match lobby-leave**: two real clients pair, one leaves the
   lobby/ready-up screen before either readies up. Confirm the burst is
   detected, `hasStarted()` reads false, and the session/relay tear down
   within `resignBurstDrain` - not 30-40s later. Confirm the freed slot
   becomes available for a new match promptly (same check
   `session-cleanup-design.md`'s own verification used). **Also confirm
   the surviving peer's own game window actually registers the
   departure** - the first live test of this design passed the "tears
   down on the backend" check but failed this one (the departed player
   kept showing as present on the other client's screen), which is what
   led to the `resignBurstDrain` fix above; don't consider this step
   verified by backend logs alone.
2. **In-match burst, left alone**: let a real 2-client match actually
   start (both ready up) and run past the 2-minute mark. Confirm the
   periodic burst is detected, logged as "in-match," and - critically -
   that nothing gets torn down, exactly as before this change. This is
   the regression check that would have caught 2026-08-11 before it
   shipped and must keep passing after this change too, since
   `hasStarted()` being true is now what routes traffic into the
   unchanged, still-cautious branch.
3. **Debounce sanity check**: confirm only one teardown/log line fires
   per real pre-match departure, not ten (one per burst copy).

## History

**First draft (2026-08-23, same day)**: gated on confirmed silence after
the burst - the feature every *previously* known real occurrence (2026-
08-03 mid-game resign, 2026-08-08 host-detected timeout) shared, that
the periodic false positive didn't. Live-tested against the actual
motivating case (a player leaving the lobby) and failed: the connection
stayed fully active afterward, so the silence check correctly declined
to act, but that meant the design couldn't do the one thing it was built
for. Revised same day to gate on `matchState.hasStarted()` instead -
*when* the burst happens, not what happens after - which is what
actually separates a pre-match leave and the known false positive
(confirmed only ever observed during active gameplay), and doesn't
depend on an assumption (silence) that's now known to be wrong for this
specific case.

**Second draft (2026-08-23, same day)**: `hasStarted()` gating live-
tested and the backend-side teardown fired correctly, but the surviving
peer's own game window never registered the departure - the departed
client kept showing as present. Root cause: tearing down synchronously,
in the same call that detected the burst, closed the connection/relay
before the burst packet that triggered detection had actually been
forwarded - losing not just that one packet but, since the debounce
hadn't engaged yet, causing the real client's own reconnect-on-reset
behavior to loop against the remaining nine copies of its own burst.
Fixed by deferring the actual teardown by `resignBurstDrain` (100ms) -
long enough for the whole burst to flow through normally first, short
enough to stay near-instant relative to the 30s baseline this design
replaces.
