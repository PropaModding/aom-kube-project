# Lonely-player kick: spoofed chat notice + slot-targeted removal

**Status: implemented and live-verified, 2026-08-23/24.** Written
2026-08-23, policy specified by the user the same day (previously left
open below - see "History"). Depends on **`lobby/chat-injection-design.md`**
(implemented and live-verified) for `synthesizeChatMessage`, which this
feature reuses directly for the warning message.

Confirmed live: two hosts each stuck with exactly one real client for
over a minute get consolidated - the younger lonely host's client is
warned, kicked, torn down, and its reconnect lands on the *other*,
still-waiting host, confirmed by real peer-to-peer relay traffic between
the two survivors. See
`archiving/sessions/20260823-lonely-player-kick/` for the full evidence,
including a first attempt where the warn/kick/teardown mechanics all
worked but the kicked client's reconnect landed right back on the same
host it was kicked from - see "History" for the fix.

## The policy

A host counts as **lonely** once it's had exactly one real client for
`lonelyPlayerWaitThreshold` (1 minute) -
`hostCandidate.firstClientJoinedAt` doubles as the "lonely since" clock,
since it's already re-stamped exactly when a host goes from zero real
clients to its current one (`selectLocked`'s empty-tier branch,
`lobby/join-cushion-design.md`).

`sessionProxy.sweepLonelyHosts` checks every `lonelyPlayerSweepInterval`
(10s) for **two or more** currently-lonely hosts. When found, it acts on
the **youngest** one (the one that became lonely most recently, sorted
by `firstClientJoinedAt` descending) - the intent being to free that
player to reconnect and land on the *other*, longer-waiting host
instead, consolidating two half-full games into one real match.

**Correction (2026-08-24)**: this section previously claimed host
selection on reconnect was "still plain round-robin," not guaranteeing
the reconnect lands on the intended host. Wrong, and stale - re-checked
directly against `hostPool.selectLocked`: it already implements a
3-tier priority (prefer a host already waiting for a second player,
then empty, then reject) that's been in place since the original N-host
work (`docs/multi-peer-routing-design.md`'s own "N-host round-robin
matchmaking (2026-08-13)" section - `CLAUDE.md`'s summary of this had
drifted stale and got copied forward into this doc without
cross-checking the more detailed one). As long as the *other* lonely
host is still the only currently-waiting host when the kicked client
reconnects, `selectLocked` deterministically routes them there - not a
coin flip, confirmed by the 2-host live test. Round-robin only breaks
ties *within* the waiting tier if more than one host is waiting at once
(the genuine 3+-simultaneously-lonely case, still untested - see "Not
yet tested" below), never causes a fallback to an empty host while any
waiting one exists.

## The sequence, per kicked client (`warnAndKickLonelyPlayer`)

1. **Warn**: send a spoofed chat message (via `synthesizeChatMessage`,
   same as the welcome message) reading:

   > There is another game with a player waiting, Please connect to
   > TheIP again, you will be removed from this game in 15 seconds

   `TheIP` is sent as the literal placeholder text, not a substituted
   real address - per explicit request.

2. **Wait** `lonelyKickWarning` (15s).
3. **Re-check**: is the host *still* lonely, with the *same* client? If
   a second real client joined, or the original one already left, on
   its own during the wait - abort, nothing to kick. This is a real,
   deliberate re-check, not a formality: the 15s warning window is
   plenty of time for the situation to have resolved itself.
4. **Kick**: click *both* kick-icon slots, not just the occupied one -
   see "Slot targeting" below for why.
5. **Tear down explicitly**: `matchState.onClientGone` +
   `backendConn.Close()`, the same way `resign-burst-design.md` already
   established for this project - not relying on the in-game kick's own
   network effect alone to eventually surface as a read error.
   `sess.backendConn.Close()` also triggers `backendToClient`'s own
   read-error cleanup path, so this is the same teardown every other
   departure already goes through, just triggered explicitly rather
   than waited for.
6. **Clear the sticky host assignment**: `hostPool.clearAssignment(clientAddr.IP.String())`.
   Found live: without this, the kicked client's very next reconnect
   hit `assignForClient`'s own sticky per-IP cache (`hostPool.assigned` -
   deliberate, for normal reconnect continuity) and landed right back on
   the exact host it was just kicked from, silently defeating the entire
   point of this feature. See "History" for the full trace.

`hostCandidate.lonelyKickInProgress` guards against the sweep
re-triggering this whole sequence for a host that already has one
running (the sweep's own 10s interval is shorter than the ~15s+ warn-
then-kick sequence itself) - cleared on the way out regardless of
whether step 4 actually fired.

## Slot targeting: resolved by kicking both

Previously an open question (see "History"): which of the lobby's two
fixed slots (`maxRealClientsPerHost = 2`) does a lonely host's one real
client actually occupy? Resolved by sidestepping it entirely, per the
user's own suggestion - click **both** kick-icon coordinates:

- Slot 2 kick icon: `(37, 115)`
- Slot 3 kick icon: `(37, 143)`

Live-tested previously against a real connected client (see
`docs/host-flow.md`): the kicked client's own game reports being
disconnected, and the host's own chat log system-announces `"<host>:
<name> was kicked out of the room."` Since a lonely host's *other* slot
is guaranteed to be either empty or AI-filled (never a second real
client, by definition of "lonely"), clicking it too is harmless - it
either does nothing (empty) or removes/no-ops an AI - and this reliably
catches the real client regardless of which slot they're actually in,
with no slot-discovery mechanism needed at all.

Same `input-agent` `POST /click?x=&y=` primitive `triggerMatchStart`
already uses for the Ready crystal.

## Confirmed live, 2026-08-23/24

Verified end to end against real clients (two `aom-headless` pods, three
spoofed test clients): the sweep fired once two hosts had been lonely
for over a minute, correctly picked the *younger* one, the warning
message rendered, the kick removed the client from the lobby, teardown
was clean, and - after the sticky-assignment fix below - the
reconnecting client landed on the *other* lonely host and formed a real
match with it, confirmed by genuine peer-to-peer relay traffic between
the two survivors. See
`archiving/sessions/20260823-lonely-player-kick/` for the full trace.

## Not yet tested

- **3+ simultaneously-lonely hosts**: the sweep only ever acts on one
  pair per tick (the single youngest host) - untested whether repeated
  ticks correctly work through a larger backlog, or whether some other
  policy is needed once there are more than two.
- **Round-robin tie-breaking among 3+ simultaneously-waiting hosts** -
  see the correction above: `selectLocked` always prefers *some* waiting
  host over an empty one, but if more than one host is waiting at the
  same moment (3+ simultaneously-lonely hosts, not yet live-tested - see
  the bullet above), round-robin picks among them without targeting the
  specific one this sweep had in mind. Not necessarily a problem for
  this feature's actual goal (landing on *any* waiting host still
  consolidates two half-full games into one), but genuinely untested -
  the 2-host test that confirmed this feature only ever had one possible
  "other" host to land on.

## History

**First draft (2026-08-23)**: written with the policy/slot-targeting
questions explicitly open, pending further design. Same day, the user
specified the concrete policy directly: the 1-lonely-minute threshold,
"the younger of the 2", the exact warning message text, the 15s
countdown, and "might be easier to just kick both slots" - resolving
both open questions at once. Implemented directly against that
specification rather than a separate design pass.

**First live test (2026-08-23)**: warn/kick/teardown mechanics all
fired correctly against real clients - the younger of two lonely hosts
was correctly identified, warned, and kicked. But the kicked client's
own reconnect landed right back on the *same* host it was just kicked
from, and never successfully rejoined anyone. Root cause:
`hostPool.assigned`'s sticky per-client-IP cache (deliberate, for normal
reconnect continuity - `assignForClient`'s own doc comment) was never
cleared by the kick, so the client's next connection attempt hit the
cache and skipped `selectLocked` entirely. Fixed with
`hostPool.clearAssignment`, called from `warnAndKickLonelyPlayer` right
after teardown. Same day, also swapped the warning message's real
substituted address for the literal placeholder `"TheIP"`, per explicit
request.

**Second live test (2026-08-23/24, same session)**: with the fix, the
kicked client's reconnect landed on the *other* lonely host instead,
confirmed by genuine peer-to-peer relay traffic between the two
survivors. See "Confirmed live" above and
`archiving/sessions/20260823-lonely-player-kick/`.

**Third live test attempt (2026-08-24), scaling to 3 hosts / 5
clients**: surfaced a second, more general sticky-assignment bug,
unrelated to this feature's own mechanics. A client whose original host
later filled with two *other* real clients (not via this feature's own
kick - just normal matchmaking) got permanently stuck: `assignForClient`
only ever invalidated a sticky `hostPool.assigned` entry when the host
had been removed from the pool, never when it was simply full, so every
reconnect attempt kept landing back on the same full host and
idle-timing out - "game is full," with no way out, ever. Fixed by also
checking `!p.hostFull(h)` before honoring a sticky assignment, falling
through to a fresh `selectLocked` pick either way. See
`archiving/sessions/20260823-lonely-player-kick/`'s
`3-sticky-full-host-bug-evidence.log`.

**Correction (2026-08-24)**: while investigating host selection for the
3-host test, re-verified this doc's own "Known limitation" claim (host
selection "still plain round-robin") directly against
`hostPool.selectLocked` rather than trusting `CLAUDE.md`'s summary of
it - the claim was simply wrong and stale, carried forward from
`CLAUDE.md` without cross-checking `docs/multi-peer-routing-design.md`'s
more detailed (and correct) account. The real 3-tier selection priority
has been in place since the original N-host work, 2026-08-13. See "The
policy" section above and `CLAUDE.md`'s own correction note for the
full trail. The 3-host test itself was then interrupted by unrelated
resource pressure (3 concurrent `aom-headless` pods oversubscribing the
minikube VM's memory) before reaching a clean conclusion - still
pending a retry.
