# Feature test plan (as of 2026-08-22)

A structured pass over every feature this project currently implements,
written after the 2026-08-20 byte-1 detection fix and N-host/LAN-
sanitization commit. Each section names what's being verified, the
concrete setup/steps, the expected result, and exactly how to check it -
reusing the specific tools and techniques this project has already
proven out (VNC/`vncdotool`, `tshark`/node-level `tcpdump`, the status
endpoints, live log greps) rather than inventing new ones.

**Environment reset, run once before any section below** (minikube was
found stopped writing this doc - a normal state after any gap in work,
not a bug):

```bash
minikube start                    # resumes the existing --cni=calico profile
./deploy-minikube.sh              # rebuilds+redeploys, reapplies iptables/NetworkPolicy
./fix-client-isolation.sh         # only needed if deploy-minikube.sh's own
                                   # sudo iptables step was skipped/failed
```

Sections are ordered roughly build-up: infra/discovery first, then
matchmaking, then the P2P mechanism the last session's work centered on,
then automation and durability. Each is independently runnable once the
environment reset above has happened once.

---

## A. Discovery-port rewriting (single client, single host)

**What**: the `0x25`/`0x26`/`0x20`/`0x21` discovery exchange correctly
substitutes `aom-lobby`'s own address for the real backend's.

**Setup**: one host pod up and self-hosted (`curl $(minikube ip):8080/hosts`
returns `1`).

**Steps**: launch one spoofed client (`run-aom-spoofed-client.sh`), open
its LAN/Direct-IP screen, type the cluster IP, do not connect yet.

**Expected**: the game list shows a joinable game at the address typed,
with the correct session name (`"TheIP's Game"` by default, or
`HOST_NICKNAME` if overridden).

**Verify**: `tshark -r <client-capture.pcap> -Y "udp.port==2299"` -
confirm every `0x26`/`0x21` reply's embedded `sockaddr_in` shows
`aom-lobby`'s public address, never the pod's real `10.244.x.x` address.
Cross-check against `aom-lobby`'s own log:
`grep '\[discovery\] reply type 0x26' <lobby-log>` shows
`rewritten=true`.

---

## B. Session-port client<->host relay

**What**: the `0x00`/`0x02` handshake's self/peer address rewriting is
correct in both directions.

**Setup**: one client, Direct-Connect and Join.

**Steps**: none beyond joining - this exercises automatically.

**Expected**: client reaches the in-lobby screen, sees the host's
nickname and the map/settings the host configured.

**Verify**: `grep '\[session\] rewriting' <lobby-log>` - confirm every
line shows `client-self`/`host-self` rewritten to `aom-lobby`'s public
address, and `peer` rewritten to the *other* real endpoint's real
address (host's real pod IP when writing to the client; the client's
real IP when writing to the backend). Full-match address-leak audit
(the technique that confirmed the reference session was leak-free on
2026-08-20): scan every payload the lobby sends the client for the real
host pod IP appearing anywhere, not just at the rewritten offset -
`python3` snippet in `docs/multi-peer-routing-design.md`'s "Update
(2026-08-20)" section shows the exact method.

---

## C. N-host pool matchmaking

**C1 - single empty host, first client**: scale to 1 host. First client
joins. Expect: routed to that host, `/waiting` flips `true`,
`/hosts` reads `1`.

**C2 - second client on an already-waiting host**: with C1's client still
connected, join a second client. Expect: routed to the *same* host (not
a second one), not a fresh host - confirms `selectLocked`'s "prefer
waiting" tier over "prefer empty."

**C3 - full pool, third client**: with two real clients on the one host
(and no second host scaled up), attempt a third client join. Expect:
synthesized "lobby full" rejection (see section E), not a hang or a
misrouted session onto the already-full host.

**C4 - two hosts, tier round-robin**: scale to 2 hosts (`kubectl scale
deployment/aom-headless --replicas=2`), both empty. Join two clients
from two *separate* matches (not intending to pair with each other -
simplest is one client each, joined far enough apart in time that
neither becomes the other's peer). Expect: round-robin across the two
empty hosts, not both landing on the same one.

**Verify (all of C1-C4)**: `curl $(minikube ip):8080/hosts`,
`/waiting`, `/full` before and after each step;
`grep '\[pool\]\|\[session\] new session' <lobby-log>` to see which
host each client's session actually attached to.

**C5 - sticky assignment across a same-client reconnect**: with a client
already assigned to a host, have it quit to menu and rejoin (same
container, same IP). Expect: lands back on the *same* host, not
re-round-robined elsewhere.

**C6 - stale assignment after host removal (currently fails - see
`lobby/session-cleanup-design.md`'s Gap 1)**: with a client assigned to
host A, delete host A's pod (`kubectl delete pod <host-A>`) while a
fresh host B exists. Have the same client reconnect. **Expected once
Phase 1 of the cleanup design ships**: routed to host B. **Actual
today**: the client's session-port traffic keeps dialing the now-dead
host A indefinitely (`dial udp ...: connect: invalid argument` repeating
in the log) until `aom-lobby` itself is restarted. Run this test to
confirm the bug is still present before Phase 1 lands, and to confirm
it's fixed once it does.

---

## D. Client<->client P2P pairing (the 2026-08-20 fix)

**What**: two real clients on the same host learn each other's relay
address, complete the open/ack/name-exchange/player-announce sequence,
and sustain healthy bidirectional traffic - the mechanism at the center
of the last session's entire investigation.

**Setup**: one fresh host pod (delete and let `auto-host.sh` re-run
uninterrupted - do not drive VNC manually mid-sequence, confirmed
today's session to cause misclicks under concurrent load). Two fresh
spoofed clients.

**Steps**: client A joins and waits in-lobby. Client B joins after.

**Expected**: both clients see each other in the player list, both can
send chat, and neither shows "Attempting to Connect" past the initial
handshake.

**Verify**:
- `grep 'rewrote.*broadcast\|\[pair\] new' <lobby-log>` - both
  `existing-peer broadcast` and `0x29 new-peer broadcast` should show as
  **rewritten by the reactive path directly** with no `watchdog` log
  line anywhere (that machinery was removed 2026-08-20 - if a watchdog
  line appears, something regressed on the detection fix).
- `grep '\[pair\] relaying' <lobby-log> | tail -50` - confirm sustained,
  ongoing traffic in both directions well past the initial handshake
  (30s+), not just the open/ack burst.
- Player-announce reciprocity check (the exact failure mode found
  2026-08-20 before the byte-1 fix): confirm **both** clients' own
  ~93-141 byte player-announce messages appear on the `[pair] relaying`
  channel, not just one side's.
- Address-leak audit, same technique as section B: scan every packet
  `aom-lobby` sends each real client (both the session channel and the
  pair-relay channel) for the *other* client's real IP appearing
  anywhere in the payload. Zero hits expected.

**D2 - client rejoin mid-match**: with a pair formed, have one client
quit to menu and rejoin (same container/IP). Expect: re-pairs cleanly,
`ensurePairRelay` reuses the existing relay port. Verify via `[pair] new
A<->B pair relay` appearing only once in the log, not a second time for
the rejoin.

---

## E. No-eligible-host synthesis ("lobby full" / empty pool)

**E1 - empty pool**: scale `aom-headless` to 0. Client attempts
Direct-Connect. Expect: client sees a rejection message in-game
("Attempting to Connect" resolves to an error, not an indefinite hang).

**E2 - full pool**: with a host already paired (two real clients),
attempt a third client's Direct-Connect. Expect: same synthesized
rejection sequence.

**Verify**: `grep 'no eligible host\|lobby-full rejection' <lobby-log>` -
confirm the full synthesized sequence fires (`name-broadcast` then
`lobby-full rejection`), and that the rejecting client's own resign
burst appears afterward, matching the documented real-capture reference
in `lobby/packet-handling-design.md`'s "Canonical byte-level reference"
section.

---

## F. Ready-up automation

**Setup**: a fully paired match (section D).

**Steps**: both real clients press Ready in-game.

**Expected**: the host's own Ready crystal gets clicked automatically
(no manual VNC/kubectl exec needed), and the match visibly starts on the
host's screen.

**Verify**: `grep 'ready-toggle\|triggerMatchStart\|input-agent' <lobby-log>`
- both clients' toggles should be detected within ~1s of each other,
`triggerMatchStart` should fire exactly once (not per-client), and the
`input-agent` click log line should show coordinates `(511, 89)`. Visual
confirmation via VNC screenshot of the host pod is the final check.

---

## G. Self-hosting automation (`auto-host.sh`)

**What**: a freshly-created `aom-headless` pod reaches a joinable,
Observer-Mode, 3-Players lobby with zero external trigger.

**Steps**: `kubectl delete pod -l app=aom-headless`, then poll
`/hosts` without touching VNC:

```bash
until [ "$(curl -s $(minikube ip):8080/hosts)" = "1" ]; do sleep 10; done
```

**Expected**: `/hosts` flips to `1` within ~2-3 minutes, unattended.

**Verify**: `kubectl logs <new-pod>` shows the full click sequence
completing (`EULA` through `kick slot 3 AI`) with no gaps; a VNC
screenshot at that point shows the in-lobby screen with `Players: 3`,
Observer Mode set, both non-host slots `Open`. **Known flakiness**:
today's session hit one run where the automated sequence silently
misclicked (landed on the wrong screen) despite reporting success in its
own log - if `/hosts` doesn't flip within the expected window, check a
VNC screenshot before assuming a code bug; a clean pod recreation
usually resolves it.

---

## H. LAN/broadcast sanitization

**H1 - NetworkPolicy egress lockdown**: `kubectl exec` into an
`aom-headless` pod, attempt to reach a client container's IP directly
(`wget --timeout=2 <client-ip>:<any-port>`). Expected: times out (not
"connection refused" - a refusal means the packet reached the client and
only the specific port was closed, which would mean the NetworkPolicy
isn't enforcing at all).

**H2 - client<->client iptables isolation**: from one spoofed-client
container, attempt to reach the other spoofed-client container's IP
directly. Expected: blocked (no route/timeout).

**H3 - broadcast suppression**: open a *third*, real (non-lobby-aware)
AoM client's LAN/Direct-IP browse screen on the same physical network
segment as the docker bridge (if available) or verify via `tcpdump`
that broadcast traffic to `255.255.255.255:2299` and the subnet-directed
broadcast address never reaches `aom-lobby`'s own listening socket.
Expected: the games list stays empty via LAN browse; Direct-Connect to
the cluster's own address still works.

**Verify all three**: this project's own litmus test, from CLAUDE.md's
"LAN/broadcast sanitization" section - real client's LAN/browse list
must be empty, Direct-Connect must still work.

---

## I. Durability / idle-session reaping

**I1 - clean client quit, single-channel**: with only a client<->host
session active (no pair formed yet), quit the client. Expect: session
removed from `aom-lobby`'s state within ~30-40s
(`idleTimeout`/`reapEvery`).

**I2 - paired match, one client goes idle on the host channel only**:
with a pair formed and actively exchanging P2P traffic, artificially
quiet just the host-facing channel if possible (or rely on the natural
case where P2P chatter continues during a slow lobby moment). Expect:
session is **not** reaped while `pairRelay.idleFor` shows recent
activity - this is the exact 2026-08-11 regression this dual-channel
check exists to prevent; a regression here would look like a live match
getting killed ~30s in for no reason.

**Verify**: `grep 'closed idle session\|resign-shaped burst' <lobby-log>`.
Confirm I1 reaps around the expected window; confirm I2 does *not* reap
while relay traffic is flowing.

---

## J. Status API

**Setup**: any of the states above.

**Steps**: `curl $(minikube ip):8080/hosts`, `/waiting`, `/full` at each
distinct pool/match state (empty pool, one empty host, one waiting host,
one full/paired host, two hosts mixed).

**Expected**: matches `docs/lobby-status-api.md`'s own definitions
exactly - `/full` true only once a host's pair relay has actually
formed (or its real session count hits 2), not merely "a client would be
welcome to browse in."

---

## K. Session/host lifecycle cleanup (not yet implemented - planned tests)

These are written against `lobby/session-cleanup-design.md`'s own
"Verifying the fix" section, listed here so this test plan stays
complete once that design ships. **All of K1-K4 are expected to fail or
be not-yet-applicable today** - re-run once Phases 1-3 of that design
are implemented.

**K1**: scale to 2 hosts, pair two real clients on one, delete that
host's pod mid-match. Expect (post-fix): affected clients' sessions
close within seconds, not the 30s idle window.

**K2**: same setup - confirm the `pairRelay` goroutine actually exits
(no longer present in a `pprof` goroutine dump, or a log line confirming
`relay.conn.Close()` fired).

**K3**: repeatedly scale a host count up and down several times: confirm
the `hostProbe` goroutine count doesn't grow across the churn (currently
leaks one per removed host, unconditionally).

**K4**: after K1's pod deletion, reconnect the same client IPs. Expect
(post-fix): routed to the surviving host. This is the same scenario as
section C's C6 - once Phase 1 ships, C6 and K4 should both pass.

---

## Not covered by this plan

- **Voobly client support** - no implementation exists yet to test.
- **Protocol reverse-engineering backlog** (unexplained bytes) - not
  feature behavior, tracked separately in
  `lobby/unexplained-bytes-reverse-engineering-plan.md`.
- **In-game gameplay-sync correctness** (actual match play once started)
  - this project's scope has never required a match to run past
  ready-up; the protocol for that layer is largely undecoded (see the
  reverse-engineering backlog doc's Tier 2).
