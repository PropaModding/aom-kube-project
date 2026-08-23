# Lonely-player kick: spoofed chat notice + slot-targeted removal

**Status: design only, not yet implemented.** Written 2026-08-23.
Depends on a separate, independently-tracked feature for the actual
notice message: **`lobby/chat-injection-design.md`** (split out
2026-08-23 - was originally "Part 1" of this doc, moved out so chat
injection can be investigated/tested on its own track). That doc covers
the packet format, the still-open 2-byte trailer question, and
`synthesizeChatMessage`; it must reach a working, live-tested state
before this doc's kick-reason message can be sent safely. This doc now
covers only the kick mechanism and policy (formerly "Part 2").

## The kick itself

### The click mechanism already exists and is already calibrated

`docs/host-flow.md`'s own kick-icon coordinates, live-tested against a
**real connected client** already (not just AI cleanup):

- Slot 2 kick icon: `(37, 115)`
- Slot 3 kick icon: `(37, 143)`
- Confirmed: the kicked client's own game reports being disconnected;
  the host's own chat log system-announces
  `"<host>: <name> was kicked out of the room."`

Same `input-agent` `POST /click?x=&y=` primitive `triggerMatchStart`
already uses for the Ready crystal - no new pod-side capability needed,
just a new call site in `aom-lobby` choosing which of the two
coordinates to hit.

### The one real unknown: which slot is the lonely player in?

This project's own scope caps every host at exactly `maxRealClientsPerHost
= 2` real clients, and `docs/host-flow.md`'s own calibrated sequence
always starts both slots AI-filled-then-kicked-open in the same order
(slot 2 before slot 3) - so the *lobby layout itself* is always exactly
two fixed slots, not a dynamic N. That significantly narrows this to a
binary question (slot 2 or slot 3) rather than genuine slot discovery,
but **which specific slot a specific real client ended up in has not
been confirmed to reliably follow connection order** - not yet tested
whether a first-arriving client always lands in slot 2, or whether that
can vary (e.g. after an earlier occupant of slot 2 left and the *next*
arrival filled the now-open slot 2 vs. a still-Standard-AI slot 3).
**Needs a live test**: connect a lone client, VNC-screenshot the lobby,
confirm which slot they occupy, repeat after a slot-2 departure to see
if a fresh solo arrival is deterministic. Until confirmed, don't assume
"first real client always = slot 2."

### The policy question - explicitly not designed yet

*When* to trigger a kick is a pool-consolidation policy decision, not a
packet-mechanism one, and this doc deliberately doesn't answer it yet:

- How many hosts, each with exactly one lone (unpaired) real client,
  before consolidating? Any single such host, or only once it's clearly
  wasteful (e.g. 2+ simultaneously)?
- How long does a lone player get before being kicked - immediately once
  a second lonely-host exists, or some grace period in case a second
  real client is about to join *this* host specifically?
- Where does this logic live - `hostPool` (it already tracks every
  host's real session count) seems like the natural home, likely a new
  periodic sweep alongside `reapIdleSessions`, but this needs its own
  design pass once `chat-injection-design.md` is confirmed working and
  the slot-targeting question above is resolved.

### Teardown after the kick

Reuses existing, already-proven machinery - no new teardown logic
needed. Once the real client's own game reports being kicked and
disconnects, the existing session-cleanup path
(`backendToClient`'s read-error handling → `onSessionRemoved` →
`matchState.onClientGone`) fires exactly the same way any other
departure already does. The only new piece is triggering the click and
sending the notice chat message beforehand - not the cleanup after.

## Suggested build order

1. `chat-injection-design.md`'s welcome-message test - resolves the
   trailer question, proves the injection mechanism works at all,
   lowest risk.
2. Confirm slot-targeting (the live VNC test above) - independent of
   (1), can happen in parallel.
3. Only once both of those are confirmed: design the actual consolidation
   policy (the open question above) as its own follow-up pass, then wire
   the kick-reason message + slot-targeted click + reuse of existing
   teardown together.
