// input-agent is a small HTTP server that runs inside the aom-headless
// container itself, alongside Xvfb/wine (see entrypoint.sh), exposing a
// single POST /click endpoint that shells out to xdotool against the
// container's own DISPLAY.
//
// It exists because aom-lobby's UI-automation needs split into two
// different shapes: the startup sequence (EULA through Observer Mode) now
// runs once, top-to-bottom, entirely inside the pod itself via
// auto-host.sh (backgrounded by entrypoint.sh right alongside this
// program - see that script and CLAUDE.md's Key files list; originally
// driven externally via `kubectl exec host-game-kube.sh` before the
// 2026-08-13 N-host pass moved it in-container so a scaled-up pod needs
// no external trigger to self-host). But driving the host's own response
// to a live event aom-lobby detects mid-session (e.g. isReadyToggle in
// lobby/main.go seeing both real clients ready) needs aom-lobby, a
// long-running process, to trigger a click at the moment it sees that
// packet - and unlike a one-shot startup script, that can't be baked into
// the pod's own startup ahead of time. Rather than give aom-lobby
// kubectl-exec access to drive it directly (aom-lobby does have
// *read-only* Kubernetes API access as of 2026-08-13, to list
// aom-headless pods for its own host-discovery - see
// k8s/lobby-rbac.yaml - but that's list/watch on Pods, nothing that can
// exec into one), this exposes the one primitive aom-lobby needs as a
// plain HTTP call straight to this pod's own dynamically-discovered
// podIP:8082 (hostCandidate.inputAgentAddr in lobby/main.go) - no Service
// involved, same as every other per-host address aom-lobby uses.
//
// No auth, cluster-internal only - same posture as aom-lobby's own
// /hosts, /waiting, /full status endpoints (see lobby-status-api.md).
package main

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handleClick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	x, err := strconv.Atoi(r.URL.Query().Get("x"))
	if err != nil {
		http.Error(w, "invalid or missing x", http.StatusBadRequest)
		return
	}
	y, err := strconv.Atoi(r.URL.Query().Get("y"))
	if err != nil {
		http.Error(w, "invalid or missing y", http.StatusBadRequest)
		return
	}

	cmd := exec.Command("xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y), "click", "1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("click (%d, %d) failed: %v: %s", x, y, err, out)
		http.Error(w, "xdotool failed", http.StatusInternalServerError)
		return
	}
	log.Printf("click (%d, %d)", x, y)
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	addr := getenv("LISTEN_ADDR", ":8082")
	http.HandleFunc("/click", handleClick)
	log.Printf("input-agent: listening on %s (POST /click?x=&y=)", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
