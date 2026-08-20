// aom-lobby is a UDP reverse proxy that sits in front of the real AoM
// game pod's DirectPlay8 ports. It relays traffic to/from the real host
// untouched, except for packets that embed an address the sender believes
// is its own (see docs/directplay8-protocol.md): it rewrites those to
// point at itself, so a client outside the cluster is always told to talk
// back to the proxy rather than the pod's internal address — on the
// discovery port (2299) as well as the session port (2300), whose
// handshake packets leak the same kind of address if left alone.
//
// # Protocol layering and how it maps to the official DirectPlay 8 docs
//
// AoM (2002) predates real DirectPlay 8 (shipped with DirectX 8, 2000) and
// uses "classic DirectPlay" instead (Wine modules dpwsockx.dll/dplayx.dll,
// not dpnet.dll) - confirmed 2026-08-10, see docs/directplay8-protocol.md.
// Microsoft's official Open Specifications for real DirectPlay 8
// ([MC-DPL8CS] "Core and Service Providers", [MC-DPL8R] "Reliable" -
// fetched into docs/directplay8-reference/, gitignored, not checked in)
// do NOT byte-match anything below - classic DirectPlay's wire format is
// undocumented and was reverse-engineered from scratch (see
// docs/directplay8-protocol.md). What the official docs DO give us is a
// same-vendor, same-problem-space structural reference: DirectPlay 8 is
// an evolution of the same design lineage, and its documented layering
// and message *sequences* line up suspiciously well with what's been
// independently reverse-engineered here. Every cross-reference below is
// exactly that - a structural hypothesis worth knowing about, not a
// confirmed byte-for-byte mapping. See
// docs/directplay8-packet-classification.md for the full writeup.
//
// Two layers, matching the official docs' own split into two documents:
//
//  1. A reliable-delivery layer, roughly analogous to [MC-DPL8R]'s DFRAME
//     (data frame) structure (section 2.2.2): bCommand(1)+bControl(1)+
//     bSeq(1)+bNRcv(1), a FIXED 4-byte header, followed by optional
//     SACK/send-mask/signature/session-ID fields (gated by bControl
//     flags), followed by the upper-layer payload. This proxy's own
//     comments have historically mislabeled this as a "03 00 <seq>
//     <conn-id>" 6-byte wrapper (2-byte type + 2-byte seq + 2-byte
//     conn-id) - the offsets used for detection/rewriting throughout
//     this file are all empirically confirmed against real captures and
//     are NOT in question, but the semantic labels likely are: what's
//     been called "seq" (2 bytes) is more likely bSeq+bNRcv as two
//     separate 1-byte fields (matching independently-observed
//     single-byte-incrementing counters in capture analysis - see
//     docs/directplay8-protocol.md), and what's been called "conn-id"
//     (2 bytes right after) is more likely just the start of the
//     upper-layer payload below, not a reliable-layer field at all.
//     [MC-DPL8R] also documents CFRAMEs (command frames, no payload -
//     CONNECT/CONNECTED/CONNECTED_SIGNED/HARD_DISCONNECT/SACK) as a
//     separate message class from DFRAMEs; this proxy has never observed
//     or needed to distinguish those explicitly, since every message
//     handled below already carries payload (the type-0x00/0x02 session
//     handshake and everything under the "03 00" wrapper).
//  2. An upper-layer payload, analogous to [MC-DPL8CS] "Core and Service
//     Providers" - session/name-table management (connect, add player,
//     instruct-connect, disconnect, etc.). This is the layer where every
//     message this file actually parses lives - see each function's own
//     doc comment for its specific [MC-DPL8CS] cross-reference, where one
//     exists.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

const (
	sockaddrLen = 16
	idleTimeout = 30 * time.Second
	reapEvery   = 10 * time.Second
	bufSize     = 2048

	probeInterval = 3 * time.Second
	probeTimeout  = 1 * time.Second

	// otherSessionPollTimeout/Interval bound pollOtherSessionClientAddr's
	// retry loop - see that method's doc comment. 5s is generous relative
	// to how close together both real clients' sessions were observed
	// landing in every capture so far (well under a second), while still
	// bounded so a genuinely-solo session (host probe, or a client that
	// never gets a second peer) doesn't block indefinitely.
	otherSessionPollTimeout  = 5 * time.Second
	otherSessionPollInterval = 100 * time.Millisecond
)

// discoveryAddressOffsets gives the byte offset(s) within a UDP 2299
// packet, keyed by the message-type byte at offset 0, of embedded
// sockaddr_in blocks that carry the real host address and need rewriting
// before the packet reaches a client. Every other message type (0x25
// query, 0x20 client ping) is forwarded byte-for-byte unmodified.
//
// Likely official-DP8 analog (structural, not byte-confirmed - see this
// file's top-of-file "Protocol layering" comment): [MC-DPL8CS] section
// 1.2.2 lists a *third*, separate informative reference alongside Core
// and Reliable - [MC-DPLHP], "DirectPlay 8 Host and Port Enumeration
// Protocol" - for exactly this job (LAN host discovery, port 2299's
// role here). Not fetched/read as part of this pass; if picking up
// discovery-port work again, that document is the one to read first,
// same way [MC-DPL8CS] turned out to be for the session-port work below.
var discoveryAddressOffsets = map[byte][]int{
	0x26: {5, 21}, // discovery/Direct-Connect reply: two duplicate blocks
	// Ping reply/request is the same 41-byte shape as 0x26 minus the
	// trailing name string: header + two back-to-back sockaddr_in blocks,
	// not one - confirmed 2026-08-07 via a raw hex dump showing offset 21
	// still carrying the real, unrewritten address in both directions.
	// 0x21 (host->client) leaking the backend pod's internal IP explains a
	// client silently refusing to Join (it has every reason to pre-check
	// reachability of an address embedded in the reply it just received).
	// 0x20 (client->backend) leaking the client's own real address is the
	// other half of the same bug: left unrewritten, the backend pod learns
	// the client's real address directly from this packet and later
	// starts sending UDP 2300 session data straight to it - bypassing this
	// proxy entirely (see the session proxy's backend-initiated-relay
	// logic below, which exists specifically to handle that).
	0x21: {5, 21},
	0x20: {5, 21},
}

// UDP 2300's session/join handshake packet: an 8-byte header followed by
// two back-to-back 16-byte sockaddr_in blocks — the sender's own address
// first ("self"), then whoever it believes it's talking to ("peer").
// Confirmed against a live capture 2026-08-05 (see
// docs/directplay8-protocol.md): the "self" block is what the receiving
// side's DirectPlay8 stack uses as the destination for its own subsequent
// sends, bypassing this proxy entirely if left unrewritten.
//
// Likely official-DP8 analog (structural, not byte-confirmed - see this
// file's top-of-file "Protocol layering" comment): type 0x00 ("open") and
// type 0x02 (ack) never carry the "03 00" settings-sync wrapper seen
// everywhere else on this port (they're a flat 40 bytes with no such
// prefix) - i.e. they sit *outside* whatever this proxy's reliable-layer
// wrapper is, structurally the same way [MC-DPL8R]'s CFRAMEs (CONNECT/
// CONNECTED/CONNECTED_SIGNED, section 2.2.1) are a distinct class from
// payload-carrying DFRAMEs, distinguished by not having
// PACKET_COMMAND_DATA set. [MC-DPL8R]'s own sample connection sequence
// (section 4.1) is CFRAME CONNECT -> CFRAME CONNECTED (x2, one each
// direction) -> DFRAME KeepAlive both ways - a 2-step connect handshake
// before any reliable-layer traffic, versus classic DirectPlay's
// documented 4-step 0x00/0x00/0x02/0x02 (see
// docs/directplay8-protocol.md's "Connection-establishment handshake has
// 4 steps, not 2" section) - different step count, same shape of idea
// (a short unwrapped handshake precedes the wrapped data layer).
//
// Correction (2026-08-07): originally believed "peer" never needed
// touching, on the assumption each hop rewriting its own outgoing "self"
// would keep "peer" correct by the time it's read. That holds for a
// single-hop relay but not this proxy's actual two-hop path
// (client<->lobby<->backend): the lobby rewrites "self" on both legs, but
// "peer" just gets echoed through unchanged end to end, so by the time it
// reaches the client it's carrying the lobby's own internal
// backend-dialed ephemeral port - not the client's real address - because
// that's what the *backend* saw as "self" one hop earlier. A client doing
// any kind of "does the host's record of my address match my own"
// handshake validation would reject that and keep retrying forever,
// which is exactly the symptom this fixes (an endless "Attempting to
// Connect" with no visible error). The backend->client leg now also
// rewrites "peer" to the real client's address.
const (
	sessionHandshakeLen = 40
	sessionSelfOffset   = 8
	sessionPeerOffset   = 24
)

// newPeerBroadcast is the settings-sync sub-message (nested inside the
// 6-byte "03 00 <seq> <conn-id>" wrapper shared by that whole layer - see
// docs/directplay8-protocol.md's "Post-handshake session-settings sync"
// section) that the host sends to each already-connected peer whenever a
// new peer joins, carrying the new peer's real address. Confirmed
// 2026-08-08 (docs/directplay8-protocol.md's "Sessions are genuinely
// peer-to-peer" section) as the one message that seeds every
// peer-to-peer session - see docs/multi-peer-routing-design.md.
//
// Likely official-DP8 analog (structural, not byte-confirmed - see this
// file's top-of-file "Protocol layering" comment): [MC-DPL8CS] section
// 2.2.1.7 DN_ADD_PLAYER - "sent from the host, instructs peers to add
// the specified peer to the game session," carrying the new peer's name
// table entry (dpnid, name, url - an addressing structure). Per
// [MC-DPL8CS] Figure 6 ("Peer-to-Peer Connect Sequence," section
// 3.1.5.2), the host sends this to already-connected peers at the SAME
// moment it responds to the connecting peer with DN_SEND_CONNECT_INFO -
// matching newPeerBroadcast's own confirmed timing exactly (fired
// alongside the new peer's own join, not on any later trigger).
//
// Rewritten live (2026-08-10) to point at a dedicated A<->B pair relay
// (see newPairRelay/matchState below) instead of just logged: confirmed
// via a real proxied 2-client join test that leaving this unrewritten
// embeds this proxy's own fixed session-port address
// (cfg.publicIP:cfg.publicPort) for every client, because that's what
// sessionProxy's existing "self" rewrite always substitutes regardless
// of which real client sent the original packet - the backend can never
// record a distinguishing address for any client under the old behavior,
// so the existing peer was always being told to reconnect to the proxy
// itself, not to the new peer. That's the root cause of the second
// client's indefinite "Attempting to Connect" hang.
//
// Correction (2026-08-10): docs/directplay8-protocol.md describes this as
// nested inside an "8-byte wrapper" and gives the sockaddr offsets (13,
// 29) as relative to after that wrapper is stripped - but the wrapper it
// actually diagrams is only 6 bytes (2-byte type + 2-byte seq + 2-byte
// conn-id). Confirmed against a real captured packet (2026-08-10 2-client
// join test): sub-type sits at absolute offset 6, and the doc's "13"/"29"
// are already absolute packet offsets, not relative to a stripped 8-byte
// wrapper - the doc's own parenthetical about the 8-byte wrapper is the
// part that's wrong.
const (
	newPeerWrapperType0  = 0x03
	newPeerSubType0      = 0x29
	newPeerSubType1      = 0x00
	newPeerSubTypeOffset = 6 // right after the 6-byte 03 00/seq/conn-id wrapper

	// Absolute offsets into the raw packet of the two back-to-back
	// sockaddr_in blocks carrying the new peer's address - see the
	// correction above.
	newPeerAddrOffset1 = 13
	newPeerAddrOffset2 = 29

	// Minimum length to safely read the sub-type tag and both sockaddr_in
	// blocks without a short-packet panic.
	newPeerBroadcastMinLen = newPeerAddrOffset2 + sockaddrLen
)

// isNewPeerBroadcast reports whether payload is a session-port (0x03
// wrapper) settings-sync message with the nested 0x29 sub-type - see
// newPeerBroadcast above.
//
// Correction (2026-08-20): this used to also require payload[1] == 0x00,
// on the assumption the wrapper's second byte was a fixed part of the
// "type" marker. It isn't - see docs/directplay8-protocol.md's "Common
// wrapper" correction for the full evidence (seq, at bytes 2-3,
// increments perfectly independent of byte 1's value; connection ID
// stays fixed at bytes 4-5 regardless of it too). Requiring it silently
// dropped genuine 0x29 broadcasts whenever byte 1 happened to be
// nonzero - confirmed via a live node-level capture showing the real
// host sending one with byte 1 = 0x33, and the exact same failure
// already present, undetected, in the archived 2026-08-12 baseline this
// project has always cited as "confirmed working." Only byte 0 and the
// subtype are load-bearing for identifying this message.
func isNewPeerBroadcast(payload []byte) bool {
	if len(payload) < newPeerBroadcastMinLen {
		return false
	}
	return payload[0] == newPeerWrapperType0 &&
		payload[newPeerSubTypeOffset] == newPeerSubType0 && payload[newPeerSubTypeOffset+1] == newPeerSubType1
}

// sockaddrInAddr reads the sin_port/sin_addr fields (big-endian, matching
// rewriteSessionField's write side) out of the sockaddr_in block at
// offset, for logging purposes only.
func sockaddrInAddr(payload []byte, offset int) (net.IP, uint16) {
	port := binary.BigEndian.Uint16(payload[offset+2 : offset+4])
	ip := net.IP(append([]byte(nil), payload[offset+4:offset+8]...))
	return ip, port
}

// rewriteNewPeerBroadcast overwrites both embedded sockaddr_in blocks in a
// detected 0x29 broadcast with relayIP:relayPort - the dedicated A<->B
// pair relay's own address (see newPairRelay) - so the existing peer
// dials the relay instead of whatever address the backend actually
// recorded for the new peer (which, per newPeerBroadcast's doc comment,
// is never usable directly).
func rewriteNewPeerBroadcast(payload []byte, relayIP [4]byte, relayPort uint16) []byte {
	out := rewriteSessionField(payload, newPeerAddrOffset1, relayIP, relayPort)
	return rewriteSessionField(out, newPeerAddrOffset2, relayIP, relayPort)
}

// newPeerBroadcastTemplate/existingPeerBroadcastTemplate and the
// startNewPeerWatchdog synthesis fallback that used them were removed
// 2026-08-20. They existed to paper over the genuine 0x29/
// existingPeerBroadcast apparently never arriving for some clients -
// resolved that same day: it always arrived, but isNewPeerBroadcast/
// isExistingPeerBroadcast wrongly required the wrapper's byte 1 to be
// 0x00, which isn't a real constant (see docs/directplay8-protocol.md's
// "Common wrapper" correction) and silently rejected the genuine message
// whenever it wasn't. With that detection fixed, the reactive rewrite
// path (rewriteToClient's isNewPeerBroadcast/isExistingPeerBroadcast
// branches, a few dozen lines below) catches every real broadcast
// directly - no synthesized fallback needed. See
// docs/multi-peer-routing-design.md's "Update (2026-08-20)" section for
// the full investigation.

// existingPeerBroadcast is the settings-sync sub-message counterpart to
// newPeerBroadcast above, going the *other* direction: the host sends
// this to a *newly-joined* peer (B), telling it the real address of an
// *already-connected* peer (A) - the piece that was missing entirely
// until 2026-08-10. Earlier notes (docs/directplay8-protocol.md's
// "Sessions are genuinely peer-to-peer" section, 2026-08-08) concluded
// "the new peer is never told about existing peers... it just listens" -
// that was wrong, or at least incomplete: this message was missed
// because it doesn't share newPeerBroadcast's shape (87 bytes here vs 51
// for 0x29, and a longer, still-unclear header before the address), not
// because it doesn't exist. Found by re-examining the archived
// 2026-08-08 genuine 3-client capture
// (archiving/sessions/20260808-234138-3client-p2p-check/host-capture.pcap):
// B receives this from the host ~138ms before B sends its own outbound
// session-handshake to A - clearly reactive, not coincidental.
//
// Confirmed shape (host->B, 87 bytes total):
//
//	03 00 <seq> <conn-id>              common wrapper (see newPeerBroadcast)
//	4d 00                              unclear (length/flags? matches remaining-byte-count loosely)
//	15 02 00 00 00 ... (36 bytes)      unclear, no variable-length strings observed -
//	                                    fixed-width, unlike the player-announce/map-name
//	                                    sub-messages, which is what makes a fixed
//	                                    absolute-offset rewrite safe here
//	<sockaddr_in>                      existing peer's (A's) real address, at
//	<sockaddr_in>                      absolute offsets 49 and 65 - duplicated,
//	                                    same pattern as every other address message
//	02 00 00 00 00 00                  unclear trailer
//
// Not yet re-decoded against a fresh capture with this understanding in
// hand - the sub-type/length markers above are best-effort labels, not
// confirmed semantics. What's confirmed is the address offsets and the
// 87-byte total length, both directly measured from the real packet.
//
// Likely official-DP8 analog (structural, not byte-confirmed - see this
// file's top-of-file "Protocol layering" comment): [MC-DPL8CS] section
// 2.2.1.9 DN_INSTRUCT_CONNECT - "instructs a peer to connect to a
// designated peer," sent by the host to BOTH the connecting peer and
// existing peers simultaneously per Figure 6 (section 3.1.5.2), not just
// the existing side - which is exactly the missing-direction bug this
// message and its detection/rewrite exist to fix (see the "Correction"
// paragraph above). One structural mismatch worth flagging: DN_INSTRUCT_
// CONNECT itself only carries a dpnid (numeric player ID), no address -
// the spec's model has the connecting peer already holding every
// existing peer's address from its own earlier DN_SEND_CONNECT_INFO
// reply (2.2.1.4), with DN_INSTRUCT_CONNECT just the "go now" signal.
// Classic DirectPlay's existingPeerBroadcast instead embeds the address
// directly in this one message - a plausible protocol-family difference
// rather than a reason to doubt the mapping, since the confirmed
// behavior (host actively tells the new peer about the existing one,
// timed with the new peer's own join) matches regardless of which side
// carries the address bytes.
//
// 2026-08-12 (was unresolved when written): a live 2-client join
// reproducibly showed a same-shaped 51-byte 0x29 packet reaching the
// EXISTING peer's own network interface with this proxy's rewrite never
// having touched it, theorized at the time as the backend pod bypassing
// this process's socket entirely via a raw pod-network route. **Resolved
// 2026-08-20, and that theory was wrong**: it wasn't a bypass at all -
// the packet genuinely arrived at this process's own socket every time,
// but isNewPeerBroadcast/isExistingPeerBroadcast's byte-1 check silently
// rejected it. See docs/directplay8-protocol.md's "Common wrapper"
// correction for the full evidence trail.
const (
	existingPeerWrapperType0 = 0x03

	// Absolute offsets into the raw packet of the two back-to-back
	// sockaddr_in blocks carrying the existing peer's address.
	existingPeerAddrOffset1 = 49
	existingPeerAddrOffset2 = 65

	// Exact length rather than a minimum: unlike newPeerBroadcast, this
	// message has no observed variable-length fields (no nickname/session
	// name), so 87 bytes appears to be constant - matched exactly here to
	// keep the false-positive rate low given the offset/subtype semantics
	// above aren't fully understood yet. Revisit if a real capture ever
	// shows a same-shaped message at a different length.
	existingPeerBroadcastLen = 87
)

// isExistingPeerBroadcast reports whether payload is a session-port
// (0x03 wrapper) settings-sync message matching existingPeerBroadcast's
// shape above. The length match already does most of the work; the
// sockaddr sin_family sanity check (mirroring isSessionHandshake) guards
// against the remaining risk of a same-length gameplay/tick packet
// coincidentally matching.
func isExistingPeerBroadcast(payload []byte) bool {
	if len(payload) != existingPeerBroadcastLen {
		return false
	}
	// byte 1 is deliberately not checked here - see this const block's
	// own 2026-08-20 correction and isNewPeerBroadcast's doc comment.
	if payload[0] != existingPeerWrapperType0 {
		return false
	}
	return binary.LittleEndian.Uint16(payload[existingPeerAddrOffset1:existingPeerAddrOffset1+2]) == 2 &&
		binary.LittleEndian.Uint16(payload[existingPeerAddrOffset2:existingPeerAddrOffset2+2]) == 2
}

// rewriteExistingPeerBroadcast overwrites both embedded sockaddr_in
// blocks in a detected existingPeerBroadcast message with
// relayIP:relayPort - see rewriteNewPeerBroadcast, same mechanism,
// different offsets.
func rewriteExistingPeerBroadcast(payload []byte, relayIP [4]byte, relayPort uint16) []byte {
	out := rewriteSessionField(payload, existingPeerAddrOffset1, relayIP, relayPort)
	return rewriteSessionField(out, existingPeerAddrOffset2, relayIP, relayPort)
}

// synthesizeLobbyFullRejection builds the packet a joiner's client needs
// to display "Error Joining: Host game is full" - see
// lobby/packet-handling-design.md's "Case 3 in detail" section for the
// confirmed 2026-08-15 capture this is built from
// (archiving/sessions/20260815-*-lobby-full-rejection/). connID here is
// NOT the client's own connID (unlike the new-peer-broadcast watchdog's
// use of session.connID) - a wrapper's conn-ID identifies its *sender*
// (confirmed against the capture: the real rejection used the host's
// own connID, "6f11", not the joiner's "e7f5"), and aom-lobby is the
// sender here, impersonating a host it has no real backend for. So this
// is an arbitrary value aom-lobby invents and stays consistent about,
// not one learned from anything - see fakeHostConnID. seq is this
// synthesized message's own sequence number in the (fake) exchange with
// this client - 0 if this is the first (and, for now, only) synthesized
// message sent, matching a rejection-only attempt with no preamble.
//
// Resolved (2026-08-15/16): yes, this needs to be preceded by a
// synthesized 0x00 self/peer-open + 0x02 ack + name-broadcast exchange
// (what a real host's rejection was preceded by in the reference
// capture) - rejection-only, sent as an immediate reply with no
// preamble, was tried first and confirmed NOT to work live (client sat
// in "Attempting to Connect" indefinitely). simulateSessionPacket +
// beginSimulatedHostOpen/beginSimulatedNameBroadcast build that full
// preceding exchange today. The 13-byte field this builds (`22 07` then
// 11 zero bytes) plus 2 further trailing zero bytes is copied
// byte-for-byte from the real capture - not decoded further than "this
// is the rejection," doesn't need to be understood to be reproduced
// correctly.
// fakeHostConnID is the arbitrary, self-chosen connID aom-lobby uses
// whenever it needs to impersonate a host it has no real backend for -
// see synthesizeLobbyFullRejection's own doc comment on why this is
// invented rather than learned. Any consistent value should work as
// well as any other (the real capture's own "6f11" is itself just
// whatever the game engine happened to assign, not a meaningful
// protocol constant) - picked to be visually distinct in a hex dump
// from real captured examples, not for any deeper reason.
var fakeHostConnID = [2]byte{0xfa, 0xed}

func synthesizeLobbyFullRejection(connID [2]byte, seq uint16) []byte {
	payload := []byte{
		0x03, 0x00, 0x00, 0x00, 0x00, 0x00, // wrapper: type, seq (patched below), conn-id (patched below)
		0x0d, 0x00, // length prefix = 13
		0x22, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 13-byte field the length prefix declares
		0x00, 0x00, // 2 trailing bytes beyond the declared length, present in the real capture
	}
	binary.LittleEndian.PutUint16(payload[2:4], seq)
	copy(payload[4:6], connID[:])
	return payload
}

// readyToggle is the client->host settings-sync sub-message a client
// sends when its in-lobby Ready checkbox is toggled - see
// docs/directplay8-protocol.md's "Ready-toggle sub-message" section for
// how this was found (by packet-length frequency analysis against a live
// 2-real-client session: routine settings-sync/heartbeat traffic is
// entirely 10/12/16 bytes, and toggling Ready reliably produced a single
// 22-byte outlier each time). No address embedded, so unlike
// newPeerBroadcast/existingPeerBroadcast there's nothing to rewrite here
// - just something to detect (see matchState.setReady below, which
// drives triggerMatchStart via input-agent once both real clients read
// ready - built 2026-08-11, docs/multi-peer-routing-design.md's
// "Ready-up automation" item).
//
// No official-DP8 analog expected, unlike every other message this file
// parses: [MC-DPL8CS]'s own message set (section 2.2) is exhaustively
// session/name-table management - connect, disconnect, groups, update
// info - with no concept of a player-ready flag anywhere in it. That
// tracks: "ready to start" is game-defined semantics riding as opaque
// data *payload*, not something either the reliable layer or the
// session-management layer has any reason to know about. This is the
// one message in this file that's genuinely all classic-AoM, not
// DirectPlay-anything.
//
// Shares its `01 08 02 <2 bytes>` sub-header with the player-announce
// sub-message documented above it - the trailing 2 bytes are a stable
// per-connection value (confirmed against 2 real clients: one
// consistently `16 16`, the other `17 17`), not something worth matching
// on here since it varies per client. What's matched instead is the
// constant run at readyToggleConstOffset (`03 00 00 00 00 02`, identical
// across both clients and both ready states in every capture) plus the
// fixed total length - the boolean itself lives at readyToggleBoolOffset,
// immediately after that constant run.
const (
	readyToggleLen           = 22
	readyToggleSubType0      = 0x01
	readyToggleSubType1      = 0x08
	readyToggleSubType2      = 0x02
	readyToggleSubTypeOffset = 8
	readyToggleConstOffset   = 13
	readyToggleBoolOffset    = 19
)

var readyToggleConst = []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x02}

// isReadyToggle reports whether payload is a session-port (0x03 wrapper)
// settings-sync message matching readyToggle's shape above.
//
// byte 1 deliberately not checked - see isNewPeerBroadcast's 2026-08-20
// doc comment; it isn't part of a fixed type marker and requiring it
// drops genuine matches whenever it's nonzero.
func isReadyToggle(payload []byte) bool {
	if len(payload) != readyToggleLen {
		return false
	}
	if payload[0] != newPeerWrapperType0 {
		return false
	}
	if payload[readyToggleSubTypeOffset] != readyToggleSubType0 ||
		payload[readyToggleSubTypeOffset+1] != readyToggleSubType1 ||
		payload[readyToggleSubTypeOffset+2] != readyToggleSubType2 {
		return false
	}
	return bytes.Equal(payload[readyToggleConstOffset:readyToggleConstOffset+len(readyToggleConst)], readyToggleConst)
}

// readyToggleState reads the ready boolean out of a payload already
// confirmed by isReadyToggle - true means the client just marked itself
// ready, false means it just unmarked itself.
func readyToggleState(payload []byte) bool {
	return payload[readyToggleBoolOffset] == 0x01
}

func isSessionHandshake(payload []byte) bool {
	if len(payload) != sessionHandshakeLen {
		return false
	}
	// Both blocks' sin_family (offsets 8 and 24) must read AF_INET (2,
	// little-endian) - a cheap sanity check against mistaking a
	// same-length gameplay tick packet for a handshake.
	return binary.LittleEndian.Uint16(payload[8:10]) == 2 &&
		binary.LittleEndian.Uint16(payload[24:26]) == 2
}

// rewriteSessionField overwrites the sin_port/sin_addr fields of the
// sockaddr_in block at the given offset (sessionSelfOffset or
// sessionPeerOffset) with ip:port. sin_family and the still-unexplained
// sin_zero bytes are left untouched.
func rewriteSessionField(payload []byte, offset int, ip [4]byte, port uint16) []byte {
	out := append([]byte(nil), payload...)
	binary.BigEndian.PutUint16(out[offset+2:offset+4], port)
	copy(out[offset+4:offset+8], ip[:])
	return out
}

// wrapperConnID extracts the 2-byte conn-ID from a "03 00 <seq> <conn-id>"
// settings-sync wrapper (see this file's top-of-file "Protocol layering"
// comment), if payload is shaped like one. Confirmed stable per-sender
// for the life of a session across every capture this project has taken
// (docs/directplay8-protocol.md, packet-handling-design.md) - used
// 2026-08-12 onward to attribute an unprompted backend broadcast to the
// right client by packet content (see session.connID,
// relayBackendInitiated) instead of guessing by IP recency.
//
// byte 1 deliberately not checked - see isNewPeerBroadcast's 2026-08-20
// doc comment.
func wrapperConnID(payload []byte) (id [2]byte, ok bool) {
	if len(payload) < 6 || payload[0] != 0x03 {
		return id, false
	}
	copy(id[:], payload[4:6])
	return id, true
}

// discoveryRewrite returns a copy of payload with the sin_port/sin_addr
// fields of any embedded sockaddr_in blocks (per discoveryAddressOffsets)
// replaced with the proxy's public address. sin_family and the still-
// unexplained sin_zero bytes are left untouched rather than guessed at.
func discoveryRewrite(payload []byte, cfg config) []byte {
	if len(payload) == 0 {
		return payload
	}
	offsets, ok := discoveryAddressOffsets[payload[0]]
	if !ok {
		return payload
	}
	out := append([]byte(nil), payload...)
	for _, off := range offsets {
		if off+sockaddrLen > len(out) {
			log.Printf("discovery packet type 0x%02x too short (%d bytes) for sockaddr_in at offset %d, leaving unmodified", payload[0], len(payload), off)
			continue
		}
		binary.BigEndian.PutUint16(out[off+2:off+4], cfg.publicPort)
		copy(out[off+4:off+8], cfg.publicIP[:])
	}
	return out
}

// simulatedFullLobbyGameName is the game name shown in the fake 0x26
// reply synthesizeEmptyPoolDiscoveryReply builds - sent whenever a
// client hits the "no eligible host" path in forwardToBackend. Named
// after host-flow.md's own "TheIP" nickname convention for the real
// auto-hosted lobby, so it looks consistent with a genuine one.
const simulatedFullLobbyGameName = "TheIP's Game"

// ourOwnSinZero is the sin_zero pattern aom-lobby uses whenever it
// builds a sockaddr block describing its OWN address (host
// impersonation) - see synthesizedSockaddr's doc comment for why this
// is safe to leave arbitrary while a real client's own sin_zero (used
// for blocks describing THAT client) is not. This specific 8-byte value
// is just the first one this codebase happened to observe in a real
// capture (2026-08-15) - no more "correct" than any other, since
// nothing else is expected to independently know or check it.
var ourOwnSinZero = [8]byte{0x18, 0xa0, 0x4e, 0x06, 0x20, 0xa0, 0x4e, 0x06}

// synthesizedSockaddr builds one 16-byte sockaddr_in block for ip:port,
// for use in the synthesized-reply functions below.
//
// sinZero (bytes 8-15): **not a fixed or per-role constant** - corrected
// 2026-08-18 after a Direct-Connect-only capture
// (archiving/sessions/20260818-*-directconnect-only-capture/NOTES.md)
// caught self-block and peer-block sin_zero genuinely differing within
// one real host's own 0x00 message, and proved the peer-block value was
// an EXACT 8-byte echo of what the client itself had reported as its
// own sin_zero in its earlier 0x20 ping - not something the host
// invents. Two earlier reference captures (2026-08-15, 2026-08-17)
// showed self-block and peer-block sin_zero as identical within one
// message and were misread as "a fixed pattern" - in hindsight, that
// was because host and joiner's own independently-generated tags
// happened to coincide in both of those specific sessions, not because
// the field doesn't vary. Callers describing THIS proxy's own address
// should pass ourOwnSinZero (arbitrary and safe - nobody else has
// independently generated our tag to check it against). Callers
// describing a REAL CLIENT's address must pass that client's own
// self-reported sin_zero, learned from its 0x20 ping (see
// onDiscoveryPingNoHost/beginSimulatedHostOpen) - substituting anything
// else is a confirmed-wrong value a real client's own validation can
// trivially catch.
func synthesizedSockaddr(ip [4]byte, port uint16, sinZero [8]byte) []byte {
	sockaddr := make([]byte, sockaddrLen)
	binary.LittleEndian.PutUint16(sockaddr[0:2], 2) // sin_family = AF_INET
	binary.BigEndian.PutUint16(sockaddr[2:4], port)
	copy(sockaddr[4:8], ip[:])
	copy(sockaddr[8:16], sinZero[:])
	return sockaddr
}

// synthesizeEmptyPoolDiscoveryReply builds a fake 0x26 discovery reply
// from scratch - sent whenever forwardToBackend finds no eligible host
// (empty pool, or every host full), so a client always sees a joinable-
// looking game rather than silence. Byte layout matches
// discoveryAddressOffsets' 0x26 entry and
// docs/directplay8-protocol.md's own confirmed format: 5-byte header,
// two duplicate 16-byte sockaddr_in blocks (both pointing at this
// proxy's own public address, same as a real rewritten reply would),
// then a 4-byte little-endian byte-length prefix and the UTF-16LE game
// name + null terminator.
func synthesizeEmptyPoolDiscoveryReply(cfg config) []byte {
	nameUTF16 := utf16.Encode([]rune(simulatedFullLobbyGameName))
	var nameBytes []byte
	for _, u := range nameUTF16 {
		nameBytes = binary.LittleEndian.AppendUint16(nameBytes, u)
	}
	nameBytes = binary.LittleEndian.AppendUint16(nameBytes, 0) // null terminator

	sockaddr := synthesizedSockaddr(cfg.publicIP, cfg.publicPort, ourOwnSinZero)
	out := []byte{0x26, 0x01, 0x00, 0x00, 0x00}
	out = append(out, sockaddr...)
	out = append(out, sockaddr...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(nameBytes)))
	out = append(out, nameBytes...)
	return out
}

// synthesizeEmptyPoolPingReply builds a fake 0x21 liveness-ping reply -
// see docs/directplay8-protocol.md's "Type 0x20 / 0x21" section: client
// and host exchange this, once per LAN-browse refresh cycle, *separately*
// from the 0x25/0x26 enumerate exchange - same double-sockaddr shape as
// 0x26 but no trailing name string, 41 bytes total (5-byte header + two
// 16-byte sockaddr_in blocks + 4 trailing zero bytes).
//
// This is the piece the first SIMULATE_FULL_LOBBY attempt was missing
// (2026-08-15): the code originally answered *every* query type - both
// the client's 0x25 enumerate query and its separate 0x20 ping query -
// with the same synthesized 0x26 shape regardless, since nothing
// inspected the incoming payload's own type byte. A real client sending
// a 0x20 and getting a 0x26-shaped reply back is receiving something
// its ping-handling logic has no reason to expect - a real host never
// does this. Plausible explanation for why the game never showed as
// joinable despite the 0x26 reply itself being byte-correct.
func synthesizeEmptyPoolPingReply(cfg config) []byte {
	sockaddr := synthesizedSockaddr(cfg.publicIP, cfg.publicPort, ourOwnSinZero)
	out := []byte{0x21, 0x01, 0x00, 0x00, 0x00}
	out = append(out, sockaddr...)
	out = append(out, sockaddr...)
	out = append(out, 0x00, 0x00, 0x00, 0x00)
	return out
}

// synthesizedSessionUnclearConstant is the 4 bytes that sit between a
// 0x00 self/peer-open message's connID and its first sockaddr_in block
// (offset 4-7) - identical across every real capture this project has
// taken, still not decoded further than "always this value" (see
// sessionSelfOffset's own doc comment). Reproduced exactly rather than
// guessed at, same posture as every other still-unclear field.
var synthesizedSessionUnclearConstant = []byte{0xd8, 0xf6, 0x32, 0x00}

// synthesizeSelfPeerOpen builds a fake 0x00 self/peer-open message - the
// same message type sessionSelfOffset/sessionPeerOffset/rewriteSessionField
// already handle everywhere else in this file (see sessionHandshakeLen's
// own doc comment), just built from scratch here instead of rewriting a
// real backend's message. connID is aom-lobby's own invented identity
// (fakeHostConnID) - see synthesizeLobbyFullRejection's doc comment for
// why this is invented rather than learned. selfIP/selfPort is this
// proxy's own public address (what it's claiming as "the host");
// peerIP/peerPort is the real client's own address, echoed back exactly
// like a genuine host would. peerSinZero MUST be the client's own
// self-reported sin_zero (learned from its 0x20 ping - see
// onDiscoveryPingNoHost/beginSimulatedHostOpen), not an invented value -
// see synthesizedSockaddr's own doc comment for why this matters.
func synthesizeSelfPeerOpen(connID [2]byte, selfIP [4]byte, selfPort uint16, peerIP [4]byte, peerPort uint16, peerSinZero [8]byte) []byte {
	out := []byte{0x00, 0x00}
	out = append(out, connID[:]...)
	out = append(out, synthesizedSessionUnclearConstant...)
	out = append(out, synthesizedSockaddr(selfIP, selfPort, ourOwnSinZero)...)
	out = append(out, synthesizedSockaddr(peerIP, peerPort, peerSinZero)...)
	return out
}

// synthesizedHandshakeAckHostTrailer is the 34 bytes that follow the two
// connID fields in every real host-sent 0x02 handshake-ack this project
// has captured - see synthesizeHandshakeAck's own doc comment for why
// the overall structure is real, not leaked memory. Two byte positions
// are NOT safe to treat as fixed, now confirmed across four independent
// real captures (2026-08-15's lobby-full-rejection, 2026-08-17's
// session-establishment-investigation, and 2026-08-18's real-host-
// joiner-namecheck, which alone contributed two more samples - a real
// aom-headless host and a real joiner in the same live session):
//
//   - Byte offsets 10 and 22 (always equal to each other within one
//     packet): the JOINER's value was 0xa4 in all four samples - solid
//     enough to treat as role-constant. The HOST's value was 0xa6 once
//     and 0xa8 twice - NOT a stable per-role constant, likely a per-
//     process resource handle's low byte. Left at 0xa6 (this codebase's
//     first-confirmed host value) since no value is more "correct" than
//     any other and the receiving side doesn't appear to validate it.
//   - Byte offsets 12 and 24 (also equal to each other within one
//     packet, but shared between BOTH roles' acks in the SAME session):
//     0x01 in the 2026-08-15 session, 0x02 in 2026-08-17, 0x01 again in
//     2026-08-18 - genuinely live, session-specific data, not a
//     constant. **Explained 2026-08-18** by cross-checking the official
//     `[MC-DPL8R]` spec (`docs/directplay8-reference/`): every
//     connection-establishment CFRAME it documents (CONNECT, CONNECTED,
//     CONNECTED_SIGNED, HARD_DISCONNECT, SACK) carries a `tTimestamp`
//     field defined as "the sender's computer system tick count, in
//     millisecond units" - exactly the live-clock-reading shape these
//     bytes exhibit (varies session to session, matches between two
//     independent processes - host and joiner - reading within ~90ms of
//     each other, consistent with both being on the same underlying
//     kernel clock). Classic DirectPlay almost certainly carries its own
//     version of this same concept, different wire format, same idea.
//     These two positions are now computed live at send time (see
//     livenessTickByte, synthesizeHandshakeAck) instead of hardcoded -
//     left as 0x00 placeholders here since synthesizeHandshakeAck
//     overwrites them unconditionally on every call.
//     (Byte offsets 10/22 are a SEPARATE field, still hardcoded - ruled
//     out as tick-count-shaped since the joiner's value never varied
//     across any sample while byte 12/24 did; more likely a per-process
//     resource handle, see above.)
var synthesizedHandshakeAckHostTrailer = []byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xe9, 0xe9, 0x7f, 0xa6,
	0x00, 0x00, 0x00, 0x18, 0xf7, 0x32, 0x00, 0x3f, 0xe4, 0xd4, 0x7f,
	0xa6, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x18, 0xf7, 0x32, 0x00,
}

// processStartTime is set once at package init - the reference point
// livenessTickByte measures elapsed time from, standing in for "when
// this sender's system started" (real DirectPlay measures from actual
// system boot; this codebase only has its own process's own start
// available, close enough for the purpose - see livenessTickByte).
var processStartTime = time.Now()

// livenessTickByte is our best-effort analog of DirectPlay's own
// tTimestamp field (see synthesizedHandshakeAckHostTrailer's doc
// comment) - a coarse, single-byte slice of milliseconds elapsed since
// this aom-lobby process itself started, standing in for "the sender's
// computer system tick count" the official spec describes. The exact
// scale/formula real classic DirectPlay uses for its own equivalent
// field is unconfirmed (only 3 real samples exist, all small values -
// not enough to fit a precise formula), so this is an honest best
// effort at "live and plausible" rather than a confirmed-correct
// reproduction - the known-wrong alternative was a frozen constant that
// never changes across any session, which is strictly worse regardless
// of whether this exact scale is right.
func livenessTickByte() byte {
	return byte(time.Since(processStartTime).Milliseconds() % 256)
}

// synthesizeHandshakeAck builds a fake 0x02 handshake-ack message - see
// docs/directplay8-packet-classification.md's confirmed-shapes table
// ("0x02, 40 bytes... echoes sender IDs, no address"). ownConnID is
// aom-lobby's own invented identity; peerConnID is the real client's own
// connID, learned from its own 0x00 message (see the "session" case in
// forwardToBackend's "no eligible host" branch).
//
// Correction (2026-08-17): the 34 trailing bytes were originally zeroed
// here on the theory that they're leaked Wine process memory - wrong,
// the overall structure is real and mostly fixed. NOT fully role-
// constant though - see synthesizedHandshakeAckHostTrailer's own doc
// comment (corrected 2026-08-18 after a third and fourth independent
// capture, then explained via the official DirectPlay 8 spec's
// tTimestamp field the same day) for exactly which byte positions are
// safe to hardcode and which are genuinely live, session-specific data.
// Byte offsets 18 and 30 of the full packet below (trailer offsets
// 12/24) are computed live via livenessTickByte instead of hardcoded,
// as of this correction. Whether this fix unblocks the still-open
// "client never sends its own 0x02" mystery (this no-eligible-host
// synthesis path specifically - the real backend-routing path was
// separately confirmed working end-to-end 2026-08-18, see
// lobby/packet-handling-design.md) is not yet confirmed either way.
func synthesizeHandshakeAck(ownConnID, peerConnID [2]byte) []byte {
	out := []byte{0x02, 0x00}
	out = append(out, ownConnID[:]...)
	out = append(out, peerConnID[:]...)
	out = append(out, synthesizedHandshakeAckHostTrailer...)
	tick := livenessTickByte()
	out[18] = tick
	out[30] = tick
	return out
}

// synthesizeHeartbeat builds a fake "07ff" heartbeat/ack - see
// docs/directplay8-packet-classification.md's confirmed-shapes table
// ("07ff, 10 bytes... Heartbeat/ack for the settings-sync layer").
// connID is aom-lobby's own invented identity.
func synthesizeHeartbeat(connID [2]byte) []byte {
	out := []byte{0x07, 0xff, 0x00, 0x00}
	out = append(out, connID[:]...)
	out = append(out, 0x00, 0x00, 0x00, 0x00)
	return out
}

// simulatedFullLobbyCrackSignature is the string sent in the session-port
// name-broadcast synthesizeNameBroadcast builds. Despite this message
// type's name, it does NOT carry a real player nickname - every real
// capture this project has ever taken (accepted and rejected sessions
// alike, both roles) sends this exact same ASCII string, byte-for-byte,
// regardless of either player's actual configured name. Leading theory:
// a vestigial CD-key-check field - aomxnocd1.exe is a No-CD crack, and
// this is plausibly a fixed salt/placeholder value the crack sends in
// place of whatever a real, uncracked client would derive from an
// actual CD key (see lobby/packet-handling-design.md's Phase 2 section
// and the paullovesjade-not-a-name memory note for the full history).
// Not confirmed as literally CD-key-related, but confirmed NOT a
// nickname, and confirmed load-bearing - see below.
//
// Corrected 2026-08-19 - previously hardcoded to "TheIP" (a leftover
// from before this field's true nature was understood, when it was
// still assumed to be a real nickname slot). "TheIP" never appeared in
// any real capture this project has ever taken; every single one sends
// "paullovesjade" instead. This turned out to be THE fix for the
// long-standing "client never sends its own name-broadcast" mystery -
// live-tested: a real client would receive "TheIP" here and simply
// never reply with its own name-broadcast, silently stalling until its
// own ~2-minute patience timeout, regardless of every other byte/timing
// fix made earlier. Switching to "paullovesjade" (matching every real
// capture) fixed it immediately. This field's exact content is
// functionally required, not cosmetic - do not change this value
// without new capture evidence.
const simulatedFullLobbyCrackSignature = "paullovesjade"

// synthesizeNameBroadcast builds a fake 03-wrapped player-name broadcast
// - see docs/directplay8-packet-classification.md's confirmed-shapes
// table ("sub-type player-announce... Self-announce: GUID + nickname")
// and the real capture decoded in lobby/packet-handling-design.md's
// "Case 3 in detail" section. Encoding note, confirmed against that
// capture: this name string is plain ASCII/UTF-8 (one byte per
// character), NOT UTF-16LE like the discovery-port 0x26 reply's game
// name - a genuinely different encoding between the two message
// families, easy to get wrong by assuming consistency that isn't there.
// connID is aom-lobby's own invented identity; seq is this message's
// position in the fake exchange (0, then 1 for the observed retry/second
// copy - see the real capture's own sequence).
func synthesizeNameBroadcast(connID [2]byte, seq uint16, name string) []byte {
	nameBytes := append([]byte(name), 0x00) // ASCII/UTF-8 + null terminator
	out := []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00}
	binary.LittleEndian.PutUint16(out[2:4], seq)
	copy(out[4:6], connID[:])
	out = binary.LittleEndian.AppendUint16(out, uint16(len(nameBytes)))
	out = append(out, nameBytes...)
	out = append(out, 0x00, 0x00) // trailer, matches the real capture
	return out
}

// discoveryQuery is the exact 9-byte 0x25 "enumerate hosts" packet AoM's
// own client broadcasts while its LAN browse screen is open (see
// docs/directplay8-protocol.md) - constant across every capture, no
// target/session info needed. Sending it straight to a backend and
// checking for a 0x26 reply is the same check the real game performs to
// decide whether a host is listed at all, so it's used here as the
// "is this host actually open and waiting for players" check behind the
// /hosts status endpoint below.
var discoveryQuery = []byte{0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// hostProbe periodically asks a single backend "are you open?" the same
// way a real client's LAN browse screen would, and remembers the answer.
// One hostProbe per pool member (see hostCandidate/hostPool) - each
// backend's own dynamically-discovered pod IP now, not a shared Service
// address (see CLAUDE.md's Architecture intention).
//
// History: briefly (2026-08-14) also actively probed the session port
// (2300) directly - a synthetic empty datagram, checking for a hard
// ECONNREFUSED vs. a timeout - after a live incident (pod "kwlj7") where
// a host seemed to answer discovery fine while /proc/net/udp showed no
// 2300 listener. That /proc/net/udp signal was already known-unreliable
// by the time the probe was built (a *different* pod showed the
// identical "missing" signature while demonstrably relaying real session
// traffic for two live clients), and the probe meant to replace it with
// something trustworthy turned out to have the same class of problem for
// a different reason: confirmed live that AoM's DirectPlay8 session
// socket isn't bound at the OS level until a real client's own handshake
// starts arriving - it's genuinely not there yet while a freshly-hosted
// lobby just sits open and waiting, which is indistinguishable from
// "actually dead" to any probe sent before a real client approaches. No
// synthetic pre-connect probe can fix that - it's a timing problem, not
// a probe-format one. Removed rather than reworked; the only signal
// that's ever actually reliable for a dead session port is what already
// exists elsewhere: sessionProxy.forwardToBackend's own Dial/Write/Read
// against genuine client traffic.
type hostProbe struct {
	backendAddr string // discovery port (2299)

	mu   sync.Mutex
	open bool
}

func (h *hostProbe) run() {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		h.probeOnce()
		<-ticker.C
	}
}

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

	h.mu.Lock()
	h.open = open
	h.mu.Unlock()
}

// count returns the number of hosts this probe currently sees as open and
// waiting for players - 0 or 1 today, see the type comment above.
func (h *hostProbe) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		return 1
	}
	return 0
}

// hasWaitingGame reports whether this backend currently has an open lobby
// with a host waiting for an opponent - the signal matchmaking needs to
// route a newly-connecting client into an existing game rather than
// provisioning a new pod. Same underlying check as count() (one backend
// today, so the two currently agree) - kept as its own method because once
// there's a slice of probes, one per on-demand pod (see CLAUDE.md's
// Architecture intention), matchmaking will want to pick a specific
// waiting probe to round-robin into, not just a yes/no across all of them.
func (h *hostProbe) hasWaitingGame() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open
}

// hostCandidate is one N-host pool member's full identity - every address
// a real aom-headless backend needs (discovery probing, session routing,
// driving its own input-agent instance) plus its own independent
// liveness probe and match state. See
// lobby/packet-handling-design.md's "N-host round-robin matchmaking"
// section for the design this implements.
//
// This is phase 1 of that design - per-host *scoping* - not phase 2
// (the real selectForNewClient priority policy). Everything that used to
// assume "there is exactly one host" (session attribution, matchState,
// clientTracker's consumers, triggerMatchStart) now threads a
// *hostCandidate through instead, so a second host can't silently
// cross-wire with the first - see hostPool.assignForClient's doc comment
// for why assignment itself is still a placeholder.
type hostCandidate struct {
	// id is just for logging - "host-0", "host-1", ... in pool order.
	id                    string
	discoveryBackendAddr  string
	sessionBackendAddr    string
	sessionBackendUDPAddr *net.UDPAddr
	inputAgentAddr        string

	probe *hostProbe
	// match is this host's own matchState (ready-toggle tracking, pair
	// relay) - previously a single global instance shared by every
	// client regardless of which backend they were actually talking to.
	// Set once during pool construction in main(), same pattern as
	// today's single match.closeSession wiring.
	match *matchState
}

// hostPool tracks every known aom-headless backend. Replaces the old
// single cfg.discoveryBackendAddr/cfg.sessionBackendAddr/
// cfg.inputAgentAddr.
//
// N-host phase 2 update (2026-08-13): hosts is no longer built once at
// startup from static config - see podLister below. It's now mutated
// live by podLister.run's reconciliation loop as pods are created/
// deleted/scaled, which is also why every method here that touches hosts
// now holds mu (addHost/removeHost/hostForBackendIP) - previously safe
// to read hosts lock-free since it never changed after construction, not
// true anymore.
type hostPool struct {
	mu    sync.Mutex
	hosts []*hostCandidate

	// nextWaitingRR/nextEmptyRR are round-robin cursors for
	// selectLocked's two eligible tiers, kept independent so a long run
	// of "waiting" picks can't starve the round-robin position for
	// "empty" hosts once that tier is needed, or vice versa - exactly
	// the concern packet-handling-design.md's hostPool sketch flagged
	// before this was built.
	nextWaitingRR int
	nextEmptyRR   int
	// nextPeekRR is peekHost's own separate cursor - kept independent of
	// the two above so non-committing discovery-time peeks don't perturb
	// selectLocked's own round-robin fairness for real, sticky
	// assignments.
	nextPeekRR int
	// sessionCountForHost is sessionProxy's own per-host session
	// counter (sessionProxy.sessionCountForHost), wired up in main()
	// once sessionProxy exists - same chicken-and-egg reason
	// podLister.closeSession is wired up after the fact. selectLocked
	// needs this to tell "empty" from "waiting for a second player"
	// apart: hostProbe.hasWaitingGame() alone can't (it's true for both
	// - a host keeps answering discovery queries whether 0 or 1 real
	// clients are connected, see docs/lobby-status-api.md's "How it
	// decides open" section), but the real session count sessionProxy
	// already tracks per host can.
	sessionCountForHost func(*hostCandidate) int
	// assigned makes host selection sticky per real client IP for the
	// life of that client's connection (and any later reconnect/rejoin -
	// matches this project's already-confirmed same-client-rejoin
	// behavior, which depends on landing on the same host again). Without
	// this, a client's discovery-port query and session-port traffic
	// could each independently round-robin to a *different* host - see
	// packet-handling-design.md's "session-port stickiness" section,
	// pulled forward into this phase since matchmaking is unsafe without
	// it, not a later nice-to-have. Keyed by IP only (not full addr):
	// discovery-port and session-port traffic use different local client
	// ports for the same real player (see clientTracker's own doc
	// comment on the same distinction).
	//
	// Known limitation, unchanged by the move to dynamic discovery: if a
	// client's assigned host is later removed from the pool (its pod
	// died/rescheduled mid-match - see removeHost), this map still points
	// at the now-gone hostCandidate, and that client's traffic will keep
	// trying to reach a dead pod until its session naturally idles out
	// (reapIdleSessions) rather than being re-matched. Graceful handling
	// of a host disappearing mid-match is deliberately not solved in this
	// pass - same "flag the gap, don't silently assume it away" practice
	// this file already applies elsewhere (see onClientGone's own
	// not-wired-up status).
	//
	// Also still leaks one entry per distinct client IP ever seen for the
	// life of the process - acceptable for this project's dev/test scope
	// (CLAUDE.md's Goals), revisit if this ever runs long-lived against
	// real traffic.
	assigned map[string]*hostCandidate
}

// assignForClient returns clientIP's assigned host, picking one via
// selectLocked on first contact and remembering it thereafter - see this
// type's own doc comment for why sticky assignment isn't optional here.
// Returns (nil, false) if no host is currently eligible - the pool is
// empty (brief window at startup before podLister's first successful
// poll lands, or every known host has been removed - see removeHost), or
// every host is full (packet-handling-design.md's case 3). Callers must
// handle this rather than assume a host always exists, unlike the old
// static-config version where an empty pool was a startup-time fatal
// error, not a runtime possibility. Case 3 is a plain drop today, not a
// spoofed "lobby full" rejection - see synthesizeLobbyFullRejection's own
// doc comment on why that's still blocked on a reference capture that
// hasn't been taken.
func (p *hostPool) assignForClient(clientIP string) (*hostCandidate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.assigned[clientIP]; ok {
		return h, true
	}
	h, ok := p.selectLocked()
	if !ok {
		return nil, false
	}
	if p.assigned == nil {
		p.assigned = make(map[string]*hostCandidate)
	}
	p.assigned[clientIP] = h
	return h, true
}

// maxRealClientsPerHost is this project's fixed 1v1 scope (CLAUDE.md's
// Goals: "host + exactly two real playing clients") - see hostFull's doc
// comment for why this hard cap matters independently of
// matchState.full().
const maxRealClientsPerHost = 2

// hostFull reports whether h has no room for another real client -
// either its pairRelay has already formed (matchState.full(), the
// authoritative "these two are matched" signal used everywhere else in
// this file), OR it already has maxRealClientsPerHost real sessions
// registered, whichever comes first.
//
// Correction (2026-08-14): originally checked matchState.full() alone.
// Found live, with 2 hosts and 3 real clients: matchState.full() only
// goes true once the *host's own broadcast exchange* between the two
// paired clients actually completes - which can lag behind (or, per this
// project's still-unresolved local-topology broadcast-bypass anomaly,
// never complete at all - see existingPeerBroadcast's own doc comment)
// the moment a host genuinely has no room left. A third real client's
// selectLocked call landed on an already-2-real-client host because
// full() hadn't caught up yet, with nowhere for it to actually go (AoM's
// own lobby only ever has 2 open slots per this project's scope) - stuck
// "Attempting to Connect" by construction, not a network issue. Session
// count is the more immediate, reliable signal; checking both closes the
// gap regardless of which one lags.
func (p *hostPool) hostFull(h *hostCandidate) bool {
	if h.match.full() {
		return true
	}
	return p.sessionCountForHost != nil && p.sessionCountForHost(h) >= maxRealClientsPerHost
}

// hostLive reports whether h is actually usable right now - its discovery
// port answering (hostProbe.hasWaitingGame()) - added alongside hostFull
// so a pod that's in the pool but not actually up (crashed, hung, or just
// hasn't self-hosted yet) can't eat a brand-new client's ~15s connect
// budget on a host that was never going to work. Note this is a liveness
// check only, not a capacity one - see hostFull/sessionCountForHost for
// why it alone can't distinguish "empty" from "waiting for a second
// player" the way capacity selection needs.
//
// History: briefly (2026-08-14) also required a separate session-port
// (2300) probe here, after the kwlj7 incident (see hostProbe's own doc
// comment for the full history). Deliberately not brought back: the
// session-port probe's own failure mode was worse than what it replaced
// - it read a genuinely healthy, freshly-hosted lobby as dead, because
// DirectPlay8 doesn't bind that socket until a real client's handshake
// arrives, which no pre-connect probe can distinguish from "actually
// dead." A dead session port is caught for real where it always was:
// sessionProxy.forwardToBackend's own Dial/Write/Read against genuine
// client traffic.
//
// Narrow, self-healing false-negative: a host added to the pool within
// the last podPollInterval might not have its first probe result in yet
// (hostProbe.run() ticks every probeInterval, 3s) and would look
// not-live for that brief window even though it's actually fine - not
// worth a separate fix, it clears on its own within one probe cycle.
func (p *hostPool) hostLive(h *hostCandidate) bool {
	return h.probe != nil && h.probe.hasWaitingGame()
}

// selectLocked implements packet-handling-design.md's "Selection policy"
// priority order for a newly-arriving client: prefer a host already
// waiting for a second player (round-robin among those specifically, if
// more than one), otherwise an empty host (round-robin among those),
// otherwise ineligible. Must be called with p.mu held - only ever called
// from assignForClient, which already holds it.
//
// A host is excluded entirely if it's full (hostFull) or not currently
// live (hostLive). Below that, sessionCountForHost distinguishes 0 real
// sessions (empty) from 1 (waiting) - see this type's own doc comment on
// why hostProbe's liveness check alone can't make that distinction.
//
// On the race packet-handling-design.md's "The race this needs to guard
// against" section flags (two clients connecting within the same few
// seconds both landing on the same host before either session is
// registered): this is actually already closed, not just unaddressed.
// assignForClient (the only caller of this method) is only ever reached
// via sessionProxy.forwardToBackend, which in turn is only ever called
// from sessionProxy.run()'s single sequential ReadFromUDP loop - there is
// no other call site. Go processes that loop one packet at a time, and
// forwardToBackend doesn't return until the new session is fully
// inserted into p.sessions, so two different clients' assignment
// decisions can never actually interleave - by the time a second
// client's packet is even read, the first client's session already
// exists and sessionCountForHost already reflects it. This invariant
// depends on assignForClient never being called from anywhere else
// (e.g. a future second goroutine) - worth remembering if that ever
// changes, since it's what makes a claim/reservation mechanism
// unnecessary today rather than merely un-implemented.
func (p *hostPool) selectLocked() (*hostCandidate, bool) {
	var waiting, empty []*hostCandidate
	for _, h := range p.hosts {
		if p.hostFull(h) || !p.hostLive(h) {
			continue
		}
		n := 0
		if p.sessionCountForHost != nil {
			n = p.sessionCountForHost(h)
		}
		if n == 0 {
			empty = append(empty, h)
		} else {
			waiting = append(waiting, h)
		}
	}

	if len(waiting) > 0 {
		h := waiting[p.nextWaitingRR%len(waiting)]
		p.nextWaitingRR++
		return h, true
	}
	if len(empty) > 0 {
		h := empty[p.nextEmptyRR%len(empty)]
		p.nextEmptyRR++
		return h, true
	}
	return nil, false
}

// peekHost returns any currently-eligible (not full) host, round-robin
// via its own independent cursor - deliberately NOT sticky and NOT
// tier-aware like selectLocked/assignForClient. Used by discoveryProxy
// to pick a backend to relay a 0x25/0x26 discovery exchange through
// without committing the client to it - see this session's "Defer host
// assignment from discovery-time to session-connect-time" change.
// Discovery/enumeration traffic doesn't represent real intent to connect
// (a client's LAN/Direct-IP screen fires 0x25 queries just from being
// open - see docs/directplay8-protocol.md and the official [MC-DPL8CS]
// spec, both confirming Direct-Connect reuses the exact same enumeration
// exchange as passive LAN browsing), so matchmaking's real,
// sticky, tiered decision belongs on the session-port handshake that
// follows, not here - see assignForClient.
//
// Still relays through a genuine backend rather than synthesizing a
// reply from nothing: the reply's meaningful fields get rewritten to
// this proxy's own address regardless of which backend answered (see
// discoveryRewrite), but the message also carries several bytes of
// still-unexplained data (docs/directplay8-protocol.md's "sin_zero"
// notes) that this project has never fabricated - every rewrite path
// elsewhere only overwrites known fields on top of a genuine relayed
// reply, and this keeps that same posture.
func (p *hostPool) peekHost() (*hostCandidate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var eligible []*hostCandidate
	for _, h := range p.hosts {
		if !p.hostFull(h) && p.hostLive(h) {
			eligible = append(eligible, h)
		}
	}
	if len(eligible) == 0 {
		return nil, false
	}
	h := eligible[p.nextPeekRR%len(eligible)]
	p.nextPeekRR++
	return h, true
}

// hostForBackendIP returns whichever pool member's session-port backend
// address matches ip, if any - used by run() to recognize a host's own
// unprompted traffic (see relayBackendInitiated) and know which host's
// sessions to scope the relay search to, now that there's more than one
// to confuse it with.
func (p *hostPool) hostForBackendIP(ip net.IP) (*hostCandidate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.hosts {
		if h.sessionBackendUDPAddr != nil && h.sessionBackendUDPAddr.IP.Equal(ip) {
			return h, true
		}
	}
	return nil, false
}

// addHost appends a newly-discovered pod to the pool, starting its own
// liveness probe and match state - the runtime equivalent of what main()
// used to do once, up front, for every statically-configured host. Safe
// to call for a host that's already present (by id) - a no-op, since
// podLister's reconciliation loop only ever calls this for pods it
// hasn't already added.
func (p *hostPool) addHost(h *hostCandidate) {
	p.mu.Lock()
	p.hosts = append(p.hosts, h)
	p.mu.Unlock()
	log.Printf("[pool] host added: %s (discovery %s, session %s, input-agent %s)",
		h.id, h.discoveryBackendAddr, h.sessionBackendAddr, h.inputAgentAddr)
}

// removeHost drops a pod that's no longer listed (deleted/rescheduled/
// scaled down) from the pool. Does NOT touch any client's sticky
// assignment (see assigned's own doc comment on the known gap that
// leaves) or tear down the removed host's pairRelay/matchState - existing
// sessions referencing it simply become orphaned and eventually idle out
// via reapIdleSessions, same as any other silently-gone backend.
func (p *hostPool) removeHost(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, h := range p.hosts {
		if h.id == id {
			p.hosts = append(p.hosts[:i], p.hosts[i+1:]...)
			log.Printf("[pool] host removed: %s (no longer listed)", id)
			return
		}
	}
}

// hostIDs returns the id of every host currently in the pool - used by
// podLister's reconciliation loop to diff against the latest pod list
// without holding the lock for the whole reconciliation.
func (p *hostPool) hostIDs() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make(map[string]bool, len(p.hosts))
	for _, h := range p.hosts {
		ids[h.id] = true
	}
	return ids
}

// snapshot returns a copy of the current host list, safe to range over
// without holding the pool's lock - used by main()'s per-tick status
// endpoints and anywhere else that needs to look at "every host right
// now" rather than a single lookup.
func (p *hostPool) snapshot() []*hostCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*hostCandidate(nil), p.hosts...)
}

// clientTracker remembers the most recently seen real client address,
// shared between the discovery and session proxies. It exists because the
// backend pod, once it learns a client's real address (from the
// discovery-port ping - see discoveryAddressOffsets' 0x20 entry, now
// rewritten so this shouldn't happen anymore, but the tracker is the
// fallback plumbing either way), sends UDP 2300 session data directly to
// whatever address it believes the client is at rather than back through
// this proxy's session listener. The session proxy uses this to know who
// to relay that unsolicited traffic to.
//
// Its address is only trustworthy as an IP, not a port: it's updated from
// discovery-port (2299) traffic, which uses a fresh ephemeral port per
// query, not the fixed port a client's session-port (2300) traffic always
// comes from - see relayBackendInitiated, which uses the tracker's IP as
// a hint to pick the right *session*, rather than relaying to the
// tracker's address directly. Still only one entry (not one per client) -
// good enough to disambiguate "which client is this backend packet for"
// among clients that have a session already vs. one that's still
// connecting (see relayBackendInitiated), but not a real fix for multiple
// simultaneous *pending* connections - see CLAUDE.md's Architecture
// intention for the eventual per-match design.
type clientTracker struct {
	mu   sync.Mutex
	addr *net.UDPAddr
}

func (t *clientTracker) set(addr *net.UDPAddr) {
	t.mu.Lock()
	t.addr = addr
	t.mu.Unlock()
}

func (t *clientTracker) get() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

type config struct {
	discoveryListenAddr string
	sessionListenAddr   string
	statusListenAddr    string
	// verbose gates the hex-dump/per-packet debug logging below - it's
	// synchronous, allocates a hex string per packet, and runs on every
	// single packet including the high-frequency session-port ones, which
	// turned out to matter: a real successful connection completes its
	// post-handshake settings exchange within ~30ms of the handshake
	// landing (see docs/directplay8-protocol.md), a tight enough window
	// that this logging's overhead was a real suspect for breaking it.
	// Off by default; set VERBOSE=1 to turn back on for protocol
	// reverse-engineering sessions like the one that produced that doc.
	verbose  bool
	publicIP [4]byte
	// publicPort is what clients are told to use for both the discovery
	// reply's embedded address and the session handshake's rewritten
	// host "self" address - always the session proxy's own listening
	// port, since that's the one address clients ever need to know.
	publicPort uint16
}

func loadConfig() config {
	publicAddr := getenv("PUBLIC_ADDR", "127.0.0.1")
	ip := net.ParseIP(publicAddr).To4()
	if ip == nil {
		log.Fatalf("PUBLIC_ADDR %q is not a valid IPv4 address", publicAddr)
	}

	var cfg config
	cfg.discoveryListenAddr = getenv("LISTEN_ADDR", ":2299")
	cfg.sessionListenAddr = getenv("SESSION_LISTEN_ADDR", ":2300")
	cfg.statusListenAddr = getenv("STATUS_LISTEN_ADDR", ":8080")
	cfg.verbose = getenv("VERBOSE", "") != ""
	copy(cfg.publicIP[:], ip)

	_, portStr, err := net.SplitHostPort(cfg.sessionListenAddr)
	if err != nil {
		log.Fatalf("parsing SESSION_LISTEN_ADDR %q: %v", cfg.sessionListenAddr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		log.Fatalf("SESSION_LISTEN_ADDR %q has an invalid port: %v", cfg.sessionListenAddr, err)
	}
	cfg.publicPort = uint16(port)

	return cfg
}

const (
	// serviceAccountDir is the standard mount every Kubernetes pod gets
	// for its own ServiceAccount, no extra volume config needed beyond
	// setting serviceAccountName (see k8s/lobby-deployment.yaml) and
	// granting it a Role (see k8s/lobby-rbac.yaml).
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	// AoM's own fixed DirectPlay8 ports (see CLAUDE.md's Goals) plus
	// input-agent's - every aom-headless pod uses these three regardless
	// of its own pod IP, so podLister only needs to discover the IP.
	headlessDiscoveryPort  = 2299
	headlessSessionPort    = 2300
	headlessInputAgentPort = 8082

	podPollInterval = 3 * time.Second // same cadence as hostProbe's own ticker
)

// podLister discovers aom-headless backend pods dynamically via the
// Kubernetes API, replacing the old static comma-separated env-var
// config entirely - see hostPool's own doc comment and
// lobby/packet-handling-design.md's "N-host round-robin matchmaking"
// section. `kubectl scale deployment/aom-headless --replicas=N` is now
// the entire "add a host" operation - no manifest edits, no lobby
// redeploy.
//
// Deliberately a hand-rolled REST poller against the in-cluster API
// server rather than k8s.io/client-go: this project has had zero
// external Go dependencies until now (lobby/go.mod), and this is well
// under 100 lines of stdlib net/http - not worth a large transitive
// dependency tree just to poll a list-pods endpoint every few seconds,
// especially given hostProbe next door already establishes that exact
// polling shape for a different purpose.
//
// Reads the same in-cluster ServiceAccount mount every pod gets for
// free: bearer token + CA cert from serviceAccountDir, API server
// host/port from the KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT env
// vars Kubernetes always injects. Namespace and label selector come from
// AOM_HEADLESS_NAMESPACE/AOM_HEADLESS_LABEL_SELECTOR instead of reading
// the mounted namespace file - this repo has never used more than the
// `default` namespace (confirmed by grep across every k8s/*.yaml), so an
// env var with that default is simpler than parsing another file for a
// value that's effectively constant here.
type podLister struct {
	apiServer     string // e.g. "https://10.96.0.1:443"
	namespace     string
	labelSelector string
	token         string
	httpClient    *http.Client
	cfg           config

	// closeSession is sessionProxy.closeSession, set once in main() after
	// sessionProxy exists (chicken-and-egg, same reason match.closeSession
	// used to be wired after the fact) - handed to every newly-discovered
	// host's matchState, same as the old static path did for every host
	// up front.
	closeSession func(clientAddr *net.UDPAddr)
}

// newInClusterPodLister builds a podLister from the standard in-cluster
// ServiceAccount mount. Fatal on any missing piece: aom-lobby cannot
// discover any backend at all without this, so failing fast at startup
// is more useful than silently limping along with a permanently-empty
// pool - same posture loadConfig already takes for a malformed
// PUBLIC_ADDR.
func newInClusterPodLister(cfg config) *podLister {
	namespace := getenv("AOM_HEADLESS_NAMESPACE", "default")
	labelSelector := getenv("AOM_HEADLESS_LABEL_SELECTOR", "app=aom-headless")

	host := getenv("KUBERNETES_SERVICE_HOST", "")
	port := getenv("KUBERNETES_SERVICE_PORT", "")
	if host == "" || port == "" {
		log.Fatalf("KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT not set - aom-lobby must run as an in-cluster pod to discover aom-headless backends (see k8s/lobby-deployment.yaml)")
	}
	tokenBytes, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		log.Fatalf("reading ServiceAccount token: %v (is k8s/lobby-rbac.yaml applied, and serviceAccountName set on the aom-lobby Deployment?)", err)
	}
	caBytes, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		log.Fatalf("reading ServiceAccount CA cert: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		log.Fatalf("parsing ServiceAccount CA cert %s/ca.crt: no valid certificates found", serviceAccountDir)
	}

	return &podLister{
		apiServer:     fmt.Sprintf("https://%s:%s", host, port),
		namespace:     namespace,
		labelSelector: labelSelector,
		token:         strings.TrimSpace(string(tokenBytes)),
		cfg:           cfg,
		httpClient: &http.Client{
			// Generous relative to an in-cluster API call (same node,
			// same cluster network) while still bounded - mirrors
			// probeTimeout's own reasoning for hostProbe's UDP probes.
			Timeout: 3 * probeTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: caPool},
			},
		},
	}
}

// podInfo is the subset of a Pod's API representation reconcileOnce
// needs.
type podInfo struct {
	name string
	ip   string
}

// list returns every Running pod matching the configured label selector
// that has an assigned IP - pods still starting (no podIP yet) or
// terminating (phase no longer Running) are excluded, the same "don't
// route to something not actually ready" posture hostProbe already
// applies via its own liveness check.
func (pl *podLister) list() ([]podInfo, error) {
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/pods?labelSelector=%s",
		pl.apiServer, pl.namespace, url.QueryEscape(pl.labelSelector))
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pl.token)
	resp, err := pl.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %s: %s", resp.Status, body)
	}

	var parsed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
				PodIP string `json:"podIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var pods []podInfo
	for _, item := range parsed.Items {
		if item.Status.Phase != "Running" || item.Status.PodIP == "" {
			continue
		}
		pods = append(pods, podInfo{name: item.Metadata.Name, ip: item.Status.PodIP})
	}
	return pods, nil
}

// run polls list on a ticker and reconciles pool against the result -
// same ticker-poll shape as hostProbe.run() next door. Never returns;
// call with `go`.
func (pl *podLister) run(pool *hostPool) {
	ticker := time.NewTicker(podPollInterval)
	defer ticker.Stop()
	for {
		pl.reconcileOnce(pool)
		<-ticker.C
	}
}

// reconcileOnce lists pods once and diffs the result against pool: new
// pods get a full hostCandidate built and added (own probe started, own
// matchState created - the runtime equivalent of what main() used to do
// once, up front, for every statically-configured host); pods no longer
// listed get removed (see removeHost's own doc comment on what that
// does and doesn't clean up).
func (pl *podLister) reconcileOnce(pool *hostPool) {
	pods, err := pl.list()
	if err != nil {
		log.Printf("[pool] listing pods (namespace=%s, selector=%s): %v", pl.namespace, pl.labelSelector, err)
		return
	}

	seen := make(map[string]bool, len(pods))
	known := pool.hostIDs()
	for _, pod := range pods {
		seen[pod.name] = true
		if known[pod.name] {
			continue
		}
		discoveryAddr := fmt.Sprintf("%s:%d", pod.ip, headlessDiscoveryPort)
		sessionAddr := fmt.Sprintf("%s:%d", pod.ip, headlessSessionPort)
		sessionUDPAddr, err := net.ResolveUDPAddr("udp", sessionAddr)
		if err != nil {
			log.Printf("[pool] resolving session address for new pod %s (%s): %v", pod.name, sessionAddr, err)
			continue
		}
		h := &hostCandidate{
			id:                    pod.name,
			discoveryBackendAddr:  discoveryAddr,
			sessionBackendAddr:    sessionAddr,
			sessionBackendUDPAddr: sessionUDPAddr,
			inputAgentAddr:        fmt.Sprintf("%s:%d", pod.ip, headlessInputAgentPort),
			match:                 &matchState{cfg: pl.cfg, closeSession: pl.closeSession},
			probe:                 &hostProbe{backendAddr: discoveryAddr},
		}
		pool.addHost(h)
		go h.probe.run()
	}
	for id := range known {
		if !seen[id] {
			pool.removeHost(id)
		}
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type session struct {
	backendConn *net.UDPConn
	clientAddr  *net.UDPAddr // the real client this session relays for
	// host is which pool member this session was assigned to (see
	// hostPool.assignForClient) - added for N-host support. Every place
	// that used to reach for a single closed-over backend address or the
	// single global matchState now goes through this instead, so traffic
	// and match state for two different real matches can never blend.
	host *hostCandidate

	mu       sync.Mutex
	lastSeen time.Time

	// connID/connIDKnown record the backend's own wrapper conn-ID (see
	// wrapperConnID) for this specific client relationship, learned the
	// first time a "03 00"-wrapped packet is correctly attributed to
	// this session via backendToClient's own per-client dialed-socket
	// read (where sess is already known for certain, unlike
	// relayBackendInitiated's guesswork). Added 2026-08-12 - see
	// relayBackendInitiated's doc comment for why this exists: an
	// unprompted backend broadcast (e.g. newPeerBroadcast) carries this
	// same conn-ID, letting it be attributed to the right client by
	// packet content instead of IP-recency guessing. Guarded by the mu
	// field above, same as lastSeen.
	connID      [2]byte
	connIDKnown bool

	// selfSinZero/selfSinZeroKnown record this client's own self-reported
	// sin_zero tag (see docs/directplay8-protocol.md's "sin_zero" resolution -
	// a per-participant identity tag each side generates once, which whoever
	// else embeds that participant's address must echo back exactly), learned
	// from the sin_zero this client itself sent in its own genuine "self"
	// sockaddr_in block (sessionSelfOffset+8) the first time its own 0x00
	// open passes through rewriteToBackend - i.e. before this proxy rewrites
	// that block's IP/port for the backend. Used by rewriteToClient's
	// isNewPeerBroadcast/isExistingPeerBroadcast branches to overwrite a
	// genuine broadcast's own embedded sin_zero with the described peer's
	// real, learned value rather than trusting whatever the host embedded
	// (which can be a stale/zeroed placeholder on the "empty variant" of
	// these messages - see those branches' own doc comments). Guarded by
	// the mu field above, same as connID.
	selfSinZero      [8]byte
	selfSinZeroKnown bool
}

// proxy is a generic per-client UDP session relay: one shared socket
// facing clients, one dialed socket per client facing the backend so
// backend replies naturally correlate back to the right client.
// rewriteToBackend/rewriteToClient are optional hooks applied to a
// packet's payload just before it's forwarded in that direction; either
// may be nil to pass packets through untouched.
type proxy struct {
	name       string // for logging, e.g. "discovery" or "session"
	cfg        config
	clientConn *net.UDPConn

	// pool replaces the old single backendAddr string - see hostPool.
	// backendAddrForHost picks which of a hostCandidate's addresses this
	// particular proxy cares about (discoveryBackendAddr for
	// discoveryProxy, sessionBackendAddr for sessionProxy) - the two
	// proxies share one pool but each dials a different address on it.
	pool               *hostPool
	backendAddrForHost func(h *hostCandidate) string
	// selectHost picks which host a new client's first packet through
	// this proxy gets dialed to - the two proxies plug in different
	// behavior here (added when host assignment moved from
	// discovery-time to session-connect-time): sessionProxy uses
	// pool.assignForClient (sticky, tiered - this is the real
	// matchmaking decision, see that method's doc comment on why it
	// belongs here and not on discovery traffic). discoveryProxy uses
	// pool.peekHost (non-committing - see that method's doc comment).
	selectHost func(clientIP string) (*hostCandidate, bool)

	rewriteToBackend func(payload []byte, cfg config, sess *session) []byte
	rewriteToClient  func(payload []byte, cfg config, sess *session) []byte

	// onClientPacket, if set, is called with every packet's real sender
	// address before normal forwarding - used to feed clientTracker.
	onClientPacket func(clientAddr *net.UDPAddr)

	// detectBackendOrigin and tracker, if both set, let run() recognize
	// unsolicited packets arriving from a backend itself (not from any
	// client) on this proxy's listening socket, and relay them to the
	// last-known real client instead of mistaking the backend for a new
	// client - see relayBackendInitiated and the clientTracker doc
	// comment. Only sessionProxy sets this today, same as the old
	// backendUDPAddr-set-or-not gate did - discoveryProxy has never
	// needed it. Which specific host a given backend packet came from is
	// resolved per-packet via pool.hostForBackendIP, not stored here,
	// since (unlike the old single backendUDPAddr) there's more than one
	// possible source now.
	detectBackendOrigin bool
	tracker             *clientTracker

	// onDiscoveryPingNoHost, if set, is called by discoveryProxy whenever
	// it answers a client's 0x20 liveness ping with a synthesized reply
	// (no eligible host in the pool) - wired in main() to
	// sessionProxy.beginSimulatedHostOpen. This is what lets the session
	// proxy kick off its proactive host-initiated open before the client
	// ever sends anything on port 2300 itself - see that method's own doc
	// comment for why this exists. discoveryProxy has no direct reference
	// to sessionProxy (see the var-then-literal wiring for the reverse
	// direction just above sessionProxy's own construction in main()), so
	// this indirection is the same pattern as podLister's callbacks below.
	// clientSinZero is the client's own self-reported sin_zero, extracted
	// from the same 0x20 ping - see synthesizedSockaddr's doc comment
	// (corrected 2026-08-18) for why this must be threaded through rather
	// than invented at the session-proxy side.
	onDiscoveryPingNoHost func(clientIP net.IP, clientSinZero [8]byte)

	// onSessionRemoved, if set, is called whenever a client's session is
	// torn down, however that happens - backendToClient's own read-error
	// cleanup or reapIdleSessions' idle-timeout cleanup both call it, so
	// callers get one consistent "this client is gone" signal regardless
	// of which path caught it. Takes the full session (not just the
	// address) so callers can reach sess.host - added 2026-08-14 when
	// this got wired up for real, see main()'s sessionProxy construction.
	//
	// History: NOT wired to anything on sessionProxy from 2026-08-12
	// until now - firing pair-relay teardown from a plain host-channel
	// idle-timeout broke the second client's join outright back then,
	// because a client mid-handshake with its peer over the pair relay
	// can look idle on the host channel alone while genuinely still
	// present. Fixed at the source this time: reapIdleSessions itself
	// now checks the pair relay's own per-client activity
	// (pairRelay.idleFor) before ever reaping a session whose host is
	// already paired, so by the time this hook fires, both channels have
	// actually gone quiet - not just the one this file happened to be
	// watching.
	onSessionRemoved func(sess *session)

	mu       sync.Mutex
	sessions map[string]*session

	// simulatedClients tracks per-client state for the synthesized
	// handshake+rejection sequence sent whenever there's no eligible
	// host - see forwardToBackend's "session" case. Guarded by its own
	// mutex, deliberately separate from mu/sessions above since this
	// tracks fake, no-real-backend clients rather than real ones -
	// keeping it separate avoids any risk of this synthesis path
	// interacting with real session bookkeeping. Reactive, not a fixed
	// script - see simulatedClientState's own doc comment for why a
	// fixed one-shot sequence (tried first, 2026-08-15) wasn't enough.
	simulatedClientsMu sync.Mutex
	simulatedClients   map[string]*simulatedClientState
}

// simulatedClientState is one client's progress through the synthesized
// handshake sent whenever forwardToBackend finds no eligible host.
//
// Reactive, not a fixed timer-driven script: a first attempt
// (2026-08-15) fired self/peer-open, ack, two name-broadcasts, and the
// rejection on a fixed ~150ms timer regardless of what the client
// itself sent - confirmed live NOT to work (client stayed in
// "Attempting to Connect"). Closely re-reading the real reference
// capture's own timing showed why: both sides retry and interleave
// somewhat independently (six 0x00 retries over ~600ms, the joiner
// sending its own 0x02 before the host's, the host re-sending an older
// name-broadcast seq after already sending a newer one) - consistent
// with an underlying reliable-transport retry/ack layer neither this
// struct nor the rest of this file has full visibility into. This
// version instead reacts to each of the client's own packets as they
// arrive and replies in kind (its own 0x00 answered with the host's own
// 0x00, every retry included; its own 0x02 answered with the host's;
// its own name-broadcast answered with the host's, then the rejection)
// - matching the real capture's own apparent shape of "the host
// responds to what it's just been told, not on a fixed schedule."
type simulatedClientState struct {
	mu sync.Mutex
	// clientConnID is learned from the client's own first 0x00 message
	// (see synthesizeSelfPeerOpen's doc comment on why this is never
	// guessed).
	clientConnID     [2]byte
	knowClientConnID bool
	// nameBroadcastStarted guards beginSimulatedNameBroadcast against
	// being kicked off more than once per client - see that method's own
	// doc comment.
	nameBroadcastStarted bool
	// nextNameBroadcastSeq is the next seq value to stamp on an outbound
	// 03-wrapped message (name-broadcast or the rejection) - see
	// runSimulatedNameBroadcastLoop's doc comment for why this can't be
	// hardcoded to a fixed value. Starts at 0, incremented after every
	// send.
	nextNameBroadcastSeq uint16
	// rejected guards against sending the rejection (or anything after
	// it) more than once per client.
	rejected bool
	// lastActivity is a UnixNano timestamp of the most recent send or
	// receive touching this client - atomic so beginSimulatedHostOpen's
	// guard (see simulatedClientIdleExpiry) can read it without taking
	// st.mu, avoiding a lock-ordering dependency between it and
	// simulatedClientsMu (see beginSimulatedHostOpen's own doc comment
	// for the bug this exists to fix).
	lastActivity atomic.Int64
}

// touch records that this client's entry just saw real activity (a send
// or a receive) - see lastActivity's own doc comment.
func (st *simulatedClientState) touch() {
	st.lastActivity.Store(time.Now().UnixNano())
}

// idleFor reports how long it's been since touch was last called. A
// never-touched entry (lastActivity still zero) reports 0, not some huge
// duration - callers only use this to decide whether an EXISTING entry
// has gone stale, and a just-created entry is never stale.
func (st *simulatedClientState) idleFor() time.Duration {
	last := st.lastActivity.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// simulatedClientIdleExpiry is how long a simulatedClients entry can sit
// with no activity before beginSimulatedHostOpen treats it as abandoned
// and starts a genuinely fresh attempt instead of silently doing nothing
// (see that function's own doc comment for the bug this fixes). Set
// comfortably past the ~2-minute patience window a real client has been
// confirmed to wait (docs/lobby/packet-handling-design.md) before giving
// up and resigning on its own, so a client that's still legitimately
// waiting is never evicted out from under itself.
const simulatedClientIdleExpiry = 3 * time.Minute

// closeSession forcibly closes and removes clientAddr's session, the
// same effect as its backend connection erroring out naturally (that
// existing path - backendToClient's own cleanup - is what actually does
// the map removal and fires onSessionRemoved; this just triggers it).
// Idempotent and safe to call on an already-gone session: a resign burst
// arrives as roughly ten rapid duplicate packets (see isResignBurst), so
// this will typically be called several times per real departure.
func (p *proxy) closeSession(clientAddr *net.UDPAddr) {
	p.mu.Lock()
	sess, ok := p.sessions[clientAddr.String()]
	p.mu.Unlock()
	if !ok {
		return
	}
	sess.backendConn.Close()
}

func (p *proxy) run() {
	buf := make([]byte, bufSize)
	for {
		n, srcAddr, err := p.clientConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[%s] read from client: %v", p.name, err)
			continue
		}
		payload := append([]byte(nil), buf[:n]...)

		// Correction (2026-08-12): matched on IP *and* port until today;
		// relaxed to IP-only after reproducing live a case where a
		// backend-originated broadcast (newPeerBroadcast) never reached
		// rewriteToClient at all - best explanation is the backend using
		// a different local port for this message than the one normal
		// per-client reply traffic uses (see relayBackendInitiated's doc
		// comment), which the old exact-match check would silently
		// misroute through forwardToBackend instead, treating the
		// backend's own packet as a brand-new client. IP-only is safe
		// here specifically because a host's own IP is never a real
		// client's (see clientTracker's doc comment on why real client
		// addresses never reach the backend directly to begin with).
		//
		// N-host update: was a single p.backendUDPAddr equality check;
		// now a pool lookup, since there's more than one legitimate
		// backend IP to recognize. Which host matched is threaded into
		// relayBackendInitiated so it scopes its client search to that
		// host's own sessions only - searching every host's sessions
		// indiscriminately here is exactly the kind of cross-match
		// misattribution this whole N-host pass exists to prevent (see
		// otherSessionClientAddr's doc comment for the same concern on
		// the pairing side).
		if p.detectBackendOrigin {
			if host, ok := p.pool.hostForBackendIP(srcAddr.IP); ok {
				p.relayBackendInitiated(payload, host)
				continue
			}
		}

		if p.onClientPacket != nil {
			p.onClientPacket(srcAddr)
		}
		p.forwardToBackend(srcAddr, payload)
	}
}

// sessionClientAddrForIP returns the real client address (on this proxy's
// own port) of an existing session belonging to ip *and assigned to
// host*, or nil if none does. Used by relayBackendInitiated to turn
// clientTracker's IP-only-trustworthy address into the right session
// when more than one might exist.
//
// N-host update: now filters by host too, not just ip. Without this, two
// different real clients on two different hosts that happen to share
// clientTracker's IP hint (or, worse, two clients whose sessions both
// exist but only one belongs to the host that actually sent this
// packet) could cross-attribute a backend broadcast to the wrong match
// entirely - the exact per-host isolation this whole pass exists for.
func (p *proxy) sessionClientAddrForIP(ip net.IP, host *hostCandidate) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.host == host && sess.clientAddr != nil && sess.clientAddr.IP.Equal(ip) {
			return sess.clientAddr
		}
	}
	return nil
}

// sessionClientAddrForConnID returns the real client address of the
// session whose learned backend conn-ID (session.connID, see
// wrapperConnID) matches id *and which belongs to host*, or nil if none
// is known yet. Added 2026-08-12 as relayBackendInitiated's primary
// attribution method - see that function's doc comment for why this is
// more reliable than the IP-recency guessing it previously relied on
// exclusively. Host-scoped as of the N-host pass - a conn-ID is only
// guaranteed unique *per host*, not across the whole pool, since it's
// each backend's own counter.
func (p *proxy) sessionClientAddrForConnID(id [2]byte, host *hostCandidate) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.host != host {
			continue
		}
		sess.mu.Lock()
		known := sess.connIDKnown && sess.connID == id
		sess.mu.Unlock()
		if known && sess.clientAddr != nil {
			return sess.clientAddr
		}
	}
	return nil
}

// anySessionClientAddr returns the real client address of any one
// existing session belonging to host - only used by relayBackendInitiated
// as a last-resort guess when clientTracker doesn't point at a session of
// its own (see that method). Host-scoped as of the N-host pass - an
// unscoped "any session at all" guess would happily hand a host A
// broadcast to a client actually playing on host B.
func (p *proxy) anySessionClientAddr(host *hostCandidate) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.host == host && sess.clientAddr != nil {
			return sess.clientAddr
		}
	}
	return nil
}

// sessionCountForHost returns how many of this proxy's currently-tracked
// sessions belong to host - used by hostPool.selectLocked to tell
// "empty" from "waiting for a second player" apart (see that method's
// doc comment). Only meaningful called on sessionProxy - discoveryProxy's
// own sessions are ephemeral per-query traffic, not real per-client
// occupancy, so wiring this to discoveryProxy instead would misclassify
// hosts. Counts idle-but-not-yet-reaped sessions too (up to
// idleTimeout's staleness window) - same acceptable imprecision every
// other idle-timeout-adjacent signal in this file already has.
func (p *proxy) sessionCountForHost(host *hostCandidate) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, sess := range p.sessions {
		if sess.host == host {
			n++
		}
	}
	return n
}

// otherSessionClientAddr returns the real address of the one session in
// p.sessions besides skip that's also assigned to the same host as skip -
// used by the 0x29 new-peer-broadcast rewrite to find the newly-joined
// peer's real address from sessionProxy's own session map, since the
// broadcast's own embedded address is never usable directly (see
// newPeerBroadcast's doc comment). Safe to assume at most one "other"
// session exists *within one host's match*: this project only ever hosts
// 1v1s (host + exactly two real clients, see CLAUDE.md's Goals), so
// besides skip there's never more than one candidate *for that host*.
//
// N-host update (this is the single most important fix in this pass):
// previously scanned every session on the proxy with no host filter at
// all - harmless with one host (there was only ever one "other" to find,
// full stop), but with two concurrent matches this would have handed
// clienta (host A) whichever session Go's map iteration happened to hit
// first, including a client actually playing on host B. That's not a
// crash or a dropped packet, it's a silently wrong pairing - exactly the
// failure mode flagged before starting this work.
func (p *proxy) otherSessionClientAddr(skip *net.UDPAddr, host *hostCandidate) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.host != host || sess.clientAddr == nil {
			continue
		}
		if sess.clientAddr.IP.Equal(skip.IP) && sess.clientAddr.Port == skip.Port {
			continue
		}
		return sess.clientAddr
	}
	return nil
}

// sessionSinZero looks up addr's own learned self-reported sin_zero
// (session.selfSinZero) - used by rewriteToClient's isNewPeerBroadcast/
// isExistingPeerBroadcast branches to describe addr correctly in a
// broadcast sent to its peer, rather than trusting whatever sin_zero the
// host happened to embed (see those branches' own doc comments). Returns
// ok=false if no session is known for addr yet, or its sin_zero hasn't
// been learned from real traffic yet.
func (p *proxy) sessionSinZero(addr *net.UDPAddr) (sinZero [8]byte, ok bool) {
	p.mu.Lock()
	sess, exists := p.sessions[addr.String()]
	p.mu.Unlock()
	if !exists {
		return sinZero, false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.selfSinZero, sess.selfSinZeroKnown
}

// pollOtherSessionClientAddr is otherSessionClientAddr, retried for up to
// pollTimeout (checking every pollInterval) if no other session exists
// yet.
//
// Added 2026-08-12 after reproducing live the race this exists to
// survive: the host's address-broadcast messages (newPeerBroadcast/
// existingPeerBroadcast) can arrive at this proxy before the other real
// client's own session has been registered here yet - purely a timing
// question on this proxy's side, since goroutine scheduling across the
// discovery/session listeners gives no ordering guarantee even when the
// underlying network events happened in a sensible order. Originally
// handled by just logging the miss and forwarding the packet unmodified,
// on the assumption the host would retry the broadcast shortly after
// (observed happening in one earlier test). Confirmed live that the host
// does NOT reliably retry: a real 2-client join reproduced this exact
// miss with zero retry, permanently starving one client of the other's
// address and reproducing the full 120s "Attempting to Connect" hang
// this whole feature exists to prevent - even though the *other* client
// (which didn't hit the race) correctly reached out hundreds of times
// over the relay, all silently ignored by the client that never got
// armed to expect them. Polling here means the callers can synthesize
// the rewrite themselves the moment the session appears, instead of
// depending on the host to resend anything.
func (p *proxy) pollOtherSessionClientAddr(skip *net.UDPAddr, host *hostCandidate, pollTimeout, pollInterval time.Duration) *net.UDPAddr {
	deadline := time.Now().Add(pollTimeout)
	for {
		if other := p.otherSessionClientAddr(skip, host); other != nil {
			return other
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(pollInterval)
	}
}

// relayBackendInitiated handles a packet the backend sent unprompted to
// this proxy's listening socket (rather than as a reply on a per-client
// dialed connection) - see clientTracker's doc comment for why the backend
// ends up doing this at all, and why the backend can't address such a
// packet at a specific client itself: it always sends to this same
// well-known listening address regardless of which client it means, since
// every client looks identical to it (same rewritten proxy address).
//
// Correction (2026-08-12): this used to be reached only for packets
// matching p.backendUDPAddr's IP *and* port exactly; now IP-only (see
// run()'s own correction comment) after reproducing live a
// newPeerBroadcast that reached this proxy's listening socket from a
// different local port than ordinary per-client reply traffic uses -
// meaning this function now needs to handle a wider range of backend
// traffic than before, including messages that DO know which client
// they're for (their own wrapper conn-ID, see wrapperConnID) even though
// this function previously had no way to know that.
//
// Picking the right client to relay to is therefore inferred, in order:
//  1. **Wrapper conn-ID match** (added 2026-08-12,
//     sessionClientAddrForConnID) - if payload is "03 00"-wrapped, its
//     conn-ID is protocol content the backend itself chose specifically
//     to identify which client relationship this packet belongs to, not
//     a guess. Confirmed stable per-sender across every capture this
//     project has taken. Most reliable option when available; only
//     available once at least one packet has been correctly attributed
//     to that session already (see backendToClient, which learns it).
//  2. Whichever real client most recently sent discovery-port traffic
//     (clientTracker) - reliably whoever is currently mid-connect, since a
//     client stops browsing/pinging once actually joined - matched by IP
//     against this proxy's own sessions to find that same client's address
//     *on this proxy's own port* (clientTracker's own port is wrong here,
//     see its doc comment). This is what makes a second, still-connecting
//     client resolve correctly even while a first client's already-
//     established session exists (2026-08-08 fix - see
//     docs/directplay8-protocol.md for the symptom this was causing: the
//     backend's packets for a new client kept landing on an existing one
//     instead).
//  3. If no session matches that IP yet (the client hasn't sent its own
//     first packet on this port), any one existing session - correct by
//     construction when there's only one, a reasonable guess otherwise.
//  4. If there are no sessions at all, the tracker's raw address - wrong
//     port, so effectively a no-op send, but harmless and better than
//     dropping the packet outright (matches pre-fix behavior for the
//     single-client bootstrap case).
//
// host is which pool member sent this (resolved by run() via
// pool.hostForBackendIP before calling this) - every fallback tier below
// that searches p.sessions now scopes that search to host, so a packet
// from host A can never get attributed to a client actually playing on
// host B. Tier 4 (the tracker's raw address) is the one exception: it's
// only reached when there are no sessions on this proxy for *any* host
// yet, so there's nothing to scope against - same "wrong port, harmless
// no-op" caveat as before N-host support.
func (p *proxy) relayBackendInitiated(payload []byte, host *hostCandidate) {
	var clientAddr *net.UDPAddr
	if id, ok := wrapperConnID(payload); ok {
		clientAddr = p.sessionClientAddrForConnID(id, host)
	}
	if clientAddr == nil {
		if recent := p.tracker.get(); recent != nil {
			clientAddr = p.sessionClientAddrForIP(recent.IP, host)
		}
	}
	if clientAddr == nil {
		clientAddr = p.anySessionClientAddr(host)
	}
	if clientAddr == nil {
		clientAddr = p.tracker.get()
	}
	if clientAddr == nil {
		log.Printf("[%s] backend %s sent unsolicited data but no client seen yet, dropping", p.name, host.id)
		return
	}
	out := payload
	if p.rewriteToClient != nil {
		// No real *session exists for this direction (see the doc comment
		// above) - just enough of one to carry the real client's address
		// and host, which rewriteToClient needs for the session
		// handshake's "peer" field and for looking up the right
		// per-host matchState respectively.
		out = p.rewriteToClient(payload, p.cfg, &session{clientAddr: clientAddr, host: host})
	}
	if _, err := p.clientConn.WriteToUDP(out, clientAddr); err != nil {
		log.Printf("[%s] relay backend-initiated packet to client %s: %v", p.name, clientAddr, err)
	}
}

func (p *proxy) forwardToBackend(clientAddr *net.UDPAddr, payload []byte) {
	key := clientAddr.String()

	p.mu.Lock()
	sess, ok := p.sessions[key]
	p.mu.Unlock()

	if !ok {
		// Host selection + dialing happen here, on this client's first
		// packet through this proxy - deliberately OUTSIDE p.mu (see
		// correction below). See proxy.selectHost's own doc comment for
		// why discoveryProxy and sessionProxy plug in different behavior
		// here (sessionProxy's call is the real matchmaking decision,
		// discoveryProxy's is a non-committing peek).
		//
		// Correction (2026-08-14): this used to hold p.mu across this
		// entire branch, including selectHost and the blocking DialUDP
		// call - wrong on two counts. First, plain lock-hygiene: no
		// proxy-internal reason blocking I/O needs to happen under this
		// lock, and holding it that long serializes every other client's
		// packets behind one new client's dial. Second, and what actually
		// surfaced it: sessionProxy's selectHost (pool.assignForClient) can
		// call back into sessionProxy.sessionCountForHost (see hostPool's
		// selectLocked), which takes this exact same p.mu - a guaranteed
		// self-deadlock (Go's sync.Mutex isn't reentrant) the moment
		// sessionProxy's own call is the first one to reach
		// selectLocked's real logic rather than assignForClient's cached
		// fast path. That only started happening once discovery-time
		// assignment was removed (see hostPool.peekHost's doc comment) -
		// before that, discoveryProxy always populated pool.assigned
		// first, so this path was dormant. Reproduced live: the whole
		// status HTTP server wedged too, since the deadlocked goroutine
		// never reached assignForClient's own deferred pool.mu.Unlock()
		// either.
		host, ok := p.selectHost(clientAddr.IP.String())
		if !ok {
			// No eligible host (pool empty, or every host full) - synthesize
			// a host's own responses rather than silently dropping the
			// client's packet. Real production behavior, not test-only
			// scaffolding (promoted from behind SIMULATE_FULL_LOBBY
			// 2026-08-18 - see lobby/packet-handling-design.md's "Case 3 in
			// detail" section for the byte-level justification of every
			// synthesized message below). Sends directly on p.clientConn
			// (this proxy's shared client-facing socket) rather than
			// through the normal per-session backendConn machinery, since
			// there's deliberately no real backend involved.
			switch p.name {
			case "discovery":
				// The client's LAN-browse/Direct-Connect screen fires
				// two genuinely different query types on this port,
				// not just one - the 0x25 enumerate query and a
				// separate 0x20 liveness ping (see
				// synthesizeEmptyPoolPingReply's own doc comment for
				// the live bug this distinction fixes). Reply with
				// the shape that actually matches what was asked.
				var queryType byte
				if len(payload) > 0 {
					queryType = payload[0]
				}
				var out []byte
				if queryType == 0x20 {
					out = synthesizeEmptyPoolPingReply(p.cfg)
					// Real host capture (2026-08-17) confirmed the host
					// proactively opens the session-port handshake right
					// around this point, unprompted - see
					// onDiscoveryPingNoHost's own doc comment. Fire-and-
					// forget: does nothing if already kicked off for
					// this client (retried pings are common).
					if p.onDiscoveryPingNoHost != nil {
						// The client's own self-described sockaddr sits
						// at payload offset 5 (5-byte 0x20 header), its
						// sin_zero within that at offset 5+8=13, 8 bytes -
						// see synthesizedSockaddr's doc comment (corrected
						// 2026-08-18) for why this must be learned and
						// echoed back, not invented.
						var clientSinZero [8]byte
						if len(payload) >= 21 {
							copy(clientSinZero[:], payload[13:21])
						}
						p.onDiscoveryPingNoHost(clientAddr.IP, clientSinZero)
					}
				} else {
					out = synthesizeEmptyPoolDiscoveryReply(p.cfg)
				}
				log.Printf("[%s] no eligible host: sending synthesized reply (query type 0x%02x) to client %s", p.name, queryType, key)
				if _, err := p.clientConn.WriteToUDP(out, clientAddr); err != nil {
					log.Printf("[%s] sending synthesized discovery reply to %s: %v", p.name, key, err)
				}
			case "session":
				p.simulateSessionPacket(clientAddr, payload)
			}
			return
		}
		backendAddr := p.backendAddrForHost(host)
		backendUDPAddr, err := net.ResolveUDPAddr("udp", backendAddr)
		if err != nil {
			log.Printf("[%s] resolving backend %s (%s): %v", p.name, host.id, backendAddr, err)
			return
		}
		// Bind the backend-facing socket to our own known public IP
		// rather than letting the OS pick a local address via routing -
		// this pod runs hostNetwork, so binding to the node's own
		// address is always valid, and it guarantees the address we
		// might embed in a rewritten packet (see rewriteToBackend on the
		// session proxy) is actually reachable, instead of depending on
		// whatever interface the kernel happened to route the backend
		// dial through.
		localAddr := &net.UDPAddr{IP: net.IP(p.cfg.publicIP[:])}
		backendConn, err := net.DialUDP("udp", localAddr, backendUDPAddr)
		if err != nil {
			log.Printf("[%s] dialing backend %s (%s) for client %s: %v", p.name, host.id, backendAddr, key, err)
			return
		}
		sess = &session{backendConn: backendConn, clientAddr: clientAddr, host: host, lastSeen: time.Now()}
		p.mu.Lock()
		p.sessions[key] = sess
		p.mu.Unlock()
		log.Printf("[%s] new session: client %s -> %s (%s)", p.name, key, host.id, backendAddr)
		go p.backendToClient(key, clientAddr, sess)
	}

	sess.mu.Lock()
	sess.lastSeen = time.Now()
	sess.mu.Unlock()

	out := payload
	if p.rewriteToBackend != nil {
		out = p.rewriteToBackend(payload, p.cfg, sess)
	}
	if _, err := sess.backendConn.Write(out); err != nil {
		log.Printf("[%s] write to backend for client %s: %v", p.name, key, err)
	}
}

// beginSimulatedHostOpen proactively sends the host-initiated self/peer-
// open toward a client on the session port, without waiting for the
// client to speak on 2300 first - only sessionProxy calls this
// meaningfully (wired via onDiscoveryPingNoHost in main()), even though
// it's a method on *proxy like everything else here.
//
// This was the missing piece behind the whole "client never sends its
// own 0x02" investigation. Found 2026-08-17 by diffing a freshly-
// captured genuine success (both host's and joiner's own vantage points
// independently, archiving/sessions/20260817-212506-session-
// establishment-investigation) against what this proxy's own reactive-
// only simulateSessionPacket does: the real host sends the first 0x00
// completely unprompted - zero prior packets from the client on port
// 2300 at all - roughly 8 times over ~700ms, and only THEN does the
// joiner answer, with its own 0x02 (not a 0x00 - it doesn't send its own
// self/peer-open until immediately after that). This proxy's dispatch
// only ever reacted to something the client sent first, which is
// backwards from what a real host does, and left the client waiting
// forever for an open that would never come. The client's session-port
// address is always clientIP:2300 by convention (confirmed live - both
// sides use the fixed port, not an ephemeral one), so the client's IP
// alone (known as soon as it pings discovery) is enough to start this;
// no extra address discovery needed.
//
// clientSinZero is the client's own self-reported sin_zero, learned
// from its 0x20 ping - see synthesizedSockaddr's own doc comment
// (corrected 2026-08-18) for why the "peer" sockaddr block below must
// echo this back exactly rather than use an invented value.
//
// Correction (2026-08-18): the guard below used to be a permanent
// "already have an entry for this key, do nothing" check that never
// expired - once ANY discovery ping from a given client IP created a
// simulatedClients entry, every subsequent ping from that same IP,
// forever (this map has no other cleanup), silently returned without
// ever sending another proactive open, even if the first attempt timed
// out completely unanswered. Found by diffing a synthesis capture taken
// against a long-lived aom-lobby pod (already fielding earlier test
// sessions against the same client IP) against the code path here: the
// capture showed zero proactive 0x00 sends at all, only a reactive 0x02
// once the client gave up waiting and opened on its own - exactly what
// this stale guard produces. In production this is a real bug, not just
// a testing artifact: any player whose first Direct-Connect attempt
// fails and who retries would hit this same permanently-stuck guard on
// every attempt after the first, for as long as the pod stays up. Now
// an existing entry only blocks a fresh attempt while it's genuinely
// still active (idleFor() < simulatedClientIdleExpiry); a stale one is
// evicted and replaced instead of silently doing nothing.
func (p *proxy) beginSimulatedHostOpen(clientIP net.IP, clientSinZero [8]byte) {
	sessionAddr := &net.UDPAddr{IP: clientIP, Port: int(p.cfg.publicPort)}
	key := sessionAddr.String()

	p.simulatedClientsMu.Lock()
	if p.simulatedClients == nil {
		p.simulatedClients = make(map[string]*simulatedClientState)
	}
	if existing, ok := p.simulatedClients[key]; ok {
		existing.mu.Lock()
		terminal := existing.rejected
		existing.mu.Unlock()
		// A rejected session is done, not stuck - live-tested 2026-08-19: a
		// client that gets rejected and immediately retries (completely
		// normal real-player behavior, e.g. right after seeing "Host game
		// is full") would otherwise sit blocked for up to
		// simulatedClientIdleExpiry, since that timer is sized for "how
		// long to wait before assuming an UNANSWERED attempt was
		// abandoned" - the wrong yardstick for a session that already
		// reached a clean terminal state in under a second. Don't make a
		// legitimate immediate retry wait out a timer meant for a
		// different problem.
		if !terminal && existing.idleFor() < simulatedClientIdleExpiry {
			p.simulatedClientsMu.Unlock()
			return // already kicked off (or already talking) for this client, still active
		}
		// Stale (idle past the point any real client would still be
		// waiting) or terminal (already rejected) - either way, treat this
		// ping as a genuinely fresh attempt instead of a continuation of
		// the old one.
		delete(p.simulatedClients, key)
	}
	st := &simulatedClientState{}
	st.touch()
	p.simulatedClients[key] = st
	p.simulatedClientsMu.Unlock()

	go func() {
		var clientIP4 [4]byte
		copy(clientIP4[:], sessionAddr.IP.To4())
		open := synthesizeSelfPeerOpen(fakeHostConnID, p.cfg.publicIP, p.cfg.publicPort, clientIP4, uint16(sessionAddr.Port), clientSinZero)

		// Real host retried ~8x over ~700ms (roughly every 100ms) before
		// the joiner answered - match that cadence, and stop early once
		// the client's own reply arrives (learned via simulateSessionPacket
		// setting knowClientConnID/rejected on this same st).
		for i := 0; i < 20; i++ {
			st.mu.Lock()
			done := st.knowClientConnID || st.rejected
			st.mu.Unlock()
			if done {
				return
			}
			if _, err := p.clientConn.WriteToUDP(open, sessionAddr); err != nil {
				log.Printf("[%s] no-eligible-host: proactive host-open to %s: %v", p.name, key, err)
			}
			st.touch()
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// beginSimulatedNameBroadcast proactively sends the host's own name-
// broadcast once the connID exchange (0x00/0x02) is mutually complete,
// instead of waiting for the client to send its own 0x03 first - same
// missing-proactive-step bug as beginSimulatedHostOpen, one layer up the
// protocol, found the same way: the 2026-08-17 retest of that first fix
// showed the 0x00/0x02 exchange completing correctly, then the
// connection settling into a pure 07ff heartbeat loop forever - never
// progressing to 0x03 on either side. The real capture's own timeline
// explains why: the host sends its own 0x03 first (54.468042, ~84ms
// after both sides' 0x02 acks), and only then does the joiner reply
// with its own 0x03 (54.477559) - the client was waiting on the host to
// speak first here too, exactly like the connection-open phase.
//
// Guard-and-set (st.nameBroadcastStarted) is the CALLER's job, done
// under the same st.mu lock simulateSessionPacket already holds when it
// decides to call this - st.mu must NOT be locked again in here, only
// by runSimulatedNameBroadcastLoop's own periodic check, which runs
// later in its own goroutine after the caller has returned/unlocked.
func (p *proxy) beginSimulatedNameBroadcast(clientAddr *net.UDPAddr, st *simulatedClientState) {
	go p.runSimulatedNameBroadcastLoop(clientAddr, st)
}

// Sends a fresh seq value each retry, not a fixed 0 - a real host's own
// retries were confirmed (2026-08-18, lobby/packet-handling-design.md's
// "Case 3 in detail" section) to vary seq across sends (0, 1, 0 in that
// capture) rather than resending byte-identical packets. This codebase
// doesn't know the real rule (only one, non-monotonic sample exists),
// but a plain incrementing counter is a closer match than a frozen
// constant, which is what shipped here before this fix.
//
// Waits BEFORE sending too, not just between retries - corrected
// 2026-08-18 after finding a real host's own ack-to-name-broadcast gap
// is consistently ~84-100ms across two independent captures (never
// instant), while this proxy's own first send used to fire in the same
// instant as the ack that triggers it (~0.2ms gap, confirmed live -
// roughly 500x faster than any real host). Plausible mechanism: AoM's
// own lobby-state update loop runs at a leisurely ~2-5Hz (a real client
// only checks for incoming lobby updates a few times a second, not
// continuously), so two logically-distinct messages arriving within
// microseconds of each other land in the exact same processing cycle
// from the client's own perspective, rather than as two separate
// events - see lobby/packet-handling-design.md for the full reasoning.
// Pacing every send to roughly one per real host tick, starting with
// the very first one, is a closer match to what a real host's own
// sending cadence actually looks like.
func (p *proxy) runSimulatedNameBroadcastLoop(clientAddr *net.UDPAddr, st *simulatedClientState) {
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		st.mu.Lock()
		done := st.rejected
		seq := st.nextNameBroadcastSeq
		st.nextNameBroadcastSeq++
		st.mu.Unlock()
		if done {
			return
		}
		// A real host bundles an UNPROMPTED heartbeat with each name-
		// broadcast (retry) send, not just the first one - confirmed
		// 2026-08-18 from the host's own vantage point of the reference
		// capture: heartbeat then name-broadcast, only 0.3ms apart, both
		// on the same ~100ms tick. This proxy used to only ever send
		// 07ff reactively (in response to a client's own heartbeat) -
		// meaning at the exact tick where a real host proactively
		// signals "I'm alive and about to speak", this one stayed silent
		// on that specific signal. Order matches the real capture:
		// heartbeat first, then name-broadcast.
		hb := synthesizeHeartbeat(fakeHostConnID)
		if _, err := p.clientConn.WriteToUDP(hb, clientAddr); err != nil {
			log.Printf("[%s] no-eligible-host: proactive heartbeat to %s: %v", p.name, clientAddr.String(), err)
		}
		nb := synthesizeNameBroadcast(fakeHostConnID, seq, simulatedFullLobbyCrackSignature)
		if _, err := p.clientConn.WriteToUDP(nb, clientAddr); err != nil {
			log.Printf("[%s] no-eligible-host: proactive name-broadcast to %s: %v", p.name, clientAddr.String(), err)
		}
		st.touch()
	}
}

// resetIfStaleConnID learns connIDBytes as st's clientConnID, resetting
// the rest of st's fields first if this is actually a NEW session
// reusing an old one's key rather than a retry of the same session.
// Returns true the first time a given connID is learned (for the
// caller's own "just learned it" log line) - false on every subsequent
// retry with the same connID.
//
// Found live 2026-08-18, back-to-back testing the always-on no-
// eligible-host synthesis path (promoted from behind SIMULATE_FULL_LOBBY
// the same day): classic DirectPlay's session port is always 2300 on
// both sides (see beginSimulatedHostOpen's doc comment), so
// simulatedClients' key (clientAddr.String(), i.e. "ip:2300") is
// entirely IP-derived - a genuinely new connection attempt from the
// same IP (a real client reconnecting, or - what actually happened here
// - Docker handing a fresh test container the same IP a prior one had)
// reuses the exact same map key. Without this check, the old
// simulatedClientState (already knowClientConnID=true from the PRIOR
// session) just kept echoing the stale old connID back to the new
// client forever, which a real client's own validation would have every
// reason to reject - this was masking as part of the deeper "client
// never sends its own 0x02" mystery when it's actually a separate,
// simpler bug.
func resetIfStaleConnID(st *simulatedClientState, connIDBytes []byte) bool {
	var connID [2]byte
	copy(connID[:], connIDBytes)
	if st.knowClientConnID && st.clientConnID == connID {
		return false
	}
	if st.knowClientConnID {
		// A genuinely new session reusing this address - not this
		// client's first packet, but the state below is now
		// meaningless. Doesn't cancel an in-flight
		// runSimulatedNameBroadcastLoop from the old session (it
		// self-stops once IT sees rejected/its own 20-iteration cap);
		// harmless overlap, not worth a cancellation channel here.
		st.rejected = false
		st.nameBroadcastStarted = false
		st.nextNameBroadcastSeq = 0
	}
	st.clientConnID = connID
	st.knowClientConnID = true
	return true
}

// simulateSessionPacket is the reactive handshake sent whenever there's
// no eligible host - see simulatedClientState's own doc comment for why
// this reacts to the client's own packets instead of running a fixed
// timer-driven script. Called from forwardToBackend's "no eligible
// host" branch for every session-port packet a client sends while
// there's no real backend at all.
func (p *proxy) simulateSessionPacket(clientAddr *net.UDPAddr, payload []byte) {
	key := clientAddr.String()

	p.simulatedClientsMu.Lock()
	if p.simulatedClients == nil {
		p.simulatedClients = make(map[string]*simulatedClientState)
	}
	st, ok := p.simulatedClients[key]
	if !ok {
		st = &simulatedClientState{}
		p.simulatedClients[key] = st
	}
	p.simulatedClientsMu.Unlock()

	st.touch()

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.rejected {
		return
	}

	send := func(stage string, b []byte) {
		if _, err := p.clientConn.WriteToUDP(b, clientAddr); err != nil {
			log.Printf("[%s] no-eligible-host: sending %s to %s: %v", p.name, stage, key, err)
		}
	}

	switch {
	case len(payload) >= 6 && payload[0] == 0x00 && payload[1] == 0x00:
		// Client's own self/peer-open - the "mirrored" open it sends
		// (right after its own 0x02 ack, once the host opened first) or,
		// in principle, one it sends unprompted.
		//
		// Correction (2026-08-17): this used to echo back ANOTHER 0x00,
		// which is backwards - confirmed identically in two independent
		// real captures (2026-08-15's lobby-full-rejection session and
		// 2026-08-17's session-establishment one) that a received 0x00 is
		// always answered with a 0x02 ack, never with another 0x00.
		// Concretely: in both captures, whichever side sends 0x00 first
		// gets ~6-8 unanswered retries over ~600-700ms (the recipient
		// stays completely silent - it's the SENDER's own internal
		// timeout driving the retries, not anything the recipient is
		// withholding), and the very first reply that ever arrives is a
		// 0x02, not a 0x00. This branch mistakenly modeled "answered
		// every single one" as "replied to every retry in kind" - it
		// actually meant "every 0x00 this proxy received (from whichever
		// exchange) eventually got one ack", not "reply with 0x00".
		if resetIfStaleConnID(st, payload[2:4]) {
			log.Printf("[%s] no-eligible-host: learned connID %x from client %s's self/peer-open, replying in kind", p.name, st.clientConnID, key)
		}
		send("handshake ack", synthesizeHandshakeAck(fakeHostConnID, st.clientConnID))
		if !st.nameBroadcastStarted {
			st.nameBroadcastStarted = true
			p.beginSimulatedNameBroadcast(clientAddr, st)
		}

	case len(payload) >= 2 && payload[0] == 0x07 && payload[1] == 0xff && st.knowClientConnID:
		// Client's own "07ff" heartbeat/ack (see
		// docs/directplay8-packet-classification.md's confirmed-shapes
		// table) - the real capture showed the host answering these too,
		// periodically, alongside the 0x00 retries; this proxy's own
		// reactive loop never sent any at all until this was added,
		// found live 2026-08-15 by re-capturing the client's own
		// traffic and noticing it kept sending these with no reply.
		send("heartbeat", synthesizeHeartbeat(fakeHostConnID))

	case len(payload) >= 6 && payload[0] == 0x02 && payload[1] == 0x00:
		// Client's own handshake ack. Learn its connID here too, not
		// just from a client-sent 0x00 - confirmed live 2026-08-17 (see
		// beginSimulatedHostOpen's doc comment) that once the host opens
		// first, the client's very FIRST packet on this port is its own
		// 0x02, not a 0x00; requiring knowClientConnID already true (as
		// this case used to) meant this branch could never fire at all
		// in that ordering.
		if resetIfStaleConnID(st, payload[2:4]) {
			log.Printf("[%s] no-eligible-host: learned connID %x from client %s's handshake ack, replying in kind", p.name, st.clientConnID, key)
		}
		send("handshake ack", synthesizeHandshakeAck(fakeHostConnID, st.clientConnID))
		if !st.nameBroadcastStarted {
			st.nameBroadcastStarted = true
			p.beginSimulatedNameBroadcast(clientAddr, st)
		}

	case len(payload) >= 6 && payload[0] == 0x03 && payload[1] == 0x00 && st.knowClientConnID:
		// Client's own 03-wrapped broadcast - during this fake
		// sequence, the only thing a genuinely-connecting client sends
		// on this port shaped like this is its own name-broadcast (see
		// synthesizeNameBroadcast's doc comment on scope: ready-toggle/
		// resign only happen later, in a real match this session never
		// reaches). Reply with the host's own name-broadcast, then -
		// matching the real capture's own shape, where the rejection
		// followed directly after the joiner announced its name - send
		// the rejection right after. Both seq values continue counting
		// up from st.nextNameBroadcastSeq (shared with
		// runSimulatedNameBroadcastLoop's own retries) rather than using
		// fixed 0/1 - matches the real capture's own pattern, where the
		// rejection's seq was one past whatever the host's own name-
		// broadcast retries had last used, not an independent constant
		// (see lobby/packet-handling-design.md's "Case 3 in detail",
		// corrected 2026-08-18).
		nameSeq := st.nextNameBroadcastSeq
		st.nextNameBroadcastSeq++
		rejectSeq := st.nextNameBroadcastSeq
		st.nextNameBroadcastSeq++
		send("name-broadcast", synthesizeNameBroadcast(fakeHostConnID, nameSeq, simulatedFullLobbyCrackSignature))
		send("lobby-full rejection", synthesizeLobbyFullRejection(fakeHostConnID, rejectSeq))
		st.rejected = true
		log.Printf("[%s] no-eligible-host: sent lobby-full rejection to client %s", p.name, key)
	}
}

func (p *proxy) backendToClient(key string, clientAddr *net.UDPAddr, sess *session) {
	buf := make([]byte, bufSize)
	for {
		n, err := sess.backendConn.Read(buf)
		if err != nil {
			log.Printf("[%s] read from backend for client %s: %v", p.name, key, err)
			break
		}

		payload := buf[:n]

		sess.mu.Lock()
		sess.lastSeen = time.Now()
		if !sess.connIDKnown {
			// Learned here, not in relayBackendInitiated: this read loop
			// is scoped to sess's own per-client dialed backendConn, so
			// sess is known for certain, unlike relayBackendInitiated's
			// guesswork - see session.connID's doc comment and
			// relayBackendInitiated's own doc comment for why this
			// mapping matters.
			if id, ok := wrapperConnID(payload); ok {
				sess.connID = id
				sess.connIDKnown = true
			}
		}
		sess.mu.Unlock()

		if p.rewriteToClient != nil {
			payload = p.rewriteToClient(payload, p.cfg, sess)
		}
		if _, err := p.clientConn.WriteToUDP(payload, clientAddr); err != nil {
			log.Printf("[%s] write to client %s: %v", p.name, key, err)
		}
	}

	p.mu.Lock()
	delete(p.sessions, key)
	p.mu.Unlock()
	if p.onSessionRemoved != nil {
		p.onSessionRemoved(sess)
	}
}

// reapIdleSessions closes sessions that have gone quiet on this proxy's
// own channel (idleTimeout, checked every reapEvery). Shared by both
// discoveryProxy and sessionProxy.
//
// Cross-channel check (added 2026-08-14, sessionProxy-relevant only):
// before reaping a host-channel-idle session, check whether its host is
// already paired (sess.host.match.currentPair()) and, if so, whether
// this exact client has been active on that pair relay more recently
// than idleTimeout (pairRelay.idleFor). If the relay says they're still
// there, skip reaping this cycle - this is exactly the 2026-08-11
// regression's shape: a client mid-handshake (or just actively playing)
// with its peer can go quiet on the host channel alone without having
// left at all. Only once *both* channels have been silent past
// idleTimeout does this actually remove the session - see
// proxy.onSessionRemoved's own doc comment for what that then triggers.
// A session with no host or an unpaired match (sess.host.match.
// currentPair() == nil) has no second channel to check and reaps exactly
// as before - this only changes behavior for already-paired matches.
func (p *proxy) reapIdleSessions() {
	ticker := time.NewTicker(reapEvery)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		var removed []*session
		p.mu.Lock()
		for key, sess := range p.sessions {
			sess.mu.Lock()
			idle := now.Sub(sess.lastSeen)
			sess.mu.Unlock()
			if idle <= idleTimeout {
				continue
			}
			if sess.host != nil && sess.host.match != nil {
				if relay := sess.host.match.currentPair(); relay != nil {
					if relay.idleFor(sess.clientAddr) <= idleTimeout {
						continue
					}
				}
			}
			sess.backendConn.Close()
			delete(p.sessions, key)
			removed = append(removed, sess)
			log.Printf("[%s] closed idle session for client %s", p.name, key)
		}
		p.mu.Unlock()
		if p.onSessionRemoved != nil {
			for _, sess := range removed {
				p.onSessionRemoved(sess)
			}
		}
	}
}

func mustListen(addr string) *net.UDPConn {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Fatalf("resolving listen address %q: %v", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("listening on %q: %v", addr, err)
	}
	return conn
}

// pairRelay bridges exactly two real, already-known client addresses
// directly - the A<->B leg of a match that sessionProxy alone can't
// provide, since sessionProxy's single shared port only ever bridges a
// client to the fixed AoM backend. See
// docs/multi-peer-routing-design.md's "one dedicated relay port per
// pair" section.
//
// No official-DP8 analog: this is pure proxy-side infrastructure, not
// anything classic DirectPlay or [MC-DPL8CS]/[MC-DPL8R] know about or
// need to. A dedicated per-pair relay port is this proxy's own answer to
// a proxy-specific problem (see the type's own doc comment below for
// why), invented to make newPeerBroadcast/existingPeerBroadcast's
// rewritten addresses point somewhere that actually demultiplexes real
// clients correctly - the protocol itself has no concept of a relay at
// all, every peer in a genuine deployment just dials the other
// directly.
//
// This is deliberately NOT built by reusing the proxy struct above.
// proxy's model is asymmetric by design - one fixed backend, clients
// learned dynamically from whoever calls in - which fits client<->host
// (there's genuinely only one backend) but not client<->client, where
// BOTH sides are real, already-known endpoints and BOTH sides' traffic
// arrives on the same shared listening socket (see the doc comment on
// isSessionHandshake's "self" field: the receiver trusts the payload's
// embedded address for where to send future traffic, not the raw UDP
// source port, so both A and B end up addressing the relay's one shared
// port symmetrically). A first attempt reusing proxy here (2026-08-10)
// confirmed this the hard way: B's own replies landed on the shared
// listening socket, didn't match A's session key, and got misdiagnosed
// as "a new client" trying to reach backendAddr - which was ALSO B,
// producing a self-referential dial (B "connecting to" B) that failed
// immediately and repeated in a tight create/fail/retry loop. Since both
// real addresses are already known up front here (unlike client<->host,
// where the client is discovered dynamically), there's no need for
// proxy's per-client session/dial machinery at all - just a fixed
// two-way address switch.
type pairRelay struct {
	name string
	cfg  config
	conn *net.UDPConn
	// selfPort is conn's own bound port, embedded in the "self" field of
	// every session-handshake packet relayed through here (both
	// directions use the same relay port, unlike proxy's per-direction
	// dial-vs-listen split - see the type doc comment).
	selfPort uint16
	addrA    *net.UDPAddr
	addrB    *net.UDPAddr
	// onResign, if set, is called with a peer's address when a resign
	// burst crosses this relay (see isResignBurst) - lets main() hook
	// client<->client resign detection into the same session-teardown
	// path as client<->host resign detection (see proxy.closeSession).
	onResign func(peerAddr *net.UDPAddr)

	// mu guards lastSeenA/lastSeenB - per-endpoint activity tracking,
	// added so reapIdleSessions can tell "genuinely idle on both
	// channels" from "quiet on the host channel but still actively
	// exchanging traffic here" before freeing a match's slot - see that
	// function's own doc comment on the 2026-08-11 regression this
	// exists to avoid repeating.
	mu        sync.Mutex
	lastSeenA time.Time
	lastSeenB time.Time

	// notified records which real client addresses have definitely
	// received their own peer-address broadcast - either the genuine one
	// rewritten from the host, or (if the host's own never showed up in
	// time) the synthesized fallback - see ensurePairRelay's watchdog and
	// docs/multi-peer-routing-design.md's "Regression investigation
	// (2026-08-14)" section for why this exists. Keyed by
	// clientAddr.String(), guarded by mu above.
	notified map[string]bool
}

// markNotified records that addr has received its peer-address
// broadcast - called both for the genuine reactive-rewrite path (every
// ensurePairRelay call marks its own selfAddr) and the synthesized
// fallback path (the watchdog marks otherAddr once it sends). Idempotent.
func (r *pairRelay) markNotified(addr *net.UDPAddr) {
	r.mu.Lock()
	r.notified[addr.String()] = true
	r.mu.Unlock()
}

// isNotified reports whether addr has already received its peer-address
// broadcast - what the watchdog checks before deciding whether to
// synthesize one.
func (r *pairRelay) isNotified(addr *net.UDPAddr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notified[addr.String()]
}

// newPairRelay binds a fresh, dynamically-allocated port (never the
// fixed shared session port - reusing that would reproduce the exact bug
// this relay exists to fix, see newPeerBroadcast's doc comment) and
// starts relaying between addrA and addrB.
func newPairRelay(name string, cfg config, addrA, addrB *net.UDPAddr) *pairRelay {
	conn := mustListen(":0")
	now := time.Now()
	r := &pairRelay{
		name:      name,
		cfg:       cfg,
		conn:      conn,
		selfPort:  uint16(conn.LocalAddr().(*net.UDPAddr).Port),
		addrA:     addrA,
		addrB:     addrB,
		lastSeenA: now,
		lastSeenB: now,
		notified:  make(map[string]bool),
	}
	log.Printf("[%s] new A<->B pair relay on %s:%d, bridging %s <-> %s", name, net.IP(cfg.publicIP[:]), r.selfPort, addrA, addrB)
	return r
}

// idleFor reports how long addr has been silent on this relay - used by
// reapIdleSessions to distinguish a client that's genuinely gone from
// one that's just quiet on its host-channel session while still active
// here. An addr matching neither endpoint returns a very large duration
// deliberately (never protects a session it has no relationship to).
func (r *pairRelay) idleFor(addr *net.UDPAddr) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case udpAddrEqual(addr, r.addrA):
		return time.Since(r.lastSeenA)
	case udpAddrEqual(addr, r.addrB):
		return time.Since(r.lastSeenB)
	default:
		return time.Hour
	}
}

func udpAddrEqual(a, b *net.UDPAddr) bool {
	return a.IP.Equal(b.IP) && a.Port == b.Port
}

// isResignBurst reports whether payload matches the 3-byte "01 <connID>"
// client resign/leave signature (see docs/directplay8-protocol.md's
// "Client resign/leave signature" section) - sent as roughly ten rapid
// identical copies per real resign event, so callers should be
// idempotent (see closeSession/onClientGone). The trailing 2 bytes are
// the sender's own connection ID, which varies per session, so only the
// shape (3 bytes, leading 0x01) is matched, matching the doc's own
// conclusion that this is a fixed-shape signal, not a fixed-value one.
//
// Likely official-DP8 analog for the *intended* signal (structural, not
// byte-confirmed - see this file's top-of-file "Protocol layering"
// comment): [MC-DPL8R] section 2.2.1.4 HARD_DISCONNECT - "used to
// quickly disconnect... without waiting for remaining packets to be
// delivered," a reliable-layer CFRAME, not an upper-layer message - a
// plausible fit for a deliberately small, best-effort, repeatedly-sent
// signal like this one. A second, equally plausible candidate for what
// the *false-positive* case below might actually be:
// [MC-DPL8R] section 3.1.6.6 KeepAlive - "a reliable data frame (DFRAME)
// with no application payload," sent periodically whenever no other
// traffic has flowed recently, restarted "after another period of
// inactivity" on receipt of *any* valid packet. A periodic, otherwise-
// unexplained fixed-shape message with no user action behind it (see the
// 2026-08-11 correction below) is exactly what a KeepAlive would look
// like if classic DirectPlay's own reliable layer has an equivalent -
// worth checking directly against [MC-DPL8R]'s KeepAlive timing model
// before assuming this shape is resign-specific at all.
//
// Correction (2026-08-11): matching on shape alone turned out to be
// unsafe to *act* on. Live 2-real-client testing caught this exact
// shape firing as a precise, repeating burst exactly 2 minutes after
// every successful pairing (three cycles observed, each gap exactly
// 2:00), with no user action behind it at all - not a resign, some
// other periodic protocol message (a keepalive or re-sync tick?)
// coincidentally matching the same "rapid burst of identical 3-byte
// packets starting with 0x01" signature. The original capture this
// function was based on only ever saw the burst once, right before a
// confirmed clean stop - never checked whether the *shape* alone is
// unique to that event or whether something else on a timer produces
// the same shape. Turns out it isn't unique: acting on it (closing the
// session) was killing every real match about 2 minutes in, reproducing
// the exact "Attempting to Connect" hang the whole durability pass was
// meant to fix. Both call sites now only log this, they don't close
// anything - kept detecting (not removed) since the logging is useful
// forensic data for whoever eventually captures a real resign to diff
// against and find the actual distinguishing feature (a different
// connID pattern? bracketed by different neighboring traffic? never
// repeating exactly every 2 minutes?).
func isResignBurst(payload []byte) bool {
	return len(payload) == 3 && payload[0] == 0x01
}

// run reads every packet arriving on this relay's shared socket and
// forwards it to whichever of addrA/addrB ISN'T the sender - rewriting
// the session handshake's self/peer fields the same way sessionProxy
// does (self -> this relay's own address, so the receiver replies
// through the relay; peer -> the receiver's own real address, matching
// what its own validation expects, per isSessionHandshake's doc comment)
// - every other message type passes through unmodified.
//
// Also watches for a resign burst from either side (see isResignBurst)
// and reports it via onResign, if set - this is the client<->client
// half of departure detection (docs/multi-peer-routing-design.md's
// "Durability" section); the client<->host half lives in sessionProxy's
// rewriteToBackend. Unlike that direction, a resign burst crossing this
// relay hasn't actually been confirmed in a real capture yet (see the
// protocol doc's note that only client->host has been observed) - kept
// here anyway since the shape check is cheap and this is the only place
// that would ever see genuine client<->client resign traffic if it
// exists.
//
// Returns (via the loop simply ending) once r.conn is closed - closing
// the conn is how a caller tears this relay down (see
// matchState.onClientGone), and ReadFromUDP returning an error on a
// closed conn is the expected, only way this loop ever exits; it must
// break rather than retry, or a closed conn would spin this goroutine
// in a tight error-logging loop forever.
func (r *pairRelay) run() {
	buf := make([]byte, bufSize)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[%s] read: %v (relay stopping)", r.name, err)
			return
		}
		payload := append([]byte(nil), buf[:n]...)

		var dst *net.UDPAddr
		r.mu.Lock()
		switch {
		case udpAddrEqual(src, r.addrA):
			dst = r.addrB
			r.lastSeenA = time.Now()
		case udpAddrEqual(src, r.addrB):
			dst = r.addrA
			r.lastSeenB = time.Now()
		default:
			r.mu.Unlock()
			log.Printf("[%s] packet from unexpected sender %s (expected %s or %s), dropping", r.name, src, r.addrA, r.addrB)
			continue
		}
		r.mu.Unlock()

		if isResignBurst(payload) {
			// Logging only, NOT calling onResign - see isResignBurst's doc
			// comment's 2026-08-11 correction. onResign is left wired to
			// nothing for now (see ensurePairRelay) until this shape is
			// properly confirmed as an actual resign in this direction too.
			log.Printf("[%s] resign-shaped burst from %s seen (not acted on, see isResignBurst)", r.name, src)
			if r.onResign != nil {
				r.onResign(src)
			}
		}

		out := payload
		if isSessionHandshake(payload) {
			var dstIP [4]byte
			copy(dstIP[:], dst.IP.To4())
			out = rewriteSessionField(payload, sessionSelfOffset, r.cfg.publicIP, r.selfPort)
			out = rewriteSessionField(out, sessionPeerOffset, dstIP, uint16(dst.Port))
		}
		if r.cfg.verbose {
			log.Printf("[%s] relaying %s -> %s (%d bytes): %s", r.name, src, dst, len(payload), hex.EncodeToString(payload))
		}
		if _, err := r.conn.WriteToUDP(out, dst); err != nil {
			log.Printf("[%s] write to %s: %v", r.name, dst, err)
		}
	}
}

// matchState holds the one A<->B pair relay this proxy currently needs.
// This project only ever hosts 1v1s (host + exactly two real clients, see
// CLAUDE.md's Goals), so there's at most one such pairing active at a
// time - no need for a keyed collection of matches yet (see
// docs/multi-peer-routing-design.md's "Open questions" on multiple
// concurrent matches, which is explicitly out of scope for now).
type matchState struct {
	cfg config
	// closeSession is sessionProxy.closeSession, set once during main()'s
	// wiring - handed to every pairRelay this match creates (as its
	// onResign callback) so a client<->client resign burst tears down
	// that client's session via the same path as a client<->host one.
	closeSession func(clientAddr *net.UDPAddr)

	mu      sync.Mutex
	pair    *pairRelay
	ready   map[string]bool // clientAddr.String() -> last-seen ready state
	started bool            // guards against triggering match-start more than once
}

// ensurePairRelay returns the A<->B relay for this match, creating and
// starting it on first use. selfAddr is the existing peer receiving the
// 0x29 broadcast (A); otherAddr is the newly-joined peer it just learned
// about (B) - see sessionProxy's rewriteToClient closure in main(), which
// calls this upon detecting that broadcast. Both real addresses are
// already known at this point (see pairRelay's doc comment for why that
// matters), so the relay can be built directly with no per-client
// discovery step.
//
// Not yet torn down and rebuilt on a player dropping - onClientGone
// below exists to do that (docs/multi-peer-routing-design.md's
// "Durability" section) but isn't currently wired to fire from anything
// (see sessionProxy's construction in main() for why: firing it from a
// plain idle timeout broke the second client's join outright).
// ensurePairRelay returns the A<->B relay for this match, creating and
// starting it on first use. selfAddr is whichever real client is the
// confirmed recipient of the genuine broadcast that triggered this call;
// otherAddr is its peer.
//
// Correction (2026-08-20): this used to also thread a peerBroadcastKind
// and start a startNewPeerWatchdog fallback in case otherAddr's own
// mirror-image broadcast never arrived. Removed - the "never arrived"
// premise was wrong. Both isNewPeerBroadcast and isExistingPeerBroadcast
// always caught the genuine broadcast, we just weren't recognizing it
// (see their own doc comments) - the reactive rewrite path below is
// sufficient on its own now that detection is fixed.
func (m *matchState) ensurePairRelay(selfAddr, otherAddr *net.UDPAddr) *pairRelay {
	m.mu.Lock()
	if m.pair != nil {
		relay := m.pair
		m.mu.Unlock()
		// Every call - not just the one that creates the relay - marks
		// its own selfAddr notified: see this method's own doc comment,
		// selfAddr is always the confirmed recipient of whichever real
		// broadcast triggered this specific call.
		relay.markNotified(selfAddr)
		return relay
	}
	relay := newPairRelay("pair", m.cfg, selfAddr, otherAddr)
	// NOT wiring relay.onResign to m.closeSession right now - see
	// isResignBurst's doc comment's 2026-08-11 correction on why acting
	// on this shape is currently disabled in both directions.
	go relay.run()
	m.pair = relay
	m.mu.Unlock()
	relay.markNotified(selfAddr)
	return relay
}

// onClientGone tears down whatever this match knows about addr - the
// scoped-teardown half of durability (docs/multi-peer-routing-design.md's
// "Durability" section), meant to be the single place that reacts to a
// departure regardless of cause.
//
// Correction (2026-08-12): not currently called from anywhere - see
// sessionProxy's construction in main(). A previous wiring (fire this
// from proxy.onSessionRemoved, covering both an eventual real resign
// signal and the idle-timeout reaper) reproduced the exact "Attempting
// to Connect" hang durability was meant to fix: the idle-timeout reaper
// doesn't distinguish "client actually left" from "client's host-facing
// traffic happened to go quiet because it's busy on the P2P leg
// instead," and firing this function on the latter kills a pair relay
// that's still needed - observed live, killing the second client's
// handshake mid-attempt. Kept defined and ready to wire back up once
// there's a real departure signal (a confirmed resign burst, not the
// still-unconfirmed shape isResignBurst currently only logs).
//
// Deliberately scoped to just addr: if addr isn't part of the current
// pair, this only removes its ready-state entry and does nothing to
// m.pair, so the surviving client's own session (tracked entirely
// separately, in sessionProxy.sessions) and its pairing are never
// touched - isolation falls out of this function only ever acting on
// the one address it's given, not from any global reset.
//
// m.started is deliberately NOT reset here: it guards against re-
// triggering the host's Ready-crystal click, not connection lifecycle -
// a reconnect after match-start shouldn't cause a second click into a
// match already in progress.
func (m *matchState) onClientGone(addr *net.UDPAddr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ready, addr.String())
	if m.pair == nil {
		return
	}
	if !udpAddrEqual(addr, m.pair.addrA) && !udpAddrEqual(addr, m.pair.addrB) {
		return
	}
	log.Printf("[match] %s departed, tearing down pair relay", addr)
	m.pair.conn.Close()
	m.pair = nil
}

// full reports whether this match's A<->B pair relay has been created -
// which only happens once both real clients have connected and the host
// has broadcast each one's address to the other (see ensurePairRelay's
// callers in sessionProxy's rewriteToClient). That makes it a reliable
// "host + 2 real clients, no open slots" signal for free: unlike
// hostProbe's discovery-query check (which answers "is a client welcome to
// browse in", true right up until the lobby actually fills), this reflects
// the pairing having actually completed. Matchmaking needs this to decide
// a newly-connecting client should get a new pod rather than being routed
// into a match that's already staffed - see docs/lobby-status-api.md.
func (m *matchState) full() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pair != nil
}

// currentPair returns this match's pair relay, or nil if the two real
// clients haven't been paired yet - used by reapIdleSessions to check a
// client's peer-relay activity before treating host-channel idleness
// alone as a departure (see that function's own doc comment).
func (m *matchState) currentPair() *pairRelay {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pair
}

// hostReadyX/hostReadyY are the host's own Ready-crystal coordinates in
// the 800x600 lobby screen - the same control a human host would click,
// pixel-measured live 2026-08-11 (see docs/host-flow.md). Confirmed to
// be the actual match-start trigger: with both real (or, in the
// confirming test, AI) slots already showing ready, clicking this alone
// started the match immediately - there is no separate "Start Game"
// button.
const (
	hostReadyX = 511
	hostReadyY = 89
)

// setReady records a real client's latest Ready-toggle state (see
// isReadyToggle/readyToggleState) and reports whether both real clients
// are now ready - "both" meaning exactly 2 distinct clients have ever
// reported in, and every one of them currently reads ready, matching
// this project's fixed 1v1 scope (see CLAUDE.md's Goals). A client that
// un-readies after both were ready simply un-latches this - only a
// fresh transition into "both ready" returns true, so a flapping toggle
// doesn't retrigger every time it happens to land on ready again once
// startTriggered has already fired (see startTriggered/markStarted).
func (m *matchState) setReady(clientAddr *net.UDPAddr, ready bool) (bothReady bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ready == nil {
		m.ready = make(map[string]bool)
	}
	m.ready[clientAddr.String()] = ready
	log.Printf("[match] ready state: %v", m.ready)
	if len(m.ready) < 2 {
		return false
	}
	for _, r := range m.ready {
		if !r {
			return false
		}
	}
	return true
}

// markStarted reports whether match-start has already been triggered,
// and marks it triggered if not - the guard that makes triggerMatchStart
// fire at most once per proxy lifetime even if setReady keeps reporting
// "both ready" (e.g. a client toggles ready off and back on again).
func (m *matchState) markStarted() (alreadyStarted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	alreadyStarted = m.started
	m.started = true
	return alreadyStarted
}

// triggerMatchStart asks host's own input-agent (see input-agent/main.go)
// to click that host's Ready crystal, the confirmed match-start trigger.
// Plain HTTP over the pod network, straight to that host's own
// dynamically-discovered podIP:8082 (hostCandidate.inputAgentAddr) - no
// Service involved, same as every other per-host address in this file.
//
// N-host update: used to post to a single fixed cfg.inputAgentAddr: now
// takes the specific host whose match just went both-ready, and posts to
// *that* host's own input-agent instead - with a pool of more than one
// host, the old fixed-address version would have clicked the Ready
// crystal on an arbitrary host, not necessarily the one that actually
// just filled up.
func triggerMatchStart(host *hostCandidate) {
	url := fmt.Sprintf("http://%s/click?x=%d&y=%d", host.inputAgentAddr, hostReadyX, hostReadyY)
	resp, err := http.Post(url, "", nil)
	if err != nil {
		log.Printf("triggerMatchStart: POST %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		log.Printf("triggerMatchStart: POST %s: unexpected status %s", url, resp.Status)
		return
	}
	log.Printf("triggerMatchStart: clicked %s's Ready crystal (%d, %d)", host.id, hostReadyX, hostReadyY)
}

func main() {
	cfg := loadConfig()

	// pool starts empty - podLister's reconciliation loop (started below,
	// once sessionProxy exists to wire podLister.closeSession) fills it
	// in dynamically as aom-headless pods come and go. See hostPool and
	// podLister's own doc comments - this replaces the old static,
	// built-once-at-startup pool entirely.
	pool := &hostPool{}
	podLister := newInClusterPodLister(cfg)

	// Shared between both proxies - see clientTracker's doc comment for why
	// the session proxy needs to know who the discovery proxy last heard
	// from.
	tracker := &clientTracker{}

	discoveryProxy := &proxy{
		name:               "discovery",
		cfg:                cfg,
		clientConn:         mustListen(cfg.discoveryListenAddr),
		pool:               pool,
		backendAddrForHost: func(h *hostCandidate) string { return h.discoveryBackendAddr },
		selectHost:         func(string) (*hostCandidate, bool) { return pool.peekHost() },
		onClientPacket:     tracker.set,
		rewriteToClient: func(payload []byte, cfg config, sess *session) []byte {
			out := discoveryRewrite(payload, cfg)
			if cfg.verbose && len(out) > 0 {
				_, rewritten := discoveryAddressOffsets[out[0]]
				log.Printf("[discovery] reply type 0x%02x (%d bytes) -> client, rewritten=%v (address -> %s:%d)",
					out[0], len(out), rewritten, net.IP(cfg.publicIP[:]), cfg.publicPort)
				log.Printf("[discovery] raw backend payload:  %s", hex.EncodeToString(payload))
				log.Printf("[discovery] sent to client:       %s", hex.EncodeToString(out))
			}
			return out
		},
		rewriteToBackend: func(payload []byte, cfg config, sess *session) []byte {
			out := discoveryRewrite(payload, cfg)
			if cfg.verbose && len(payload) > 0 && payload[0] != 0x25 {
				// Anything other than the well-known 9-byte enumerate-hosts
				// query is unexpected - log it in full. 0x20 (client ping)
				// is now rewritten above; anything else not yet seen would
				// need its own entry in discoveryAddressOffsets if it turns
				// out to embed an address too.
				_, rewritten := discoveryAddressOffsets[payload[0]]
				log.Printf("[discovery] client->backend type 0x%02x (%d bytes), rewritten=%v: %s",
					payload[0], len(payload), rewritten, hex.EncodeToString(out))
			}
			return out
		},
		sessions: make(map[string]*session),
	}

	// sessionProxy's own rewriteToClient closure below needs to look
	// itself up (to find the newly-joined peer's real address via
	// otherSessionClientAddr) - hence declaring the variable before the
	// struct literal that closes over it, rather than the usual := form.
	var sessionProxy *proxy
	sessionProxy = &proxy{
		name:                "session",
		cfg:                 cfg,
		clientConn:          mustListen(cfg.sessionListenAddr),
		pool:                pool,
		backendAddrForHost:  func(h *hostCandidate) string { return h.sessionBackendAddr },
		selectHost:          pool.assignForClient,
		detectBackendOrigin: true,
		tracker:             tracker,
		// History: wiring onSessionRemoved to match.onClientGone broke the
		// second real client's join outright back on 2026-08-12 -
		// reproduced live, the client<->host session for whichever client
		// was mid-handshake could look idle to reapIdleSessions (30s)
		// while that client was actually busy retrying its P2P handshake
		// through the just-created pair relay, not sending much toward
		// the host in that window. That idle reap fired onClientGone,
		// which tore down the pair relay mid-handshake with nothing to
		// rebuild it. Left unwired for a long time afterward as a result.
		//
		// Re-wired 2026-08-14, safely this time: reapIdleSessions itself
		// now checks the pair relay's own per-client activity
		// (pairRelay.idleFor) before ever reaping a session belonging to
		// an already-paired match - see that function's own doc comment.
		// A quiet host channel alone no longer reaps a paired session; both
		// channels have to be genuinely silent first. That's what makes
		// firing onClientGone here safe again: durability (freeing a
		// host back up when a real client leaves, so a replacement second
		// player gets routed there - see hostFull/selectLocked) without
		// reproducing the original false-positive.
		onSessionRemoved: func(sess *session) {
			if sess.host != nil && sess.host.match != nil {
				sess.host.match.onClientGone(sess.clientAddr)
			}
		},
		rewriteToBackend: func(payload []byte, cfg config, sess *session) []byte {
			if isResignBurst(payload) {
				// Logging only, NOT closing the session - see isResignBurst's
				// doc comment's 2026-08-11 correction. Real 2-real-client
				// testing showed this exact shape fires on a precise 2-minute
				// period after every successful pairing (e.g. 12:36:30 ->
				// 12:38:30 -> 12:40:41 -> ..., each gap exactly 2:00), with no
				// user action behind it - not a resign, some other periodic
				// protocol message (keepalive/re-sync?) coincidentally
				// matching the documented resign shape. Acting on it was
				// killing healthy sessions ~2 minutes into every game,
				// reproducing the exact "Attempting to Connect" hang this
				// whole durability pass was meant to fix. Left logged (not
				// removed) since it's useful forensic data for whoever
				// eventually captures a real resign to compare against.
				log.Printf("[session] client %s resign-shaped burst seen (not acted on, see isResignBurst)", sess.clientAddr)
			}
			if isReadyToggle(payload) {
				ready := readyToggleState(payload)
				log.Printf("[session] client %s (%s) ready-toggle: %v", sess.clientAddr, sess.host.id, ready)
				if sess.host.match.setReady(sess.clientAddr, ready) && !sess.host.match.markStarted() {
					go triggerMatchStart(sess.host)
				}
			}
			if !isSessionHandshake(payload) {
				if cfg.verbose {
					log.Printf("[session] client->backend non-handshake (%d bytes): %s", len(payload), hex.EncodeToString(payload))
				}
				return payload
			}
			// Learn this client's own self-reported sin_zero from its
			// genuine self block, before it's rewritten below - see
			// session.selfSinZero's doc comment. Stable for the life of
			// the session (the client generates it once), so an idempotent
			// re-set on every handshake packet is harmless.
			sess.mu.Lock()
			copy(sess.selfSinZero[:], payload[sessionSelfOffset+8:sessionSelfOffset+16])
			sess.selfSinZeroKnown = true
			sess.mu.Unlock()
			var backendIP [4]byte
			copy(backendIP[:], sess.host.sessionBackendUDPAddr.IP.To4())
			out := rewriteSessionField(payload, sessionSelfOffset, cfg.publicIP, cfg.publicPort)
			out = rewriteSessionField(out, sessionPeerOffset, backendIP, uint16(sess.host.sessionBackendUDPAddr.Port))
			if cfg.verbose {
				log.Printf("[session] rewriting client-self to %s:%d, peer to backend %s",
					net.IP(cfg.publicIP[:]), cfg.publicPort, sess.host.sessionBackendUDPAddr)
				log.Printf("[session] client->backend raw: %s", hex.EncodeToString(payload))
				log.Printf("[session] client->backend sent: %s", hex.EncodeToString(out))
			}
			return out
		},
		rewriteToClient: func(payload []byte, cfg config, sess *session) []byte {
			// Skip both broadcast-shape checks below once this client is
			// already confirmed notified (see pairRelay.notified/
			// markNotified) - they're only ever meaningful once, at join
			// time, but without this guard they'd run their length+byte
			// comparisons against every single backend->client packet for
			// the rest of a potentially long match. Still runs the checks
			// whenever relay is nil (pairing hasn't happened yet at all -
			// exactly when they're expected to fire and create it) or this
			// client specifically isn't yet marked notified.
			if pair := sess.host.match.currentPair(); pair == nil || !pair.isNotified(sess.clientAddr) {
				if isNewPeerBroadcast(payload) {
					// Polls rather than a one-shot lookup - see
					// pollOtherSessionClientAddr's doc comment (2026-08-12):
					// this can legitimately arrive before the other client's
					// session is registered here yet, and the host does not
					// reliably retry the broadcast if we just give up on the
					// first miss. Blocks this client's own backendToClient
					// read loop for at most otherSessionPollTimeout, which is
					// fine - this message fires once, not on a hot path.
					other := sessionProxy.pollOtherSessionClientAddr(sess.clientAddr, sess.host, otherSessionPollTimeout, otherSessionPollInterval)
					if other == nil {
						log.Printf("[session] 0x29 new-peer broadcast to client %s (%s): no other real client session on that host after polling %s, forwarding unmodified", sess.clientAddr, sess.host.id, otherSessionPollTimeout)
						return payload
					}
					relay := sess.host.match.ensurePairRelay(sess.clientAddr, other)
					rewritten := payload
					// Live-tested 2026-08-20: the host doesn't always have
					// other's real info filled in yet when it sends this -
					// an "empty variant" 0x29 (address 0.0.0.0:0, sin_zero
					// already zeroed) can still arrive AFTER our own
					// pollOtherSessionClientAddr already finds other's
					// session (this proxy learns about a client the moment
					// its own traffic starts flowing, which can outpace the
					// host's own internal "player added" bookkeeping).
					// rewriteNewPeerBroadcast correctly overwrites the
					// address either way, but sin_zero is left as whatever
					// the host embedded - genuinely other's own real tag in
					// the normal case, but the empty variant's own zeroed
					// placeholder otherwise, silently producing a
					// valid-address/garbage-identity hybrid packet. Always
					// overwrite with other's own proxy-learned real
					// sin_zero rather than trusting whatever's already in
					// the packet, since we have a reliable source for it
					// either way.
					if sinZero, ok := sessionProxy.sessionSinZero(other); ok {
						rewritten = append([]byte(nil), payload...)
						copy(rewritten[newPeerAddrOffset1+8:newPeerAddrOffset1+16], sinZero[:])
						copy(rewritten[newPeerAddrOffset2+8:newPeerAddrOffset2+16], sinZero[:])
					} else {
						log.Printf("[session] 0x29 new-peer broadcast to client %s: other %s's sin_zero not learned yet, forwarding host's own value unpatched", sess.clientAddr, other)
					}
					out := rewriteNewPeerBroadcast(rewritten, cfg.publicIP, relay.selfPort)
					origIP, origPort := sockaddrInAddr(payload, newPeerAddrOffset1)
					log.Printf("[session] rewrote 0x29 new-peer broadcast to client %s: %s:%d -> relay %s:%d",
						sess.clientAddr, origIP, origPort, net.IP(cfg.publicIP[:]), relay.selfPort)
					return out
				}
				if isExistingPeerBroadcast(payload) {
					// Mirror image of the 0x29 branch above: this client (sess,
					// the newly-joined one) is being told an *existing* peer's
					// address - see existingPeerBroadcast's doc comment.
					// ensurePairRelay/otherSessionClientAddr are already
					// symmetric in self/other, so whichever of the two
					// broadcasts arrives first creates the relay and the other
					// just reuses it. Polls for the same race-survival reason
					// as the 0x29 branch above.
					other := sessionProxy.pollOtherSessionClientAddr(sess.clientAddr, sess.host, otherSessionPollTimeout, otherSessionPollInterval)
					if other == nil {
						log.Printf("[session] existing-peer broadcast to client %s (%s): no other real client session on that host after polling %s, forwarding unmodified", sess.clientAddr, sess.host.id, otherSessionPollTimeout)
						return payload
					}
					relay := sess.host.match.ensurePairRelay(sess.clientAddr, other)
					// See the isNewPeerBroadcast branch's own comment above -
					// same "host's embedded sin_zero can't be trusted, always
					// overwrite with our own proxy-learned value" fix,
					// mirrored for this message's own address offsets.
					rewritten := payload
					if sinZero, ok := sessionProxy.sessionSinZero(other); ok {
						rewritten = append([]byte(nil), payload...)
						copy(rewritten[existingPeerAddrOffset1+8:existingPeerAddrOffset1+16], sinZero[:])
						copy(rewritten[existingPeerAddrOffset2+8:existingPeerAddrOffset2+16], sinZero[:])
					} else {
						log.Printf("[session] existing-peer broadcast to client %s: other %s's sin_zero not learned yet, forwarding host's own value unpatched", sess.clientAddr, other)
					}
					out := rewriteExistingPeerBroadcast(rewritten, cfg.publicIP, relay.selfPort)
					origIP, origPort := sockaddrInAddr(payload, existingPeerAddrOffset1)
					log.Printf("[session] rewrote existing-peer broadcast to client %s: %s:%d -> relay %s:%d",
						sess.clientAddr, origIP, origPort, net.IP(cfg.publicIP[:]), relay.selfPort)
					return out
				}
			}
			if !isSessionHandshake(payload) {
				if cfg.verbose {
					log.Printf("[session] backend->client non-handshake (%d bytes): %s", len(payload), hex.EncodeToString(payload))
				}
				return payload
			}
			var clientIP [4]byte
			copy(clientIP[:], sess.clientAddr.IP.To4())
			out := rewriteSessionField(payload, sessionSelfOffset, cfg.publicIP, cfg.publicPort)
			out = rewriteSessionField(out, sessionPeerOffset, clientIP, uint16(sess.clientAddr.Port))
			if cfg.verbose {
				log.Printf("[session] rewriting host-self to %s:%d, peer to client %s",
					net.IP(cfg.publicIP[:]), cfg.publicPort, sess.clientAddr)
				log.Printf("[session] backend->client raw: %s", hex.EncodeToString(payload))
				log.Printf("[session] backend->client sent: %s", hex.EncodeToString(out))
			}
			return out
		},
		sessions: make(map[string]*session),
	}
	// podLister.closeSession and pool.sessionCountForHost can only be
	// wired up now that sessionProxy exists (same chicken-and-egg reason
	// match.closeSession used to be set after the fact) - every host
	// reconcileOnce discovers from here on gets closeSession, and
	// selectForNewClient's tier classification (empty vs. waiting) starts
	// working correctly. Then start the discovery loop itself - pool goes
	// from empty to however many pods match AOM_HEADLESS_LABEL_SELECTOR
	// within one podPollInterval.
	podLister.closeSession = sessionProxy.closeSession
	pool.sessionCountForHost = sessionProxy.sessionCountForHost
	// See onDiscoveryPingNoHost's own doc comment on *proxy - lets
	// discoveryProxy kick off sessionProxy's proactive synthesized
	// host-open as soon as it answers a client's 0x20 ping, rather than
	// sessionProxy only ever reacting to a client that speaks first.
	discoveryProxy.onDiscoveryPingNoHost = sessionProxy.beginSimulatedHostOpen
	go podLister.run(pool)
	log.Printf("aom-lobby: discovering aom-headless pods (namespace=%s, selector=%s, poll every %s), listening on discovery %s / session %s, public address %s:%d",
		podLister.namespace, podLister.labelSelector, podPollInterval,
		cfg.discoveryListenAddr, cfg.sessionListenAddr,
		net.IP(cfg.publicIP[:]), cfg.publicPort)

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/hosts", func(w http.ResponseWriter, r *http.Request) {
		total := 0
		for _, h := range pool.snapshot() {
			total += h.probe.count()
		}
		fmt.Fprintf(w, "%d\n", total)
	})
	statusMux.HandleFunc("/waiting", func(w http.ResponseWriter, r *http.Request) {
		waiting := false
		for _, h := range pool.snapshot() {
			if h.probe.hasWaitingGame() {
				waiting = true
				break
			}
		}
		fmt.Fprintf(w, "%t\n", waiting)
	})
	statusMux.HandleFunc("/full", func(w http.ResponseWriter, r *http.Request) {
		// N-host update: used to mean "does the one match have both real
		// clients"; now means "is every host in the pool full" - the
		// meaningful pool-wide signal (no capacity anywhere) now that
		// there can be more than one match. A single-host pool (today's
		// only actual deployment) means these are the same question. An
		// empty pool (no pods discovered yet/at all) reports true - "no
		// capacity anywhere" is accurate either way, even though the
		// cause differs from every host being staffed.
		full := true
		for _, h := range pool.snapshot() {
			if !pool.hostFull(h) {
				full = false
				break
			}
		}
		fmt.Fprintf(w, "%t\n", full)
	})
	go func() {
		log.Printf("aom-lobby: status endpoint on %s (GET /hosts -> number of hosts waiting for players, GET /waiting -> is there a game with a player waiting, GET /full -> is every host in the pool full)", cfg.statusListenAddr)
		if err := http.ListenAndServe(cfg.statusListenAddr, statusMux); err != nil {
			log.Fatalf("status server on %q: %v", cfg.statusListenAddr, err)
		}
	}()

	go discoveryProxy.reapIdleSessions()
	go sessionProxy.reapIdleSessions()
	go discoveryProxy.run()
	sessionProxy.run()
}
