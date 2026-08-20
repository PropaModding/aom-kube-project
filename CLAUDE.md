# AoM Kube Project

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
- `auto-host.sh` — baked into `dockerfile.k8s`, backgrounded by `entrypoint.sh` alongside `input-agent`; runs the EULA→menus→hosted-lobby click sequence *inside* each `aom-headless` pod itself on startup (no `kubectl exec` needed), so every pod self-hosts with zero external trigger — what makes `kubectl scale deployment/aom-headless --replicas=N` alone sufficient to add hosts, see "Architecture intention" below
- `k8s/` — Deployment/Service/RBAC manifests for all of the above
- `k8s/lobby-rbac.yaml` — ServiceAccount/Role/RoleBinding granting `aom-lobby` read-only (`get`/`list`/`watch`) access to `Pods`, nothing else — what its dynamic host-discovery poller (`lobby/main.go`'s `podLister`) needs
- `k8s/aom-headless-netpol.yaml` — NetworkPolicy locking down `aom-headless`'s own egress to just `aom-lobby`'s address + DNS (2026-08-14 LAN-sanitization work, see "LAN/broadcast sanitization" below) — requires a NetworkPolicy-enforcing CNI (`minikube start --cni=calico`); a non-enforcing CNI silently accepts and ignores it
- `deploy-minikube.sh` — builds/applies everything into a local minikube cluster
- `fix-client-isolation.sh` — reapplies just the client<->client isolation iptables rule (one of the three LAN-sanitization layers below) without a full redeploy; needed after a plain `minikube start` (e.g. post-sleep/crash recovery), since that alone doesn't reapply any of `deploy-minikube.sh`'s host-level iptables state
- `docs/directplay8-protocol.md` — reverse-engineered wire protocol findings
- `docs/lobby-status-api.md` — `aom-lobby`'s `GET /hosts`/`GET /waiting`/`GET /full` status endpoints
- `docs/host-flow.md` — driving `aom-headless` from cold pod to hosted lobby via `xdotool`/VNC calibration
- `docs/multi-peer-routing-design.md` — design + implementation notes for routing a full 1v1 match (not just client↔host, implemented 2026-08-10) and for N-host round-robin matchmaking (implemented 2026-08-13) — see Architecture intention below; also has a 2026-08-14 regression investigation + fix (a second real client intermittently never getting told about the first, `aom-lobby`'s new-peer-broadcast watchdog)
- `docs/directplay8-packet-classification.md` — classification reference cross-checking our reverse-engineered packets against the official DirectPlay 8 Open Specifications, plus a list of confirmed vs. still-open message types for the next capture
- `lobby/packet-handling-design.md` — **"Next investigation" section**: the no-eligible-host synthesis mystery this section used to describe (client never sends its own name-broadcast against `aom-lobby`'s synthesized host) **is resolved as of 2026-08-19** — root cause was the synthesized name-broadcast's content (`"TheIP"` instead of `"paullovesjade"`, the CD-key-check field, not a nickname — see `simulatedFullLobbyCrackSignature`'s doc comment in `lobby/main.go`); a real client now completes the full sequence (open, ack, name-broadcast exchange, rejection, resign) exactly like a real host's. **A different, genuinely open mystery replaces it**: the real client<->client P2P pairing path (`pairRelay`, used once a real `aom-headless` backend exists and two real clients are matched to it) — two real clients correctly learn each other's relay address and both send their own peer-open, but neither ever acks the other's, confirmed against 7,000+ packets over ~8 minutes with zero acks either direction. Content, delivery, and LAN-isolation config all confirmed correct; the gap is specifically that a byte-perfect received packet doesn't get acted on client-side, which needs client-side instrumentation to go further. Full writeup: `docs/multi-peer-routing-design.md`'s "Update (2026-08-19)" section. **Correction to an earlier claim**: this doc previously said the real backend-routing path was "confirmed working end-to-end, including full real matches" — that was true under the architecture as of the last actual commit (2026-08-12, single static host, no LAN isolation); the N-host pool + LAN-sanitization work built since then (2026-08-13/14) is entirely uncommitted, and under *that* environment, two real clients pairing has not been re-confirmed to complete — see the P2P mystery above
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
- **`k8s/aom-headless-netpol.yaml`** — a NetworkPolicy locking `aom-headless`'s
  own egress to just `aom-lobby`'s address + DNS. Requires
  `minikube start --cni=calico`; the default bridge CNI silently ignores
  NetworkPolicy resources entirely.
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

**Currently regressed, open as of 2026-08-19**: two real clients matched
on the same real host correctly learn each other's relay address (both
directions confirmed byte-perfect) but the peer-to-peer handshake
between them never completes an ack, indefinitely — see
`docs/multi-peer-routing-design.md`'s "Update (2026-08-19)" section for
the full investigation. Not a regression in this mechanism's own code
(confirmed byte-for-byte unchanged since the commit that first proved it
working) — the N-host pool and LAN-sanitization layers built since then
(2026-08-13/14) are both still entirely uncommitted, and the 2026-08-10
baseline's success may have depended partly on a direct-reachability
bypass those layers correctly closed.

### Dual client-variant support (retail DirectPlay + Voobly)

Two distinct AoM client variants need to work, and they don't speak the
same protocol — `aom-lobby` will eventually need to classify incoming
traffic and run two separate handling pipelines, not just one rewrite
path.

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