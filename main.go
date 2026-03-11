// SPDX-License-Identifier: AGPL-3.0-or-later
// Command openstack-mock runs the miscellaneous OpenStack mock services
// implemented under kops/cloudmock/openstack, prints their base endpoints, and
// exposes a single dispatcher endpoint that forwards requests to the
// appropriate mock service based on URI prefixes.
//
// This is intended for local development and testing. Each mock service spins up
// its own in-memory HTTP server (using net/http/httptest) listening on a random
// localhost port. This program wires up the default set of mock services and
// keeps running until interrupted.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/go-http-utils/headers"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/testutils"
)

const IdentityPath = "/v3/identity"
const VersionDiscoveryPath = "/"

func main() {
	// Reduce klog noise unless overridden
	if os.Getenv("KLOG_V") == "" {
		_ = os.Setenv("KLOG_V", "1")
	}

	port := flag.Int("port", 19090, "Port for the dispatcher to listen on")
	listen := flag.String("listen", "127.0.0.1", "Address/interface for the dispatcher to bind to")
	flag.Parse()

	klog.Infof("Starting OpenStack mock services...")

	cloud := testutils.SetupMockOpenstack()

	// For interactive use, clear any pre-seeded images so listing returns an empty set.
	if cloud.MockImageClient != nil {
		cloud.MockImageClient.Reset()
	}

	computeBase := cloud.ComputeClient().Endpoint
	networkingBase := cloud.NetworkingClient().Endpoint
	lbBase := cloud.LoadBalancerClient().Endpoint
	blockBase := cloud.BlockStorageClient().Endpoint
	dnsBase := cloud.DNSClient().Endpoint
	imageBase := cloud.ImageClient().Endpoint

	// Print service endpoints for convenience
	fmt.Println("OpenStack mock service endpoints (set your clients to these base URLs):")
	fmt.Printf("  compute      (nova):        %s\n", computeBase)
	fmt.Printf("  networking   (neutron):     %s\n", networkingBase)
	fmt.Printf("  loadbalancer (octavia):     %s\n", lbBase)
	fmt.Printf("  blockstorage (cinder):      %s\n", blockBase)
	fmt.Printf("  dns          (designate):   %s\n", dnsBase)
	fmt.Printf("  image        (glance):      %s\n", imageBase)

	dispatcher := NewDispatcher(Endpoints{
		Compute:      computeBase,
		Networking:   networkingBase,
		LoadBalancer: lbBase,
		BlockStorage: blockBase,
		DNS:          dnsBase,
		Image:        imageBase,
	})

	addr := fmt.Sprintf("%s:%d", *listen, *port)
	server := &http.Server{Addr: addr, Handler: dispatcher}

	go func() {
		klog.Infof("Dispatcher listening on http://%s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("dispatcher failed: %v", err)
		}
	}()

	fmt.Println("Press Ctrl-C to stop.")

	// Wait for termination signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	klog.Infof("Shutting down OpenStack mock services...")
}

// Endpoints defines base URLs for each mock service backend.
type Endpoints struct {
	Compute      string
	Networking   string
	LoadBalancer string
	BlockStorage string
	DNS          string
	Image        string
}

// NewDispatcher constructs the HTTP handler that serves token/identity endpoints
// and proxies requests to the provided backend endpoints based on path prefixes.
func NewDispatcher(e Endpoints) http.Handler {
	// Build reverse proxies for each backend
	mkProxy := func(base string) *httputil.ReverseProxy {
		u, err := url.Parse(base)
		if err != nil {
			log.Fatalf("invalid backend URL %q: %v", base, err)
		}
		rp := httputil.NewSingleHostReverseProxy(u)
		// Preserve the original Host header so handlers that rely on it still work if needed.
		rp.Director = func(req *http.Request) {
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			// Keep the original path and rawpath; backend muxes expect the same path prefixes
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}
			req.Host = u.Host
		}
		return rp
	}

	computeProxy := mkProxy(e.Compute)
	networkingProxy := mkProxy(e.Networking)
	lbProxy := mkProxy(e.LoadBalancer)
	blockProxy := mkProxy(e.BlockStorage)
	dnsProxy := mkProxy(e.DNS)
	imageProxy := mkProxy(e.Image)

	// Routing table: URI prefix -> proxy
	routes := map[string]*httputil.ReverseProxy{
		// Compute (Nova)
		"/servers/":             computeProxy,
		"/servers":              computeProxy,
		"/os-keypairs/":         computeProxy,
		"/os-keypairs":          computeProxy,
		"/flavors/":             computeProxy,
		"/flavors":              computeProxy,
		"/os-instance-actions/": computeProxy,
		// Image (Glance)
		"/v2/images/": imageProxy,
		"/v2/images":  imageProxy,
		"/images/":    imageProxy,
		"/images":     imageProxy,
		// BlockStorage (Cinder)
		"/volumes/":             blockProxy,
		"/volumes":              blockProxy,
		"/types/":               blockProxy,
		"/types":                blockProxy,
		"/os-availability-zone": blockProxy,
		// DNS (Designate)
		"/zones/": dnsProxy,
		"/zones":  dnsProxy,
		// Networking (Neutron)
		"/v2.0/networks/":        networkingProxy,
		"/v2.0/networks":         networkingProxy,
		"/networks/":             networkingProxy,
		"/networks":              networkingProxy,
		"/ports/":                networkingProxy,
		"/ports":                 networkingProxy,
		"/routers/":              networkingProxy,
		"/routers":               networkingProxy,
		"/security-groups/":      networkingProxy,
		"/security-groups":       networkingProxy,
		"/security-group-rules/": networkingProxy,
		"/security-group-rules":  networkingProxy,
		"/subnets/":              networkingProxy,
		"/subnets":               networkingProxy,
		"/v2.0/floatingips/":     networkingProxy,
		"/v2.0/floatingips":      networkingProxy,
		"/floatingips/":          networkingProxy,
		"/floatingips":           networkingProxy,
		// LoadBalancer (Octavia)
		"/lbaas/listeners/":     lbProxy,
		"/lbaas/listeners":      lbProxy,
		"/lbaas/loadbalancers/": lbProxy,
		"/lbaas/loadbalancers":  lbProxy,
		"/lbaas/pools/":         lbProxy,
		"/lbaas/pools":          lbProxy,
	}

	// Prepare ordered list of prefixes for deterministic matching
	prefixes := make([]string, 0, len(routes))
	for p := range routes {
		prefixes = append(prefixes, p)
	}
	// Sort by length descending to match the most specific path first
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })

	// Minimal Keystone v3 token issuance handler
	tokenHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set(headers.ContentType, "application/json")
		// Generate a token and set X-Subject-Token header as Keystone does.
		tok := uuid.New().String()
		w.Header().Set("X-Subject-Token", tok)
		// Build a minimal token document with a service catalog
		region := "RegionOne"
		makeEndpoint := func(urlStr string) map[string]interface{} {
			return map[string]interface{}{
				"id":        uuid.New().String(),
				"interface": "public",
				"region":    region,
				"region_id": region,
				"url":       urlStr,
			}
		}
		// Determine the external base URL of the dispatcher (scheme and host)
		base := fmt.Sprintf("%s://%s", func() string {
			if r.Header.Get("X-Forwarded-Proto") != "" {
				return r.Header.Get("X-Forwarded-Proto")
			}
			if r.URL.Scheme != "" {
				return r.URL.Scheme
			}
			if r.TLS != nil {
				return "https"
			}
			return "http"
		}(), r.Host)
		catalog := []map[string]interface{}{
			{"id": uuid.New().String(), "type": "compute", "name": "nova", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "network", "name": "neutron", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "load-balancer", "name": "octavia", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "block-storage", "name": "cinder", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "dns", "name": "designate", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "image", "name": "glance", "endpoints": []map[string]interface{}{makeEndpoint(base)}},
			{"id": uuid.New().String(), "type": "identity", "name": "keystone", "endpoints": []map[string]interface{}{makeEndpoint(base + IdentityPath)}},
		}
		resp := map[string]interface{}{
			"token": map[string]interface{}{
				"expires_at": time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339),
				"project":    map[string]string{"id": "mock-project-id", "name": "mock"},
				"user":       map[string]string{"id": "mock-user-id", "name": "mock-user"},
				"catalog":    catalog,
			},
		}
		b, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(b)
	}

	// Minimal Identity discovery endpoint under /v3/identity
	identityHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set(headers.ContentType, "application/json")
		base := fmt.Sprintf("%s://%s", func() string {
			if r.Header.Get(headers.XForwardedProto) != "" {
				return r.Header.Get(headers.XForwardedProto)
			}
			if r.URL.Scheme != "" {
				return r.URL.Scheme
			}
			if r.TLS != nil {
				return "https"
			}
			return "http"
		}(), r.Host)
		// Construct a lightweight, but plausible identity discovery document
		resp := map[string]interface{}{
			"identity": map[string]interface{}{
				"version": "v3",
				"status":  "ok",
				"updated": time.Now().UTC().Format(time.RFC3339),
				"links": []map[string]string{
					{"rel": "self", "href": base + IdentityPath},
				},
			},
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}

	// Minimal version discovery endpoint under /
	versionHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set(headers.ContentType, "application/json")
		// Construct a minimal version discovery document containing
		// some expected OpenStack service version numbers
		resp := map[string]interface{}{
			"Versions": []map[string]interface{}{
				{
					"ID":         "v2.1",
					"Status":     "SUPPORTED",
					"Version":    "",
					"MaxVersion": "",
					"MinVersion": "",
				},
				{
					"ID":         "v3.1",
					"Status":     "SUPPORTED",
					"Version":    "",
					"MaxVersion": "",
					"MinVersion": "",
				},
			},
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}

	// Some routes which are not handled by any proxy code in kops, yet,
	// but are needed for the Accounting health check.
	okRoutes := []string{
		"/v2.0/address-scopes",
		"/v2/zones",
		"/v3/projects",
	}

	// This handler just returns status ok and an empty response:
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set(headers.ContentType, "application/json")
		// return an empty response
		resp := map[string]interface{}{}

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/v3/auth/tokens" {
			tokenHandler(w, r)
			return
		}
		if path == IdentityPath || strings.HasPrefix(path, "/v3/identity/") {
			identityHandler(w, r)
			return
		}
		if path == VersionDiscoveryPath {
			versionHandler(w, r)
			return
		}

		for _, okRoute := range okRoutes {
			if strings.HasPrefix(path, okRoute) {
				okHandler(w, r)
				return
			}
		}

		for _, p := range prefixes {
			if strings.HasPrefix(path, p) {
				routes[p].ServeHTTP(w, r)
				return
			}
		}

		// Default: 404 with some guidance
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no route for path: " + path + "\n"))
	})
}
