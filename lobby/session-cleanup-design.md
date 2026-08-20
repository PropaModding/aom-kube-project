# Session/host lifecycle cleanup: closing the leaks left by N-host churn

**Status: design only, not yet implemented. Top priority as of 2026-08-20.**
Written after a live test hit `hostPool.assigned`'s already-documented
staleness gap directly: a client stayed sticky-bound to a host pod that
had already been deleted, and its session-port traffic kept failing
against a dead backend indefinitely - the only fix available at the time
was restarting the whole `aom-lobby` deployment to clear its in-memory
state. That's a real, immediate correctness bug, not a hypothetical -
this doc exists because tracing it fully surfaced three more related
gaps in the same area, all stemming from the same root cause: **nothing
in `lobby/main.go` was ever built to actively tear a host down.** The
N-host pool (`podLister`/`hostPool`, 2026-08-13) added the ability for
hosts to come and go dynamically, but every piece of per-host state that
gets created when a host joins the pool assumes, implicitly, that it
never has to be undone.

## The four gaps, traced from the actual code

### Gap 1: `hostPool.assigned` is never revalidated (the bug that motivated this doc)

`assignForClient` (`lobby/main.go`):

```go
func (p *hostPool) assignForClient(clientIP string) (*hostCandidate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.assigned[clientIP]; ok {
		return h, true
	}
	...
}
```

This returns the cached host unconditionally - it never checks whether
`h` is still a member of `p.hosts`. Once a client's IP has been sticky-
assigned to a host, it stays bound to that exact `*hostCandidate` value
forever, even after `removeHost` has dropped it from the pool entirely.
`removeHost`'s own doc comment already flags this as a "known
limitation," but live testing (2026-08-20) turned it from a theoretical
gap into an actual reproduction: a fresh host pod was created, the old
one deleted, and a client that had been assigned to the old pod kept
retrying a session-port dial against it indefinitely
(`dial udp 192.168.49.2:0->10.244.120.91:2300: connect: invalid
argument`, repeating), never getting re-matched to the surviving host.
Recovery required restarting `aom-lobby` itself to clear the in-memory
map - there is currently no in-process recovery path at all.

### Gap 2: a removed host's `pairRelay` is never closed

`removeHost` only ever does one thing:

```go
func (p *hostPool) removeHost(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, h := range p.hosts {
		if h.id == id {
			p.hosts = append(p.hosts[:i], p.hosts[i+1:]...)
			log.Printf("[pool] host removed: %s (no longer listed)", id)
			return
		}
	}
}
```

If the removed host had a formed `matchState.pair` (two real clients
already paired), that `pairRelay`'s goroutine and UDP socket
(`pairRelay.run()`) just keep running - nothing ever calls
`relay.conn.Close()` for this reason. This is worse than a generic
"eventually idles out" gap: `pairRelay` bridges the two real clients
*directly*, not through the host, so it can keep looking perfectly
healthy from `reapIdleSessions`'s point of view purely on client<->client
traffic, indefinitely, even though the host it belongs to has been gone
for however long. `reapIdleSessions` deliberately protects a session
from being reaped while its relay looks active (`relay.idleFor(...) <=
idleTimeout` - see that function's own doc comment on the 2026-08-11
regression this exists to prevent), which means this specific case isn't
just slow to clean up - it may never clean up on its own at all.

### Gap 3: a removed host's `hostProbe.run()` goroutine never stops

```go
func (h *hostProbe) run() {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		h.probeOnce()
		<-ticker.C
	}
}
```

No stop channel, no context, no cancellation of any kind. Every host
`removeHost` drops leaks exactly one goroutine, permanently, for the
life of the `aom-lobby` process - each one still dialing and probing
whatever address it was given every `probeInterval` (3s) forever. Under
routine N-host pool churn (pod restarts, rolling updates, scale-down),
this accumulates without bound.

### Gap 4: a removed host's existing sessions aren't proactively closed

Both `discoveryProxy.sessions` and `sessionProxy.sessions` are separate
maps, each with their own `reapIdleSessions` loop, and neither is ever
told "host X is gone, clean up anything pointing at it." They're left to
the idle-timeout path alone - which, per Gap 2, isn't even a guaranteed
backstop for a session protected by a still-"active" orphaned relay.

## A fifth, lower-priority item found along the way

`hostPool.assigned` also grows one entry per distinct client IP *ever*
seen, for the life of the process, with no expiry - already flagged in
its own doc comment as accepted for this project's dev/test scope. Fixing
Gap 1 properly (removing entries for churned hosts) shrinks this
naturally for the common case; a full TTL sweep on top is optional, not
required by anything else in this doc.

## The fix, in four phases

### Phase 1 - `hostPool.assigned` invalidation (fixes the reproduced bug directly)

Two complementary, cheap changes:

- **Proactive**: `removeHost` deletes every `assigned[...]` entry whose
  value is the host being removed, so the next packet from an affected
  client falls through to a fresh `selectLocked()` call instead of
  hitting stale cache. Needs an O(n) scan over `assigned` (n = distinct
  client count, small and bounded by this project's own scope) or a
  reverse index (host -> set of assigned client IPs) if that ever
  matters at scale - not needed to start.
- **Reactive backstop**: `assignForClient` checks that the cached host is
  still present in `p.hosts` before trusting it, falling through to
  `selectLocked()` if not. Catches anything the proactive path might
  miss (ordering edge cases between `removeHost` and a client's next
  packet) without a linear scan on every lookup - a `map[string]bool` of
  live host IDs, maintained alongside `p.hosts` by `addHost`/`removeHost`,
  keeps this O(1).

### Phase 2 - a real host teardown routine

- `matchState` gets a new, unconditional teardown method - distinct from
  `onClientGone`, which is scoped to one departing address and leaves the
  other peer's state alone by design. This one is for "the whole host is
  gone," so there's no "is the other side still active" check to make:
  close `m.pair` if one exists, clear ready/started state.
- `hostProbe` gets a `stop chan struct{}`, selected against alongside its
  ticker in `run()`:
  ```go
  func (h *hostProbe) run() {
  	ticker := time.NewTicker(probeInterval)
  	defer ticker.Stop()
  	for {
  		select {
  		case <-h.stop:
  			return
  		case <-ticker.C:
  			h.probeOnce()
  		}
  	}
  }
  ```
  `removeHost` closes it. This is the fix for Gap 3, and it's a small,
  self-contained change to a type that currently has no cancellation
  story at all.

### Phase 3 - proactively close a removed host's sessions

`hostPool` has no reference to either `proxy` today, for the same
chicken-and-egg construction-order reason `sessionCountForHost`/
`closeSession` are already threaded in after the fact in `main()` (see
`podLister.closeSession`'s own doc comment for the precedent). Add a
`pool.closeSessionsForHost func(hostID string)` callback, wired the same
established way, implemented by scanning each proxy's `sessions` map for
`sess.host.id == id` - the exact same filter `otherSessionClientAddr`
already uses elsewhere in this file - and closing/removing each match.
`removeHost` calls it. This removes the Gap 2/Gap 4 dependency on a
timeout that isn't even guaranteed to fire, and makes host-loss
detection immediate instead of waiting out the idle timer.

### Phase 4 - bound `assigned`'s long-run growth (optional, lower priority)

Not required by anything above. Once Phase 1 lands, the map stops
accumulating permanent garbage from host churn specifically; a full
TTL/last-used sweep on top (mirroring `reapIdleSessions`'s own ticker
pattern) would be the natural next step if this ever matters beyond this
project's current dev/test scope, but isn't needed to close the four
gaps above.

## Explicitly out of scope for this pass

**Fast client-departure detection** (the *client* side of "clean up
properly when something drops," as opposed to the *host* side this doc
covers). `isResignBurst`'s client->host fast path is disabled (confirmed
false-positive-prone, firing on an unexplained ~2-minute period unrelated
to any real departure - see `isResignBurst`'s own doc comment) and its
host->client counterpart (`isHostResignNotice`) was never built - both
explicitly blocked on a reference capture (deliberately resign one known
client, capture all three vantage points) this project has deferred
since 2026-08-08 and never taken. Building on top of an unconfirmed
signal isn't worth it; this stays on the 30-40s idle-timeout path until
that capture happens. Not blocking anything in this doc - Phase 3's
`closeSessionsForHost` is host-triggered, independent of how client
departure eventually gets detected.

## Verifying the fix

A single live test exercises all four gaps at once: scale to 2 hosts,
pair two real clients on one of them, then delete that host's pod
mid-match. Confirm:

- The affected clients' sessions close immediately, not after the 30s
  idle window.
- The `pairRelay` goroutine actually exits (no longer visible in a
  `pprof` goroutine dump, or confirmed via a log line on close).
- The `hostProbe` goroutine count doesn't grow across repeated
  scale-up/scale-down churn of the same host count.
- A reconnect from the same client IPs gets freshly routed to the
  surviving host, not retried against the dead one - i.e. Phase 1's own
  fix, confirmed end-to-end rather than just unit-level.
