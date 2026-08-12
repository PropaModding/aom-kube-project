# aom-lobby packet handling: reference + durability design

**Status: design/reference only, not yet implemented.** Written
2026-08-12. This is a companion to `main.go` (kept alongside it rather
than in `docs/`, since it's meant to be read next to the code it
describes) with two jobs:

1. A complete reference of every packet shape this proxy has confirmed
   evidence for, and which function in `main.go` handles it today (or
   would need to, once built).
2. A durability design - a session losing one client and gaining a
   replacement should keep routing correctly for everyone involved,
   including the surviving client, without disturbing anything that's
   still healthy. This extends (does not replace)
   `docs/multi-peer-routing-design.md`'s own "Durability" section, which
   still has the original 2026-08-08 design intent and open questions -
   read that first for context this doc assumes.

**Why a fresh pass on durability specifically**: two same-day attempts at
this already went wrong. 2026-08-11's session wired a plain 30s
client<->host idle timeout to tear down the A<->B pair relay, which
killed the second client's handshake mid-attempt (a healthy relay
getting killed because the *host-facing* channel looked quiet, not
because anyone actually left). 2026-08-11 also found a 3-byte resign-
shaped burst firing every ~2 minutes with no user action behind it, and
had already (correctly) disabled acting on it before this doc was
written. Both mistakes came from treating an *ambiguous* signal as a
confirmed departure. This design tries to avoid that class of mistake by
being explicit about which signals are actually trustworthy.

## Timing model (informs every timeout below)

Per direct observation of the real client's own behavior (2026-08-12):

- **Initial connect**: a client attempting Direct-Connect to the lobby
  gives up after **~15 seconds** if it can't reach a host at all (the
  discovery-port `0x25`/`0x26` exchange never completes).
- **Post-join pairing**: once a client has successfully joined the host's
  lobby, it then waits up to **~2 minutes** for the P2P handshake with
  the other real client plus receiving map/player-roster details before
  giving up (the "Attempting to Connect" dialog). This is the window
  `docs/multi-peer-routing-design.md`'s durability section didn't have a
  number for - it's also, not coincidentally, in the same neighborhood as
  the mystery periodic burst's ~2-minute period, which is worth keeping
  in mind as a possible (unconfirmed) connection between the two.

**Implication**: any timeout this proxy uses to decide "this client is
gone" must be safely longer than 2 minutes, or scoped to a channel that
genuinely reflects departure rather than normal quiet periods during that
window - the existing `idleTimeout` (30s, `main.go`) is far too short to
safely gate anything durability-related, which is exactly what broke on
2026-08-11.

## Packet reference

Every row has real captured evidence (epoch-timestamped examples: see
`archiving/sessions/20260808-234138-3client-p2p-check/NOTES.md` for the
non-proxied genuine baseline, and `docs/directplay8-protocol.md`/
`docs/directplay8-packet-classification.md` for the fuller writeups).
"Handled by" names an existing `main.go` function; "**Proposed**" names
one that doesn't exist yet, with its own justification.

### Discovery port (2299)

| Type | Direction | Bytes | Handled by |
|---|---|---|---|
| `0x25` enumerate-hosts query | client→host (broadcast) | 9, constant | Passed through unmodified (not in `discoveryAddressOffsets`) |
| `0x26` discovery reply | host→client | ~65-67 (name-length-dependent) | `discoveryRewrite` via `discoveryAddressOffsets[0x26]` |
| `0x20` client ping | client→host | 41 | `discoveryRewrite` via `discoveryAddressOffsets[0x20]` |
| `0x21` ping reply | host→client | 41 | `discoveryRewrite` via `discoveryAddressOffsets[0x21]` |

### Session port (2300) - connection handshake (unwrapped, no `03 00` prefix)

| Type | Direction | Bytes | Handled by |
|---|---|---|---|
| `0x00` "open" (self/peer) | either→either | 40 | `isSessionHandshake` + `rewriteSessionField` (`sessionSelfOffset`/`sessionPeerOffset`), in both `sessionProxy`'s hooks and `pairRelay.run()` |
| `0x02` handshake ack | either→either | 40 | Same as above - `isSessionHandshake` doesn't distinguish `0x00` vs `0x02`, both get identical self/peer rewriting since neither carries any other address-bearing field |

### Session port (2300) - settings-sync layer (`03 00`-wrapped)

| Type | Direction | Bytes | Handled by |
|---|---|---|---|
| Session-name echo | host→client | 45 | Passed through (no embedded address) - undecoded sub-type, not yet named |
| Player-announce | either→either | 93-95 (nickname-length-dependent) | Passed through (no embedded address needing rewrite) |
| Map-name (long) | host→client | 139 | Passed through |
| Map-name (short) | host→client | 69 | Passed through - undecoded sub-type, distinct from the 139-byte one, not yet named |
| Lobby chat | either→either | 41 | Passed through |
| `07ff` heartbeat | either→either | 10 | Passed through |
| `0x29` new-peer broadcast (empty variant) | host→existing peer | 51 | `isNewPeerBroadcast` detects it; `other == nil` after polling, forwarded unmodified since there's nothing to rewrite to yet - confirmed genuine (fires before the new peer exists), not a bug |
| `0x29` new-peer broadcast (full variant) | host→existing peer | 51 | `isNewPeerBroadcast` + `rewriteNewPeerBroadcast` |
| `existingPeerBroadcast` | host→new peer | 87 | `isExistingPeerBroadcast` + `rewriteExistingPeerBroadcast` |
| Ready-toggle | client→host | 22 | `isReadyToggle` + `readyToggleState`, drives `matchState.setReady`/`markStarted`/`triggerMatchStart` |
| Ready-toggle (same shape) | **host→client** | 22 | Confirmed to exist (`NOTES.md`'s 2026-08-12 addition) but **not currently read** - no functional need today (only client ready-state drives match-start), noted here for completeness in case host-side ready-state ever matters |
| Resign/leave burst | client→host (as currently coded) | 3, `01 <connID>`, ~10x rapid-fire | `isResignBurst`, checked in `sessionProxy.rewriteToBackend` - **currently disabled from acting on anything**, log-only (see Durability section) |
| Resign/leave burst, **host→client direction** | host→client | 3, same shape | **Not currently checked at all** - see Durability section, this is the proposed real signal |

## Durability design

### The core problem

Two logically separate questions, both currently unsolved safely:

1. **How do we know a client is really gone**, as opposed to just quiet
   for a while? (2026-08-11's mistake: used channel quietness as a proxy
   for departure. Wrong - channel quietness during the 2-minute pairing
   window is *expected*, not a departure signal.)
2. **Once we know, how do we tear down only that client's state** without
   touching the survivor, and let a replacement client re-pair cleanly?

### Question 1: a trustworthy departure signal

**Proposed**: re-point resign detection at the **host→client** direction,
based on today's cross-check against the archived genuine capture
(`NOTES.md`), where the confirmed resign burst goes host→client, not
client→host as currently coded. The existing client→host check should
stay in place but move to log-only permanently (matches its current
disabled state) until the ~2-minute periodic false positive on that
channel is independently explained - **do not re-enable acting on it**
without a fresh capture ruling out coincidence.

Open question worth flagging honestly: the one captured instance of the
host→client burst has embedded connection ID `bc66`, which is the same
ID seen throughout the host↔clienta relationship elsewhere in that
capture - consistent with either "the host is telling clienta its own
pairing is ending" or "the host is echoing clienta's own resignation back
to it," and the archived capture's own manual notes don't record *which*
client resigned. **Before implementing this**, the reference capture
`docs/multi-peer-routing-design.md` has called for since 2026-08-08
(deliberately resign one specific, known client, capture all three
vantage points) needs to actually happen - this is the second design pass
in a row to reach that same conclusion, which is itself a signal that
skipping it has a real cost.

**Proposed function**:

```go
// isHostResignNotice reports whether payload matches the 3-byte
// "01 <connID>" resign/leave shape (see isResignBurst) arriving from the
// HOST rather than a client - the direction confirmed against the
// archived genuine 3-client capture
// (archiving/sessions/20260808-234138-3client-p2p-check/NOTES.md), as
// opposed to the client->host instance of the same shape, which has a
// confirmed, unrelated, ~2-minute-periodic false-positive source and
// should not be acted on (see isResignBurst's own doc comment).
//
// NOT YET CONFIRMED whose departure this signals when it fires - see
// packet-handling-design.md's "Durability design" section. Land the
// targeted reference capture that section calls for before wiring this
// to any teardown action; until then this should ship log-only, the
// same validate-before-acting discipline already applied to
// isResignBurst itself.
func isHostResignNotice(payload []byte) bool {
    return isResignBurst(payload) // same shape; direction is the caller's job to check
}
```

Wiring: checked in `sessionProxy`'s `rewriteToClient` (the host→client
direction), logged the same way `isResignBurst` already is elsewhere -
**not** wired to any teardown until the reference capture above confirms
what it actually means.

### Question 2: scoped teardown + clean re-fill

The existing `matchState.onClientGone`/`proxy.closeSession` machinery
from 2026-08-11 is architecturally fine - the mistake was only ever in
*what triggered it* (the idle timeout), not the teardown logic itself
(already correctly scoped to just the departing address, already leaves
the survivor's own `sessionProxy` session and `m.pair` untouched if the
departing address isn't part of the current pair). Once question 1 has a
trustworthy signal, wiring `onClientGone` to *that* instead of the idle
reaper should be safe to re-enable - this doc doesn't propose new
teardown logic, just correcting what feeds it.

**Update (2026-08-12, live evidence, not yet a full reference capture)**:
the open question above - does the host re-send
newPeerBroadcast/existingPeerBroadcast on rejoin - got a first real data
point during ad-hoc testing of the conn-ID attribution fix (see this
doc's packet reference section history). Client B exited to the lobby
menu and rejoined mid-session (**without** its underlying wine process
restarting, so its real address:port was unchanged) - the host sent
fresh copies of *both* broadcasts, both correctly rewritten to the
existing pair relay port, and the rejoin worked cleanly with zero new
code. This is real signal that the host *does* re-announce on rejoin,
at least for this case - but it's not yet the harder case below, and
still isn't the deliberate, both-vantage-points reference capture this
section calls for. Treat as "encouraging, not conclusive" until that
capture happens.

**Proposed function** - the piece that's genuinely missing today, the
re-fill half:

```go
// matchState.ensurePairRelay already handles a fresh, from-scratch pair
// correctly (idempotent, symmetric - see its own doc comment), and
// 2026-08-12 testing showed it already handles a same-client rejoin
// correctly too (see the "Update" note above) - purely because the
// stale relay's addresses were still valid, not because of any new
// logic. The remaining open question is the harder case: a genuinely
// *different* replacement client (different real address) joining the
// vacated slot - still unconfirmed whether the host re-announces for
// that case the same way, needs its own reference capture (drop one
// client, replace it with a different one, capture all vantage points -
// docs/multi-peer-routing-design.md has called for this exact capture
// since 2026-08-08 too).
//
// ensurePairRelayIfNeeded is a proposed fallback for if the host does
// NOT reliably re-announce for that harder case: rather than only ever
// creating the pair relay reactively (in response to seeing one of the
// two broadcast messages), proactively check for a re-pairing
// opportunity every time ANY client completes its own session handshake
// with the host (i.e. every isSessionHandshake match in sessionProxy,
// not just the two broadcast-triggered call sites). If there are
// currently exactly two real sessions and no pair relay, build one
// directly from sessionProxy's own session map - no dependency on the
// host's broadcast behavior at all, since sessionProxy already knows
// both real addresses once both sessions exist. Only justified as a
// fallback if the reference capture above shows the host does NOT
// reliably re-announce for a genuine replacement; if it does, the
// existing reactive path is simpler and should stay primary.
func (m *matchState) ensurePairRelayIfNeeded(sp *proxy) {
    // pseudocode, not implemented:
    // if m.pair != nil { return }
    // addrs := sp.allSessionClientAddrs() // proposed helper, doesn't exist yet
    // if len(addrs) != 2 { return }
    // m.ensurePairRelay(addrs[0], addrs[1])
}
```

### Summary of what's actually proposed vs. already built

| Piece | Status |
|---|---|
| Scoped teardown (`onClientGone`, `closeSession`) | Already built 2026-08-11, architecturally sound, just needs re-wiring to a trustworthy trigger |
| Idempotent pair-relay creation (`ensurePairRelay`) | Already built and confirmed working 2026-08-10/11 |
| Trustworthy departure signal | **Proposed**: `isHostResignNotice`, needs a reference capture before wiring to any action |
| Proactive re-fill fallback | **Proposed**: `ensurePairRelayIfNeeded`. Same-client rejoin already confirmed working with zero new code (2026-08-12); only needed if a reference capture shows the host doesn't reliably re-announce for a genuinely *different* replacement client |

Neither proposed function should be wired to live teardown/creation
behavior until its own reference capture lands - this doc's whole point
is not repeating 2026-08-11's mistake of acting on an assumption instead
of evidence.

## N-host round-robin matchmaking (design)

Everything above assumes a single match (one `aom-headless` backend, one
pair relay). This section is the other half of CLAUDE.md's Architecture
intention - "the lobby decides whether to route [a client] into an
existing open... match or provision a brand new `aom-headless` pod on
demand" - specifically the *decision* of which of several candidate
hosts a newly-arriving client gets matched into. Nothing here is
implemented; `cfg.discoveryBackendAddr`/`cfg.sessionBackendAddr` are
still single, static, env-set addresses today (see `deploy-minikube.sh`).

### The constraint that shapes everything else

Players type **one fixed cluster address** into AoM's Direct-IP field -
there's no port entry, so per-match addressing behind a NodePort isn't
possible (already established, see `k8s/lobby-deployment.yaml`'s own
comments). That means there can only be one externally-reachable
discovery/session endpoint for the whole cluster, not one per match.
**Therefore this has to be one `aom-lobby` instance matchmaking across a
pool of N `aom-headless` backends internally - not N independent
lobby+host pairs each with their own address.** Worth stating explicitly
since "just run more lobby+host pairs" is the more obvious-looking
design and doesn't actually work given how players connect.

### Budget: the 15-second window is the client's, not ours to spend

Per this doc's "Timing model" section, a client gives up on its *own*
Direct-Connect attempt after ~15s if it can't complete the discovery
exchange. That's the client's total patience for the whole flow
(including its own retries), not a budget this proxy gets to spend
deliberately - matchmaking (probe candidates, pick one, reply) needs to
be fast in practice (sub-second), not "up to 15 seconds." The 15s figure
matters here mainly as a hard ceiling: whatever matchmaking does, a
client should see *some* usable `0x26` reply well before it, or the
client gives up regardless of whether a decision was still being made.

This creates a real tension with on-demand pod provisioning (CLAUDE.md's
"provision a brand new pod on demand" line): a fresh pod's own startup
sequence (`entrypoint.sh` → Xvfb/wine → `host-game-kube.sh`'s EULA-
through-lobby click sequence) is documented elsewhere as taking well
over a minute, not 15 seconds. **A client whose query triggers
provisioning a brand-new pod cannot itself wait for that pod to be
ready** - it will hit its own 15s timeout first. Flagged here as an open
problem, not solved: on-demand provisioning likely needs to be
decoupled from any single client's own connect attempt (e.g. pods
pre-provisioned slightly ahead of demand, or the provisioning client
told to retry shortly after rather than blocked on it).

### Selection policy

Round-robin, in this priority order:

1. **Prefer a host that's already waiting for a second player** - fill
   an open match before starting a new one. If more than one host is
   simultaneously waiting, round-robin among *those specifically*
   (fairness/load spread) rather than always picking the same one.
2. **Otherwise, an empty host** (nobody connected yet) - round-robin
   among those.
3. **Otherwise, none available** - turn the client away with a "lobby
   full" rejection rather than the on-demand-provisioning path (see
   below for why a rejection is preferable to silently timing out).

A host with two real clients already paired (`matchState.full()`'s
existing per-match semantics, generalized to per-host here) is **never**
eligible for new routing - matches the user's "flag if a game is
currently full" ask, and this flag already exists today, just scoped to
one match rather than a pool.

### Case 3 in detail: turning a client away, and what the official docs say about it

Checked `[MC-DPL8CS]` for an official precedent before inventing
anything - **there's no dedicated "session full" error code**, but
section 2.2.1.3 `DN_CONNECT_FAILED` ("the DN_CONNECT_FAILED packet
indicates that a connection attempt failed") lists an `hResultCode`
enum that includes `DPNERR_HOSTREJECTEDCONNECTION` ("Application
declined connection attempt") - the generic "the game itself said no"
code, which a full session is a completely ordinary reason to use.
Notably, this is also the **only** one of `DN_CONNECT_FAILED`'s failure
codes documented as carrying a `reply` field ("Reply data is only
expected when the failure type is DPNERR_HOSTREJECTEDCONNECTION") - the
protocol was built with exactly this "let the application supply its
own rejection message" case in mind, not just a bare error code. Worth
knowing too: the *success* reply, `DN_SEND_CONNECT_INFO` (2.2.1.4),
carries `dwMaxPlayers`/`dwCurrentPlayers` directly - a real DP8 host has
native capacity fields to check before ever getting that far, so
"reject because full" is a first-class, anticipated case in the design,
not a workaround.

**Important caveat**: this is `[MC-DPL8CS]`'s DN_CONNECT_FAILED, which
fires at the *connect-to-a-specific-discovered-host* step (after a
client has already picked a host to Direct-Connect/join) - the
structural analog of classic DirectPlay's session-port (2300) handshake
this proxy already reverse-engineers, not the discovery-port (2299)
`0x25`/`0x26` LAN-enumeration step matchmaking operates at. **Classic
DirectPlay's own equivalent of DN_CONNECT_FAILED, if one exists, has
never been captured** - every capture this project has taken so far is
a successful join; there is currently zero evidence for what a genuine
rejection looks like on the wire for the actual client AoM uses.

**Decided (2026-08-12): spoof this packet.** Rather than silently not
replying to the `0x25` query (the no-protocol-risk option, which was
also on the table - a turned-away client would just see no host listed
and hit its own connect-timeout with no explanation), `aom-lobby` will
synthesize a rejection so the player actually sees a "lobby full"-style
message instead of a bare timeout - matching the pattern
`DN_CONNECT_FAILED`/`DPNERR_HOSTREJECTEDCONNECTION` establishes as the
intended, application-message-carrying way to decline a connection.

This decision does **not** remove the prerequisite: classic DirectPlay's
real wire shape for a rejection is still unconfirmed, and this is the
one message in this whole design that would be fabricated from scratch
rather than reverse-engineered from a real capture - every other
spoofed/rewritten field elsewhere in this project started from bytes
observed on the wire. **A reference capture of a genuine rejection is
required before implementing this, not optional** - host a real 2-slot
game, fill both slots, have a third real client attempt to Direct-
Connect, and capture all vantage points the same way every other finding
in this project was established. Implementing a guessed byte layout
instead risks a client silently ignoring or mishandling a malformed
packet, which would be strictly worse than today's plain timeout.

> **TODO (2026-08-12): capture the genuine rejection.** Not done yet -
> deferred to a later session. Setup: host a real 2-slot game (e.g.
> `run-aom-verbose-3clients.sh` or the proxied equivalent), fill both
> slots with real clients, then attempt to Direct-Connect a *third* real
> client and capture all vantage points (host + the rejected client at
> minimum). This is the blocking prerequisite for
> `synthesizeLobbyFullRejection` below and for case 3 of the selection
> policy generally - nothing in this section should be implemented
> before it.

**Proposed function**, to be filled in once that capture exists:

```go
// synthesizeLobbyFullRejection builds whatever packet classic DirectPlay
// clients expect in response to a connect attempt the host declines -
// see packet-handling-design.md's "Case 3 in detail" section. Byte
// layout is a placeholder until a genuine rejection has been captured
// and reverse-engineered the same way every other message in this
// project was (see docs/directplay8-protocol.md for the method) - do
// NOT ship a guessed layout, a malformed rejection is worse for the
// player than today's plain connect-timeout.
func synthesizeLobbyFullRejection(cfg config) []byte {
    panic("not implemented - needs a reference capture first, see doc comment")
}
```

### Extending existing infrastructure

`hostProbe` (`main.go`) already anticipates exactly this in its own doc
comments - "One backend today... at which point this becomes a slice of
probes summed together" and "matchmaking will want to pick a specific
waiting probe to round-robin into, not just a yes/no across all of
them." That's this section, several sessions later.

**Proposed**:

```go
// hostCandidate is one pool member - both addresses a real host needs
// (discovery probing uses the Service DNS name; session routing needs
// the pod's raw IP, per SESSION_BACKEND_ADDR's own existing doc comment
// in deploy-minikube.sh on why those two can't be the same value).
type hostCandidate struct {
    probe              *hostProbe
    discoveryBackendAddr string
    sessionBackendAddr   string
}

// hostPool tracks every known aom-headless backend and implements the
// selection policy above. Replaces today's single cfg.discoveryBackendAddr/
// cfg.sessionBackendAddr with a slice once this is built - single-backend
// mode (today's actual behavior) is just a hostPool of size 1 with no
// selection logic needed.
type hostPool struct {
    mu    sync.Mutex
    hosts []*hostCandidate

    // nextRoundRobin tracks position independently per eligibility tier
    // (waiting vs. empty) - a naive single counter would let a long run
    // of "waiting" picks starve the round-robin position for "empty"
    // hosts once that tier is needed, or vice versa.
    nextWaitingRR int
    nextEmptyRR   int
}

// selectForNewClient implements the priority order above. Returns
// (nil, false) if no host is eligible (case 3).
func (p *hostPool) selectForNewClient() (*hostCandidate, bool) {
    // pseudocode, not implemented - waiting-tier round robin first,
    // empty-tier round robin second, false if neither tier has a
    // candidate.
    return nil, false
}
```

### The race this needs to guard against

`hostProbe.probeOnce` already only polls every `probeInterval` (3s) -
between polls, a host's real "waiting" state can be stale relative to
what `selectForNewClient` reads. Two clients whose discovery queries
land in the same few seconds could both get matched to the same
"waiting" host before either one's actual join completes, over-filling a
1v1 slot. **Proposed**: a short-lived claim/reservation - when
`selectForNewClient` picks a host, mark it claimed for a few seconds
(bounded well under the 15s connect budget) so a second concurrent
selection skips it even though the underlying `hostProbe` hasn't
re-polled yet. Not designed in detail here; flagged so it isn't silently
assumed away, since this proxy has already been bitten once today
(2026-08-11) by an under-specified concurrency assumption.

### The other half: session-port stickiness

Matchmaking only decides where a client's *discovery* query gets routed.
Once decided, that client's *session*-port (2300) traffic also needs to
consistently land on the same chosen backend for the rest of the match -
CLAUDE.md's Architecture intention already flags this as open
("`aom-lobby` itself needs to route the actual UDP 2300 session traffic
to the correct pod per client/match... Open question, not yet
designed"). The natural extension of this proxy's own established
pattern: `clientTracker` today remembers one global "most recently seen
client" address, assuming a single backend. A multi-host version needs a
per-client (not global) record of *which host* that client was matched
to - likely a `map[string]*hostCandidate` keyed by real client address,
populated at matchmaking time and consulted by `forwardToBackend`/
`relayBackendInitiated` instead of the single `cfg.sessionBackendAddr`
constant those currently use. Not designed in further detail here - this
section's job was the matchmaking *decision*, this is the natural next
section once that's built.

---

## Workshop: gameplay traffic (speculative, not captured)

Everything above is the lobby/connection-management layer, which has
real capture evidence behind it. Nothing below does - this section is
scratch space for once a genuine in-match capture exists, not a design
commitment.

- **Working assumption**: once a match starts, gameplay state sync
  probably continues to ride the same `03 00`-wrapped settings-sync
  layer and the same three pairwise channels (host↔A, host↔B, A↔B) this
  proxy already relays - no new channels, just new, undecoded sub-types
  layered on the existing wrapper. If true, the existing pass-through
  behavior (anything that isn't a session handshake or one of the two
  address broadcasts goes through untouched) should already handle
  gameplay traffic correctly without any new code - worth confirming
  rather than assuming.
- **Open question**: does the *volume/frequency* of gameplay traffic
  change the durability timing model above? If real gameplay is chattier
  than lobby idle traffic (plausible), a 30s-class idle timeout might
  never realistically fire during an actual match, making the original
  2026-08-11 bug harder to hit in real play than in this session's
  testing (where the host and clients likely sat idle in menus, not
  actually playing) - worth knowing before concluding the idle-timeout
  approach is unsafe under all conditions, versus just unsafe during the
  specific pre-match window this session's testing exercised.
- **Open question**: `docs/directplay8-protocol.md`'s length-distribution
  notes mention new, not-yet-decoded sizes appearing once a match starts
  (128, 49, 39 bytes, per the archived 3-client capture) - worth a
  dedicated decode pass if gameplay-layer work ever becomes a priority,
  same method as everything else in this project (diff a live capture
  against a genuine baseline).
- **Not investigated at all**: host migration (what happens if the
  *host* pod itself needs to restart mid-match) - out of scope per
  CLAUDE.md's Goals (this project doesn't support host migration, the
  host pod is expected to stay up for the match's duration), noted here
  only so it's not silently assumed away.
