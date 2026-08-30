# NAT traversal lab: testing AoM's client-behind-NAT bug locally

**Status: infrastructure built (2026-08-29), not yet validated against
the real game.** The namespace and docker NAT topologies both exist and
should work mechanically (real iptables translation, not a lookalike),
but neither has actually been run yet, and the core hypothesis this lab
exists to test - the client-self-address theory from the AKS
investigation - is still unconfirmed. Written before further live
testing, matching this project's own practice of designing before
building for anything this size (see `lobby/backend-port-binding-
design.md`, `docs/host-health-probe-design.md`).

## Why this exists

The AKS migration's own stuck-handshake investigation (see `CLAUDE.md`'s
AKS section and its own extensive session history) spent a long stretch
ruling things out: proxy fidelity, addressing on both legs, the game
binary and Wine version (byte-identical between environments), packet
delivery (confirmed via both live capture and the backend's own socket-
level receive trace), and the ack's own byte content (confirmed
identical to acks from dozens of genuinely successful archived
sessions). What's left is narrower and specific: **the client's own
game process never sends its reciprocal ack for the host's open,
despite receiving it correctly** - and web research turned up a well-
documented, longstanding AoM community finding that matches closely:
classic AoM determines its own IP by reading its local network
interface directly, with no NAT awareness, and originally relied on
querying "the ESO Address Server" (Ensemble Studios Online) to learn
its real external address - a service that's been dead for years. The
client's own WINEDEBUG trace already shows exactly this: failed DNS
lookups for `eso.com`/`configx.aom.eso.com` at startup, every time.

Testing this needs a client that's **genuinely** behind NAT - not just
made to look that way.

## What didn't work, and why (important not to repeat)

An earlier attempt used two differently-numbered docker networks (one
with a "public-looking" subnet) and relied on the physical host's own
`DOCKER-USER` iptables chain to allow routing between them. This never
did real NAT at all - Docker's own inter-bridge routing (once allowed)
is a flat, directly-routed L3 space, not a translation boundary. The
client's own address never actually changed from anything's point of
view; two containers just had different-looking IP ranges while
directly routable to each other the whole time. That's structurally
incapable of reproducing the actual bug mechanism (a client that
genuinely only knows its own private address, with no way to learn its
real external one) - see the AKS investigation's own session history
for where this was tried and abandoned.

**The fix:** real translation, done by an actual NAT device (a
namespace or container running genuine iptables MASQUERADE/DNAT), not
address-range cosmetics.

## Two implementations, both built

### 1. Raw network namespaces (`setup.sh` / `teardown.sh`)

```
[client-ns] --veth--> [router-ns] --veth--> [internet-ns]
                (real iptables MASQUERADE here)
```

The minimal, most precisely-controllable version - three Linux network
namespaces connected by veth pairs, with genuine NAT translation
(`iptables -t nat -A POSTROUTING -j MASQUERADE`) running in the middle
one. `NAT_MODE=symmetric` switches to `--random-fully`, simulating the
specific NAT type the AoM community's own documented failure mode is
tied to (a genuinely different external port per destination, as
opposed to the endpoint-independent mapping most consumer routers
default to).

Good for: quickly validating the NAT mechanism itself, or testing
simple tools (`nc`, `ping`) through it. Needs root (namespace/veth/
iptables creation) - not something this session can run itself; hand
off to a real terminal with `sudo`.

**Limitation:** not practical for running the actual AoM container
through it. Getting the real `aks-test-client`-style container's
traffic into this topology means either running `wine`/`aomxnocd1.exe`
directly inside a namespace (losing all the X11/GPU-passthrough
plumbing `run-aom-spoofed-client.sh` already handles) or forcibly
reparenting a running container's network interface into a namespace
Docker doesn't know about (fragile, fights Docker's own bookkeeping).

### 2. Docker-native (`docker-setup.sh` / `run-client.sh` / `docker-teardown.sh`)

```
[aks-test-client-equivalent] --(aom-nat-client-lan)--> [nat-router] --(aom-nat-internet)--> [target]
                                              (same real MASQUERADE, containerized)
```

Same NAT mechanism, but the router is a small Alpine container
(`aom-nat-router`) running its own internal `iptables`, attached to
*two genuinely separate* docker networks (no shared bridge, no host-
level `DOCKER-USER` editing - the actual mistake in the earlier failed
attempt). The client container attaches to the private-side network
exactly like any other `docker run`, and Docker wires up its own veth
pair automatically - no manual reparenting needed. `run-client.sh`
mirrors `run-aom-spoofed-client.sh`'s own structure (X11 auth handling,
`aom-head` image, bind-mounted `/tmp` for captures that outlive the
container) so it's a drop-in sibling to the project's existing spoofed-
client tooling, not a divergent new pattern.

This is the one to actually run AoM through.

## Verbose capture: three independent sources, all wired up

`run-client.sh` captures all three simultaneously by design - matching
the project's own established "no single source tells the whole story"
practice (see `run-aom-verbose-clients.sh`'s own header comment for the
same reasoning applied to the client-vs-proxy investigation):

- **tcpdump** - raw wire bytes, on the client container's own interface.
  What actually crosses the wire, byte-for-byte.
- **WINEDEBUG** (`+dpwsockx,+dplay,+dplaysvc,+winsock`) - Wine's own
  socket-level call tracing. What the game's own socket API calls look
  like, sizes and call sequence, from inside Wine's translation layer.
- **strace** (`-f -e trace=network`, new to this lab, not used by the
  project's existing spoofed-client scripts) - every network-relevant
  syscall the process (and every thread/fork Wine spawns - hence `-f`)
  makes, including the *exact argument bytes* passed to `sendto()`
  before any network involvement at all. This is what actually lets
  "the game generated a packet with the wrong address baked in" (a
  userspace decision) be told apart cleanly from "the packet was fine
  but got mangled in transit" (a network-layer question) - WINEDEBUG
  alone shows sizes and Win32-level intent, not the raw kernel-level
  argument the application handed over.

Filtered to `-e trace=network` deliberately, not traced unfiltered -
same "targeted over exhaustive" tradeoff `run-aom-verbose-clients.sh`'s
own `FULL_RELAY_TRACE` option already documents for Wine's `+relay`
channel: an unfiltered strace against a GUI Wine process is an
enormous, mostly-irrelevant firehose (window messages, file I/O, memory
ops) that would bury the handful of syscalls that actually matter and
meaningfully slow the traced process down.

**Still needed, not yet built:** capturing on `nat-router`'s own *two*
interfaces (client-facing and internet-facing) simultaneously - this is
actually the single most valuable vantage point in the whole lab, since
comparing those two captures directly shows the real translation
happening (client's private address on one side, the router's
translated address:port on the other), which is the literal mechanism
under test. `docker-setup.sh` installs `tcpdump` in `aom-nat-router`
already but doesn't yet start captures on it automatically.

## Archiving

**Not yet built as a script.** Should follow this project's existing
`archiving/sessions/YYYYMMDD-description/` convention exactly (see any
existing session directory, e.g. `archiving/sessions/20260823-lonely-
player-kick/` for the pattern: numbered/labeled capture files plus a
`NOTES.md` write-up). Planned contents for a NAT-lab session:

- Client's own pcap + WINEDEBUG log + strace log (from `run-client.sh`'s
  own `$WORKDIR`)
- `nat-router`'s two-sided pcaps (once that capture gap above is closed)
- A `NOTES.md` covering: NAT mode tested (cone/symmetric/port-forward),
  what the client's own self-reported address looked like in its open
  message (the actual thing under test), whether it ever sent its own
  reciprocal ack, and a plain confirmed/disproven verdict on the
  hypothesis for that run.

## Port forwarding as a potential fix - outline, with an honest caveat

Two shapes, both mechanically straightforward to add to this lab:

- **Static, simulating manual router config**: a DNAT rule on
  `nat-router` (`iptables -t nat -A PREROUTING -p udp --dport 2300 -j
  DNAT --to-destination <client-ip>:2300`) paired with a port-preserving
  SNAT for the return leg - the programmatic equivalent of the classic
  "forward UDP 2300 on your router" community fix. Planned as a third
  `NAT_MODE=portforward` option in `docker-setup.sh`, not yet added.
- **Automatic, production-viable version**: UPnP IGD (or NAT-PMP/PCP) -
  a small companion tool (not a change to AoM's own binary, which can't
  be touched) that requests the same port mapping programmatically via
  `AddPortMapping`, no manual player configuration needed. Testable in
  this lab by adding `miniupnpd` to `nat-router` and validating against
  it with `miniupnpc`'s `upnpc` before ever considering it for real
  players. Not universal - some routers ship UPnP disabled, and it
  cannot reach past an ISP's own carrier-grade NAT for players who sit
  behind one.

**Important open question, not yet resolved either way**: port-
forwarding solves *inbound reachability* for unsolicited connections -
not obviously our bug. This project's client only ever *initiates*
outbound to `aom-lobby`'s known address, and ordinary NAT already lets
the return traffic back through on that same mapped flow (verified
extensively - it always has). The actual gap already found is the
client embedding the *wrong self-address* in its own outgoing payload,
a pure self-knowledge problem, not a reachability one - which
`OverrideAddress` targets directly and port-forwarding doesn't.

The reason it's still worth testing rather than dismissing: every real
community writeup of the actual working fix paired `OverrideAddress`
*together with* port-forwarding, never one alone. That could mean the
client's own address-embedding logic only trusts/uses an override once
there's also a stable, predictable external mapping to go with it -
making port-forwarding a necessary companion rather than a fix by
itself. This lab is what settles it, not more theorizing.

## Known open threads on the `OverrideAddress` side, for context

Two launch-parameter syntax attempts against the AKS-hosted real
backend both failed to cleanly apply (`OverrideAddress="..."` produced
a garbled address on retry attempts only; `+OverrideAddress="..."`
produced no effect at all, worse than the first attempt) - see
`CLAUDE.md`'s AKS section for the full trail. Unclear whether this
specific No-CD executable (`aomxnocd1.exe`) implements the parameter
differently than the official retail exe the community sources
describe, doesn't implement it at all, or needs a different syntax
still. This lab's own client-side captures (strace's exact `sendto()`
argument bytes especially) are positioned to actually answer that
directly, once run - rather than continuing to guess syntax variations
against the real, cost-incurring AKS cluster.

## Confirmed live, 2026-08-30: OverrideAddress - command line never works, user.cfg does

Full round-trip testing finally happened, through the docker-native lab
(`docker-setup.sh` + two `run-client.sh` instances, one per side of the
NAT boundary - `run-client.sh` was generalized this session to take
`NETWORK`/`CONTAINER_NAME`, so both sides use the same script rather
than a separate `run-host.sh`). Two real bugs in the lab itself had to
be fixed before any of this evidence could be trusted:

- **Docker's own implicit bridge gateway is not `aom-nat-router`.** Even
  though the router is attached to both `aom-nat-client-lan` and
  `aom-nat-internet`, a container on either network still defaults to
  that network's own `.1` gateway for anything not on its local subnet -
  which has no idea the other subnet exists and silently black-holes
  it. This is exactly what "timing out, the router doesn't work" turned
  out to be. Fixed by having `run-client.sh` add an explicit route to
  the *other* subnet via the router's own address on whichever network
  it's attached to, automatically, right after its tcpdump capture
  starts - both directions now, not just the one direction manually
  patched earlier in this same session.
- **Session/container state does not survive a sandbox reset.** Mid-
  session, every running container and `/tmp/aom-nat-lab` itself
  vanished with no teardown command ever issued - confirmed by `docker
  ps` coming back empty and a fresh shell's `/tmp` not having the
  directory at all. `aom-nat-router` and the two docker networks turned
  out to be independently relaunchable/reusable without needing a full
  `docker-setup.sh` re-run (the networks themselves survived), but this
  is worth remembering next time state looks lost.

### The methodology problem: three shells re-parse the same string

The original `run-client.sh` built its wine invocation as one deeply
nested string - `sh -c "... su aomuser -c \"...\""` - which gets parsed
independently by three different shells before wine ever sees it (this
script's own bash, the container's `sh -c`, and `su -c`'s internal
shell). Each layer strips one more level of quote/backslash escaping.
That's harmless for a fixed command line with no embedded quote
characters, but every community-documented `OverrideAddress` syntax
*is* a value containing its own literal `"` characters
(`OverrideAddress="1.2.3.4"`), and there's no reliable way to thread
that through three independent re-parses and have it arrive at wine's
actual argv unmolested - a naive attempt would either lose the quotes
entirely or, worse, silently produce something subtly different from
what was intended, undermining any conclusion drawn from testing it.

Fixed by writing the wine invocation to an actual script **file**
(`$WORKDIR/$CONTAINER_NAME-launch.sh`) and executing that, instead of
inlining it. A file's content is parsed by exactly one shell, once, when
it runs - no re-embedding, no ambiguity about what survived. This is
also what made it possible to verify, independently of any in-game
behavior, exactly what wine's process received: `/proc/<pid>/cmdline`
inside the running container, read directly, shows the real argv with
no shell interpretation left to second-guess.

### Command-line argv: confirmed reaching the process, confirmed having zero effect

Four syntax variants tested, each confirmed byte-for-byte correct in
`/proc/<pid>/cmdline` before ever touching the in-game result:

1. `OverrideAddress="203.0.113.2"` (no `+`, matching the dedicated
   IPDirect-configuration thread)
2. `+OverrideAddress="203.0.113.2"` (matching the tweaks gist)
3. `OverrideAddress=203.0.113.2` (no quotes at all)
4. `+OverrideAddress="203.0.113.2" +hostPort="2300" +directIPConnectivity`
   (all three community-documented flags together, `hostPort`
   immediately after `OverrideAddress` per further research, `+directIPConnectivity`
   sourced from an actual Ensemble Studios rep's own forum reply)

Every single one landed in the process's real argv exactly as written.
**None of them changed anything.** Decoded via a hex dump of `p1`'s own
outgoing open message (offset 8 into the payload, the client's own
self-block sockaddr_in), the embedded self-address was `0a 14 01 03`
(`10.20.1.3`, `p1`'s real private LAN IP) in all four runs - byte-for-
byte identical to a completely unmodified launch with no override at
all. This lines up with the "worked on 98/ME, broke on 2000/XP with
identical steps" unreliability already found in community reports
(`docs`... see the "Known open threads" section above) - for this
specific binary (`aomxnocd1.exe`) under Wine, the command-line form of
this parameter is confirmed dead on arrival, not merely unreliable.

### user.cfg: confirmed working

A completely different code path - `aomxnocd1.exe`'s own startup config
file, not argv - and it produced a genuinely different result.
Confirmed present in this image already, at
`/home/aomuser/aom/startup/user.cfg` (the non-Titans-style path,
alongside the exe rather than under `My Documents`), already carrying
real settings (`xres`/`yres`/`noIntroCinematics`/etc.) the game
demonstrably reads at startup. `run-client.sh` gained a
`USER_CFG_OVERRIDE` option that backs the existing file up to
`user.cfg.orig-backup` (never overwriting a backup that already exists,
so re-runs don't stack backup-of-a-backup) and writes
`OverrideAddress="<value>"` as the new file's first line, followed by
the original content.

With that in place and zero command-line args, `p1`'s own outgoing open
messages showed:

```
1st-2nd retry:  self = 0a 14 01 03  ->  10.20.1.3    (still real)
3rd retry on:   self = cb 00 71 02  ->  203.0.113.2  (the override!)
```

The override value actually appears on the wire, starting a couple of
retries in rather than immediately - plausibly the difference between
an early DirectPlay service-provider self-address snapshot (taken
around the `WSAStartup`/ESO-lookup point established earlier this
session - before `user.cfg` for this run had even been read yet, since
the write happens at container-entrypoint time, ahead of wine's own
launch, but *within* wine there may still be an even-earlier cached
value from a first pass) and a later, fresher read once the actual
session-port open logic runs. Not fully explained, but the *effect* -
the client's self-reported address changing from its real one to the
configured override - is unambiguous and repeatable.

### Open question this doesn't resolve by itself

`lobby/main.go`'s `forwardToBackend`/`rewriteToBackend` (session proxy)
already rewrites this exact same field - the client's self-block - to
`aom-lobby`'s own address before ever forwarding a client's open to the
real backend pod (`rewriteSessionField(payload, sessionSelfOffset,
cfg.internalIP, cfg.publicPort)`, see that function's own doc comments).
That means, for a connection actually routed through `aom-lobby` (the
real production path, not this lab's raw client<->host test), the
backend never even sees whatever the client itself embedded - overridden
or not - since `aom-lobby` overwrites it regardless before relaying
onward. Whether the still-open AKS "client never sends its own ack"
symptom is actually explained by this mechanism at all, given that
existing server-side rewrite, is **not settled by this lab's testing so
far** - every test here has been a direct client<->host connection with
no lobby in the loop. The natural next step is wiring a real `aom-lobby`
instance into this same lab topology and re-running the same test
through it, rather than assuming this result transfers.

See `docs/player-connection-requirements.md` for how this gets shipped
to real players regardless (a `user.cfg`-writing script per platform,
since no home player has a static IP) - written with that open question
stated plainly rather than presented as a confirmed fix for the
production symptom.

## Testing plan

1. `./docker-setup.sh` (cone mode first - establish the lab works at
   all before testing failure-specific NAT types).
2. `./run-client.sh`, Direct-Connect from inside it to `nat-router`'s
   own client-facing address (standing in for `aom-lobby`, until a real
   target is wired up on `aom-nat-internet`) or to a real reachable
   `aom-lobby` if one's up.
3. Check the client's own captured open message: does its self-address
   field show its real private IP (`10.20.1.x`), same as every AKS
   capture already on file? This should reproduce the bug's starting
   condition even before touching NAT mode or overrides - confirms the
   lab itself is faithfully representing the failing case.
4. Re-run with `NAT_MODE=symmetric` - does anything change? (Per the
   "does the NAT *type* matter at all" open question - our own leading
   theory says no, since the bug is about self-knowledge not
   translation behavior, but this settles it rather than assuming it.)
5. Add the `portforward` NAT mode and retest with `OverrideAddress` set
   correctly (once the syntax question above is resolved) - the
   specific combination every real community fix report used together.
6. Archive whichever run(s) produce a real result either way, following
   the plan above.
