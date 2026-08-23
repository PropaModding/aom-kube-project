# Chat injection: spoofed lobby chat messages, decoded from real captures

**Status: welcome message implemented and live-verified, 2026-08-23.**
Written 2026-08-23, split out of `lobby/lonely-player-kick-design.md`'s
former "Part 1" into its own doc so chat injection can be investigated
and tested on its own track, independent of the kick feature it was
originally motivating. `lonely-player-kick-design.md` now covers only
the kick mechanism/policy (Part 2 there) and links back here for the
chat-message piece it depends on.

Confirmed live: a real client landing in the lobby now gets a spoofed,
personalized `"Welcome, <name>!"` chat message (falling back to
`"Welcome! Connected to <IP>."` if the name isn't learned in time),
confirmed rendering correctly in the client's own chat log by direct
observation, for two independent real clients. See
`archiving/sessions/20260823-welcome-message-chat-injection/` for the
full evidence, including a first attempt that sent cleanly but was never
seen - see "History" at the end for the fix.

## Landing signal: why the first backend->client 0x03 packet, not `isCDKeyEcho`

The welcome message needs to fire once a client is genuinely in the
lobby, not merely once the connection is confirmed real. Two signals
already in the codebase were considered and rejected:

- **`isCDKeyEcho`/`firstClientLandedAt`** (the join-cushion signal) is a
  connection-layer proof - it confirms the client completed the
  handshake round-trip - not a lobby-rendering one. It fires mid-
  handshake, before there's any confirmation the client's own lobby/chat
  UI is actually up.
- **Relay creation** (`isNewPeerBroadcast`/`isExistingPeerBroadcast`
  firing, which is what creates `pairRelay`) only happens once a
  *second* real client exists to pair with - a lone first arrival would
  never trigger it, so it can't be the general "landed" signal every
  client needs.

**Chosen instead: the first backend->client `0x03`-wrapped
(settings-sync layer) packet observed for that session** -
`session.connIDKnown`'s existing false->true transition in
`backendToClient` (`main.go`), which already exists to learn `connID`
the first time real settings-sync traffic reaches this specific client.
This is the first point the host is demonstrably treating the client as
a live interactive lobby member, and it fires for a solo first arrival
exactly the same as a paired second one. Unlike `isCDKeyEcho` or the
peer broadcasts, this specific signal has never been used as a trigger
for anything before this feature, so its exact timing relative to what's
actually painted on the client's screen is unverified - `welcomeMessageDelay`
(3s, added on top, not as a substitute) and the live test below are what
will confirm or correct it.

## Implementation (`main.go`)

- `wrapperSeq` (mirrors `wrapperConnID`) extracts the same 6-byte
  wrapper's seq field.
- `session.hostSeq`/`hostSeqKnown` track the most recently observed real
  seq value on this connection's settings-sync layer, updated on every
  `0x03` packet (not just the first, unlike `connID`).
- `synthesizeChatMessage(connID, seq, text) []byte` - the format below,
  implemented as sketched, trailer sent as `00 00`.
- `proxy.sendWelcomeMessage` - fired once, from `backendToClient`'s own
  `connIDKnown` transition (`p.name == "session"` only), via
  `time.AfterFunc(welcomeMessageDelay, ...)`. Picks `seq = sess.hostSeq +
  1` at fire time - continuing the host's own real numbering, not
  jumping ahead of it (see "History" below for why this matters).
- `isPlayerAnnounce`/`playerAnnounceName` - decode a client's own
  self-announcement (client->host, real handshake) of its player GUID +
  display name, based on a single real sample (see "Player-reference
  hypothesis" above for that sample's own decode). `session.playerName`/
  `playerNameKnown` cache the result, checked in `rewriteToBackend`
  alongside the existing `isCDKeyEcho` check.
- Message text: `"Welcome, <name>!"` if `playerNameKnown` by send time,
  else falls back to `"Welcome! Connected to <IP>."` - the name is
  best-effort, not a hard dependency for sending the message at all.

## The format, fully decoded

Every chat message shares the same `0x03`-wrapped settings-sync-layer
shape everything else in this layer uses (`docs/directplay8-protocol.md`'s
"Common wrapper"):

```
offset  bytes  field
0       1      0x03 (wrapper type, fixed)
1       1      byte1 (varies per message - NOT part of a fixed marker,
                       same as every other message in this layer since
                       the 2026-08-20 byte-1 correction)
2-3     2      seq (LE, per-sender)
4-5     2      connID (LE, per-sender, stable for the session)
6-7     2      declared length (LE) - see "Declared length" below
8-10    3      01 47 04 - fixed chat sub-type marker
11      1      string length, in UTF-16 code units, INCLUDING the null
               terminator
12-14   3      00 00 00 - fixed, always zero in every sample
15..    N*2    the message text itself, UTF-16LE, null-terminated
                (N = the length byte above)
+0-3    4      00 00 00 00 - fixed, always zero in every sample
+4-5    2      UNKNOWN - varies per message, see "The open question" below
```

**This is a materially more complete decode than existed in this project
before today** - `docs/directplay8-protocol.md`'s only prior sample
(`"blah blah"`) left the sub-header and trailer both marked "not yet
decoded." Cross-referencing four *additional* independent samples found
sitting unindexed in already-archived captures (never previously
diffed against each other) closes all but one field.

## The five samples this decode is built from

| Message | Source | Total len | Declared len (bytes 6-7) | strlen byte (byte 11) |
|---|---|---|---|---|
| `"blah blah"` | `docs/directplay8-protocol.md` (pre-existing) | 41 | 31 | 10 |
| `"here is a chat for the packet reading...."` | `archiving/sessions/20260803-3v3-win-loss/session.pcap` | 105 | 95 | 42 |
| `"wooooewwwww"` x2 | `archiving/sessions/20260812-215320-readyup-resign-quit-test/clienta-capture.pcap` + `clientb-capture.pcap` | 45 | 35 | 12 |
| `"still chatting after resign"` x2 | same session, both client captures | 77 | 67 | 28 |

All four (five including the pre-existing one) independently confirm
the same field layout. The two `"wooooewwwww"`/`"still chatting..."`
pairs are the same message captured from both sender and receiver's own
pcap - useful because they also proved the trailer isn't sender/receiver-
dependent, only per-*packet* (see below).

## Declared length, fully solved

`declared_len = 7 (sub-header) + string_bytes + 4` - i.e. it covers the
sub-header and string in full, plus the *first* four bytes of the
trailing 6, but not the last two. Confirmed exactly across all five
samples with zero deviation. This matches an already-established pattern
in this protocol (`lobby/unexplained-bytes-reverse-engineering-plan.md`'s
note on the ready-toggle message: "declared length doesn't cover a small
fixed trailer" - not a one-off, the same shape recurring in a second,
independently-discovered message type).

## The open question: the final 2 trailer bytes

**Correction (2026-08-23, re-verifying against raw captures)**: this
section originally mischaracterized the `"wooooewwwww"` and `"still
chatting..."` pairs as "the same wire packet, captured from two
vantage points" - re-pulling the raw bytes shows that's wrong. Each pair
is actually **two different senders independently sending identical
canned text** ~0.3ms apart (this test session's clients evidently fire
the same scripted message near-simultaneously): each sender's own packet
appears once in their own capture, and is forwarded **byte-for-byte
unchanged** into the peer's capture - proving `pairRelay` passes chat
payloads through raw, without rewriting seq/connID/trailer. That also
means this doc has 4 fully independent real packets to work with, not 2
duplicated observations. One transcription bug also caught and fixed in
the process: `"wooooewwwww"` #2's trailer was previously written as
`9612` - the actual bytes are `96 0e`.

Not constant across any of the 4: identical text, identical sub-header,
different seq/connID, and **different trailer value each time**:

| Message | sender | seq | connID | trailer (LE) |
|---|---|---|---|---|
| `"wooooewwwww"` | A | `8312` | `f139` | `ff12` |
| `"wooooewwwww"` | B | `930e` | `3a32` | `96 0e` |
| `"still chatting..."` | A | `c00f` | `8519` | `f90f` |
| `"still chatting..."` | B | `f403` | `d837` | `1f0b` |

Tried and ruled out (checked programmatically against all four samples,
re-verified against the corrected bytes above): CRC-16/CCITT-FALSE,
CRC-16/XMODEM, CRC-16/IBM, and CRC-16/MODBUS, each over several byte
ranges (sub-header+string alone, connID+sub-header+string,
seq+connID+sub-header+string, declared-length+sub-header+string, the
full packet minus the trailer); plain 16-bit additive sums of the same
ranges. **None hold across all four samples.** One additive-sum
combination (`declared-length+body`) appeared to match at first, but
only for one of the four samples - a coincidence, since the two
`"wooooewwwww"` samples share identical text/body and therefore an
identical sum, which can't simultaneously equal both packets' different
trailers. Genuinely ruled out now, not just under-tested. This is the
same open-question *shape* as the ready-toggle message's own trailer
(`lobby/unexplained-bytes-reverse-engineering-plan.md`: "most likely a
checksum or seq-derived value, not further decoded").

### Player-reference hypothesis, checked and not supported

Per explicit request, tested whether the trailer might be a reference to
the sending player's own identity rather than a checksum, by cross-
referencing against a player-announce message from the same connection
as one of the known chat samples. Pulled a 97-byte message from
`archiving/sessions/20260803-3v3-win-loss/session.pcap`, same sender
(connID `0b04`) as the `"here is a chat for..."` sample, at
`frame.time_relative 68.69`:

```
03 05 07 00 0b 04 57 00   wrapper: type=03 byte1=05 seq=0x0007 connID=0x0b04
01 08 02 16 16 4e         sub-header (player-announce shape, distinct
                           from chat's 01 47 04)
00 00 00 24 30 27 00 00   more sub-header / reserved fields
00 00 00 00
7b 37 45 38 46 33 41 41   "{7E8F3AAF-FA4E-4193-856D-0FF7CF10551B}"
46 2d 46 41 34 45 2d 34   - a full DirectPlay player GUID, sent as a
31 39 33 2d 38 35 36 44   38-byte ASCII string (not binary), plus a
2d 30 46 46 37 43 46 31   trailing 00 null terminator
30 35 35 31 42 7d 00
12 00 00 00 00 00 00 00   0x12=18: declared length of the name field
                           below (8 UTF-16 chars incl. null terminator
                           = 18 bytes), then 7 bytes reserved/padding
6e 00 69 00 63 00 6b 00   "nickname" UTF-16LE
6e 00 61 00 6d 00 65 00
00 00                     null terminator for the name string
01 00 00 32 00            trailer - 5 bytes, not 2
```

**Result: doesn't support a direct GUID-reference reading.** The
player's real identity here is a 38-byte ASCII GUID string - nowhere
close to fitting in chat's 2-byte trailer, and player-announce's own
trailer is a different size (5 bytes) from chat's (2 bytes), so the two
aren't the same field reused. This doesn't rule out a *short* reference
(e.g. a small per-session player index or slot number distinct from
connID, which is already redundant with the wrapper's own connID field)
but there's no evidence for it either - no message type observed so far
exposes a candidate short player-index value to cross-reference against.
Ordering/counter hypothesis (a second counter distinct from `seq`)
remains untested - would need several chat messages from the *same*
sender in strict succession with known relative order to check whether
the trailer increments monotonically; none of the currently-archived
samples happen to be consecutive messages from one sender.

**What this means for building the feature**: we can't yet *compute* a
provably-correct trailer for an arbitrary constructed message, and
further hand-analysis of the bytes we already have is largely exhausted
- real checksums are now ruled out with confidence (not just
under-tried), and the one remaining untested hypothesis (a monotonic
counter distinct from `seq`) needs data we don't have (2+ consecutive
sends from one sender), not more arithmetic on the current 4 samples.
Two live-testable hypotheses remain, cheapest first:

1. **The client doesn't validate it at all** - plausible given every
   other confirmed-decoded field in this protocol is address/state data
   the client actually needs, while this sits *after* the declared
   length even claims the message ends, suggesting it's not something
   the receiving side's parser necessarily reads before rendering the
   text. If true, any placeholder (even `00 00`) works.
2. **It's genuinely validated** (a real checksum or reference) and a
   wrong value gets the message silently dropped or the connection
   penalized somehow.

**Recommended next step is the empirical test, not more analysis** - it
answers the practically-load-bearing question (does the feature need
this field correct) regardless of what the field actually encodes, and
is cheaper than capturing a fresh same-sender-consecutive-messages
session to keep chasing the counter hypothesis. See "Test plan" below.

**Resolved for practical purposes, 2026-08-23**: the welcome message
shipped with the trailer set to `00 00` and rendered correctly in a real
client's chat log (see "Confirmed live" above and that session's
`NOTES.md`). Direct empirical support for hypothesis 1 - sufficient to
keep shipping `00 00` as a documented placeholder. This does NOT confirm
what the field actually *encodes* for a real client's own genuine chat
sends (still unknown), only that a receiving client doesn't reject a
message over a wrong/placeholder value here.

## Checked, per explicit request: the official DirectPlay8 spec

Nothing further to find. `docs/directplay8-packet-classification.md`
already established AoM uses classic DirectPlay (Wine's
`dplayx.dll`/`dpwsockx.dll`), not real DirectPlay 8 - the official
`[MC-DPL8CS]`/`[MC-DPL8R]` wire formats don't byte-match at all. Checked
further this pass: `docs/directplay8-reference/wine-source/` (Wine's own
classic-DirectPlay implementation, the actual code translating AoM's
calls to wire bytes) has no chat-specific message structure anywhere in
`dplayx_messages.h`/`.c` - because classic DirectPlay's chat API
(`IDirectPlay4::SendChatMessage`/receiving a `DPCHAT` struct) is just a
thin wrapper over the *generic* application-message-send primitive.
**The `01 47 04`-tagged format above is AoM's own application-level
framing, not a DirectPlay protocol feature** - there is no external spec
to check for it. This project's own byte-level decode is the only
reference that will ever exist for this specific format.

## A second external source, checked and mostly contradicted by our own evidence

A generic writeup was proposed describing AoM's chat packet stack in
different terms - worth recording for traceability, but treat with the
same skepticism `CLAUDE.md`'s already-disproven "TCP 47624 handshake"
hypothesis gets, since it has the same shape: plausible-sounding,
unsourced, and directly contradicted by this project's own real packet
captures where it makes checkable claims.

**Claims that contradict confirmed evidence - treat as wrong for this
project:**
- *"Initial connectivity happens on port 6073... match traffic drops
  into a range dynamically allocated between ports 2300-2400."* Every
  capture in this repo shows fixed UDP 2299 (discovery) and UDP 2300
  (session) - no 6073, no dynamic range. This is the same disproven
  shape as the Voobly-hypothesis's "2300-2400 TCP+UDP range" claim
  already flagged wrong in `CLAUDE.md`'s "Dual client-variant support"
  section (that one was about a proposed TCP handshake; this one
  reuses the same wrong "2300-2400 range" number for UDP instead - a
  suspicious coincidence, more likely a generic/hallucinated detail
  than independently-sourced).
- *"Text strings... are generally sent as Null-Terminated ASCII / UTF-8
  strings."* Directly contradicted - every confirmed sample decoded in
  this doc (and every other UTF-16 field this project has decoded
  elsewhere, e.g. player names) is UTF-16LE, not ASCII/UTF-8.
- The DirectPlay/AoM-header byte-size ranges given ("Approx. 12-20
  bytes" / "Approx. 4-8 bytes") don't match our confirmed 6-byte wrapper
  + 7-byte chat sub-header (13 bytes total, inside that approximate
  range by coincidence, but the claimed internal split doesn't line up
  with what we've actually decoded field-by-field above).

**One claim that's plausible and untested - worth checking, not
assuming:** in-lobby chat rendering formatting tags, e.g. `<color="r,g,b">
text</color>`, `<u>text</u>`, `<icon="...">`. AoM's UI is known to
support rich text rendering in some contexts (unit tooltips, some UI
strings use embedded markup), so a client-side markup scanner on chat
text specifically is plausible even though nothing in this project's own
packet-level work has confirmed or exercised it. **Worth adding to the
welcome-message test plan below** - if true, it's a nice bonus for
making an injected welcome/kick-notice message stand out, but should not
be relied on until actually observed rendering from a synthesized
message.

## Constructing a message: `synthesizeChatMessage`

```go
// cdKeyString-style pattern, same file region as isCDKeyEcho:
func synthesizeChatMessage(connID [2]byte, seq uint16, text string) []byte {
	utf16Text := utf16.Encode([]rune(text))
	strLen := len(utf16Text) + 1 // +1 for the null terminator
	if strLen > 255 {
		// byte 11 is a single byte - see "Declared length" above
		panic("chat message too long for one packet")
	}
	stringBytes := make([]byte, strLen*2)
	for i, r := range utf16Text {
		binary.LittleEndian.PutUint16(stringBytes[i*2:], r)
	}
	// stringBytes[len(utf16Text)*2:] is already zero - the null terminator

	body := make([]byte, 0, 7+len(stringBytes))
	body = append(body, 0x01, 0x47, 0x04, byte(strLen), 0x00, 0x00, 0x00)
	body = append(body, stringBytes...)

	declaredLen := uint16(len(body) + 4)

	out := make([]byte, 0, 8+len(body)+6)
	out = append(out, 0x03, 0x00) // byte1=0x00 arbitrary - unchecked field, see other messages
	out = binary.LittleEndian.AppendUint16(out, seq)
	out = append(out, connID[:]...)
	out = binary.LittleEndian.AppendUint16(out, declaredLen)
	out = append(out, body...)
	out = append(out, 0x00, 0x00, 0x00, 0x00) // fixed trailer prefix
	out = append(out, 0x00, 0x00)             // trailer's open 2 bytes - see above
	return out
}
```

`connID`/`seq` need to be values the *receiving* client will accept as
coming from a legitimate sender on this connection - reuse the same
learned-connID machinery `rewriteToClient`'s other synthesis paths
already depend on (`session.connID`), not an invented value.

## Test plan (do this before the kick feature depends on it)

1. **Welcome message, lowest stakes**: implemented - fires
   `welcomeMessageDelay` (10s) after the landing signal described above,
   reading `"Welcome, <name>!"` (falling back to `"Welcome! Connected to
   <IP>."` if the name isn't known yet) with the trailer's open bytes
   set to `00 00`.
2. **Confirm it renders** in the client's own in-lobby chat log, with
   correct text, no garbling, no visible error.
3. **While confirming rendering, also try one formatting tag** (e.g.
   wrap part of the welcome text in `<color="255,0,0">...</color>`) to
   cheaply check the markup-rendering claim above. Not required for the
   trailer question, just cheap to check in the same live test.
4. **If it renders cleanly**: hypothesis 1 (trailer unvalidated) holds -
   proceed to the kick feature with `00 00` as the standing placeholder
   value, documented clearly as "known unvalidated field, not a real
   checksum we compute."
5. **If it doesn't render / connection misbehaves**: hypothesis 2 holds
   - stop, this needs the trailer actually cracked (wider sample
   collection + the same programmatic-diff technique already used for
   the ready-toggle trailer, plus the untested ordering hypothesis
   above) before the kick feature is safe to build on top of.

## Used by

`lobby/lonely-player-kick-design.md` depends on this doc's
`synthesizeChatMessage` and the resolved-or-not trailer question before
its own kick-reason message can be sent safely.

## History

**First live test (2026-08-23)**: welcome message sent with `seq =
sess.hostSeq + 1000` and `welcomeMessageDelay = 3s`. Sent cleanly (no
socket error) but never appeared in the client's own chat log. Leading
suspect, not packet-captured in isolation: classic DirectPlay is a
reliable-UDP layer (sequencing + ACKs), so jumping 1000 ahead of the
host's own real numbering plausibly looked like a gap of 999 missing
packets to the client's own ordering logic - buffered indefinitely,
never reaching the chat renderer, with no visible symptom. Fixed by
using `seq = sess.hostSeq + 1` (continuing the host's own real
numbering, same as its actual next packet would use).
`welcomeMessageDelay` was also raised to 10s in the same pass, at the
user's request as extra margin - precautionary, not itself the fix.

**Second live test (2026-08-23, same day)**: with both fixes, the
message rendered correctly. See
`archiving/sessions/20260823-welcome-message-chat-injection/`.

**Player-name personalization (2026-08-23, same day)**: swapped the
message from `"Welcome! Connected to <IP>."` to `"Welcome, <name>!"`,
decoding the client's own player-announce message
(`isPlayerAnnounce`/`playerAnnounceName`) - based on a single real
sample, riskier than chat's five-sample decode. Live-tested against two
independent real clients, both correctly personalized
(`"Welcome, clienta!"` / `"Welcome, clientb!"`) - the single-sample
decode held up beyond the one capture it was built from. IP fallback
implemented but not yet exercised live (both names arrived well within
the delay in this test).
