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
- `k8s/` — Deployment/Service manifests for all of the above
- `deploy-minikube.sh` — builds/applies everything into a local minikube cluster
- `docs/directplay8-protocol.md` — reverse-engineered wire protocol findings
- `docs/lobby-status-api.md` — `aom-lobby`'s `GET /hosts`/`GET /waiting` status endpoints
- `docs/host-flow.md` — driving `aom-headless` from cold pod to hosted lobby via `xdotool`/VNC calibration
- `docs/multi-peer-routing-design.md` — design + implementation notes for routing a full 1v1 match (not just client↔host), implemented 2026-08-10 — see Architecture intention below
- `docs/directplay8-packet-classification.md` — classification reference cross-checking our reverse-engineered packets against the official DirectPlay 8 Open Specifications, plus a list of confirmed vs. still-open message types for the next capture
- `host-game-kube.sh` — automates hosting a game on `aom-headless` (EULA → menus → lobby, Players set to 3)
- `run-aom-spoofed-client.sh` — one "second PC" container for testing Direct-Connect against the cluster
- `run-aom-verbose-clients.sh` — two spoofed clients (host + 1 joiner) joining directly (no lobby/proxy), with full WINEDEBUG + in-container tcpdump byte capture, for diffing a genuinely successful connection against a failing proxied one
- `run-aom-verbose-3clients.sh` — same, but host + 2 joiners, for capturing client↔client peer-to-peer traffic specifically

## Goals
- Proxy UDP traffic on DirectPlay ports (2299/2300)
- Spoof packets so AoM lobby sees a valid host
- Run headlessly in a Kubernetes pod
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
- **Pods are per-match and on-demand**, not a pre-warmed fixed pool —
  created when a match is actually forming, torn down when it ends.
- **`aom-lobby` itself needs to route the actual UDP 2300 session traffic
  to the correct pod per client/match**, not just the single static
  backend (`SESSION_BACKEND_ADDR`) it's hardcoded to today. Open
  question, not yet designed: Quilkin was originally slated to own this
  (Token Router + Capture filters, per-match token, dynamic xDS/filesystem
  config) but was ripped out 2026-08-10 as dead weight — it never got
  past a parked, unused `replicas: 0` deployment, since the client↔client
  routing problem turned out to be the actually-blocking one (see the
  next section) and got solved directly inside `aom-lobby` instead (the
  `pairRelay`/dynamic-port pattern in `lobby/main.go`). The same
  per-match dynamic-port approach is the likely shape for multi-backend-pod
  routing too, once it's needed - no plan to reintroduce Quilkin.

This is a substantial step up from the first working version (single
always-on pod, static backend) — treat that version as the groundwork,
not the end state.

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
requirements for one player dropping mid-match without disturbing the
other — is written up in `docs/multi-peer-routing-design.md`. **Not yet
implemented** — next session should start there.

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