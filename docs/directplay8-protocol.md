# DirectPlay8 LAN Protocol Findings

Captured from AoM: Titans (No-CD) running under Wine, hosted from one
container (`run-aom-head.sh`) and browsed-for from a second container on the
same physical machine (`run-aom-client.sh`). Both containers use
`--net=host`, so client/host traffic in this capture travels over `lo`
instead of a real LAN interface — on separate machines the same exchange
would appear on the real NIC in both directions.

Capture method: `record-session.sh` (tcpdump, `CAPTURE_FILTER=udp`) +
`tcpdump -tt -r` / `tcpdump -X` for payload inspection. Source session:
`archiving/sessions/20260802-231930/session.pcap`, 2026-08-02.

## Ports

| Port | Purpose |
|------|---------|
| UDP 2299 | Session discovery (broadcast query / unicast reply, and a ping/liveness pair) |
| UDP 2300 | Actual game session traffic (address advertised inside the discovery reply, not observed directly in this capture) |

`CLAUDE.md`'s original assumption of "ports 2300-2400" was half right — 2300
is real, but discovery itself happens one port lower, on 2299, and wasn't
in that range.

## Discovery is query/response, not announce

A hosted game transmits nothing on its own. It sits listening on 2299. A
client only generates traffic when its LAN browse screen is open, and it's
the client that initiates every exchange:

1. Client broadcasts an "enumerate hosts" query to `255.255.255.255:2299`,
   repeated roughly every 330ms for as long as the browse screen is open.
2. Any listening host replies directly (unicast) to the querying client's
   ephemeral port, also on 2299.
3. Once per browse-screen refresh, client and host also exchange a second,
   different unicast pair on 2299 — looks like a liveness/ping check (see
   below), possibly what drives a ping-time column in the game list.

No traffic occurs at all until a client actively browses. To capture
anything, you need a second client instance actually on the LAN list
screen, not just a host sitting in its lobby.

## Message formats

All three message types share an embedded `sockaddr_in` (Windows layout,
16 bytes, copied byte-for-byte onto the wire):

```
02 00                      sin_family = AF_INET (little-endian uint16)
08 fc                      sin_port   = 2300 (big-endian uint16 = 0x08fc)
c0 a8 01 67                sin_addr   = 192.168.1.103 (dotted-order bytes)
18 c0 4e 06 20 c0 4e 06    sin_zero   = NOT actually zero, meaning unknown
```

The `sin_family`/`sin_port`/`sin_addr` fields decode cleanly and
confidently. The 8 bytes where `sin_zero` should be are consistently
non-zero and vary slightly between messages — flagged as an open question,
not yet decoded.

### Type 0x25 — discovery query (9 bytes, broadcast)

```
25 00 00 00 00 00 00 00 00
```

Constant across every capture. No target/session info — a generic "is
anyone hosting" broadcast.

### Type 0x26 — discovery reply (67 bytes, unicast, host → client)

```
26 01 00 00 00                                          header/type prefix
<sockaddr_in>                                           16 bytes (see above)
<sockaddr_in>                                           16 bytes, duplicate of the above
1a 00 00 00                                              string byte-length = 26
61 00 64 00 6d 00 69 00 6e 00 27 00 73 00 20 00
47 00 61 00 6d 00 65 00 00 00                            UTF-16LE "admin's Game\0"
```

The trailing string is the session name shown in the client's LAN game
list — length-prefixed (uint32, byte count including the UTF-16 null
terminator), then UTF-16LE text.

### Type 0x20 / 0x21 — liveness ping (41 bytes each way, unicast)

Sent once per browse-screen refresh cycle, after the repeating 0x25/0x26
broadcast exchange, directly between client and host (not broadcast):

```
client -> host:  20 01 00 00 00  <sockaddr_in>   (no name string)
host   -> client: 21 01 00 00 00  <sockaddr_in>   (no name string)
```

Same `sockaddr_in` shape as the discovery reply, minus the name. Best
guess: this is what the game uses to compute/display a ping time per
listed entry, independent of the broadcast enumeration.

## Practical implications for the Quilkin proxy / spoofing work

- The `sockaddr_in` embedded in the 0x26 reply and the 0x20/0x21 ping is
  the thing that needs rewriting if traffic is being proxied/NATed —
  it bakes in the real IP:2300 the client is told to connect to next. If
  the host's real address isn't reachable by the client as-is (e.g. across
  a Kubernetes pod boundary), this field has to be rewritten in flight to
  point at the proxy's externally-reachable address instead.
- The message-type byte at payload offset 0 (`0x25`/`0x26`/`0x20`/`0x21`)
  is the cheapest thing to switch on when writing a packet filter/rewriter.
- The `sin_zero` bytes are unexplained — don't assume they're safe to zero
  out or ignore until we understand what varies them.
- Actual gameplay traffic on UDP 2300 hasn't been captured yet — this
  capture only covers the LAN discovery phase, not what happens once a
  client actually connects and plays. That's the natural next capture to
  run.

## Open questions

- What are the 8 non-zero `sin_zero` bytes? They differ slightly between
  the discovery reply and the ping reply from the same host — possibly a
  sequence number, session token, or uninitialized memory from Wine's
  `dpnet` implementation (worth checking Wine source/debug logs for).
- What's inside the 0x26 header's `00 00` (bytes 2-3, before the trailing
  `00` at byte 4) — likely a version or reserved field, unconfirmed.
- What does actual session traffic on UDP 2300 look like once a client
  joins and plays?
