package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/billing"
)

const stripeHooksPath = "/v1/stripe/webhook"

// hooksPaths are the only routes the webhook host serves. Both verify a
// signature from their sender before reading anything else.
var hooksPaths = map[string]bool{
	stripeHooksPath:                  true,
	billing.GitHubSecretScanningPath: true,
}

// hooksOnly answers every request addressed to host ("*" for any) with a bare 404 unless it
// is a POST to one of hooksPaths, and, when stripe is not empty, a Stripe
// delivery from one of those addresses. The balancer in front already routes
// only those paths; this holds if it ever stops doing so.
func hooksOnly(host string, stripe []netip.Prefix, address func(*http.Request) string, next http.Handler) http.Handler {
	if host == "" {
		return next
	}
	host = strings.ToLower(host)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host != "*" && requestHost(r) != host {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || !hooksPaths[r.URL.Path] || r.URL.RawQuery != "" ||
			(r.URL.Path == stripeHooksPath && len(stripe) > 0 && !within(address(r), stripe)) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hooksHealthPath is what the balancer in front of the hooks listener checks.
const hooksHealthPath = "/healthz"

// hooksEndpoint is the listener Google's balancer delivers webhooks to. A
// port of its own because the balancer negotiates only classical key
// exchange, which the public listener refuses: this one accepts it and in
// exchange serves nothing but the two signed webhook routes and a health
// check. Off unless an address is given; with billing off the webhook routes
// answer 404.
func hooksEndpoint(address string, billingRoutes http.Handler, stripe []netip.Prefix, certificate *tls.Certificate) *http.Server {
	if address == "" {
		return nil
	}
	webhooks := billingRoutes
	if webhooks == nil {
		webhooks = http.NotFoundHandler()
	}
	gated := hooksOnly("*", stripe, billing.ClientAddress, webhooks)
	routes := http.NewServeMux()
	routes.HandleFunc("GET "+hooksHealthPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	routes.Handle("/", gated)
	endpoint := &http.Server{
		Addr:              address,
		Handler:           routes,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	if certificate != nil {
		endpoint.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{*certificate},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return endpoint
}

func within(address string, networks []netip.Prefix) bool {
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	parsed = parsed.Unmap()
	for _, network := range networks {
		if network.Contains(parsed) {
			return true
		}
	}
	return false
}

// parseNetworks reads a comma-separated list of addresses or prefixes.
func parseNetworks(list string) ([]netip.Prefix, error) {
	var networks []netip.Prefix
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			address, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", entry, err)
			}
			networks = append(networks, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		network, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", entry, err)
		}
		networks = append(networks, network.Masked())
	}
	return networks, nil
}

func requestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
