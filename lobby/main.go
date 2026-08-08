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

// clientTracker remembers the most recently seen real client address,
// shared between the discovery and session proxies. It exists because the
// backend pod, once it learns a client's real address (from the
// discovery-port ping - see discoveryAddressOffsets' 0x20 entry, now
// rewritten so this shouldn't happen anymore, but the tracker is the
// fallback plumbing either way), sends UDP 2300 session data directly to
// whatever address it believes the client is at rather than back through
// this proxy's session listener. The session proxy uses this to know who
// to relay that unsolicited traffic to. One client at a time, matching the
// single-backend/no-multi-match architecture this whole pass targets - see
// CLAUDE.md's Architecture intention for the eventual per-match design.
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

// anySessionClientAddr returns the real client address of any one existing
// session - fine given today's single-client-at-a-time scope (see
// clientTracker's doc comment).
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

// relayBackendInitiated handles a packet the backend sent unprompted to
// this proxy's listening socket (rather than as a reply on a per-client
// dialed connection) - see clientTracker's doc comment for why the backend
// ends up doing this at all. No per-client session/dial bookkeeping is
// needed here: there's only ever one real client at a time in today's
// architecture, and this direction has no way to demultiplex multiple
// clients anyway, since the backend always sends to this same well-known
// listening address regardless of which client it means.
func (p *proxy) relayBackendInitiated(payload []byte) {
	// Prefer this proxy's own already-established session (has the real
	// client's address *on this proxy's own port* - e.g. the session
	// proxy's client might use a different local port for 2300 than for
	// discovery on 2299). Only fall back to the cross-proxy tracker for
	// the bootstrap case where no session exists yet on this proxy at all.
	clientAddr := p.anySessionClientAddr()
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

	sessionProxy := &proxy{
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
	go func() {
		log.Printf("aom-lobby: status endpoint on %s (GET /hosts -> number of hosts waiting for players)", cfg.statusListenAddr)
		if err := http.ListenAndServe(cfg.statusListenAddr, statusMux); err != nil {
			log.Fatalf("status server on %q: %v", cfg.statusListenAddr, err)
		}
	}()

	go discoveryProxy.reapIdleSessions()
	go sessionProxy.reapIdleSessions()
	go discoveryProxy.run()
	sessionProxy.run()
}
