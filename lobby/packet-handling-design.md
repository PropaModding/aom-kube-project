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
| Session-name echo (subtype `03 00 02 00`) | host→client | 45-47 (name-length-dependent) | Passed through (no embedded address) - UTF-16LE-encoded session/game name confirmed 2026-08-18 (`archiving/sessions/20260818-*-real-host-joiner-namecheck/`, real host+joiner through `aom-lobby`'s real path: `...54006800650049005000270073002000470061006d0065...` decodes to "TheIP's Game", the real `aom-headless` pod's own configured game name) - inner field layout beyond that still undecoded |
| Unidentified subtype `03 00 01 00` | host→client | 15 | Passed through - newly observed 2026-08-18, same source as above (`...050014020000000000`); not yet decoded, only one sample so far |
| Player-announce | either→either | 93-95 (nickname-length-dependent) | Passed through (no embedded address needing rewrite) |
| Map-name (long) | host→client | 139 | Passed through |
| Map-name (short) | host→client | 69 | Passed through - undecoded sub-type, distinct from the 139-byte one, not yet named |
| Lobby chat | either→either | 41 | Passed through |
| `07ff` heartbeat | either→either | 10 | Passed through |
| `0x29` new-peer broadcast (empty variant) | host→existing peer | 51 | `isNewPeerBroadcast` detects it; `other == nil` after polling, forwarded unmodified since there's nothing to rewrite to yet - confirmed genuine (fires before the new peer exists), not a bug |
| `0x29` new-peer broadcast (full variant) | host→existing peer | 51 | `isNewPeerBroadcast` + `rewriteNewPeerBroadcast` - **correction (2026-08-20): both this and the row above were only ever caught when byte 1 of the wrapper happened to be `0x00`; see `docs/directplay8-protocol.md`'s "Common wrapper" correction - byte 1 isn't a fixed type byte, and requiring it silently drops genuine copies of this message, including in the confirmed-working 2026-08-12 baseline itself** |
| `existingPeerBroadcast` | host→new peer | 87 | `isExistingPeerBroadcast` + `rewriteExistingPeerBroadcast` - same byte-1 correction applies |
| Ready-toggle | client→host | 22 | `isReadyToggle` + `readyToggleState`, drives `matchState.setReady`/`markStarted`/`triggerMatchStart` - **confirmed live end-to-end 2026-08-12**: both real clients pressed Ready, both toggles detected within 1s of each other, `triggerMatchStart` fired exactly once, `input-agent` clicked the host's Ready crystal (511, 89), match actually started on the host. First full test of this path with real ready-presses (all prior verification was code-level only) |
| Ready-toggle (same shape) | **host→client** | 22 | Confirmed to exist (`NOTES.md`'s 2026-08-12 addition) but **not currently read** - no functional need today (only client ready-state drives match-start), noted here for completeness in case host-side ready-state ever matters |
| Resign/leave burst | client→host (as currently coded) | 3, `01 <connID>`, ~10x rapid-fire | `isResignBurst`, checked in `sessionProxy.rewriteToBackend` - **currently disabled from acting on anything**, log-only (see Durability section) |
| Resign/leave burst, **host→client direction** | host→client | 3, same shape | **Not currently checked at all** - see Durability section, this is the proposed real signal |

## Canonical byte-level reference: spoofing the full "lobby full" sequence

Written 2026-08-18 as a consolidated, complete reference distinct from the
investigation narrative in "Case 3 in detail" and "Next investigation"
below - this section is the answer, those are how we got here. Synthesized
from six independent real captures, cross-checked byte-for-byte against
each other. Every byte below is marked confirmed+explained,
confirmed-but-unexplained (flagged **UNKNOWN**), or session-variable
(flagged with what's known about its range).

**All six of those captures are REJECT-branch sessions** (see Phase 2's
own correction note below) - Phases 0 and 1 are branch-independent (the
open/ack/heartbeat layer is identical either way, confirmed 2026-08-19
against a genuinely accepted join), but Phase 2 forks at Step 12. A
seventh capture, the only accepted-join reference this project has
(`3client-p2p-check`), supplies Step 12b.

### Phase 0 - Discovery (UDP 2299)

**Step 1: Client → Host, `0x25` enumerate query (9 bytes, constant)**
```
25 00 00 00 00 00 00 00 00
```
All 9 bytes fully understood - type byte + 8 bytes of fixed padding, never varies.

**Step 2: Host → Client, `0x26` discovery reply (65-67 bytes, name-length-dependent)**
```
26 01 00 00 00                              [5]  header - byte 0 = type, bytes 2-3 = "00 00" UNKNOWN (never observed non-zero; likely version/reserved, unconfirmed)
<sockaddr_in self>                          [16] see sockaddr breakdown below
<sockaddr_in self, duplicated>              [16] identical to the block above
<name length, uint32 LE>                    [4]  byte count including UTF-16 null terminator
<UTF-16LE session name + null>              [N]
```

**Step 3: Client → Host, `0x20` liveness ping (41 bytes)**
```
20 01 00 00 00                              [5]  header, same UNKNOWN bytes 2-3 as above
<sockaddr_in self>                          [16]
<sockaddr_in self, duplicated>              [16]
00 00 00 00                                 [4]  trailer, always zero
```

**Step 4: Host → Client, `0x21` ping reply (41 bytes)** - identical shape to `0x20`, header byte 0 = `21` instead.

**The `sockaddr_in` block** (appears in every message above, 16 bytes):
```
02 00                    [2]  sin_family = AF_INET, little-endian
<port, uint16 BE>        [2]  always 2300 (0x08fc) in every message this project sends/rewrites
<ip, 4 bytes>            [4]  dotted-order
<sin_zero>               [8]  RESOLVED 2026-08-18 - see below, NOT unknown
```

**`sin_zero` (bytes 8-15 of every sockaddr block) - resolved, not a mystery
field.** It's a per-participant identity tag each side generates once for
itself; whoever else embeds that participant's address later must echo it
back byte-for-byte, not invent their own. Confirmed with an exact 8-byte
match across two independent captures (a client's own `0x20` ping vs. the
host's later `0x00` describing that same client). Rule for spoofing: use
one arbitrary constant when describing yourself, and the real client's own
reported value (learned from its `0x20` ping) when describing the client -
implemented as `ourOwnSinZero`/the `onDiscoveryPingNoHost` threading in
`lobby/main.go`.

### Phase 1 - Session handshake (UDP 2300)

**Step 5: Host → Client, `0x00` self/peer-open (40 bytes), sent
unprompted, retried ~100ms apart until acked**
```
00 00                                       [2]  type
<own connID, 2 bytes>                       [2]  arbitrary, invented, consistent per session
d8 f6 32 00                                 [4]  UNKNOWN - see below
<sockaddr_in self>                          [16] your own address, arbitrary sin_zero
<sockaddr_in peer>                          [16] the client's address, sin_zero = client's own reported value
```

**`d8 f6 32 00` (bytes 4-7) - still genuinely unknown.** Byte-identical
across every capture this project has ever taken - every host, every
joiner, multiple days apart. Best-supported theory (unconfirmed): a
leaked/uninitialized 32-bit stack address inside `dpwsockx.dll`'s memory
(`0x0032F6D8` read little-endian is a very plausible pre-ASLR Windows
thread-stack address), reproducible because Wine + this specific 32-bit
binary place that buffer deterministically on every launch. Safe to
hardcode for spoofing purposes since it's never varied, but not
understood.

**Step 6: Client → Host, `0x02` ack (40 bytes) - first thing the client
sends, arrival timing highly variable (400ms to 3.8+ seconds observed,
both leading to success)**
```
02 00                                       [2]  type
<own connID, 2 bytes>                       [2]
<peer connID, 2 bytes>                      [2]  echoes the connID the host used in its 0x00
<trailer, 34 bytes>                         see breakdown below
```

**The 34-byte ack trailer** - six confirmed real samples, cross-checked
byte-for-byte:
```
00 00 00 00 00 00 00                        [7]  always zero
e9 e9 7f                                    [3]  UNKNOWN, always this exact value, every sample
XX                                          [1]  role-consistent for joiner (always 0xa4, 5/5 samples); host varies (0xa6 or 0xa8) - UNKNOWN what determines the host's value
00                                          [1]  always zero
YY                                          [1]  UNKNOWN - genuinely session-variable (0x01 in 4 of 5 real sessions, 0x02 in one). Best-supported theory: analogous to DirectPlay 8's own documented `tTimestamp` field (official [MC-DPL8R] spec - "sender's system tick count in milliseconds"), but a live gdb investigation (2026-08-18) found ZERO clock-reading syscalls anywhere in the client's entire process trace, weakening (not disproving - vDSO calls are invisible to strace) that theory
00                                          [1]  always zero
18 f7 32 00                                 [4]  UNKNOWN, always this exact value
3f e4 d4 7f                                 [4]  UNKNOWN, always this exact value
XX                                          [1]  same value as the earlier XX (mirrored)
00                                          [1]  always zero
YY                                          [1]  same value as the earlier YY (mirrored)
00 01 00 00 00                              [5]  always this exact value
18 f7 32 00                                 [4]  UNKNOWN, always this exact value (same bytes as above)
```
For spoofing purposes: hardcode everything except XX/YY, which should be
treated as "pick any consistent value" - no evidence the receiving side
validates them, but they're not reproducibly derivable either.

**Step 7: Host → Client, `0x02` ack** - same shape as above, host's own
connID/peer connID and trailer (with the host's own XY values).

**Step 8: Client → Host, `0x00` mirror-open** - same shape as Step 5,
client describing itself, sent within ~200us of its own `0x02` ack.

**Step 9: Host → Client, `0x02` ack (of the client's mirror-open)** - same
shape as Step 6.

**Heartbeat, `07ff` (10 bytes), interspersed throughout, both
directions:**
```
07 ff 00 00                                 [4]  type + always-zero pad
<connID, 2 bytes>                           [2]
00 00 00 00                                 [4]  always zero
```
Fully understood, no unknowns.

### Phase 2 - Name exchange to accept-or-reject

**Correction (2026-08-19): every capture this section's Phase 2 was
originally built from is the REJECT branch specifically**, not a generic
"successful, non-lobby" reference as some of this project's own casual
descriptions of them implied - `directconnect-only-capture` and both
`session-establishment-investigation` sessions all end in the exact same
rejection this section documents (confirmed by re-checking their raw
`03 00` traffic). None of those three ever depict an accepted join; the
handshake through the client's own name-broadcast (steps 5-11 below)
completes identically either way, which is why they were still valid
ground truth for that portion, but "successful" in those write-ups meant
"the handshake progressed," not "the join was accepted." The **only**
capture in this project showing a genuinely accepted join is
`archiving/sessions/20260808-234138-3client-p2p-check/` (`Players` was
actually raised to 3 there) - see Step 12b below, added once this was
noticed.

**Step 10: Host → Client, name-broadcast (24 bytes for a 13-char name),
sent ~100ms after the last ack, bundled with a proactive heartbeat
immediately before it**
```
03 00                                       [2]  type
<seq, uint16 LE>                            [2]  genuinely varies per retry (0, 1, 0 observed in one real session) - not simply monotonic, exact rule UNKNOWN
<own connID, 2 bytes>                       [2]
<declared length, uint16 LE>                [2]
<ASCII name + null>                         [N]  "paullovesjade" in every capture, no exceptions - a fixed CD-key-check field, NOT a player nickname (real nicknames are UTF-16LE, in the separate player-announce message). **Confirmed 2026-08-19 to be functionally required, not cosmetic** - see this doc's own Step 10 fix history below
00 00                                       [2]  trailer beyond declared length, always zero
```

**Fixed 2026-08-19 - this field's exact content is load-bearing.**
`aom-lobby`'s synthesis originally sent `"TheIP"` here (a leftover from
when this field was assumed to be a real nickname slot, before
`"paullovesjade"`'s true nature was understood). That was the actual
root cause of this whole "client never sends its own name-broadcast"
investigation: a real client would complete the open/ack layer
perfectly, receive the host's name-broadcast containing `"TheIP"`, and
then simply never reply with its own - stalling until its own ~2-minute
patience timeout, every single time, regardless of every other byte/
timing fix made earlier the same night. Changing the sent value to
`"paullovesjade"` (exactly matching every real capture) fixed this
immediately - live-tested, the client now replies with its own name-
broadcast right away and the rest of the sequence (rejection, resign
burst) proceeds exactly like a real host's. Whatever the client's crack
logic actually does with this field (exact string comparison being the
simplest explanation, though unconfirmed), it clearly checks it in some
way - this is not a cosmetic/decorative field, contrary to what earlier
passes over this project concluded. Still true from before: no
variation of this string has ever been observed in any capture,
including the one genuinely accepted (non-rejected) session this
project has - so if it is CD-key-related, whatever check happens is
against this one fixed constant, not a live per-session value.

**Step 11: Client → Host, name-broadcast** - same shape, client's own
connID and seq.

**Step 12: Host → Client, REJECTION (23 bytes) - arrives ~1.5-15ms after
the client's own name-broadcast, seq = one past whatever the host's own
name-broadcast retries last used**
```
03 00                                       [2]  type
<seq, uint16 LE>                            [2]
<own connID, 2 bytes>                       [2]
0d 00                                       [2]  declared length = 13
22 07                                       [2]  UNKNOWN - the actual rejection signal, tried every reasonable decoding (ASCII, uint16 LE/BE), nothing readable
00 00 00 00 00 00 00 00 00 00 00            [11] always zero
00 00                                       [2]  trailer beyond declared length, always zero
```
For spoofing purposes: reproduce `22 07` + 11 zero bytes exactly. It
doesn't need to be understood to be correctly replicated - every sample
across every capture is byte-identical here.

**Step 12b: Host → Client, ACCEPT (15 bytes) - the branch REJECTION
above is an alternative to, occupying the same seq slot.** Found
2026-08-19, diffing `3client-p2p-check`'s two real joins (host-only
lobby getting its 1st real joiner, then that same lobby getting its 2nd)
against the REJECT-path captures above - never previously documented
since no prior capture in this project ever depicted an accepted join.
```
03 00                                       [2]  type
<seq, uint16 LE>                            [2]
<own connID, 2 bytes>                       [2]
05 00                                       [2]  declared length = 5 (vs REJECTION's 0d 00 = 13 - different opcode family, not a variant of the same message)
14                                          [1]  ACCEPT opcode (vs REJECTION's 22 07)
XX                                          [1]  02 for the 1st real joiner into a host-only lobby, 03 for the 2nd - one sample each, but shape strongly suggests a running accepted-player count/slot index
00 00 00                                    [3]  padding
00 00                                       [2]  trailer beyond declared length, always zero (same shape as REJECTION's own trailer)
```
What follows differs by scenario and is itself informative: the 1st real
joiner (host-only lobby, no other peer yet) gets a 53-byte message right
after ACCEPT carrying the session name in UTF-16LE ("host's Game"); the
2nd real joiner (one real peer already present) instead gets a 77-byte
message containing two identical `sockaddr_in` blocks describing that
already-present peer's real address - the `existingPeerBroadcast`
concept `lobby/main.go` already implements for client↔client P2P
routing, independently confirmed correct by this capture. Then a rich,
long-running settings-sync burst continues (lobby options, map settings,
ready-state, eventually live match traffic) - nothing like the
REJECTION branch's abrupt stop.

Not used by `aom-lobby`'s own no-eligible-host synthesis today (that
path always correctly chooses REJECTION - there genuinely is no seat to
accept into), but documented here since it's the first time this
project has ever had ground truth for what the *other* branch of this
same decision point looks like, and it sharpens confidence that
REJECTION really is the right choice: it's a structurally different
opcode/length, not a variant of ACCEPT the receiving client might
confuse for one.

**Step 13: Client → Host, resign burst (3 bytes x ~10 rapid copies, <1ms
apart) - REJECT branch only**
```
01                                          [1]  type
<own connID, 2 bytes>                       [2]
```
Fully understood - client's self-triggered reaction to the rejection.

### Summary of what's genuinely unexplained

| Bytes | Where | Status |
|---|---|---|
| `d8 f6 32 00` | `0x00` message, offset 4-7 | Never varies across any capture - likely a deterministic leaked pointer, unconfirmed |
| `e9 e9 7f`, `18 f7 32 00` (x2), `3f e4 d4 7f` | ack trailer | Never vary - genuinely fixed, meaning unknown |
| Ack trailer's two `XX` positions | ack trailer offsets 10/22 | Vary by role but not reproducibly (host: a6 or a8) |
| Ack trailer's two `YY` positions | ack trailer offsets 12/24 | Genuinely session-variable, best guess is a tick-count analog, unconfirmed |
| `22 07` | rejection payload | The actual "you're rejected" signal content - completely opaque, reproduced verbatim only |
| `00 00` at `0x26`'s bytes 2-3 | discovery reply header | Never observed non-zero, meaning unconfirmed |

Every other byte in the entire sequence - types, lengths, addresses,
`sin_zero`, seq semantics for the open/ack layer, heartbeat shape, resign
shape - is understood and explained.

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

**Update (2026-08-14/15): the departure-detection question this table
originally presented as entirely open is not - a working, if slower,
answer already shipped and is confirmed live.** The dual-channel idle
timeout (`idleTimeout` = 30s, checked every `reapEvery` = 10s -
`reapIdleSessions` in `lobby/main.go`) is wired all the way through to
`onClientGone`/scoped teardown/clean re-fill today, not just proposed -
confirmed end to end 2026-08-14/15 (a client dropped, was detected gone
~38s later, `onClientGone` correctly tore down the pair relay, and a
rejoin correctly re-paired on the same host). What's still genuinely
open is only the *faster* path below - `isHostResignNotice` would cut
that ~30-40s window down to near-instant for a clean quit, but the slow
path already means durability isn't blocked on it.

| Piece | Status |
|---|---|
| Scoped teardown (`onClientGone`, `closeSession`) | Built 2026-08-11, **wired and confirmed live 2026-08-14/15** via the dual-channel idle timeout below |
| Idempotent pair-relay creation (`ensurePairRelay`) | Already built and confirmed working 2026-08-10/11 |
| Departure signal actually in use today | Dual-channel idle timeout (`reapIdleSessions`, 30s) - **built, wired, and confirmed live 2026-08-14/15**, not just proposed |
| Faster departure signal | **Still proposed, not built**: `isHostResignNotice`, needs a reference capture before wiring to any action - see `docs/multi-peer-routing-design.md`'s 2026-08-15 durability discussion for a newer angle (using burst timing relative to pairing, not just shape, to distinguish a real resign from the known ~2-minute periodic false positive) |
| Proactive re-fill fallback | **Proposed**: `ensurePairRelayIfNeeded`. Same-client rejoin already confirmed working with zero new code (2026-08-12, reconfirmed 2026-08-14/15); only needed if a reference capture shows the host doesn't reliably re-announce for a genuinely *different* replacement client |

`isHostResignNotice` still shouldn't be wired to live teardown behavior
until its own reference capture lands - this doc's whole point is not
repeating 2026-08-11's mistake of acting on an assumption instead of
evidence. That caution just no longer means durability itself is
unsolved - it means the *fast path* specifically is still unsolved.

## N-host round-robin matchmaking (design)

**Status as of 2026-08-13**: implemented, including the "Selection
policy" subsection's real 3-tier priority (prefer a host already waiting
for a second player, then an empty host, then reject) -
`hostPool.selectLocked` in `lobby/main.go`, called from
`assignForClient`. Also done: per-host scoping (every place below that
used to assume one global host now threads a `*hostCandidate` through),
dynamic pod discovery via the Kubernetes API (`podLister`, replacing the
`cfg.discoveryBackendAddr`/`cfg.sessionBackendAddr` static-config
sentence below, which is now stale), self-hosting pods (`auto-host.sh`),
and session-port stickiness (pulled forward from its own section below -
turned out to be a correctness requirement, not a later nice-to-have).
Full writeup of what shipped and why: `docs/multi-peer-routing-design.md`'s
"N-host round-robin matchmaking (2026-08-13)" section. The
claim/reservation mechanism this section originally called for turned
out to be unnecessary - **resolved by construction, see "The race this
needs to guard against" below for why**. Case 3's
`synthesizeLobbyFullRejection` - stale note corrected 2026-08-22: this
paragraph is from before the fact and was never updated - see "Case 3 in
detail" below for the real status (implemented 2026-08-15, promoted to
always-on production behavior 2026-08-18).

Everything above assumes a single match (one `aom-headless` backend, one
pair relay). This section is the other half of CLAUDE.md's Architecture
intention - "the lobby decides whether to route [a client] into an
existing open... match or provision a brand new `aom-headless` pod on
demand" - specifically the *decision* of which of several candidate
hosts a newly-arriving client gets matched into. ~~Nothing here is
implemented; `cfg.discoveryBackendAddr`/`cfg.sessionBackendAddr` are
still single, static, env-set addresses today (see
`deploy-minikube.sh`).~~ Superseded - see the status note above.

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

> **TODO (2026-08-12): capture the genuine rejection.** ~~Not done yet -
> deferred to a later session.~~ **Done 2026-08-15** - see the confirmed
> finding below.

**Confirmed 2026-08-15** (`run-aom-verbose-lobby-full.sh`, host with both
real-player slots AI-filled, one real joiner Direct-Connecting against
the already-full game - non-proxied, no `aom-lobby` involved, same
technique as every other reference capture in this project). The joiner
visually showed **"Error Joining: Host game is full"** - the host's own
game window showed nothing at all, no chat line or notification, which
matters for implementation: the rejection is a pure client-side UI
event triggered by one packet, not something that also depends on
replicating any host-side state change. The wire sequence leading up to
it (`archiving/sessions/20260815-*-lobby-full-rejection/`, session-port
2300 throughout, all payloads via `tshark -e data.data`):

```
0. (port 2299, the preceding ~27s) joiner broadcasts 0x25 every ~340ms,
   host replies 0x26 - decoded the embedded UTF-16LE name field for the
   first time (2026-08-18): the game/host name is "host's Game" (24
   declared bytes = 12 UTF-16 chars incl. null terminator). This is the
   joiner sitting on the LAN-browse screen before clicking Join - not
   part of the rejection sequence itself, but establishes what "the
   game" is called throughout everything below.
1. host <-> joiner: 0x00 self/peer open (both directions, standard) -
   host opens first, unprompted (see "Next investigation" section above
   for why this direction matters)
2. host <-> joiner: 0x02 handshake ack (both directions, standard)
3. host -> joiner: 03-wrapped player-name broadcast, sent THREE times
   while retrying (corrected 2026-08-18 - previously miscounted as two):
   seq=0 (27.770813), seq=1 (28.016447), seq=0 AGAIN (28.187130) - not a
   clean monotonic counter, host resending because it hasn't heard the
   joiner's own name back yet. Decoded payload identical all three
   times: "paullovesjade" (14-byte declared length = 13 chars + null
   terminator, 2 trailing zero bytes). **Cross-validated against both
   the host-side and joiner-side captures independently - byte-for-byte
   identical packet counts on both sides, so this is a complete
   accounting, not just what one capture point happened to see.**
4. joiner -> host: 22-byte extended 07ff-shaped message, matching this
   doc's own confirmed Ready-toggle signature (see the packet reference
   table above - packet-length frequency analysis independently flagged
   22 bytes as the Ready-toggle outlier size). If genuine, the joiner's
   client pressed Ready before its own name round-trip had even
   finished - plausible for the scripted test that produced this
   capture, not yet confirmed as general client behavior.
5. joiner -> host: its own 03-wrapped name broadcast, seq 0 - "paullovesjade"
   again (matches the host's own string exactly - see "Next
   investigation" section's 2026-08-18 update on why this identical-
   string collision turned out to be harmless, not the rejection's
   cause). Then, 173 MICROSECONDS later, a second copy at seq 1: same
   name, but with a DIFFERENT 2-byte trailer (00 01 instead of seq 0's
   00 00) - newly decoded 2026-08-18, not previously noted. Best guess
   is this trailer byte encodes a ready-state flag given step 4's
   timing, but unconfirmed - only one sample.
6. host -> joiner, ~11ms after the joiner's second name-broadcast, with
   seq=2 - the next number after the host's own prior name-broadcast
   usage (0, 1, 0 - see step 3) - not an independent/fixed value
   (23-byte payload total): 03 00 02 00 6f11 0d00
   22 07 00 00 00 00 00 00 00 00 00 00 00 00 00
   ^ wrapper (type, seq=2, host's own connID 6f11), then the same
   length-prefix pattern the name-broadcast uses (0d00 = 13), then a
   13-byte field starting 22 07 followed by 11 zero bytes, then 2 more
   trailing zero bytes beyond the declared length (same "declared
   length doesn't account for every trailing byte" pattern already
   flagged elsewhere in this project, e.g. the 0x29 message's own
   trailer). Checked every reasonable decoding (ASCII, little/big-endian
   uint16) - **nothing readable, no plain-English text anywhere**; every
   byte past the first two is zero. `22 07` (decimal 34, 7) doesn't match
   a Win32/DirectPlay error code confidently identifiable without
   further reference - treat as an opaque status/error code to be
   reproduced exactly, not one that needs to be understood to implement
   correctly. This is the exact, unambiguous trigger message - nothing
   else happens between the joiner's own name landing and this arriving.
7. joiner -> host, ~4ms later: ten copies of 01 e7f5 (joiner's own
   connID) - the standard isResignBurst shape, self-triggered by the
   joiner's own client upon receiving step 6, not a host-initiated kick.
   Immediately after (confirmed 2026-08-18, was not previously traced
   this far): the joiner resumes 0x25 discovery broadcasts, i.e. fully
   exits back to the LAN-browse screen rather than staying on any kind
   of error dialog that blocks further action.
```

No map-name or player-roster broadcasts appear anywhere in this capture
- the session is rejected immediately after the second name exchange
(step 6), before ever reaching a stage where that data would be sent.
That would require a capture of a genuinely successful join that
proceeds further, e.g. `archiving/sessions/20260812-215320-readyup-resign-quit-test/`.

The critical, not-yet-resolved implementation question this raises:
**the rejection arrives late in a real, multi-step handshake, not as an
immediate bare reply** - steps 1-4 look identical to the start of a
normal, successful join. Unknown whether the joiner's client requires
that preceding exchange to reach a state where it can correctly process
a standalone rejection, or whether `aom-lobby` could send just step 5 in
isolation (as an immediate reply to the client's own session-open) and
get the same result. **Test this specifically before shipping** -
sending step 5 alone against a description of "no eligible host" is a
much smaller amount of new proxy logic than synthesizing steps 1-4 too,
so it's worth knowing which is actually required rather than assuming
the fuller sequence is necessary.

**Implemented 2026-08-15** (`synthesizeLobbyFullRejection` +
`synthesizeEmptyPoolDiscoveryReply` in `lobby/main.go`), originally
gated behind a `SIMULATE_FULL_LOBBY=1` env var to keep it test-only
while unvalidated. **Promoted to real, always-on production behavior
2026-08-18** (the env var and `config.simulateFullLobby` no longer
exist at all - removed, not just defaulted differently): any client
hitting `forwardToBackend`'s "no eligible host" branch (empty pool, or
every host full) now unconditionally gets this synthesized sequence,
same as the flag-gated version did when explicitly enabled.

One correction caught while implementing: `connID` in the rejection
packet is **not** the client's own learned conn-ID the way the
new-peer-broadcast watchdog's `sess.connID` usage works. A wrapper's
conn-ID identifies its *sender* - the real capture's rejection used the
host's own connID (`6f11`), not the joiner's (`e7f5`) - and `aom-lobby`
is the sender here, impersonating a host it has no real backend for. So
this is `fakeHostConnID`, an arbitrary self-chosen value `aom-lobby`
invents and stays consistent about, not something learned from anywhere.

**Update (2026-08-15/16, tested live against a real client)**: the
"standalone vs. needs preamble" question above is now settled -
**rejection-only does not work**, confirmed live (client sat in
"Attempting to Connect" indefinitely). Built the full reactive version
instead (`simulateSessionPacket`/`simulatedClientState` in
`lobby/main.go`): replies to the client's own `0x00` self/peer-open,
`07ff` heartbeat, `0x02` ack, and name-broadcast as each one actually
arrives, rather than firing a fixed timer-driven script - a first,
non-reactive attempt was tried and also failed. Two real bugs found and
fixed along the way (both still live in code, not reverted): the
`0x20`/`0x21` liveness-ping reply was missing entirely (the code
answered every discovery-port query type with the same `0x26` shape,
which a real host never does), and `synthesizedSockaddr`'s `sin_zero`
was zeroed instead of reusing the real captured non-zero pattern.

**Still doesn't work, even with all of the above** - confirmed live: a
byte-for-byte-verified-identical `0x00`/`07ff` exchange (diffed directly
against the reference capture, see "Next investigation" immediately
below) still never gets the client to send its own `0x02`. See the next
section for why, and the plan for actually resolving it.

## Next investigation: classic DirectPlay's hidden session-establishment layer

> **Current status (end of 2026-08-19 session) - read this first.**
> **The mystery this section originally described is resolved.** Root
> cause: `aom-lobby`'s synthesized name-broadcast sent `"TheIP"` instead
> of `"paullovesjade"` - see this doc's own Phase 2 "Step 10" fix history
> and `simulatedFullLobbyCrackSignature`'s doc comment in `lobby/main.go`
> for the full account. That one field was the actual blocker behind
> everything the six 2026-08-18 fixes (stale-connID, seq-numbering,
> live-tick-byte, `sin_zero` echo, tick-pacing, proactive
> heartbeat-bundling) correctly fixed but didn't resolve - a real client
> now completes the full no-eligible-host sequence (open, ack,
> name-broadcast exchange, rejection, resign) exactly like a real host's,
> live-tested and confirmed 2026-08-19.
>
> **A related but genuinely different mystery is now open instead**: the
> real client<->client P2P pairing path (`pairRelay`, not this
> no-eligible-host synthesis) - two real clients matched on the same real
> host correctly learn each other's relay address and both send their own
> peer-open, but neither ever acks the other's, indefinitely. Full
> writeup, evidence, and the two smaller bugs found along the way in
> `docs/multi-peer-routing-design.md`'s "Update (2026-08-19)" section -
> that's the one to read next if picking this up, not the byte-level
> reference below (which is now fully resolved and kept as historical
> record/reference, same convention as this doc's other closed
> investigations).

**Status: resolved 2026-08-19 - see the status box above.** The
narrative below is kept as the historical investigation record (same
convention this project's docs use elsewhere), not rewritten after the
fact.

**Update (2026-08-18, two changes, read both):**

1. **The synthesized "no eligible host" path was promoted from a
   `SIMULATE_FULL_LOBBY=1`-gated test feature to real, always-on
   production behavior** (see "Case 3 in detail" above) - any client
   that finds an empty or fully-full pool now hits this code path for
   real, not just during deliberate testing.
2. **Separately, the real *backend-routing* path (a genuine
   `aom-headless` pod actually exists) was confirmed working end-to-end**
   - a real client Direct-Connected through `aom-lobby`'s actual
   relay/rewrite logic (a real pod as backend, `run-aom-spoofed-client.sh`
   as the joiner) and the session ran healthily for minutes (sustained
   `07ff`/`03 00`-wrapped traffic both directions, `GET /waiting` true,
   `GET /full` false throughout -
   `archiving/sessions/20260818-*-real-host-joiner-namecheck/`).

**Net effect on priority: back up, not down.** These two updates pull
in opposite directions and it's important not to conflate them. #2 is
good news for the case a real backend exists. But #1 means this
section's still-open mystery - a real client's session-port handshake
against the synthesized host stalls indefinitely, never sending its own
`0x02` - now affects genuine production traffic (a real player hitting
a momentarily-empty or fully-booked cluster), not just deliberate
`SIMULATE_FULL_LOBBY` testing. A real client hitting "no eligible host"
today gets a synthesized sequence that's confirmed NOT to complete.

Also resolved as part of today's real-capture work: an incorrect
name-collision hypothesis (both host and joiner broadcasting the
identical string "paullovesjade") was raised and then disproven by this
same real test - both sides sent that identical string and the session
connected and ran fine regardless. That string is apparently baked into
this cracked build itself (not a configurable per-player nickname - every
copy of this crack broadcasts it), harmless to have collide. This was
never actually a documented hypothesis in this file - worth recording
here only so a future session doesn't need to re-investigate it. The
2026-08-15 "lobby full" rejection this doc's "Case 3 in detail" section
documents remains correctly attributed to genuine capacity (host with
both real-player slots deliberately AI-filled before the one real joiner
connected), not name collision - that was already right.

**Narrower than it first looks - don't over-read "harmless to have
collide" as "the value doesn't matter."** Confirmed 2026-08-19: it's
specifically the *collision* (two peers both sending it) that's
harmless, not the field in general - sending a *different* value here
entirely (as this synthesis did for a long time, `"TheIP"`) silently
broke the client's willingness to send its own name-broadcast back. See
this doc's Step 10 in the canonical byte-level reference above for the
fix and the CD-key-check theory of what this field actually is.

**Also corrected 2026-08-18: the `0x02` ack trailer's byte offsets 12/24
are NOT a fixed constant** - see `synthesizedHandshakeAckHostTrailer`'s
doc comment in `lobby/main.go` for the full, now-4-sample analysis. A
2026-08-17 fix that hardcoded these to `0x02` (reasoning: two captures
agreeing meant it fixed a transcription bug) was itself wrong - a third
and fourth independent sample (2026-08-15's capture, and TWO more from
today's real host+joiner session) show this byte genuinely differs
*between* sessions (`0x01`, `0x02`, `0x01`) while staying identical
between host and joiner *within* one session. This is very likely the
"hidden timestamp/tick count" field step 3 below hypothesized - probably
worth deriving live once this investigation resumes, rather than
hardcoding. Reverted to `0x01` (an honestly-flagged best guess, not a
confirmed value) rather than left wrong.

**Update (2026-08-18, later the same day): two more real bugs found and
fixed, and the symptom itself has changed - read this before the older
"Everything above" paragraph below, which now describes a state that no
longer reproduces.**

1. **Seq-numbering bug, found via the rejection-path decode.** Cross-
   validating the 2026-08-15 capture byte-for-byte (see "Case 3 in
   detail" above) showed a real host's `03`-wrapped `seq` field genuinely
   increments/varies across retries and the rejection's `seq` continues
   from wherever the host's own name-broadcasts left off - `aom-lobby`'s
   synthesis was sending a frozen `seq=0` for every retry and a hardcoded
   `seq=1` for the rejection. Fixed: `simulatedClientState` now tracks a
   running `nextNameBroadcastSeq`, shared between the proactive retry
   loop and the final name-broadcast+rejection send (`resetIfStaleConnID`
   and the `runSimulatedNameBroadcastLoop` call sites in `lobby/main.go`).
2. **Stale-connID bug, found testing the newly-always-on path back to
   back.** Classic DirectPlay's session port is always 2300 on both
   sides, so `simulatedClients`' map key is IP-derived only
   (`"ip:2300"`) - a genuinely new connection attempt from the same IP
   (a real client reconnecting, or a fresh test container getting the
   same Docker-assigned IP a prior one had) reused the exact same key.
   The old `simulatedClientState` kept echoing the PRIOR session's
   connID back to the new client indefinitely, which a real client's own
   validation would have every reason to reject - this was silently
   corrupting some fraction of test runs and made the deeper mystery
   look worse than it is. Fixed: `resetIfStaleConnID` (`lobby/main.go`)
   detects a connID mismatch on an already-known client key and resets
   the per-client state fresh rather than reusing it.

**With both fixes in place, the symptom has narrowed.** A clean retest
(`archiving/sessions/20260818-*-no-flag-clean-retest/`) showed, for the
first time, the full connection-open-through-ack layer completing
correctly with consistent identity: the client retried its own `0x00`
for a long time, then sent its own `0x02` ack correctly referencing
`aom-lobby`'s connID, which `aom-lobby` correctly acked in turn. **The
client then stops retrying `0x00` entirely and settles into
heartbeat-only traffic - it never sends its own name-broadcast**, even
though `aom-lobby` is proactively sending 20 name-broadcasts at it. So
the open question is no longer "does the connection-open layer work at
all" (it does, now that identity is tracked correctly) - it's narrowed
specifically to the name-broadcast layer: the client accepts our
synthesized host well enough to complete a full open+ack round trip,
then goes quiet at the very next layer.

**Decision point (2026-08-18): the packet-level/strace approach has been
worked hard today (this whole file, plus a dedicated strace session) and
has not found the mechanism, only ruled things out one at a time.**

**Update (2026-08-18, later still): the different approach paid off -
official spec cross-reference explains the one remaining discrepancy an
exhaustive real-vs-synthesized diff found.** Re-checking every message
type, order, and byte our synthesis sends against two independent real
host captures (source/dest ports, an unfiltered every-protocol capture
of the critical window, and a full byte-for-byte content diff) found
nothing missing and nothing wrong **except** the `0x02` ack trailer's
byte offsets 12/24 (see `synthesizedHandshakeAckHostTrailer`'s doc
comment above) - already known to vary session-to-session, but not
previously explained.

**`[MC-DPL8R]` (the official DirectPlay 8 Reliable spec,
`docs/directplay8-reference/`) explains it directly**: §2.2.1.1-2.2.1.5
documents a `tTimestamp` field - "the sender's computer system tick
count, in millisecond units" - present in **every** connection-
establishment CFRAME (`CONNECT`, `CONNECTED`, `CONNECTED_SIGNED`,
`HARD_DISCONNECT`, `SACK`). That's exactly the shape our bytes 12/24
exhibit: varies session to session, matches between two independent
processes (host and joiner) reading within ~90ms of each other -
consistent with both being live clock reads on the same underlying
kernel, not a session token either side invents. Classic DirectPlay
almost certainly carries its own version of this same concept (AoM
predates real DirectPlay 8 - see `docs/directplay8-packet-
classification.md`'s caveat - so the wire format differs, but the
design pattern very plausibly carries over, same as everything else
that document's spec-comparison work has found). This also retroactively
confirms the `d8 f6 32 00` bytes in the `0x00` message are genuinely a
fixed constant, not a tick-count field themselves - they've stayed
byte-identical across every capture ever taken, including sessions days
apart, which a live tick count never would.

**Implemented same day**: `livenessTickByte()` in `lobby/main.go`
computes a coarse single-byte slice of milliseconds elapsed since
`aom-lobby`'s own process started, replacing the frozen hardcoded value
at both byte positions (full-packet offsets 18 and 30) in every `0x02`
ack `synthesizeHandshakeAck` builds. The exact scale/formula real
classic DirectPlay uses is still unconfirmed (3 real samples, all small
values, not enough to fit a precise formula) - this is an honest best
effort at "live and plausible" rather than a proven-correct
reproduction, but strictly better than a value that's *always* identical
regardless of when the session happens, which is a value no genuine
sender could ever actually produce.

**Retested live same day - did not fix the mystery on its own.** Client
acked correctly (confirmed the byte really is live - `0x73`, not the
old frozen `0x01`), then sat in heartbeat-only traffic for its full
patience window and self-cancelled. This retest did produce one new,
useful confirmation though: **the client's own "give up" behavior is
directly observed on the wire for the first time** - exactly 119.9
seconds after its first packet (matching the ~2-minute timing model
above almost to the second), it sends the same `01 <connID>` resign-
burst shape a normal quit uses, self-triggered, not host-prompted. Not
the mystery's answer, but confirms the timing model precisely and shows
what a timeout actually looks like at the byte level, which nothing had
directly captured before.

**Update (2026-08-18, still later): found via a completely different
method - exhaustive real-vs-synthesized diff, then a fresh Direct-
Connect-only capture - what looks like the actual bug.**

First, an exhaustive re-check (every message type, order, an unfiltered
every-protocol capture of the critical window, source/dest ports, and a
full byte-for-byte content diff against two independent real hosts)
found nothing wrong beyond the tick-byte fix above. That ruled out
content as the remaining problem, which prompted a different kind of
test: a genuine Direct-Connect-only capture (every prior reference
capture this project had ever taken used LAN-browse instead - see
`archiving/sessions/20260818-*-directconnect-only-capture/NOTES.md` for
the full setup, including the broadcast-blocking iptables rules used to
force Direct-Connect specifically). That capture proved Direct-Connect
itself isn't the issue - a real client completes the full handshake
including its own name-broadcast against a **real** host that way - but
comparing its bytes against earlier LAN-browse captures caught something
no prior pass had checked: **`sin_zero` (the 8 bytes after every
embedded IP:port in a `sockaddr_in` block) is not a fixed or per-role
pattern at all.**

It's a per-participant identity tag each side generates once to
describe itself, and whoever else embeds that participant's address
later (host describing a joiner, or vice versa) MUST echo that
participant's own self-reported value back exactly - not substitute
their own. Confirmed with an exact 8-byte match: the joiner's own
`sin_zero` in its `0x20` ping (`18 c0 4e 06 20 c0 4e 06`) was echoed
byte-for-byte by the host's later `0x00` message when describing that
same joiner as "peer". The two original LAN-browse reference captures
(2026-08-15, 2026-08-17) showed self-block and peer-block `sin_zero` as
identical within one message and were misread as "a fixed pattern" -
in hindsight, host and joiner's own independently-generated tags just
happened to coincide in both of those sessions. `synthesizedSockaddr`
in `lobby/main.go` used one hardcoded pattern for every sockaddr block
regardless of whose address it described - meaning every synthesized
`0x00` this proxy ever sent told the client "here's your own address"
with a `sin_zero` value the client never actually reported, using our
own made-up pattern instead. A real client's own validation rejecting
this silently (while still acking the overall open, since that may be a
shallower check) is a strong, well-evidenced candidate for the whole
"connection-open completes but never progresses to name-broadcast"
symptom this section has chased all session.

**Implemented same day**: `onDiscoveryPingNoHost` now threads the
client's own `sin_zero` (extracted from its `0x20` ping, at payload
offset 13-21) through to `beginSimulatedHostOpen`/`synthesizeSelfPeerOpen`,
which echoes it back exactly in the "peer" sockaddr block instead of
using the same fixed pattern (`ourOwnSinZero`, still fine for blocks
describing this proxy's own address) for both.

**Retested live same day - inconclusive by coincidence, not disproof.**
Same symptom as every prior fix (ack completes, name-broadcast never
arrives), but checking the specific test client's own reported
`sin_zero` afterward found it happened to be byte-identical to
`ourOwnSinZero` already - meaning this particular retest didn't actually
exercise a case where the echoed value differs from what was already
being sent before the fix. The fix itself is verified correctly
implemented (offset math double-checked against raw bytes), just not
yet proven necessary or unnecessary by a real mismatched case.

**Update (2026-08-18, later still): timing, not content - the ack-to-
name-broadcast gap.** Decoding the client's own name-broadcast packet in
detail (see "Case 3 in detail" above for the field-by-field breakdown)
led to comparing its exact timestamp against the host's own preceding
messages across multiple real captures, which surfaced something no
byte-level diff could: **a real host consistently waits ~84-100ms
between sending its own ack and sending its own name-broadcast** -
confirmed independently in the 2026-08-17 capture (83.8ms) and the
2026-08-18 Direct-Connect capture (100.4ms). This proxy's own synthesis
sent its first name-broadcast **0.2ms** after its ack - roughly 500x
faster than either real sample, because `beginSimulatedNameBroadcast`
fires its retry goroutine synchronously the instant the ack is sent,
with no pacing before the first send (only between retries).

Working theory, prompted directly by the user recalling AoM's lobby-
state update loop runs at a leisurely ~2-5Hz (not continuously) while
resting in a lobby waiting for a match to start: two logically-distinct
messages arriving within microseconds of each other land in the exact
same processing cycle from the client's own perspective, rather than as
two separate events a real host's own ~100ms-paced sending would always
keep apart. **Implemented same day**: `runSimulatedNameBroadcastLoop`
now sleeps ~100ms *before* its first send too, not just between
retries - pacing every send to roughly one per real host tick from the
start, rather than bursting the ack and the first name-broadcast
together.

**Retested live same day - did not fix the mystery on its own**, same
symptom as every prior fix, but the fix itself is confirmed correctly
deployed (verified the ack-to-name-broadcast gap is now ~100.6ms live,
matching the real cadence almost exactly, vs. 0.2ms before).

**Update (2026-08-18, still later): one more gap in the same bundle.**
Re-examining the reference capture from the host's own vantage point
(rather than the joiner's) gave more precise timing: ack -> heartbeat is
~100ms, but heartbeat -> name-broadcast is only ~0.3ms - the real host
sends them as a bundle, heartbeat first, on the same tick, not name-
broadcast alone. This proxy's own `07ff` sending was (and still was
after the pacing fix above) purely reactive - only ever replying to a
client's own heartbeat, never sent unprompted. So at the exact tick
where a real host proactively announces "I'm alive, about to speak",
this one stayed silent on that signal specifically. **Implemented same
day**: `runSimulatedNameBroadcastLoop` now sends a proactive heartbeat
immediately before each name-broadcast (retry), matching the real
bundle's order and ~100ms cadence, not just on the first iteration -
the 2026-08-15 reference capture shows the host bundling a heartbeat
with more than one of its name-broadcast retries, not only the first.

**Retested live same day - did not fix the mystery**, same symptom as
every other fix tonight (confirmed the extra proactive heartbeats went
out - 500 total vs. 480 before - but the client still never sent its own
name-broadcast). That makes six confirmed, correctly-implemented,
live-tested fixes in one session (stale-connID, seq-numbering,
live-tick-byte, `sin_zero` echo, tick-pacing, heartbeat-bundling) with
no change to this specific symptom.

**Noted, not actioned (2026-08-18): a real IP-header difference exists,
but almost certainly isn't the cause.** Diffing raw IP/UDP headers
(never checked before this point - every prior comparison only looked at
application payload) between our synthesis's packets and a real host's
found the DF (Don't Fragment) bit set on every packet we send, unset on
every real host's packet - TTL matches (64/64), IP ID pattern differs
(ours strictly sequential, real host's showing gaps consistent with a
shared system-wide counter). Real, reproducible, confirmed - but not
pursued further: both fields are IP-layer only, stripped before delivery
to any application via standard `recvfrom`/`recvmsg`-style socket calls,
so AoM's own DirectPlay code has no plausible way to see either one.
More importantly: this exact `aom-lobby`/Kubernetes/Calico path has
already hosted genuine, complete real matches (join, ready-up, chat,
resign - see this doc's own Durability section and
`docs/multi-peer-routing-design.md`), which rules out anything
connectivity/infrastructure-level as a blocker regardless of the header
difference's cause. Left here as a known, real discrepancy worth fixing
for its own sake eventually, not as a live lead.

The paragraph below is the ORIGINAL pre-2026-08-18 framing and is now
stale (its own symptom - "never progresses to send its own 0x02 ack" -
no longer reproduces after the fixes above), kept for history rather
than deleted:

---

Everything above (`SIMULATE_FULL_LOBBY`, the reactive handshake) is
built and deployed but doesn't fully work: a real client reaches the
session port, exchanges byte-perfect `0x00`/`07ff` messages with
`aom-lobby` indefinitely, and never progresses to send its own `0x02`
ack - even though the exchange is confirmed identical to the working
reference capture at the message-content level. Comparing against the
official `[MC-DPL8CS]`/`[MC-DPL8R]` specs (`docs/directplay8-reference/`)
surfaced why this is plausible: real DirectPlay 8's reliable transport
has an entirely separate CFRAME-based session-establishment layer
(`CONNECT`/`CONNECTED`/`CONNECTED_SIGNED` - `dwSessID`, `bMsgID`,
`tTimestamp` fields) that must complete *before* any application-level
data flows - structurally a tier below every message this project has
ever reverse-engineered for classic DirectPlay. If classic DirectPlay
has an unreverse-engineered equivalent, that would explain everything
observed: content-correct application messages, still rejected by
whatever's underneath them.

**Reference for the working case**: `archiving/sessions/20260815-*-lobby-full-rejection/joiner-capture.pcap`
(and `host-capture.pcap`) - a genuine, real, non-proxied host+joiner
exchange, decoded in this doc's "Case 3 in detail" section above. This
is the baseline everything below should be diffed against. Also usable
as a second independent reference: `archiving/sessions/20260812-215320-readyup-resign-quit-test/`
(a real 2-client join, proxied through `aom-lobby`, from an earlier
session).

Plan, in priority order (cheapest/most-likely-to-pay-off first):

1. **Close the capture's blind spots.** Every capture so far, including
   the reference one, is port-filtered (`udp port 2299`/`2300`) and has
   no syscall-level visibility (Wine's `+dplay`/`+dplayx`/`+dpwsockx`
   channels are confirmed silent for this build). Re-capture with
   `tcpdump -i eth0` (or `-i any`, to also catch loopback) with **no
   port filter**, plus `strace -f -tt -o strace.log` attached to the
   wine process (`docker exec <container> pidof wine`, or a similar PID
   lookup) running in parallel. This is the only way to rule out a
   session-establishment step happening outside port 2300 entirely, or
   via local IPC that never touches the network.
2. **Capture two genuinely independent successful joins**, not one -
   run `run-aom-verbose-clients.sh` (host + 1 real joiner, no
   simulate-mode) twice, on separate container pairs, with real time
   between them. Diff the two captures field-by-field. Anything
   identical across *both* is a real constant; anything that differs is
   very likely session-specific data (a session ID, tick count, nonce)
   this project has been misreading as fixed because it was only ever
   checked against one or two examples (the `d8 f6 32 00` constant and
   `sin_zero`'s pattern are the two most suspect fields).
3. **Test the "hidden timestamp" hypothesis specifically** - the
   single most promising lead from the spec comparison. For each
   "unclear, seems constant" field (the 4 bytes after connID in `0x00`,
   `sin_zero`'s 8 bytes, the `0x02` ack's 34 trailing bytes), check
   whether reading it as a tick count/elapsed-time value produces
   numbers consistent with real elapsed wall-clock time since the game
   process started (cross-reference against step 1's `strace`
   timestamps and the container's own start time). A field whose value
   tracks wall-clock time is live, not constant, and needs to be
   computed at send time in `synthesizeSelfPeerOpen`/
   `synthesizeHandshakeAck`, not hardcoded the way it is today.
4. **Diff a failing no-eligible-host synthesis attempt against the
   enhanced successful capture**, both with the step-1 tooling running,
   aligned by wall-clock time rather than just packet content - look
   for any syscall present in the successful trace with no counterpart
   in the failing one, and any field value step 3 identifies as live
   that today's synthesis sends as fixed/wrong.
5. **Only then, update the synthesis code** - most likely outcome:
   `synthesizeSelfPeerOpen`/`synthesizeHandshakeAck` need a real,
   per-session live value (probably derived from `time.Now()` relative
   to when that client's fake session started) in whichever field step
   3/4 identify, not the current hardcoded constants. Redeploy and
   retest with the same live-client workflow used throughout this
   investigation (`run-aom-spoofed-client.sh` against `aom-lobby` with
   `aom-headless` scaled to 0 - no env var needed any more, this path is
   always-on as of 2026-08-18).

This is a real reverse-engineering project on its own - budget a full
session for it, not a quick follow-up alongside other work.

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

> **TODO (2026-08-14): consider a broadcast-shaped readiness probe.**
> Not designed or implemented - a raw idea to revisit. `hostProbe.probeOnce`
> already does something close to "send a spoofed packet to look for
> available games" - it sends the exact 9-byte `0x25` enumerate-hosts
> query straight to a specific backend's discovery port and checks for a
> `0x26` reply, i.e. exactly what a real client's LAN browse screen sends,
> just unicast to a known pod IP instead of broadcast to `255.255.255.255`.
> The open question worth revisiting: is there any readiness signal worth
> getting *specifically* from mimicking the real client's broadcast form
> (to `.255`) rather than today's direct unicast-to-known-pod-IP? Direct
> unicast is strictly more precise for probing one specific candidate (no
> need to guess who answers), so this isn't an obvious win - but flagged
> here rather than dismissed outright, since 2026-08-14's session-port
> probe removal (see `hostProbe`'s doc comment in `main.go`) is a reminder
> that "the real client's actual traffic shape" has caught real gaps a
> synthetic probe missed before. Also worth noting: as of 2026-08-14,
> `.255`/`255.255.255.255` broadcast to the discovery port is deliberately
> *dropped* at the iptables level for real client traffic (LAN-browse
> sanitization, see `deploy-minikube.sh`) - any probe sent from
> `aom-lobby` itself would need to originate from inside that same
> firewall boundary (it does - `aom-lobby` runs in the minikube node's own
> netns) to not just drop its own probe packets.

### The race this needs to guard against

**Resolved by construction, not by a claim/reservation mechanism -
confirmed 2026-08-13, see `hostPool.selectLocked`'s own doc comment in
`lobby/main.go`.** This section originally proposed a short-lived
claim/reservation for the concern below; that turned out to be
unnecessary once `selectForNewClient` became the real
`assignForClient`/`selectLocked`. `assignForClient` is only ever reached
via `sessionProxy.forwardToBackend`, itself only ever called from
`sessionProxy.run()`'s single sequential `ReadFromUDP` loop - no other
call site exists. Go processes that loop one packet at a time, and
`forwardToBackend` doesn't return until the new session is fully
inserted, so two different clients' assignment decisions can never
actually interleave - by the time a second client's packet is even read,
the first client's session already exists and `sessionCountForHost`
already reflects it. This invariant depends on `assignForClient` never
being called from anywhere else (e.g. a future second goroutine) - worth
remembering if that ever changes, since it's what makes a
claim/reservation mechanism unnecessary today rather than merely
un-implemented.

Original concern, kept for context: `hostProbe.probeOnce` only polls
every `probeInterval` (3s), so a host's *discovery-time* "waiting" state
can be stale relative to what a client's session-port connect later
sees. That's fine precisely because discovery-time selection
(`peekHost`) is deliberately non-committing (see its own doc comment) -
the real, sticky, race-free decision happens at session-connect-time via
`selectLocked` above, not at discovery time.

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
