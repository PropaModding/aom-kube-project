// input-agent is a small HTTP server that runs inside the aom-headless
// container itself, alongside Xvfb/wine (see entrypoint.sh), exposing a
// single POST /click endpoint that shells out to xdotool against the
// container's own DISPLAY.
//
// It exists because aom-lobby's UI-automation needs split into two
// different shapes: the startup sequence (EULA through Observer Mode,
// see host-game-kube.sh) runs once, top-to-bottom, driven from outside
// the cluster - kubectl exec works fine there. But driving the host's
// own response to a live event aom-lobby detects mid-session (e.g.
// isReadyToggle in lobby/main.go seeing both real clients ready) needs
// aom-lobby, a long-running process with no shell/kubectl access of its
// own, to trigger a click at the moment it sees that packet. Rather than
// give aom-lobby kubectl-exec/k8s-API access (a real dependency jump for
// a UDP proxy), this exposes the one primitive it needs as a plain HTTP
// call over the normal pod network - see the aom-headless-game Service's
// input-agent port in k8s/aom-headless-deployment.yaml.
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
