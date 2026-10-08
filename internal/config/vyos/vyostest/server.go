// Package vyostest is a stand-in for the REST API of a VyOS router.
//
// It behaves the way a real router was observed to behave (see
// docs/spike-vyos-container.md), including the parts that are easy to get
// wrong: a change that is not confirmed in time is reverted, a wrong key is a
// 401, and command output carries sudo noise in front of the payload.
package vyostest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

// ServerName is the DNS name in the fake router's certificate.
const ServerName = "router.test"

// sudoNoise precedes the output of every command once the host name changed.
const sudoNoise = "sudo: unable to resolve host vyos: System error\n"

// Router is a fake VyOS router.
type Router struct {
	// URL of the API.
	URL string
	// Key the API accepts.
	Key string
	// CertPEM is the certificate the API presents, KeyPEM its key.
	CertPEM []byte
	KeyPEM  []byte

	mu sync.Mutex
	// running is the active configuration, saved what a reboot would load.
	running, saved []command.Path
	// pending is the configuration to go back to if a commit-confirm is
	// not confirmed.
	pending     []command.Path
	hasPending  bool
	previous    []command.Path
	failNext    map[string]int
	version     string
	vrrp        string
	rejectPaths []command.Path
	requests    []string
	configures  int
}

// New starts a fake router with the given running configuration. It is shut
// down when the test ends.
func New(t *testing.T, config string) *Router {
	t.Helper()
	paths, err := command.Parse(config)
	if err != nil {
		t.Fatalf("vyostest: %v", err)
	}
	cert, certPEM, keyPEM := selfSigned(t)

	r := &Router{
		Key:     "test-key",
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		running: paths,
		saved:   slices.Clone(paths),
		version: "2026.10.07-0712-rolling",
		vrrp:    "VRRP data is not available (process not running or no active groups)\n",
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.URL = srv.URL
	return r
}

// Running returns the active configuration as "set" lines, sorted.
func (r *Router) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedLines(r.running)
}

// Saved returns the configuration a reboot would load, sorted.
func (r *Router) Saved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedLines(r.saved)
}

// ConfirmPending reports whether a commit-confirm is waiting.
func (r *Router) ConfirmPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hasPending
}

// ExpireConfirm plays the revert timer running out: an unconfirmed change is
// rolled back to the commit before it.
func (r *Router) ExpireConfirm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hasPending {
		r.running = r.pending
		r.pending, r.hasPending = nil, false
	}
}

// Apply changes the running configuration behind the operator's back.
func (r *Router) Apply(t *testing.T, config string) {
	t.Helper()
	paths, err := command.Parse(config)
	if err != nil {
		t.Fatalf("vyostest: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = paths
}

// RejectCommitsOf makes any commit that sets something below path fail, the
// way a component fails to apply on a real router: the rest of the commit
// goes through.
func (r *Router) RejectCommitsOf(path ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectPaths = append(r.rejectPaths, command.Path(path))
}

// AllowCommits undoes every RejectCommitsOf.
func (r *Router) AllowCommits() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectPaths = nil
}

// FailNext makes the next n requests to endpoint ("/config-file", ...) fail
// with HTTP 500 without doing anything, like a router that lost the plot or a
// connection that broke.
func (r *Router) FailNext(endpoint string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext == nil {
		r.failNext = map[string]int{}
	}
	r.failNext[endpoint] = n
}

// Port returns the TCP port the API listens on.
func (r *Router) Port() int32 {
	parsed, err := url.Parse(r.URL)
	if err != nil {
		return 0
	}
	port, _ := strconv.ParseInt(parsed.Port(), 10, 32)
	return int32(port)
}

// SetVRRP sets the output of "show vrrp".
func (r *Router) SetVRRP(output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vrrp = output
}

// Requests returns "METHOD path op" of every request received.
func (r *Router) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// Configures returns how many /configure requests changed the router.
func (r *Router) Configures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configures
}

type request struct {
	Key         string            `json:"key"`
	Op          string            `json:"op"`
	Path        []string          `json:"path"`
	ConfirmTime int               `json:"confirm_time"`
	Commands    []json.RawMessage `json:"commands"`
}

type operation struct {
	Op   string   `json:"op"`
	Path []string `json:"path"`
}

func (r *Router) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.URL.Path == "/info" {
		r.requests = append(r.requests, "GET /info")
		reply(w, http.StatusOK, map[string]string{"banner": "Welcome to VyOS", "hostname": "vyos", "version": r.version}, "")
		return
	}

	var body request
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, nil, "invalid JSON")
		return
	}
	r.requests = append(r.requests, strings.TrimSpace(req.Method+" "+req.URL.Path+" "+body.Op))
	if body.Key != r.Key {
		reply(w, http.StatusUnauthorized, nil, "Valid API key is required")
		return
	}

	if r.failNext[req.URL.Path] > 0 {
		r.failNext[req.URL.Path]--
		reply(w, http.StatusInternalServerError, nil, "internal error")
		return
	}

	switch req.URL.Path {
	case "/show":
		r.show(w, &body)
	case "/configure":
		r.configure(w, &body)
	case "/config-file":
		r.configFile(w, &body)
	default:
		reply(w, http.StatusNotFound, nil, "not found")
	}
}

func (r *Router) show(w http.ResponseWriter, body *request) {
	switch strings.Join(body.Path, " ") {
	case "configuration commands":
		// A real router prints values single-quoted.
		var out strings.Builder
		for _, path := range r.running {
			out.WriteString(path.String())
			out.WriteString("\n")
		}
		reply(w, http.StatusOK, out.String(), "")
	case "vrrp":
		reply(w, http.StatusOK, sudoNoise+r.vrrp, "")
	default:
		reply(w, http.StatusBadRequest, nil, "unknown show command")
	}
}

func (r *Router) configure(w http.ResponseWriter, body *request) {
	next := slices.Clone(r.running)
	var failed command.Path
	for _, raw := range body.Commands {
		var op operation
		if err := json.Unmarshal(raw, &op); err != nil {
			reply(w, http.StatusBadRequest, nil, "invalid command")
			return
		}
		path := command.Path(op.Path)
		switch op.Op {
		case "set":
			if rejected := r.rejected(path); rejected != nil {
				failed = rejected
				continue
			}
			if !slices.ContainsFunc(next, func(p command.Path) bool { return slices.Equal(p, path) }) {
				next = append(next, path)
			}
		case "delete":
			next = slices.DeleteFunc(next, func(p command.Path) bool { return p.HasPrefix(path) })
		default:
			reply(w, http.StatusBadRequest, nil, "unknown op "+op.Op)
			return
		}
	}

	r.configures++
	r.previous, r.running = r.running, next
	if failed != nil {
		// As observed on a real router: the components that applied stay
		// applied, and no revert timer is armed even if one was asked for.
		reply(w, http.StatusBadRequest, nil,
			fmt.Sprintf("[ %s ]\n%s[[%s]] failed\nCommit failed\n", strings.Join(failed, " "), sudoNoise, strings.Join(failed, " ")))
		return
	}

	message := sudoNoise
	if body.ConfirmTime > 0 {
		if !r.hasPending {
			r.pending, r.hasPending = r.previous, true
		}
		message += fmt.Sprintf("\nInitialized commit-confirm; %d minutes to confirm before reload\n", body.ConfirmTime)
	}
	reply(w, http.StatusOK, message, "")
}

func (r *Router) rejected(path command.Path) command.Path {
	for _, rejected := range r.rejectPaths {
		if path.HasPrefix(rejected) {
			return rejected
		}
	}
	return nil
}

func (r *Router) configFile(w http.ResponseWriter, body *request) {
	switch body.Op {
	case "confirm":
		if !r.hasPending {
			reply(w, http.StatusOK, "No confirm pending\n", "")
			return
		}
		r.pending, r.hasPending = nil, false
		reply(w, http.StatusOK, "Reload timer stopped\n", "")
	case "save":
		r.saved = slices.Clone(r.running)
		reply(w, http.StatusOK, "", "")
	default:
		reply(w, http.StatusBadRequest, nil, "unknown op "+body.Op)
	}
}

func reply(w http.ResponseWriter, status int, data any, errorMessage string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	response := map[string]any{"success": errorMessage == "", "data": data, "error": nil}
	if errorMessage != "" {
		response["error"] = errorMessage
	}
	_ = json.NewEncoder(w).Encode(response)
}

func sortedLines(paths []command.Path) []string {
	lines := command.Lines(paths)
	slices.Sort(lines)
	return lines
}

func selfSigned(t *testing.T) (cert tls.Certificate, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: ServerName},
		DNSNames:              []string{ServerName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, certPEM, keyPEM
}
