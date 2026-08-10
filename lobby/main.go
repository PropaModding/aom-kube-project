// aom-lobby is a UDP reverse proxy that sits in front of the real AoM
// game pod's DirectPlay8 ports. It relays traffic to/from the real host
// untouched, except for packets that embed an address the sender believes
// is its own (see docs/directplay8-protocol.md): it rewrites those to
// point at itself, so a client outside the cluster is always told to talk
// back to the proxy rather than the pod's internal address — on the
// discovery port (2299) as well as the session port (2300), whose
// handshake packets leak the same kind of address if left alone.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	sockaddrLen = 16
	idleTimeout = 30 * time.Second
	reapEvery   = 10 * time.Second
	bufSize     = 2048

	probeInterval = 3 * time.Second
	probeTimeout  = 1 * time.Second
)

// discoveryAddressOffsets gives the byte offset(s) within a UDP 2299
// packet, keyed by the message-type byte at offset 0, of embedded
// sockaddr_in blocks that carry the real host address and need rewriting
// before the packet reaches a client. Every other message type (0x25
// query, 0x20 client ping) is forwarded byte-for-byte unmodified.
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
	newPeerWrapperType1  = 0x00
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
func isNewPeerBroadcast(payload []byte) bool {
	if len(payload) < newPeerBroadcastMinLen {
		return false
	}
	return payload[0] == newPeerWrapperType0 && payload[1] == newPeerWrapperType1 &&
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
const (
	existingPeerWrapperType0 = 0x03
	existingPeerWrapperType1 = 0x00

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
	if payload[0] != existingPeerWrapperType0 || payload[1] != existingPeerWrapperType1 {
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
// One backend today (the static aom-headless-game address), matching the
// rest of this pass - see CLAUDE.md's Architecture intention for the
// eventual per-match multi-backend design, at which point this becomes a
// slice of probes summed together rather than a single bool.
type hostProbe struct {
	backendAddr string

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
	discoveryListenAddr  string
	discoveryBackendAddr string
	sessionListenAddr    string
	sessionBackendAddr   string
	statusListenAddr     string
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
	cfg.discoveryBackendAddr = getenv("AOM_BACKEND_ADDR", "127.0.0.1:2299")
	cfg.sessionListenAddr = getenv("SESSION_LISTEN_ADDR", ":2300")
	cfg.sessionBackendAddr = getenv("SESSION_BACKEND_ADDR", "127.0.0.1:2300")
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

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type session struct {
	backendConn *net.UDPConn
	clientAddr  *net.UDPAddr // the real client this session relays for

	mu       sync.Mutex
	lastSeen time.Time
}

// proxy is a generic per-client UDP session relay: one shared socket
// facing clients, one dialed socket per client facing the backend so
// backend replies naturally correlate back to the right client.
// rewriteToBackend/rewriteToClient are optional hooks applied to a
// packet's payload just before it's forwarded in that direction; either
// may be nil to pass packets through untouched.
type proxy struct {
	name        string // for logging, e.g. "discovery" or "session"
	cfg         config
	clientConn  *net.UDPConn
	backendAddr string

	rewriteToBackend func(payload []byte, cfg config, sess *session) []byte
	rewriteToClient  func(payload []byte, cfg config, sess *session) []byte

	// onClientPacket, if set, is called with every packet's real sender
	// address before normal forwarding - used to feed clientTracker.
	onClientPacket func(clientAddr *net.UDPAddr)

	// backendUDPAddr and tracker, if both set, let run() recognize
	// unsolicited packets arriving from the backend itself (not from any
	// client) on this proxy's listening socket, and relay them to the
	// last-known real client instead of mistaking the backend for a new
	// client - see relayBackendInitiated and the clientTracker doc comment.
	backendUDPAddr *net.UDPAddr
	tracker        *clientTracker

	mu       sync.Mutex
	sessions map[string]*session
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

		if p.backendUDPAddr != nil && srcAddr.IP.Equal(p.backendUDPAddr.IP) && srcAddr.Port == p.backendUDPAddr.Port {
			p.relayBackendInitiated(payload)
			continue
		}

		if p.onClientPacket != nil {
			p.onClientPacket(srcAddr)
		}
		p.forwardToBackend(srcAddr, payload)
	}
}

// sessionClientAddrForIP returns the real client address (on this proxy's
// own port) of an existing session belonging to ip, or nil if none does.
// Used by relayBackendInitiated to turn clientTracker's IP-only-trustworthy
// address into the right session when more than one might exist.
func (p *proxy) sessionClientAddrForIP(ip net.IP) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.clientAddr != nil && sess.clientAddr.IP.Equal(ip) {
			return sess.clientAddr
		}
	}
	return nil
}

// anySessionClientAddr returns the real client address of any one existing
// session - only used by relayBackendInitiated as a last-resort guess when
// clientTracker doesn't point at a session of its own (see that method).
func (p *proxy) anySessionClientAddr() *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.clientAddr != nil {
			return sess.clientAddr
		}
	}
	return nil
}

// otherSessionClientAddr returns the real address of the one session in
// p.sessions besides skip - used by the 0x29 new-peer-broadcast rewrite to
// find the newly-joined peer's real address from sessionProxy's own
// session map, since the broadcast's own embedded address is never
// usable directly (see newPeerBroadcast's doc comment). Safe to assume at
// most one "other" session exists: this project only ever hosts 1v1s
// (host + exactly two real clients, see CLAUDE.md's Goals), so besides
// skip there's never more than one candidate.
func (p *proxy) otherSessionClientAddr(skip *net.UDPAddr) *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sess := range p.sessions {
		if sess.clientAddr == nil {
			continue
		}
		if sess.clientAddr.IP.Equal(skip.IP) && sess.clientAddr.Port == skip.Port {
			continue
		}
		return sess.clientAddr
	}
	return nil
}

// relayBackendInitiated handles a packet the backend sent unprompted to
// this proxy's listening socket (rather than as a reply on a per-client
// dialed connection) - see clientTracker's doc comment for why the backend
// ends up doing this at all, and why the backend can't address such a
// packet at a specific client itself: it always sends to this same
// well-known listening address regardless of which client it means, since
// every client looks identical to it (same rewritten proxy address).
//
// Picking the right client to relay to is therefore inferred, in order:
//  1. Whichever real client most recently sent discovery-port traffic
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
//  2. If no session matches that IP yet (the client hasn't sent its own
//     first packet on this port), any one existing session - correct by
//     construction when there's only one, a reasonable guess otherwise.
//  3. If there are no sessions at all, the tracker's raw address - wrong
//     port, so effectively a no-op send, but harmless and better than
//     dropping the packet outright (matches pre-fix behavior for the
//     single-client bootstrap case).
func (p *proxy) relayBackendInitiated(payload []byte) {
	var clientAddr *net.UDPAddr
	if recent := p.tracker.get(); recent != nil {
		clientAddr = p.sessionClientAddrForIP(recent.IP)
	}
	if clientAddr == nil {
		clientAddr = p.anySessionClientAddr()
	}
	if clientAddr == nil {
		clientAddr = p.tracker.get()
	}
	if clientAddr == nil {
		log.Printf("[%s] backend sent unsolicited data but no client seen yet, dropping", p.name)
		return
	}
	out := payload
	if p.rewriteToClient != nil {
		// No real *session exists for this direction (see the doc comment
		// above) - just enough of one to carry the real client's address,
		// which rewriteToClient needs for the session handshake's "peer"
		// field.
		out = p.rewriteToClient(payload, p.cfg, &session{clientAddr: clientAddr})
	}
	if _, err := p.clientConn.WriteToUDP(out, clientAddr); err != nil {
		log.Printf("[%s] relay backend-initiated packet to client %s: %v", p.name, clientAddr, err)
	}
}

func (p *proxy) forwardToBackend(clientAddr *net.UDPAddr, payload []byte) {
	key := clientAddr.String()

	p.mu.Lock()
	sess, ok := p.sessions[key]
	if !ok {
		backendUDPAddr, err := net.ResolveUDPAddr("udp", p.backendAddr)
		if err != nil {
			p.mu.Unlock()
			log.Printf("[%s] resolving backend %s: %v", p.name, p.backendAddr, err)
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
			p.mu.Unlock()
			log.Printf("[%s] dialing backend %s for client %s: %v", p.name, p.backendAddr, key, err)
			return
		}
		sess = &session{backendConn: backendConn, clientAddr: clientAddr, lastSeen: time.Now()}
		p.sessions[key] = sess
		p.mu.Unlock()
		log.Printf("[%s] new session: client %s -> backend %s", p.name, key, p.backendAddr)
		go p.backendToClient(key, clientAddr, sess)
	} else {
		p.mu.Unlock()
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

func (p *proxy) backendToClient(key string, clientAddr *net.UDPAddr, sess *session) {
	buf := make([]byte, bufSize)
	for {
		n, err := sess.backendConn.Read(buf)
		if err != nil {
			log.Printf("[%s] read from backend for client %s: %v", p.name, key, err)
			break
		}

		sess.mu.Lock()
		sess.lastSeen = time.Now()
		sess.mu.Unlock()

		payload := buf[:n]
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
}

func (p *proxy) reapIdleSessions() {
	ticker := time.NewTicker(reapEvery)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		p.mu.Lock()
		for key, sess := range p.sessions {
			sess.mu.Lock()
			idle := now.Sub(sess.lastSeen)
			sess.mu.Unlock()
			if idle > idleTimeout {
				sess.backendConn.Close()
				delete(p.sessions, key)
				log.Printf("[%s] closed idle session for client %s", p.name, key)
			}
		}
		p.mu.Unlock()
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
}

// newPairRelay binds a fresh, dynamically-allocated port (never the
// fixed shared session port - reusing that would reproduce the exact bug
// this relay exists to fix, see newPeerBroadcast's doc comment) and
// starts relaying between addrA and addrB.
func newPairRelay(name string, cfg config, addrA, addrB *net.UDPAddr) *pairRelay {
	conn := mustListen(":0")
	r := &pairRelay{
		name:     name,
		cfg:      cfg,
		conn:     conn,
		selfPort: uint16(conn.LocalAddr().(*net.UDPAddr).Port),
		addrA:    addrA,
		addrB:    addrB,
	}
	log.Printf("[%s] new A<->B pair relay on %s:%d, bridging %s <-> %s", name, net.IP(cfg.publicIP[:]), r.selfPort, addrA, addrB)
	return r
}

func udpAddrEqual(a, b *net.UDPAddr) bool {
	return a.IP.Equal(b.IP) && a.Port == b.Port
}

// run reads every packet arriving on this relay's shared socket and
// forwards it to whichever of addrA/addrB ISN'T the sender - rewriting
// the session handshake's self/peer fields the same way sessionProxy
// does (self -> this relay's own address, so the receiver replies
// through the relay; peer -> the receiver's own real address, matching
// what its own validation expects, per isSessionHandshake's doc comment)
// - every other message type passes through unmodified.
func (r *pairRelay) run() {
	buf := make([]byte, bufSize)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[%s] read: %v", r.name, err)
			continue
		}
		payload := append([]byte(nil), buf[:n]...)

		var dst *net.UDPAddr
		switch {
		case udpAddrEqual(src, r.addrA):
			dst = r.addrB
		case udpAddrEqual(src, r.addrB):
			dst = r.addrA
		default:
			log.Printf("[%s] packet from unexpected sender %s (expected %s or %s), dropping", r.name, src, r.addrA, r.addrB)
			continue
		}

		out := payload
		if isSessionHandshake(payload) {
			var dstIP [4]byte
			copy(dstIP[:], dst.IP.To4())
			out = rewriteSessionField(payload, sessionSelfOffset, r.cfg.publicIP, r.selfPort)
			out = rewriteSessionField(out, sessionPeerOffset, dstIP, uint16(dst.Port))
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

	mu   sync.Mutex
	pair *pairRelay
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
// Not yet handled (see docs/multi-peer-routing-design.md's "Durability"
// section): tearing this down when a player drops and rebuilding it for
// whoever joins the vacated slot - today it's created once per proxy
// lifetime and never replaced.
func (m *matchState) ensurePairRelay(selfAddr, otherAddr *net.UDPAddr) *pairRelay {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pair != nil {
		return m.pair
	}
	relay := newPairRelay("pair", m.cfg, selfAddr, otherAddr)
	go relay.run()
	m.pair = relay
	return relay
}

func main() {
	cfg := loadConfig()

	// Shared between both proxies - see clientTracker's doc comment for why
	// the session proxy needs to know who the discovery proxy last heard
	// from.
	tracker := &clientTracker{}

	discoveryProxy := &proxy{
		name:           "discovery",
		cfg:            cfg,
		clientConn:     mustListen(cfg.discoveryListenAddr),
		backendAddr:    cfg.discoveryBackendAddr,
		onClientPacket: tracker.set,
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

	sessionBackendUDPAddr, err := net.ResolveUDPAddr("udp", cfg.sessionBackendAddr)
	if err != nil {
		log.Fatalf("resolving SESSION_BACKEND_ADDR %q: %v", cfg.sessionBackendAddr, err)
	}

	// The A<->B pair relay (see matchState/newPairRelay above) is created
	// lazily from inside sessionProxy's own rewriteToClient closure below,
	// which needs to look itself up (to find the newly-joined peer's real
	// address via otherSessionClientAddr) - hence declaring the variable
	// before the struct literal that closes over it, rather than the usual
	// := form.
	match := &matchState{cfg: cfg}
	var sessionProxy *proxy
	sessionProxy = &proxy{
		name:           "session",
		cfg:            cfg,
		clientConn:     mustListen(cfg.sessionListenAddr),
		backendAddr:    cfg.sessionBackendAddr,
		backendUDPAddr: sessionBackendUDPAddr,
		tracker:        tracker,
		rewriteToBackend: func(payload []byte, cfg config, sess *session) []byte {
			if !isSessionHandshake(payload) {
				if cfg.verbose {
					log.Printf("[session] client->backend non-handshake (%d bytes): %s", len(payload), hex.EncodeToString(payload))
				}
				return payload
			}
			var backendIP [4]byte
			copy(backendIP[:], sessionBackendUDPAddr.IP.To4())
			out := rewriteSessionField(payload, sessionSelfOffset, cfg.publicIP, cfg.publicPort)
			out = rewriteSessionField(out, sessionPeerOffset, backendIP, uint16(sessionBackendUDPAddr.Port))
			if cfg.verbose {
				log.Printf("[session] rewriting client-self to %s:%d, peer to backend %s",
					net.IP(cfg.publicIP[:]), cfg.publicPort, sessionBackendUDPAddr)
				log.Printf("[session] client->backend raw: %s", hex.EncodeToString(payload))
				log.Printf("[session] client->backend sent: %s", hex.EncodeToString(out))
			}
			return out
		},
		rewriteToClient: func(payload []byte, cfg config, sess *session) []byte {
			if isNewPeerBroadcast(payload) {
				other := sessionProxy.otherSessionClientAddr(sess.clientAddr)
				if other == nil {
					log.Printf("[session] 0x29 new-peer broadcast to client %s but no other real client session found yet, forwarding unmodified", sess.clientAddr)
					return payload
				}
				relay := match.ensurePairRelay(sess.clientAddr, other)
				out := rewriteNewPeerBroadcast(payload, cfg.publicIP, relay.selfPort)
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
				// just reuses it.
				other := sessionProxy.otherSessionClientAddr(sess.clientAddr)
				if other == nil {
					log.Printf("[session] existing-peer broadcast to client %s but no other real client session found yet, forwarding unmodified", sess.clientAddr)
					return payload
				}
				relay := match.ensurePairRelay(sess.clientAddr, other)
				out := rewriteExistingPeerBroadcast(payload, cfg.publicIP, relay.selfPort)
				origIP, origPort := sockaddrInAddr(payload, existingPeerAddrOffset1)
				log.Printf("[session] rewrote existing-peer broadcast to client %s: %s:%d -> relay %s:%d",
					sess.clientAddr, origIP, origPort, net.IP(cfg.publicIP[:]), relay.selfPort)
				return out
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

	log.Printf("aom-lobby: discovery %s -> %s, session %s -> %s, public address %s:%d",
		cfg.discoveryListenAddr, cfg.discoveryBackendAddr,
		cfg.sessionListenAddr, cfg.sessionBackendAddr,
		net.IP(cfg.publicIP[:]), cfg.publicPort)

	probe := &hostProbe{backendAddr: cfg.discoveryBackendAddr}
	go probe.run()

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/hosts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%d\n", probe.count())
	})
	statusMux.HandleFunc("/waiting", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%t\n", probe.hasWaitingGame())
	})
	go func() {
		log.Printf("aom-lobby: status endpoint on %s (GET /hosts -> number of hosts waiting for players, GET /waiting -> is there a game with a player waiting)", cfg.statusListenAddr)
		if err := http.ListenAndServe(cfg.statusListenAddr, statusMux); err != nil {
			log.Fatalf("status server on %q: %v", cfg.statusListenAddr, err)
		}
	}()

	go discoveryProxy.reapIdleSessions()
	go sessionProxy.reapIdleSessions()
	go discoveryProxy.run()
	sessionProxy.run()
}
