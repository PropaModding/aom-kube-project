# AoM Kube Project

## Recently closed (2026-08-23)
No open top-priority item as of this writing. Remaining backlog, none
currently prioritized: Voobly client support (see "Dual client-variant
support" below), **in-match** resign detection (see
`lobby/resign-burst-design.md`'s own "What's explicitly deferred"
section - pre-match is now handled, telling a genuine in-match resign
apart from the known periodic false positive is not), fast host→client
departure notice (`isHostResignNotice`, a separate, still-unbuilt signal
from the client→host `isResignBurst` work below, blocked on a
never-taken reference capture - see "Sessions are peer-to-peer" below),
on-demand pod provisioning (see "Architecture intention" below),
`host-health-probe-design.md`'s own "Explicitly out of scope" items
(readiness/pool-membership gating in particular), and
`session-cleanup-design.md`'s optional Phase 4.

- **`lobby/chat-injection-design.md`**: a real client landing in the
  lobby now gets a spoofed, personalized welcome chat message
  (`"Welcome, <name>!"`, falling back to an IP-based message if the name
  isn't learned in time) - the first feature in this project to
  synthesize new `0x03`-wrapped settings-sync content toward a real
  client rather than only detect/forward/drop it. Landing signal is the
  first backend→client `0x03` packet observed for the session
  (`session.connIDKnown`'s own transition), not `isCDKeyEcho` (too
  early, connection-layer only) or relay creation
  (`isNewPeerBroadcast`/`isExistingPeerBroadcast`, which misses a lone
  first arrival). Live-verified 2026-08-23, including a real bug caught
  along the way - the first attempt's `seq` value jumped 1000 ahead of
  the host's own real numbering and was silently never delivered
  (classic DirectPlay's reliable-UDP ordering plausibly buffered it
  forever waiting for packets that would never arrive) - fixed by
  continuing the host's own numbering (`hostSeq+1`) instead. Player-name
  personalization decodes a client's own player-announce message, based
  on a single real sample (riskier than chat's five-sample decode) but
  confirmed live against two independent real clients. Also resolves,
  for practical purposes, this doc's own long-open "final 2 trailer
  bytes" question from the chat-format decode: sent as `00 00`, rendered
  fine - the client doesn't reject a message over it. See that doc's own
  "Confirmed live"/"History" sections and
  `archiving/sessions/20260823-welcome-message-chat-injection/` for the
  evidence.
- **`lobby/resign-burst-design.md`**: a player leaving the lobby/ready-up
  screen before a match starts now tears down their session/pairRelay
  within ~100ms instead of the 30s idle timeout - `isResignBurst` has
  correctly detected this 3-byte wire signal since 2026-08-11 but was
  never safe to act on, since the identical shape is also used for an
  unrelated periodic message with no real departure behind it at all.
  Gated on `matchState.hasStarted()` (the known false positive only ever
  fires during active gameplay, never before match-start) rather than
  the silence-based approach originally tried and live-disproven for
  this specific case. Live-verified 2026-08-23, including a real bug
  caught along the way - tearing down synchronously raced ahead of the
  very packet it was reacting to, so the surviving peer never saw the
  departure - fixed with a short deferred-teardown window
  (`resignBurstDrain`). See that doc's own "Confirmed live" note and
  `archiving/sessions/20260823-resign-burst-pre-match/` for the evidence.
- **`lobby/join-cushion-design.md`**: closes the two-clients-connecting-
  close-together bug (one gets permanently stuck on "Attempting to
  Connect") found live 2026-08-22 while testing the items below. Root
  cause: the real host's own "new peer" broadcast to the first client,
  announcing the second, is built from a placeholder address
  (`0.0.0.0:0`) instead of the second client's real one - a race inside
  the host's own engine at session-port admission, confirmed insensitive
  to any pre-admission delay. Fixed not by waiting longer but by never
  letting a second client *reach* session-port admission until the first
  has genuinely finished connecting: `discoveryProxy` now makes the real,
  sticky matchmaking decision (not just a non-committing peek), and
  withholds the discovery reply entirely - silently, so the client just
  keeps retrying from its own LAN-lobby screen - until the first client's
  own CD-key-check echo (the one confirmed client→host round-trip in the
  whole handshake) is observed, plus a short cooldown after it. A flat
  pre-completion timer stays only as a fallback ceiling. Live-verified
  2026-08-23 - see that doc's own "Confirmed live" section, including two
  real bugs caught and fixed along the way (a host claimed at discovery
  time still looking "empty" to a concurrent second client's own ping,
  and a stale zombie test client winning a race after a lobby restart).
- **`lobby/session-cleanup-design.md`** (Phases 1-3): deleted a host pod
  mid-match under an actual formed, actively-relaying pair and confirmed
  all four gaps close correctly - immediate host-removal detection,
  immediate session close (not the 30-40s idle reaper), correct
  `pairRelay` teardown, and a clean reconnect onto the surviving host
  with no stuck reference to the deleted one. See that doc's "Confirmed
  live, 2026-08-22" section for the full trace. Phase 4 (bounding
  `hostPool.assigned` growth) remains optional/unimplemented, per its
  own section - not blocking.
- **`docs/host-health-probe-design.md`**: closes the `auto-host.sh`
  reliability gap found while testing the above - it intermittently
  misclicks partway through its sequence (landing on the wrong menu
  screen instead of reaching the hosted lobby) while still logging
  "done" and exiting 0, leaving a pod silently broken and permanently
  excluded from routing (`hostPool.selectLocked` already keeps this
  safe - it never receives real clients) but never recovered - `/hosts`
  just silently undercounts until someone notices and manually deletes
  it. Fixed with a new `host-health-agent` `DaemonSet` (one per node)
  that fires the same `0x25`/`0x26` discovery-port check `aom-lobby`'s
  own `hostProbe` already does against every node-local `aom-headless`
  pod, wired into each pod's own k8s `livenessProbe.exec` so Kubernetes
  restarts a persistently-stuck pod automatically (fails open on
  anything but an explicit "not hosting", so a health-agent hiccup can't
  mass-restart the whole pool). Live-verified end-to-end: paused (not
  killed) a healthy pod's game process, confirmed the agent detected it,
  confirmed Kubernetes' own `Unhealthy`/`Killing` events fired and
  restarted the container, and confirmed the pod ran a fresh
  `auto-host.sh` cycle and re-hosted successfully afterward. See that
  doc's "Confirmed live, 2026-08-22" section, including a first attempt
  that produced a restart for the *wrong* reason (killing the game
  process outright killed the container's own PID 1, not this design's
  probe) before being redone correctly.

## What this is
Kubernetes/Docker setup to modernise Age of Mythology (original) network hosting,
spoofing DirectPlay 8 packets to support modern multiplayer hosting environments.

## Stack
- Docker / docker-compose
- Bash scripts (run-aom-head.sh)

## Key files
- `dockerfile.k8s` — headless AoM game pod image (no GPU/host X11/PulseAudio)
- `dockerfile.lobby` — the Go lobby image
- `lobby/` — the Go lobby: fake matchmaking front-end (see Architecture)
- `input-agent/` — small Go HTTP server baked into `dockerfile.k8s`'s image, running alongside Xvfb/wine inside `aom-headless` itself; exposes `POST /click?x=&y=` (shells out to `xdotool`) so `aom-lobby` can trigger host-side clicks in reaction to live packet events (e.g. match-start once both real clients are ready) without needing k8s API/exec access of its own — see `docs/multi-peer-routing-design.md`'s "Ready-up automation" section
- `auto-host.sh` — baked into `dockerfile.k8s`, backgrounded by `entrypoint.sh` alongside `input-agent`; runs the EULA→menus→hosted-lobby click sequence *inside* each `aom-headless` pod itself on startup (no `kubectl exec` needed), so every pod self-hosts with zero external trigger — what makes `kubectl scale deployment/aom-headless --replicas=N` alone sufficient to add hosts, see "Architecture intention" below. Can intermittently misclick and get stuck on the wrong menu while still exiting 0 — see `docs/host-health-probe-design.md`, which now detects and recovers this automatically.
- `host-health-agent/` — small Go program, one per node (`k8s/host-health-agent-daemonset.yaml`, a `DaemonSet`, not baked into `aom-headless`'s own image); probes every node-local `aom-headless` pod's discovery port the same way `aom-lobby`'s own `hostProbe` does and exposes `GET /host-health?id=<pod name>`, which each pod's own k8s `livenessProbe.exec` (`healthcheck.sh`) calls into so Kubernetes can restart a pod whose `auto-host.sh` run got stuck. See `docs/host-health-probe-design.md`.
- `healthcheck.sh` — baked into `dockerfile.k8s`; the `aom-headless` Deployment's own `livenessProbe.exec` target, calling into `host-health-agent` above. Fails open (exit 0) on anything but an explicit "not hosting", so an unreachable health agent can't mass-restart the whole pool.
- `k8s/` — Deployment/Service/RBAC manifests for all of the above
- `k8s/lobby-rbac.yaml` — ServiceAccount/Role/RoleBinding granting `aom-lobby` read-only (`get`/`list`/`watch`) access to `Pods`, nothing else — what its dynamic host-discovery poller (`lobby/main.go`'s `podLister`) needs
- `k8s/host-health-agent-rbac.yaml` / `k8s/host-health-agent-daemonset.yaml` — same read-only Pod access as `lobby-rbac.yaml` above, granted to `host-health-agent` instead, filtered to pods on its own node (`spec.nodeName`, via the downward API)
- `k8s/aom-headless-netpol.yaml` — NetworkPolicy on `aom-headless`'s own egress (2026-08-14 LAN-sanitization work, see "LAN/broadcast sanitization" below for what it actually restricts — narrower than "just aom-lobby + DNS," corrected 2026-08-22) — requires a NetworkPolicy-enforcing CNI (`minikube start --cni=calico`); a non-enforcing CNI silently accepts and ignores it
- `deploy-minikube.sh` — builds/applies everything into a local minikube cluster
- `fix-client-isolation.sh` — reapplies just the client<->client isolation iptables rule (one of the three LAN-sanitization layers below) without a full redeploy; needed after a plain `minikube start` (e.g. post-sleep/crash recovery), since that alone doesn't reapply any of `deploy-minikube.sh`'s host-level iptables state
- `docs/directplay8-protocol.md` — reverse-engineered wire protocol findings
- `docs/lobby-status-api.md` — `aom-lobby`'s `GET /hosts`/`GET /waiting`/`GET /full` status endpoints
- `docs/host-flow.md` — driving `aom-headless` from cold pod to hosted lobby via `xdotool`/VNC calibration
- `docs/multi-peer-routing-design.md` — design + implementation notes for routing a full 1v1 match (not just client↔host, implemented 2026-08-10) and for N-host round-robin matchmaking (implemented 2026-08-13) — see Architecture intention below. Also has the full 2026-08-14→2026-08-20 investigation into a second real client intermittently never getting told about the first: **resolved 2026-08-20** — several packet detectors wrongly required the settings-sync wrapper's byte 1 to be `0x00`, a byte that was never actually constant, silently dropping genuine broadcasts whenever it wasn't (see docs/directplay8-protocol.md's "Common wrapper" correction). Not a host reliability issue and not a network bypass, despite both being seriously suspected along the way - the host was sending everything correctly the whole time. The same-session watchdog-synthesis workaround this investigation built (`startNewPeerWatchdog`) is removed as unnecessary now that detection is fixed.
- `docs/directplay8-packet-classification.md` — classification reference cross-checking our reverse-engineered packets against the official DirectPlay 8 Open Specifications, plus a list of confirmed vs. still-open message types for the next capture
- `lobby/packet-handling-design.md` — packet reference + durability design, companion to `main.go`. The no-eligible-host synthesis mystery and the P2P pairing mystery it once tracked are both resolved (see the `multi-peer-routing-design.md` entry above) - this doc's own packet-reference table has the byte-1 correction noted against every entry it affected.
- `lobby/session-cleanup-design.md` — session/host lifecycle cleanup design, four related gaps found live 2026-08-20 tracing the bug that stuck a client on a deleted host pod forever (`hostPool.assigned` never revalidated, a removed host's `pairRelay`/`hostProbe` goroutines never torn down, its sessions never proactively closed). Phases 1-3 implemented and live-verified 2026-08-22.
- `docs/host-health-probe-design.md` — design + implementation for `host-health-agent` (see above), which detects a silently-stuck `auto-host.sh` run and lets Kubernetes restart it automatically; implemented and live-verified 2026-08-22. Includes the packet-level evidence that the discovery exchange is client-initiated (no spontaneous host broadcast to passively observe).
- `lobby/chat-injection-design.md` — reverse-engineered lobby chat message wire format (five real samples decoded) and player-announce/name decode (one real sample), `synthesizeChatMessage`, `isPlayerAnnounce`/`playerAnnounceName`, and the spoofed welcome-message feature built on top of them. Implemented and live-verified 2026-08-23 — see that doc's own "Confirmed live"/"History" sections. Feeds `lobby/lonely-player-kick-design.md`.
- `lobby/lonely-player-kick-design.md` — design for auto-kicking a real client left alone at a host (no second real player), via a spoofed chat notice (now unblocked - see `chat-injection-design.md`, implemented) plus the already-calibrated `xdotool` kick coordinates from `docs/host-flow.md`. Design only, not yet implemented; slot-targeting and consolidation policy both still open.
- `lobby/join-cushion-design.md` — design + implementation fixing two real clients connecting within a few seconds of each other (one gets permanently stuck "Attempting to Connect"). Root cause: a real host-engine race sending the first client a broadcast about the second built from a placeholder address, firing at session-port admission regardless of any pre-admission delay. Fixed by gating at the discovery port instead — `aom-lobby` withholds the discovery reply silently until the first client's own CD-key-check echo confirms it's genuinely connected, plus a short cooldown. Implemented and live-verified 2026-08-23.
- `lobby/resign-burst-design.md` — design + implementation for near-instant teardown when a player leaves the lobby before a match starts (previously a 30s idle-timeout wait). `isResignBurst`'s own 3-byte wire signal is ambiguous with an unrelated periodic message (confirmed live 2026-08-11), so this gates on `matchState.hasStarted()` rather than the byte shape alone — a burst before match-start is acted on, after stays logged-only. In-match resign detection deliberately out of scope. Implemented and live-verified 2026-08-23, including a real teardown-races-the-packet bug caught and fixed along the way (`resignBurstDrain`).
- `host-game-kube.sh` — the original hosting automation (EULA → menus → lobby, Players set to 3), driven externally via `kubectl exec`; superseded for routine use by `auto-host.sh` above, kept for manual/debug re-runs against a specific pod
- `run-aom-spoofed-client.sh` — one "second PC" container for testing Direct-Connect against the cluster
- `run-aom-verbose-clients.sh` — two spoofed clients (host + 1 joiner) joining directly (no lobby/proxy), with full WINEDEBUG + in-container tcpdump byte capture, for diffing a genuinely successful connection against a failing proxied one
- `run-aom-verbose-3clients.sh` — same, but host + 2 joiners, for capturing client↔client peer-to-peer traffic specifically

## Goals
- Proxy UDP traffic on DirectPlay ports (2299/2300)
- Spoof packets so AoM lobby sees a valid host
- Run headlessly in a Kubernetes pod
- **Real players must never see each other's real IP address, directly or
  indirectly.** This is a direct design intention, not an incidental
  side effect of proxying — every address a client is ever told about
  (the host's, and each other's once a match has two real players) must
  be `aom-lobby`'s own address, never a real peer's. This is *why*
  `pairRelay` exists at all instead of just letting two matched clients
  exchange addresses directly once introduced (see "Sessions are
  peer-to-peer" below), and *why* the 2026-08-14 LAN-sanitization layer
  (NetworkPolicy + iptables isolation, see "LAN/broadcast sanitization"
  below) exists on top of the application-level rewriting: defense in
  depth, so a real player's IP is unreachable at the network layer even
  if some future bug leaked it at the application layer. Any fix to the
  client<->client pairing work must preserve this — routing two clients
  to each other's *real* addresses directly, even as a quick unblock,
  is off the table.
- **Support both AoM client variants players actually run**: the classic
  No-CD/DirectPlay client (`aomxnocd1.exe`, UDP 2299/2300 — everything
  reverse-engineered in `docs/directplay8-protocol.md` so far is this
  variant) *and* the Voobly-modernised client, which uses its own
  networking layer entirely separate from classic DirectPlay (reportedly
  UDP port 16000, single-port-multiplexed — **unconfirmed, not yet
  captured**; a `docker exec minikube tcpdump port 16000` check against
  every capture taken during the 2026-08-07 debugging session found zero
  matches, but that session only ever drove `aomxnocd1.exe`, not an
  actual Voobly client, so absence there doesn't confirm anything about
  Voobly itself). These need their own protocol capture/reverse-
  engineering pass — don't assume `aom-lobby`'s current DirectPlay
  rewrite logic (`lobby/main.go`) covers Voobly traffic at all.
  **Deprioritized 2026-08-22**: a real goal, but low priority — no
  capture exists, nothing else in this project depends on it, and
  everything confirmed working so far (N-host matchmaking, P2P pairing,
  lobby-full synthesis) is retail/No-CD only. Don't pick this up before
  the higher-priority items at the top of this file.

## Architecture intention

Players connect using AoM's **Direct-IP Connect** field only (typed at the
cluster's public address) — not LAN browsing. There is no pre-existing
"real" host to browse for: matches are provisioned on demand.

- **The Go lobby (`lobby/`) is the matchmaker.** It's the only thing
  clients ever discover/query directly on UDP 2299. It has no backing AoM
  process of its own — when a client Direct-Connects wanting to play, the
  lobby decides whether to route them into an existing open (not-yet-
  started) match or provision a brand new `aom-headless` pod on demand via
  the k8s API, then hands the client off to whichever pod is theirs.
- **Pods are per-match and on-demand.** Not yet a true provision-on-demand
  pool (pods still start via `kubectl scale`, not automatically spun up
  the instant a match is forming) — see `docs/multi-peer-routing-design.md`'s
  "N-host round-robin matchmaking (2026-08-13)" section for what's shipped
  vs. still open (on-demand provisioning specifically is still an open
  problem, flagged in `lobby/packet-handling-design.md`'s "Budget" section
  — a fresh pod's own startup sequence takes well over a minute, longer
  than a client's own 15s connect patience).
- **`aom-lobby` routes the actual UDP 2300 session traffic to the correct
  pod per client/match** — implemented 2026-08-13. Quilkin was originally
  slated to own this (Token Router + Capture filters, per-match token,
  dynamic xDS/filesystem config) but was ripped out 2026-08-10 as dead
  weight — it never got past a parked, unused `replicas: 0` deployment,
  since the client↔client routing problem turned out to be the actually-
  blocking one (see the next section) and got solved directly inside
  `aom-lobby` instead. Multi-backend-pod routing followed the same
  "solve it directly in `aom-lobby`, no Quilkin" pattern: `lobby/main.go`'s
  `hostPool`/`hostCandidate`/`podLister` — the lobby discovers every
  `aom-headless` pod dynamically via the Kubernetes API (read-only,
  `k8s/lobby-rbac.yaml`) and keeps a fully independent `pairRelay`/
  `matchState` per host, so `kubectl scale deployment/aom-headless
  --replicas=N` is the entire "add a host" operation. **What's still
  missing**: the actual selection *policy* (today it's plain round-robin,
  not the "prefer a host already waiting for a second player" priority
  `lobby/packet-handling-design.md` designs) - see that doc's "N-host
  round-robin matchmaking" section.

This is a substantial step up from the first working version (single
always-on pod, static backend) — treat that version as the groundwork,
not the end state.

### LAN/broadcast sanitization (2026-08-14) — the dev environment now actually enforces the Direct-IP-only architecture above

The "Direct-IP Connect only, not LAN browsing" claim at the top of this
section used to be true only by convention — nothing actually stopped a
client from seeing another host over LAN broadcast, or from reaching
another client directly, because the dev environment's docker bridge
network gives every container on it accidental adjacency a real
deployment would never have. Found and closed live: a pod's own
address-broadcast could reach a client container directly (bypassing
`aom-lobby`'s rewrite), and a client's LAN-browse screen could see
`aom-lobby` itself as a joinable game.

Fixed with three host/cluster-level layers, all reapplied idempotently
by `deploy-minikube.sh` on every run:
- **`k8s/aom-headless-netpol.yaml`** — a NetworkPolicy on `aom-headless`'s
  own egress. **Correction (2026-08-22, found while designing
  `docs/host-health-probe-design.md`)**: this used to be described here
  as locking egress down to "just `aom-lobby`'s address + DNS" - that's
  an oversimplification of what the rule actually does. It denies only
  the docker-bridge subnet the spoofed test-client containers live on
  (the real leak this policy exists to close) and re-allows `aom-lobby`'s
  specific node address within that denied range; egress to the rest of
  the cluster's own pod network is not restricted by this policy at all.
  That's why the `host-health-agent` `DaemonSet` (same doc) needed no
  policy change to reach every `aom-headless` pod's discovery port.
  Requires `minikube start --cni=calico`; the default bridge CNI silently
  ignores NetworkPolicy resources entirely.
- **Client↔client isolation** — a `DOCKER-USER` (physical host) iptables
  rule blocking direct traffic between client containers on the docker
  bridge, while still allowing anything to/from `aom-lobby`'s own address.
  Needs `sudo`, applied on the physical host.
- **Broadcast suppression** — an `INPUT` iptables rule *inside the
  minikube node container's own network namespace* (not the physical
  host's — `aom-lobby` runs `hostNetwork: true`, so that's where its
  actual listening socket lives) dropping discovery-port broadcasts.
  Two separate addresses needed blocking, not one: a real client's
  LAN-browse query goes to `255.255.255.255` (literal limited broadcast),
  not the subnet-directed broadcast address — confirmed live after a
  rule covering only the latter let real client traffic straight through.

Litmus test: open a real client's LAN/Direct-IP menu — the games list
must be empty (no `aom-lobby`/host visible via broadcast), while
Direct-Connect to the cluster's address must still work. See
`docs/multi-peer-routing-design.md`'s "Regression investigation
(2026-08-14)" section for the full writeup, including a real pairing
regression this same work surfaced and fixed (client↔client isolation
removed an accidental direct-reachability fallback that had been masking
a genuine gap in `aom-lobby`'s own peer-notification logic).

### Sessions are peer-to-peer, not host-relayed — the proxy has to route a whole match, not just client↔host

Confirmed 2026-08-08 (non-proxied 3-container capture, see
`docs/directplay8-protocol.md`'s "Sessions are genuinely peer-to-peer"
section): once two real clients are in the same match, they open a
**direct** UDP 2300 session with each other, bypassing the host entirely.
`aom-lobby` today only proxies client↔host, so it has no way to see or
relay that traffic at all — a second real client's connection currently
hangs forever even after the client↔host relay itself is fully working,
because neither client is ever told the other's real address.

Since this project only ever hosts 1v1s (host in Observer Mode + exactly
two real playing clients, see Goals above), the fix has a fixed, small
scope: never more than 3 real peers, 3 pairwise relationships (host↔A,
host↔B, A↔B). Full design — a dedicated relay port per pair, seeded by
rewriting a newly-decoded address-broadcast message, plus durability
requirements for one player dropping mid-match (correction here claude they should be able to drop from lobby, if they drop from the game we end the match) without disturbing the
other — is written up in `docs/multi-peer-routing-design.md`.
**Implemented 2026-08-10**, confirmed working live; as of 2026-08-13
each concurrent match (one per `aom-headless` pod) gets its own
independent relay/pairing, not just one global one — see "Architecture
intention" above.

**Resolved 2026-08-20** (was open as of 2026-08-19): two real clients
matched on the same real host correctly learned each other's relay
address but the peer-to-peer handshake between them never completed an
ack. Root cause found via a node-level packet capture: several packet
detectors (`isNewPeerBroadcast`, `isExistingPeerBroadcast`,
`isReadyToggle`, `wrapperConnID`, `wrapperSeq`) wrongly required the
settings-sync wrapper's byte 1 to be exactly `0x00` - not a real
constant, so genuine broadcasts were silently dropped whenever it wasn't.
A latent bug present since this project's first working session, not a
regression and not a host reliability issue - see
`docs/directplay8-protocol.md`'s "Common wrapper" correction and
`docs/multi-peer-routing-design.md`'s "Update (2026-08-20)" section for
the full trail. Fixing it live surfaced a second, unrelated issue in the
N-host pool's own host-removal cleanup - see
`lobby/session-cleanup-design.md`, the current top priority (top of this
file).

### Dual client-variant support (retail DirectPlay + Voobly)

**Low priority as of 2026-08-22** — see the Goals section above. Two
distinct AoM client variants need to work, and they don't speak the
same protocol — `aom-lobby` will eventually need to classify incoming
traffic and run two separate handling pipelines, not just one rewrite
path. Nothing here is blocking; revisit after the higher-priority items
at the top of this file.

**What's confirmed about the retail/No-CD client** (`aomxnocd1.exe`,
which is what every capture and protocol doc in this repo so far
reflects — see `docs/directplay8-protocol.md`): it is **UDP-only**, on
2299 (discovery/handshake) and 2300 (session), for the entire
lifecycle — discovery, Direct-Connect, the connection handshake, lobby
settings sync, and even in-lobby chat. This was checked exhaustively
during the 2026-08-07 debugging session: zero TCP packets across an
800-packet full-session capture, zero traffic on TCP 47624 specifically
(a commonly-cited legacy DirectPlay TCP port), confirmed independently
via three separate Wine `WINEDEBUG` traces (host, joiner, and a proxied
client) that never once call `connect()`/`accept()` on any TCP socket.
**Any future design should treat "retail = UDP 2299/2300 only" as
settled**, not re-open it without new packet evidence.

**What's unconfirmed about Voobly:** it's expected to use its own,
separately-networked protocol — plausibly a single multiplexed UDP port
(one commonly-cited value is 16000) that Voobly's client/injected layer
uses instead of classic DirectPlay, with the router keeping session
affinity via a `sourceIP:port → destination` map rather than DirectPlay's
own address-embedding-in-payload scheme. **None of this has been
captured or verified yet** — it needs its own reverse-engineering pass
(a Voobly-installed client, `run-aom-verbose-clients.sh`-style capture)
before any of it is implemented. Don't build routing logic against the
port/shape above as if it were confirmed.

**Unsettled hypothesis, proposed but not verified — retail client
connects via TCP 47624 with a DirectPlay8 "CoreEnumsession" handshake
signature, then hands off to a 2300-2400 TCP+UDP port range, distinct
from Voobly's UDP-16000 path, distinguishable by a Go DPI classifier
peeking at first-packet bytes/size before routing.** Recorded here
for traceability since it was seriously proposed, but as it applies to
the *retail* client, it directly contradicts everything confirmed above
via real packet captures (TCP 47624: zero traffic; any TCP at all: zero
across a full 800-packet session; fixed UDP 2300, not a 2300-2400 range).
Treat the retail half of this hypothesis as disproven, not merely
unconfirmed. The DPI-classifier *architecture pattern* (sniff first
bytes/packet size, fork to a per-variant pipeline) could still turn out
to be the right shape for distinguishing retail from Voobly if they ever
need to share a port — but that's independent of whether retail itself
uses TCP, which it doesn't.

**Rough shape for a future dual-pipeline `aom-lobby`** (design sketch,
not yet built): separate listeners per variant rather than one shared
port doing deep-packet-inspection classification — retail keeps its
current `hostNetwork` UDP 2299/2300 pipeline exactly as documented, and
Voobly gets its own independent UDP listener/session-map pipeline once
its real protocol is known. Only add payload-sniffing/DPI-based
classification if the two variants ever need to actually share a port;
don't build it preemptively.

## Don't touch
- (add anything you want Claude to leave alone)