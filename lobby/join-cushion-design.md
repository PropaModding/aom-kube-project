# Join cushion: gating at discovery, released by a real completion signal

**Status: implemented and live-verified, 2026-08-23 (fourth revision).**
Written 2026-08-22, revised three times on 2026-08-23 after live testing
kept surfacing real bugs. Read "History" at the end before touching this
code - three earlier designs looked reasonable and each failed for a
specific, evidenced reason; the same mistakes are easy to repeat without
that context.

## The problem, restated precisely

When two real clients connect within a few seconds of each other, one
gets stuck - permanently, not just delayed - on "Attempting to Connect."
Root cause, confirmed via several live tests with full packet captures
(see "History"): the real host's own `0x29` "new peer" broadcast to the
*first* client, announcing the *second*, is built from a placeholder
address (`0.0.0.0:0`) instead of the second client's real one. This is a
race entirely inside the host's own engine, firing within milliseconds
of the second client's *session-port* (2300) connect - confirmed
insensitive to how long that client had already been made to wait
beforehand.

**The fix that follows from that evidence**: since delaying admission
doesn't touch the race (it happens at admission, not before it), the
only thing that works is never letting the second client *reach*
session-port admission at all until the first client has genuinely
finished connecting. Since the client won't advance past its own
discovery exchange without a reply, withholding that reply is a complete
gate on its own.

## The four-phase packet flow this gate sits in front of

Confirmed directly from this project's own captures - every client goes
through this same sequence, in order, before it's "in" a lobby at all:

| Phase | Port | What happens |
|---|---|---|
| **1. Discovery** | 2299 | Client sends `0x25` (unicast, to the typed Direct-IP address - this project has no working LAN-browse with broadcast suppression active) roughly every ~275ms, repeating on its own, until it gets an `0x26` reply. **This is the gate's location.** |
| 2. Commit | 2299 | Client sends `0x20` liveness ping once, gets `0x21` back, moves to the session port. |
| 3. Handshake | 2300 | `0x00`/`0x02` self-open + ack, both directions; heartbeats start. |
| 4. CD-key check | 2300 | Host sends the `paullovesjade` string; client echoes it back once - the last step before real lobby traffic (name broadcast, settings sync) begins. **This is the gate's release signal.** |

The broken `0x29` broadcast to the *other* client fires around Phase 3
for the newly-connecting one - well before that client has even reached
Phase 4. Checked directly against real (non-proxied) historical captures
too: the settings-sync burst that follows Phase 4 (map filename, per-slot
player table) is entirely one-directional, host→client, with no
client-side acknowledgment anywhere in it - the CD-key echo is the only
confirmed client→host round-trip in the whole handshake, which is why
it's the signal this design releases on.

## The design

### Discovery becomes the real, sticky matchmaking decision - not just a peek

`discoveryProxy.selectHost` is `pool.assignForClient` - the same sticky,
tiered decision `sessionProxy` uses, not the old `pool.peekHost` (now
deleted). `peekHost` existed on the reasoning that "a client's LAN/
Direct-IP screen fires `0x25` queries just from being open," so discovery
traffic didn't represent real connect intent - that reasoning doesn't
hold with broadcast suppression active (confirmed live - a real
LAN-browse screen shows nothing today), so every `0x25` this proxy ever
sees is a unicast query to a specifically typed address, i.e. genuine
intent. A client's *first* `0x25` is now the actual matchmaking moment.

### Two fields track a host's current claim, not just a timestamp

```go
// on hostCandidate:
firstClientIP        string    // who this host is currently claimed by
firstClientJoinedAt  time.Time // when that claim was made (assignment time)
firstClientLandedAt  time.Time // when that client's own CD-key echo was
                                // observed (zero until then)
```

All three are stamped together, in `selectLocked`'s empty-tier branch -
at *assignment* time, which for a client's first discovery packet is
well before it ever reaches the session port. `firstClientIP` is what
makes it possible to tell "this is the same client the cushion started
for" apart from "this is someone else," from the very first discovery
packet onward, without needing a real session to exist yet.

### The gate: `hostCooling`, released by a real signal with a timer as fallback

```go
func (p *hostPool) hostCooling(h *hostCandidate, requestingIP string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h.firstClientIP == requestingIP {
		return false // never block the claiming client's own traffic
	}
	if !h.firstClientLandedAt.IsZero() {
		return time.Since(h.firstClientLandedAt) < postLandCooldown
	}
	if h.firstClientJoinedAt.IsZero() {
		return false
	}
	return time.Since(h.firstClientJoinedAt) < joinCushion
}
```

`joinCushion` (5s, measured from *assignment*) is a fallback ceiling
only - it's a guess about how long a client takes to connect, and a
guess can be wrong in both directions: too short reopens the exact race
this design exists to close, too long wastes a second client's own
limited connect budget for nothing. `postLandCooldown` (also 5s, per
explicit request) is the real gate once it's active: measured from
*actual confirmed completion*, not assignment, so it's never wrong about
whether the first client is really still settling.

### Recognizing completion: the CD-key echo

```go
var cdKeyString = append([]byte("paullovesjade"), 0x00)

func isCDKeyEcho(payload []byte) bool {
	const wrapperLen, lenPrefixLen = 6, 2
	if len(payload) < wrapperLen+lenPrefixLen+len(cdKeyString) {
		return false
	}
	if payload[0] != newPeerWrapperType0 { // 0x03 - same wrapper as isNewPeerBroadcast
		return false
	}
	body := payload[wrapperLen:]
	if body[0] != byte(len(cdKeyString)) || body[1] != 0x00 {
		return false
	}
	return bytes.Equal(body[lenPrefixLen:lenPrefixLen+len(cdKeyString)], cdKeyString)
}
```

`sessionProxy.forwardToBackend` checks every client→backend packet for
this and calls `pool.markFirstClientLanded(host, senderIP)`, which only
actually sets `firstClientLandedAt` if `senderIP` matches
`host.firstClientIP` (under `hostPool.mu`, not read unlocked by the
caller) - a *second* client's own eventual echo must never be mistaken
for the first's and re-trigger this.

### The gate fires on both new and already-open discovery sessions

A client's first `0x25` might land before the host it'll eventually
share has anyone on it at all - at that instant there's nothing to
protect against, so it correctly gets a real reply and an established
session. If a *different* client then arrives and claims that same host,
the first session doesn't automatically know that - `hostCooling` gets
re-checked on every packet of an *existing* discovery session too, not
just at creation, so a host that starts cooling after a session was
already open still gets protected. Both paths withhold the reply
silently - no synthesized "no eligible host" decline, which would
incorrectly tell a client no game exists when one genuinely does, just
isn't safe to admit onto yet.

### The session-port cushion is a backstop, not the primary gate

Same `hostCooling` check, re-run at session-port admission. Redundant in
the common case (a client that never got a real `0x26` has no host
address to open a session-port connection to at all) but kept as
defense-in-depth, matching this codebase's own established pattern
(`hostFull`'s two independent checks, `healthcheck.sh`'s fail-open
default).

### What's deliberately NOT changing

- **`selectLocked`'s own tier priority** (prefer a host already waiting
  for a second player, then an empty one) - untouched. This is what
  keeps two clients *meant* to pair together landing on the *same* host
  even while one is being held back from a reply - the first revision's
  bug (see History) was conflating "which host" with "is it safe to say
  so yet," and this design keeps those two questions fully separate.
- **`hostFull`/`hostLive`/`hostProbe`/the `/hosts`,`/waiting`,`/full`
  status endpoints, `matchState`/`pairRelay`** - untouched throughout
  every revision.

## Verifying the fix

1. **Single-host pool, two clients within ~1-2s of each other**: confirm
   client B's discovery replies simply stop - no synthesized reply, no
   session, nothing on the wire in response to B's own repeating `0x25`
   - and confirm B's game window shows no visible change, just still
   "connecting." Confirm B gets a real `0x26` once A's CD-key echo is
   observed (plus `postLandCooldown`), and pairs correctly with A.
2. **Multi-host pool**: confirm a genuinely *different*, unrelated
   client still gets routed to an empty second host immediately, no
   cushion delay at all.
3. **Does client B ever reach session-port admission before the gate
   clears, in a real capture?** With discovery withholding working, the
   answer should be a clean no - `0.0.0.0:0` never gets a chance to
   happen, because B is never in a position to trigger it.

All three confirmed live 2026-08-23 - see "Confirmed live" below.

## Tuning

`joinCushion` and `postLandCooldown` are both still round starting
values (5s each), not measured ones - `postLandCooldown` matters far
less now than `joinCushion` did in earlier revisions, since it only ever
adds margin *after* a confirmed-real completion signal rather than being
the sole thing standing between "safe" and "not," so getting it exactly
right is much lower-stakes than tuning a blind pre-completion timer ever
was.

## Confirmed live, 2026-08-23

Reproduced the full scenario - two real clients connecting within a few
seconds of each other, single-host pool - after the fourth revision
landed. Client B's discovery replies stopped cleanly (visibly retrying
from the LAN lobby screen, no error shown), client A connected normally
in ~4s, and B was admitted correctly once A's CD-key echo fired plus the
cooldown - paired successfully, no stuck client, no `0.0.0.0:0` broadcast.

Two real bugs were caught and fixed live along the way, both worth
knowing about if this code is touched again:

- **`firstClientIP` wasn't enough on its own** - `selectLocked`'s empty-
  vs-waiting bucketing still used only `sessionCountForHost`, which
  stays 0 until a client reaches the *session* port. A client claimed at
  *discovery* time (well before session-port) still looked "empty" to a
  second client's own concurrent discovery ping, which would land in the
  empty bucket and silently overwrite the first client's claim. Fixed by
  also excluding any host with a live, unexpired `firstClientIP` claim
  from the empty bucket - it now buckets as "waiting" instead, so a
  genuinely different second client still gets correctly *matched* to
  that host (the pairing decision, untouched), it just won't be *told*
  about it until `hostCooling` clears.
- **A stale zombie client can still win a race** - not a logic bug, a
  test-process one, but worth recording: restarting `aom-lobby` without
  first stopping a *previous* test's still-running client containers let
  an old, never-cleaned-up client claim the freshly-restarted host before
  the real test's clients ever got a chance, producing a confusing result
  that looked like a bucketing bug but wasn't. Always stop old client
  containers *before* restarting the lobby, not after.

## History

**First revision (2026-08-22)**: gated *selection* itself - a cooling
host was excluded from `selectLocked`'s candidate list entirely. Bug,
not a feature: two clients *meant* to pair together would get routed to
*different* hosts if a second one was empty, since the correct match
became briefly invisible to selection. Caught before implementation by
asking whether 2 replicas + 2 clients meaning to play each other would
get split up - they would have. Fixed by moving the check to admission
time instead, leaving selection alone.

**Second revision (2026-08-22/23)**: gated *admission* at the session
port only. Live-tested four ways - cushion active, disabled entirely, a
discovery-port gate on new sessions only, and that gate extended to
already-open discovery sessions too - and got the *identical* `0.0.0.0:0`
broadcast and one-directional relay silence every single time, regardless
of how long client B waited before session-port admission. That result
proved the race is inside the host's own engine, happening at admission
regardless of pre-admission delay - which motivated moving the gate
further upstream, to discovery. Also considered and set aside:
synthesizing a corrected `0x29` broadcast ourselves rather than gating
anything - rejected because it requires fabricating a live gameplay
packet with fields this project has never fully decoded
(`directplay8-protocol.md`'s own "sin_zero" notes), a materially bigger
risk than gating a reply this project already knows how to withhold
safely.

**Third revision (2026-08-23)**: moved the gate to discovery, released by
a flat `joinCushion` timer measured from assignment time. Live-tested and
confirmed the discovery gate itself worked, but a client instantly seeing
the lobby exposed the `sessionCountForHost`-based bucketing bug described
under "Confirmed live" above - fixed in this same pass by switching to
`firstClientIP`/`firstClientJoinedAt` comparison instead, which is what
made discovery-time gating actually work at all.

**Fourth revision (2026-08-23)**: replaced the flat timer as the primary
gate with the CD-key echo (`isCDKeyEcho`/`markFirstClientLanded`) as an
event-driven release, keeping `joinCushion` only as a fallback ceiling
and adding `postLandCooldown` as extra margin after the confirmed signal.
Checked real historical captures (both proxied and genuinely unproxied,
from three independent vantage points) for a cleaner alternative signal
first - a map-details or per-slot acknowledgment - and confirmed none
exists; the settings-sync burst that follows the CD-key exchange is
entirely one-directional. Live-verified working end-to-end same day.
