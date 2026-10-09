package main

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/moqom-cloud/moqom/go/internal/pqtls"
)

const (
	hsts = "max-age=63072000; includeSubDomains; preload"

	// Billing reads at most 1 MiB; this only stops the edge relaying more.
	largestBody = 2 << 20

	// Signup makes several Stripe calls in a row, each allowed thirty seconds,
	// and the control plane's public listener allows it ninety.
	upstreamHeaderTimeout = 95 * time.Second
)

// serverTLS is TLS 1.3 limited to pqtls.PublicCurves, with certificates from
// getCertificate.
//
// The one exception is a ClientHello offering only acme-tls/1, which is a
// certificate authority validating a TLS-ALPN-01 challenge: its key exchange
// is the CA's choice, the only certificate it can be shown is the challenge's
// own, and the connection is closed after the handshake (newHTTPSServer).
// HTTP-01 on port 80 would still issue without it, so this only spares the
// issuance from depending on which groups Let's Encrypt's validators offer.
func serverTLS(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	challenge := &tls.Config{
		GetCertificate: getCertificate,
		NextProtos:     []string{acme.ALPNProto},
		MinVersion:     tls.VersionTLS12,
	}
	config := pqtls.Public(&tls.Config{
		GetCertificate: getCertificate,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
	})
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
			return challenge, nil
		}
		return nil, nil
	}
	return config
}

// upstreamTransport reaches the control plane's public listener, whose
// certificate is from the deployment's private link authority and names
// serverName rather than the address dialled.
func upstreamTransport(roots *x509.CertPool, serverName string) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: pqtls.Link(&tls.Config{
			RootCAs:    roots,
			ServerName: serverName,
		}),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: upstreamHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   32,
	}
}

func newProxy(upstream *url.URL, transport http.RoundTripper, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// The host the client asked for, as the Google balancer sent it.
			r.Out.Host = r.In.Host
			// Rewrite has already dropped whatever forwarding headers the
			// client sent, so X-Forwarded-For is exactly the TCP peer: the
			// client, since the load balancer passes connections through.
			r.SetXForwarded()
		},
		Transport: transport,
		// Every write is flushed: the console's event streams and the long
		// polls depend on it, and nothing here benefits from batching.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "upstream failed", "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}` + "\n"))
		},
	}
}

type edge struct {
	hosts    map[string]bool
	redirect string
	proxy    http.Handler
}

func newEdge(hosts []string, redirect string, proxy http.Handler) *edge {
	e := &edge{hosts: map[string]bool{}, redirect: redirect, proxy: proxy}
	for _, host := range hosts {
		e.hosts[host] = true
	}
	return e
}

// ServeHTTP routes as the Google balancer's URL map did: /v1/* to the control
// plane, anything else 302 to the website.
func (e *edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Strict-Transport-Security", hsts)
	if !e.hosts[canonicalHost(r.Host)] {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.Redirect(w, r, e.redirect, http.StatusFound)
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, largestBody)
	}
	e.proxy.ServeHTTP(w, r)
}

// redirectToHTTPS is port 80 after ACME's HTTP-01 has had its turn.
func redirectToHTTPS(hosts []string) http.Handler {
	known := map[string]bool{}
	for _, host := range hosts {
		known[host] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := canonicalHost(r.Host)
		if !known[host] {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// logged writes one line per request. tls_group is the key exchange the
// connection negotiated: the count of X25519MLKEM768 is the count of clients
// not yet on ML-KEM-1024. The query string is left out; it can carry a
// confirmation or a token.
func logged(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorded := &recorder{ResponseWriter: w}
		next.ServeHTTP(recorded, r)
		group, version := "", ""
		if r.TLS != nil {
			group, version = r.TLS.CurveID.String(), tls.VersionName(r.TLS.Version)
		}
		if recorded.status == 0 {
			recorded.status = http.StatusOK
		}
		log.InfoContext(r.Context(), "request",
			"host", canonicalHost(r.Host), "method", r.Method, "path", r.URL.Path,
			"status", recorded.status, "bytes", recorded.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"proto", r.Proto, "tls_version", version, "tls_group", group)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && code >= 200 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and the deadlines.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
