// host-health-agent runs as a Kubernetes DaemonSet (one pod per node,
// hostNetwork), watching every aom-headless pod scheduled to its own
// node and answering "is it actually hosting" over a small HTTP status
// endpoint - see docs/host-health-probe-design.md for the full design
// and the packet-level evidence behind the check itself.
//
// The check is the same 0x25/0x26 discovery-port exchange aom-lobby's
// own hostProbe (lobby/main.go) already runs continuously - confirmed
// byte-identical to a real client's own query against two of this
// project's historical captures (see the design doc). It's centralized
// here rather than baked into every aom-headless pod (e.g. as another
// input-agent endpoint) because it has nothing to do with driving that
// pod's own game process - unlike input-agent's POST /click, which must
// run inside the exact pod whose xdotool/DISPLAY it drives, this check
// only needs UDP reach to a pod's discovery port, which is just as cheap
// from one shared per-node process as from N copies of the same logic
// baked into every game-pod image.
//
// Each aom-headless pod's own Kubernetes livenessProbe calls back into
// this agent (see k8s/aom-headless-deployment.yaml's healthcheck.sh) to
// decide whether to restart itself - this program never touches a pod
// directly, only reports what it's observed.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	// Same cadence/timeout as hostProbe in lobby/main.go - this is the
	// identical check, just re-homed, so there's no reason for the
	// numbers to differ.
	pollInterval  = 3 * time.Second
	probeTimeout  = 1 * time.Second
	discoveryPort = 2299
)

// discoveryQuery is byte-for-byte the same 9-byte 0x25 enumerate query
// lobby/main.go's hostProbe sends, which is itself byte-identical to a
// real client's own captured query - see docs/host-health-probe-design.md
// for both hex dumps side by side. Not an independent guess.
var discoveryQuery = []byte{0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// podInfo is the subset of a Pod's API representation this agent needs -
// same shape as lobby/main.go's own podInfo, kept as a separate type
// rather than shared since these are two independent small deployables
// (see this file's own top comment).
type podInfo struct {
	name string
	ip   string
}

// podLister talks to the in-cluster Kubernetes API the same way
// lobby/main.go's podLister does (plain net/http + ServiceAccount bearer
// token, no client-go dependency) - the only difference is the
// fieldSelector, which additionally restricts results to pods scheduled
// to this agent's own node. That's the entire point of running this as a
// DaemonSet rather than a single cluster-wide Deployment: each node's
// agent only ever tracks and probes the pods actually running alongside
// it.
type podLister struct {
	apiServer     string
	namespace     string
	labelSelector string
	nodeName      string
	token         string
	httpClient    *http.Client
}

func newInClusterPodLister() *podLister {
	namespace := getenv("AOM_HEADLESS_NAMESPACE", "default")
	labelSelector := getenv("AOM_HEADLESS_LABEL_SELECTOR", "app=aom-headless")
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatalf("NODE_NAME not set - must be wired from the downward API (fieldRef: spec.nodeName), see k8s/host-health-agent-daemonset.yaml")
	}

	host := getenv("KUBERNETES_SERVICE_HOST", "")
	port := getenv("KUBERNETES_SERVICE_PORT", "")
	if host == "" || port == "" {
		log.Fatalf("KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT not set - host-health-agent must run as an in-cluster pod")
	}
	tokenBytes, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		log.Fatalf("reading ServiceAccount token: %v (is k8s/host-health-agent-daemonset.yaml's RBAC applied?)", err)
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
		nodeName:      nodeName,
		token:         string(tokenBytes),
		httpClient: &http.Client{
			Timeout: 3 * probeTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: caPool},
			},
		},
	}
}

// list returns every Running, node-local aom-headless pod that has an
// assigned IP - same "don't consider something not actually up yet"
// posture as lobby/main.go's own podLister.list().
func (pl *podLister) list() ([]podInfo, error) {
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/pods?labelSelector=%s&fieldSelector=%s",
		pl.apiServer, pl.namespace,
		url.QueryEscape(pl.labelSelector),
		url.QueryEscape(fmt.Sprintf("spec.nodeName=%s", pl.nodeName)))
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

// probeOnce sends discoveryQuery to podIP:2299 and reports whether it
// got back a genuine 0x26 reply - identical logic to hostProbe.probeOnce
// in lobby/main.go, just operating on a plain IP instead of a
// hostCandidate.
func probeOnce(podIP string) bool {
	conn, err := net.DialTimeout("udp", fmt.Sprintf("%s:%d", podIP, discoveryPort), probeTimeout)
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

// registry holds the last probe result for every node-local pod this
// agent currently knows about. A pod present here has been seen by at
// least one list() call; its value is the outcome of the most recent
// probe against it. A pod not present here at all is "unknown" - either
// this agent hasn't run its first reconcile cycle since the pod
// appeared, or the pod no longer exists - deliberately distinct from
// "known and failing" (see status handler below and the design doc's
// "Status endpoint" section for why that distinction matters: a pod's
// own first few seconds of life must never read as an explicit health
// failure).
type registry struct {
	mu    sync.Mutex
	state map[string]bool // pod name -> last probe result
}

func newRegistry() *registry {
	return &registry{state: make(map[string]bool)}
}

func (r *registry) set(name string, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state[name] = open
}

// prune drops any pod not in `present` - keeps state from growing
// forever across normal pod churn (scale down, pod replacement) and
// makes sure a deleted pod's stale "healthy" result can't outlive it.
func (r *registry) prune(present map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.state {
		if !present[name] {
			delete(r.state, name)
		}
	}
}

// lookup reports (found, open) - found is false for a pod this agent has
// never observed.
func (r *registry) lookup(name string) (found, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	open, found = r.state[name]
	return found, open
}

// reconcileOnce lists this node's aom-headless pods and probes each one
// in turn - sequential, not concurrent: pod counts on a single node stay
// small in this project's own scope (see CLAUDE.md's Architecture
// intention), and probeTimeout (1s) bounds the worst case per pod, so a
// full cycle staying well under pollInterval (3s) even with several pods
// isn't a real concern here. Revisit only if that stops being true.
func reconcileOnce(pl *podLister, reg *registry) {
	pods, err := pl.list()
	if err != nil {
		log.Printf("[host-health-agent] listing pods (namespace=%s, selector=%s, node=%s): %v",
			pl.namespace, pl.labelSelector, pl.nodeName, err)
		return
	}

	present := make(map[string]bool, len(pods))
	for _, pod := range pods {
		present[pod.name] = true
		open := probeOnce(pod.ip)
		reg.set(pod.name, open)
	}
	reg.prune(present)
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8090")
	pl := newInClusterPodLister()
	reg := newRegistry()

	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			reconcileOnce(pl, reg)
			<-ticker.C
		}
	}()

	http.HandleFunc("/host-health", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing ?id=<pod name>", http.StatusBadRequest)
			return
		}
		found, open := reg.lookup(id)
		switch {
		case !found:
			http.Error(w, "unknown", http.StatusNotFound)
		case open:
			fmt.Fprintln(w, "ok")
		default:
			http.Error(w, "not hosting", http.StatusServiceUnavailable)
		}
	})

	log.Printf("host-health-agent: node=%s namespace=%s selector=%s, listening on %s (GET /host-health?id=<pod name>)",
		pl.nodeName, pl.namespace, pl.labelSelector, listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
