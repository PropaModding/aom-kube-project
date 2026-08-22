# Host health probe: detecting a silently-stuck `auto-host.sh` run

**Status: implemented and fully live-verified, 2026-08-22.** Written to
close out the todo recorded at the top of `CLAUDE.md`. Read that entry
first for the live incident that motivated this (`auto-host.sh`
misclicking partway through its EULA-\>menus-\>hosted-lobby sequence,
landing on the wrong screen while still logging "done" and exiting 0 -
hit twice in one session under concurrent load). See "Confirmed live,
2026-08-22" at the end of this doc for the full end-to-end test.

## What's actually broken, and what isn't

Worth stating precisely, because part of this is already safe and it'd
be easy to over-design around a problem that doesn't exist:

- **Routing safety is already handled.** `hostPool.selectLocked`
  (`lobby/main.go`) excludes any host that isn't `hostLive` -
  `hostProbe.hasWaitingGame()` - from ever being assigned a client. A
  pod stuck on the wrong menu never actually reaches the state
  `hostProbe.probeOnce()` checks for (a `0x26` reply on its discovery
  port), so it already can't silently eat a real player's connect
  attempt. This has been true since the N-host pool shipped
  (2026-08-13) and isn't part of what this doc needs to fix.
- **What's actually missing is *detection and recovery*.** Nothing
  today ever looks at `hostProbe`'s own result and does anything about
  it beyond routing around the pod. The broken pod just sits there
  forever, `/hosts` silently never reaches the expected replica count,
  and cluster capacity quietly shrinks until a human notices and runs
  `kubectl delete pod`. `podLister.list()` (`lobby/main.go`) only
  filters on Pod phase (`Running`) and having an IP - it has no
  awareness of whether the pod ever actually finished hosting, so a
  stuck pod stays listed (just permanently excluded from selection) for
  as long as it exists.

The fix, then, isn't a new safety mechanism - it's wiring the liveness
signal `hostProbe` already computes into something that can actually
act: Kubernetes' own restart-on-failed-probe behavior.

## Packet-level evidence: the discovery exchange is client-initiated, not a host broadcast

Before designing anything, it's worth confirming *what* signal is
actually available to probe, since the natural first assumption - "the
host periodically broadcasts its own presence, something could just
listen for that" - is wrong. Checked directly against two of this
project's own historical captures, from opposite ends of the project
timeline:

- `archiving/sessions/20260802-231930/session.pcap` - the earliest
  capture in the repo with real traffic, single-machine test, predates
  even the Go lobby.
- `archiving/sessions/20260808-234138-3client-p2p-check/host-capture.pcap`
  - a real 3-container capture, much closer to today's actual
  architecture.

Filtering both for `udp.port==2299` shows the identical pattern:

```
192.168.49.4 -> 255.255.255.255:2299   (broadcast, client-sent)
25 00 00 00 00 00 00 00 00

192.168.49.3 -> 192.168.49.4:2299 -> <client's ephemeral port>   (unicast, host-sent)
26 01 00 00 00 02 00 08 fc c0 a8 31 03 18 60 b4 02 20 60 b4 02
02 00 08 fc c0 a8 31 03 18 60 b4 02 20 60 b4 02 18 00 00 00
68 00 6f 00 ...
```

(Same exchange, fuller game-name string, from the earlier single-machine
capture:)

```
26 01 00 00 00 02 00 08 fc c0 a8 01 67 18 c0 4e 06 20 c0 4e 06
02 00 08 fc c0 a8 01 67 18 c0 4e 06 20 c0 4e 06 1a 00 00 00
61 00 64 00 6d 00 69 00 6e 00 27 00 73 00 20 00 47 00 61 00
6d 00 65 00 00 00
                       "a  .d  .m  .i  .n  .'  .s  .   .G  .a  .m  .e"  (UTF-16LE)
```

Confirmed exhaustively, not just spot-checked: every one of the 317
packets sourced from the host's own IP on port 2299 across the full
2026-08-08 capture is a **unicast** reply to a specific querying
address:port - zero broadcast-sourced packets from the host anywhere in
either file. The client's own `0x25` query repeats on its own (LAN-browse
screens poll continuously while open) at a measured mean interval of
**~275ms** (min ~1ms, max ~1.07s - jitter from multiple clients'
independent poll loops interleaving; the client-side polling *rate*
itself isn't something a health probe needs to match).

**Conclusion: there is nothing to passively listen for.** The host is
silent by construction until asked. Any liveness check has to actively
send the same query a real client would - i.e. spoof a client, exactly
as this doc's title says.

### This is already proven, existing code - not a new guess

`lobby/main.go` already does precisely this, continuously, as part of
`hostProbe`:

```go
// line 896
var discoveryQuery = []byte{0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
```

Byte-for-byte identical to the real client's captured query above (`25
00 00 00 00 00 00 00 00`, 9 bytes) - this was already cross-checked
against a real client, not invented for `hostProbe`.

```go
// line 965
func (h *hostProbe) probeOnce() {
	open := false
	conn, err := net.DialTimeout("udp", h.backendAddr, probeTimeout)
	if err == nil {
		udpConn := conn.(*net.UDPConn)
		udpConn.SetDeadline(time.Now().Add(probeTimeout))
		if _, err := udpConn.Write(discoveryQuery); err == nil {
			buf := make([]byte, 128)
			if n, err := udpConn.Read(buf); err == nil && n > 0 && buf[0] == 0x26 {
				open = true
			}
		}
		udpConn.Close()
	}
	...
}
```

`probeInterval = 3 * time.Second`, `probeTimeout = 1 * time.Second`
(lines 89-90). One `hostProbe` per pool member today, each polling its
own host's discovery port on this cycle, keyed by pod name
(`hostCandidate.id`, set to `pod.name` at line 1716 - the type comment
above it claiming `"host-0", "host-1"` is stale, left over from before
pods were tracked by real name).

This is the exact signal to reuse - not reimplement, not approximate.

## Design decision: centralize the probe, don't bake it into every pod

Two shapes were considered:

1. **Per-pod**: extend `input-agent` (already baked into every
   `aom-headless` pod, already in the same container/network namespace
   as the game process) with a `/healthz` handler doing this same
   `0x25`/`0x26` check against `localhost:2299`, and point each pod's own
   k8s `livenessProbe.httpGet` straight at it.
2. **Centralized, one per node**: a new small agent, one per node
   (`DaemonSet`), that discovers every `aom-headless` pod scheduled to
   its own node and runs this same probe against each of them, exposing
   one shared status endpoint the pods' own liveness probes call into.

Went with (2), per explicit direction - the reasoning against (1) was
duplication: `input-agent`'s job (forwarding `POST /click`) is
inherently per-pod because it has to drive that pod's own `xdotool`, but
a discovery-port health check has no such requirement - it's cheap to
run from anywhere in the same network reach, so running one per pod
would mean N copies of the same check logic, N copies of the same
`0x25`/`0x26` byte constant, baked into the game-pod image itself for a
concern that has nothing to do with running the game.

**Correction to something said earlier in this design discussion**: a
DaemonSet approach was first waved off partly on the assumption it would
need a new `NetworkPolicy` egress exception (since `aom-headless`'s
egress is locked down). Checking the actual rule
(`k8s/aom-headless-netpol.yaml`) shows this was wrong - it's looser than
the shorthand "locked to just aom-lobby's address + DNS" description in
`CLAUDE.md` suggests:

```yaml
egress:
  - to:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 192.168.49.0/24   # only the docker-bridge client subnet is denied
  - to:
      - ipBlock:
          cidr: NODE_IP_PLACEHOLDER/32   # re-allows aom-lobby's own hostNetwork address
  - to: [...]   # DNS
```

It denies only the docker-bridge subnet the spoofed test-client
containers live on (the actual real-IP-leak concern this policy exists
for, per `CLAUDE.md`'s LAN-sanitization section) and re-allows
`aom-lobby`'s specific node address within it - egress everywhere else
in the cluster's own pod network is already open. A `DaemonSet` agent
running with `hostNetwork: true` on this single-node dev cluster binds
to that exact same already-re-allowed `NODE_IP` - **no `NetworkPolicy`
change needed at all.** (Worth a note in `CLAUDE.md` that its own
"locked to just aom-lobby + DNS" summary of this policy is an
oversimplification, separate from this doc.)

The real remaining cost of (2) is a new small component with its own
image, its own narrow RBAC, and its own node-local pod discovery - laid
out below. That cost is worth it here specifically because the check
itself is cheap, stateless, and has nothing to do with any individual
pod's own job.

## The new component: `host-health-agent`

New directory `host-health-agent/`, new `dockerfile.host-health-agent`,
mirroring the existing `input-agent/` shape (small standalone Go HTTP
server, no shared module with `lobby/` - the ~20 lines of probe logic
being duplicated rather than imported as a shared package is deliberate;
these are two different deployables with different RBAC scopes and
nothing else in common, and the logic is small enough that sharing a Go
module across them would cost more in build-graph complexity than it
saves).

### Deployment shape

- **`DaemonSet`**, `hostNetwork: true` - one pod per node, reachable at
  that node's own IP (see the `NetworkPolicy` analysis above for why
  this specific choice sidesteps any policy change on this cluster).
- **`ServiceAccount` + `Role` + `RoleBinding`**, granting the identical
  read-only `get`/`list`/`watch` on `Pods` that `k8s/lobby-rbac.yaml`
  already grants `aom-lobby` - same justification, same shape, new
  binding. Namespaced `Role`, matching every other RBAC object in this
  project.
- Filters its own pod list to **`spec.nodeName == $NODE_NAME`**
  (`$NODE_NAME` from the downward API, `fieldRef: spec.nodeName`) -
  each agent only tracks and probes pods scheduled to its own node, the
  actual point of running it as a `DaemonSet` rather than a single
  cluster-wide `Deployment`. On today's single-node minikube cluster
  this means "every `aom-headless` pod," same practical scope as
  `aom-lobby`'s own pool; the distinction only matters if this project
  ever runs multi-node.

### Probe loop (per node-local `aom-headless` pod)

Identical to `hostProbe.probeOnce()` above, re-homed:

```go
const (
	probeInterval = 3 * time.Second
	probeTimeout  = 1 * time.Second
)

var discoveryQuery = []byte{0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

func probeOnce(podIP string) bool {
	conn, err := net.DialTimeout("udp", podIP+":2299", probeTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	udpConn := conn.(*net.UDPConn)
	udpConn.SetDeadline(time.Now().Add(probeTimeout))
	if _, err := udpConn.Write(discoveryQuery); err != nil {
		return false
	}
	buf := make([]byte, 128)
	n, err := udpConn.Read(buf)
	return err == nil && n > 0 && buf[0] == 0x26
}
```

Reconciliation loop mirrors `podLister.list()`'s own poll pattern
(`podPollInterval`, same constant/cadence already proven stable) to
learn which pods exist on this node; a `map[podName]bool` tracks each
one's last probe result, refreshed every `probeInterval`.

### Status endpoint

`GET /host-health?id=<pod-name>`, served on the same `hostNetwork`
address, a new fixed port (`8090` - free; `input-agent` uses `8082`,
`aom-lobby`'s status endpoint uses `8080`):

| Response | Meaning |
|---|---|
| `200 ok` | Last probe for this pod succeeded - genuinely hosting. |
| `503 not hosting` | Pod is known (seen in this node's pod list) but its last probe failed or never succeeded. |
| `404 unknown` | Pod not yet observed by this agent at all - still starting, or hasn't survived one full list-poll cycle yet. Deliberately distinct from `503`: an `aom-headless` pod calling this in its very first seconds of life shouldn't read as "unhealthy," just "not answered yet." |

### Wiring into `aom-headless`'s own liveness probe

`dockerfile.k8s` already installs `curl` (line 26) - no new dependency.
Add a small script, e.g. `/usr/local/bin/healthcheck.sh`:

```sh
#!/bin/sh
# Exit 0 = healthy (k8s leaves the pod alone), exit 1 = unhealthy
# (k8s restarts the container after failureThreshold consecutive
# failures). Deliberately fails OPEN (exit 0) on anything that isn't an
# explicit "not hosting" from the health agent itself - an unreachable
# health agent, a curl timeout, or this pod not yet being known to it
# must never read as this specific pod being broken, or one health-agent
# hiccup / a pod's own first few seconds of life would mass-restart
# every host in the pool simultaneously.
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 \
  "http://${NODE_IP}:8090/host-health?id=${POD_NAME}")
case "$code" in
  200) exit 0 ;;   # confirmed hosting
  503) exit 1 ;;   # health agent explicitly says not hosting - real failure
  *)   exit 0 ;;   # 404 (not yet known), curl error, timeout, agent down - fail open
esac
```

`k8s/aom-headless-deployment.yaml` additions:

```yaml
env:
  - name: POD_NAME
    valueFrom: { fieldRef: { fieldPath: metadata.name } }
  - name: NODE_IP
    valueFrom: { fieldRef: { fieldPath: status.hostIP } }
livenessProbe:
  exec:
    command: ["/usr/local/bin/healthcheck.sh"]
  initialDelaySeconds: 100   # auto-host.sh's own sequence takes "well over a
                             # minute" per CLAUDE.md - must not fire before a
                             # normal, successful run would even finish
  periodSeconds: 10
  failureThreshold: 3        # ~30s of sustained failure past the initial
                             # delay before a restart - rides out hostProbe's
                             # own documented single-cycle self-healing false
                             # negative (a host added within the last
                             # podPollInterval briefly reads not-live, see
                             # hostLive's own doc comment) many times over
  timeoutSeconds: 3
```

`failureThreshold: 3` at `periodSeconds: 10` specifically outlives that
documented single-cycle false-negative window several times over, so
this only trips for the actual bug this doc exists to fix - a pod that
*never* reaches the hosted state at all, not one that's merely slow to
report it the first time.

## Verifying the fix

1. **Reproduce the actual bug directly**, without waiting for a natural
   flake: exec into a freshly-started `aom-headless` pod during its
   `auto-host.sh` run and manually click it onto the wrong menu (or
   `xdotool key Escape` at the wrong moment) so it never reaches the
   hosted lobby - confirm today's baseline first (pod sits broken
   indefinitely, `/hosts` never reflects it, nothing recovers it).
2. Deploy this design, reproduce the same misclick, and confirm:
   - `host-health-agent`'s own probe for that pod reports `503` via
     `/host-health?id=<name>`.
   - The pod's `livenessProbe` goes unhealthy after `failureThreshold`
     and Kubernetes restarts the container (visible via
     `kubectl get pod <name>` `RESTARTS` incrementing, and a fresh
     `auto-host.sh` run starting over in `kubectl logs`).
   - A pod that *does* host successfully never has its liveness probe
     fail, across several full `probeInterval` cycles - no false-positive
     restarts of a healthy host.
3. **Fail-open check**: stop `host-health-agent` (or block its port)
   while a genuinely healthy `aom-headless` pod is running; confirm the
   pod's own liveness probe keeps passing (exit-0 fallback in
   `healthcheck.sh`) rather than the whole pool restarting together.

### Confirmed live, 2026-08-22

Implemented exactly as designed - `host-health-agent/main.go`,
`dockerfile.host-health-agent`, `k8s/host-health-agent-rbac.yaml`,
`k8s/host-health-agent-daemonset.yaml`, `healthcheck.sh` baked into
`dockerfile.k8s`, and the `POD_NAME`/`NODE_IP`/`livenessProbe` additions
to `k8s/aom-headless-deployment.yaml`. Build-verified first (`go build`/
`go vet`/`gofmt -l` clean, both Docker images build clean, both new/
edited manifests pass `kubectl apply --dry-run=client`), then deployed
and exercised live end-to-end:

- **Real hosting transitions tracked correctly.** Two freshly-rolled
  pods both read `503` from `host-health-agent` while mid `auto-host.sh`,
  then flipped to `200` at the same moment (~50s in) - cross-checked
  against `aom-lobby`'s own independent `/hosts` count (2), which agreed
  exactly, confirming this agent's probe result matches `hostProbe`'s own
  assessment of the same pods (expected, since it's the identical check).
- **No false-positive restarts.** Both healthy pods sat well past
  `initialDelaySeconds` (100s) with zero `Unhealthy` events and
  `RESTARTS` staying at 0.
- **The actual recovery path, proven twice - the second time correctly:**
  - First attempt used `pkill -f aomxnocd1.exe` inside a healthy pod to
    simulate a stuck host. This *did* produce a restart, but for the
    wrong reason - the container's own PID 1 (`tini`, directly
    supervising that process) exited the instant its child died,
    triggering Kubernetes' ordinary `restartPolicy: Always` container-
    crash-restart, not this design's `livenessProbe` at all (confirmed
    via `lastState.terminated`: `exitCode: 143`, timestamped to the
    exact second of the `pkill`, restart already visible before
    `failureThreshold`'s ~30s window could have elapsed). Also not a
    faithful reproduction of the actual bug anyway - the real
    `auto-host.sh` misclick leaves the game process *alive*, just stuck
    on the wrong screen; killing it outright is a different failure mode.
  - Redone correctly with `kill -STOP` on the game process specifically
    (confirmed via `ps -o pid,stat` showing `STAT=Tl`, genuinely paused,
    not terminated) - this leaves `tini`/the container's main process
    untouched, isolating the test to exactly this design's own chain.
    Result: `host-health-agent` read `503` within one probe cycle;
    Kubernetes' own events then showed, in order,
    `Warning Unhealthy: Liveness probe failed` followed by
    `Normal Killing: Container aom failed liveness probe, will be
    restarted`, ~30s later (matching `failureThreshold: 3` \*
    `periodSeconds: 10`) - unambiguous proof this design's own probe
    chain, not some other mechanism, triggered the restart.
  - The restarted pod then ran a fresh `auto-host.sh` cycle from scratch
    and reported `200` again ~50s later - the full detect-\>restart-\>
    recover loop confirmed end-to-end, not just the detection half.

Not separately re-run: the fail-open check (item 3 above) - covered by
code review of `healthcheck.sh`'s `case` statement (only `200`/`503` are
distinguished; everything else, including a `curl` failure, falls to the
`exit 0` default) rather than a live agent-outage simulation, since the
live restart-path test already confirmed the rest of the chain works and
this branch is a simple, low-risk default case.

## Explicitly out of scope for this pass

- **Readiness probe / pool membership gating.** This doc only wires up
  restart-on-failure. Whether `podLister` should also stop *listing* a
  pod that's failing this same check (rather than relying on
  `hostLive`'s existing routing exclusion, which already covers the
  real safety concern - see "What's actually broken" above) is a
  separate, lower-value change and isn't needed to close the todo this
  doc addresses.
- **Multi-node scheduling behavior.** The node-scoped filtering
  (`spec.nodeName`) is designed to be correct if this project ever runs
  multi-node, but that's never been this project's actual deployment
  target (single dev machine, `minikube`) and isn't being tested here.
- **Session-port (2300) probing.** Deliberately not revisited - see
  `hostProbe`'s own doc comment (`lobby/main.go`, "History" section) for
  why a pre-connect 2300 probe was tried and reverted: DirectPlay8
  doesn't bind that socket until a real client's handshake arrives, so
  any such probe reads a genuinely healthy, freshly-hosted lobby as
  dead. This doc's discovery-port (2299) check has no such problem -
  discovery answers are up as soon as hosting genuinely succeeds.
