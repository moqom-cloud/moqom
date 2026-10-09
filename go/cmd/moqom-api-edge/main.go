// Command moqom-api-edge terminates TLS for api.moqom.cloud and forwards /v1/
// to the control plane's public listener.
//
// Google's Application Load Balancer, which fronted the API before, negotiates
// only classical key exchange, and GCP SSL policies have no post-quantum
// option. So this holds the publicly trusted certificate itself and accepts
// only pqtls.PublicCurves: ML-KEM-1024, alone or hybrid with P-384, and
// X25519MLKEM768 because nothing stronger is offered by a browser. A client
// offering only classical groups is refused at the handshake. Every request is
// logged with the group it negotiated, to show who still arrives on
// X25519MLKEM768.
//
// Certificates come from Let's Encrypt over ACME: TLS-ALPN-01 on the HTTPS
// port and HTTP-01 on the HTTP one. They are cached in a private GCS bucket,
// so every replica shares one account and one certificate and a restart does
// not issue again.
//
// It runs behind a passthrough network load balancer with
// externalTrafficPolicy=Local, so the TCP peer is the client. That address
// replaces any X-Forwarded-For the client sent, which billing believes from
// the pod range (MOQOM_BILLING_TRUSTED_PROXIES).
//
// Everything outside /v1/ is redirected to the website, as the Google balancer
// did. Responses stream as they arrive, so the console's event streams work.
//
// Configuration, all from the environment:
//
//	MOQOM_API_EDGE_HOSTS                comma-separated hostnames served (api.moqom.cloud)
//	MOQOM_API_EDGE_UPSTREAM             the control plane's public listener, https://host:port (required)
//	MOQOM_API_EDGE_UPSTREAM_CA          PEM file of the authority its certificate chains to (required)
//	MOQOM_API_EDGE_UPSTREAM_SERVER_NAME the name its certificate is checked against (moqom-control)
//	MOQOM_API_EDGE_REDIRECT             where any path outside /v1/ is sent with a 302 (https://moqom.cloud/)
//	MOQOM_API_EDGE_ACME_DIRECTORY       ACME directory (Let's Encrypt production); staging is
//	                                    https://acme-staging-v02.api.letsencrypt.org/directory
//	MOQOM_API_EDGE_ACME_EMAIL           contact address for the ACME account, optional
//	MOQOM_API_EDGE_ACME_BUCKET          GCS bucket for the certificate cache, reached with the
//	                                    pod's Workload Identity token
//	MOQOM_API_EDGE_ACME_CACHE_DIR       a local directory instead of the bucket, for a laptop
//	MOQOM_API_EDGE_HTTPS_ADDR           (:8443) TLS; the load balancer's 443
//	MOQOM_API_EDGE_HTTP_ADDR            (:8000) HTTP-01 and the redirect to HTTPS; the load balancer's 80
//	MOQOM_API_EDGE_HEALTH_ADDR          (:8080) plain HTTP /livez and /healthz for the kubelet
//	MOQOM_API_EDGE_DRAIN                how long /healthz fails before the listeners close (10s)
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// version is stamped by the image build.
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("moqom-api-edge stopped", "error", err)
		os.Exit(1)
	}
}

type config struct {
	hosts              []string
	upstream           *url.URL
	upstreamRoots      *x509.CertPool
	upstreamServerName string
	redirect           string
	acmeDirectory      string
	acmeEmail          string
	cache              autocert.Cache
	httpsAddr          string
	httpAddr           string
	healthAddr         string
	drain              time.Duration
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func load() (config, error) {
	c := config{
		upstreamServerName: env("MOQOM_API_EDGE_UPSTREAM_SERVER_NAME", "moqom-control"),
		redirect:           env("MOQOM_API_EDGE_REDIRECT", "https://moqom.cloud/"),
		acmeDirectory:      env("MOQOM_API_EDGE_ACME_DIRECTORY", autocert.DefaultACMEDirectory),
		acmeEmail:          env("MOQOM_API_EDGE_ACME_EMAIL", ""),
		httpsAddr:          env("MOQOM_API_EDGE_HTTPS_ADDR", ":8443"),
		httpAddr:           env("MOQOM_API_EDGE_HTTP_ADDR", ":8000"),
		healthAddr:         env("MOQOM_API_EDGE_HEALTH_ADDR", ":8080"),
	}
	c.hosts = parseHosts(env("MOQOM_API_EDGE_HOSTS", "api.moqom.cloud"))
	if len(c.hosts) == 0 {
		return c, errors.New("MOQOM_API_EDGE_HOSTS names no host")
	}

	raw := env("MOQOM_API_EDGE_UPSTREAM", "")
	upstream, err := url.Parse(raw)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		return c, fmt.Errorf("MOQOM_API_EDGE_UPSTREAM must be https://host:port, got %q", raw)
	}
	c.upstream = upstream

	caFile := env("MOQOM_API_EDGE_UPSTREAM_CA", "")
	if caFile == "" {
		return c, errors.New("MOQOM_API_EDGE_UPSTREAM_CA is required: the upstream's certificate is private")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return c, fmt.Errorf("MOQOM_API_EDGE_UPSTREAM_CA: %w", err)
	}
	c.upstreamRoots = x509.NewCertPool()
	if !c.upstreamRoots.AppendCertsFromPEM(pem) {
		return c, fmt.Errorf("MOQOM_API_EDGE_UPSTREAM_CA %s holds no certificate", caFile)
	}

	drain, err := time.ParseDuration(env("MOQOM_API_EDGE_DRAIN", "10s"))
	if err != nil || drain < 0 {
		return c, fmt.Errorf("MOQOM_API_EDGE_DRAIN must be a duration")
	}
	c.drain = drain

	directory, err := url.Parse(c.acmeDirectory)
	if err != nil || directory.Scheme != "https" || directory.Host == "" {
		return c, fmt.Errorf("MOQOM_API_EDGE_ACME_DIRECTORY must be an https URL, got %q", c.acmeDirectory)
	}
	switch bucket, dir := env("MOQOM_API_EDGE_ACME_BUCKET", ""), env("MOQOM_API_EDGE_ACME_CACHE_DIR", ""); {
	case bucket != "":
		c.cache = newGCSCache(bucket, directory.Host)
	case dir != "":
		c.cache = autocert.DirCache(dir)
	default:
		return c, errors.New("MOQOM_API_EDGE_ACME_BUCKET is required (or MOQOM_API_EDGE_ACME_CACHE_DIR on a laptop): " +
			"without a shared cache every replica and every restart asks Let's Encrypt again, and its rate limits are weekly")
	}
	return c, nil
}

func parseHosts(raw string) []string {
	var hosts []string
	for _, host := range strings.Split(raw, ",") {
		if host = canonicalHost(host); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func run() error {
	c, err := load()
	if err != nil {
		return err
	}
	log := slog.Default()

	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(c.hosts...),
		Cache:      c.cache,
		Email:      c.acmeEmail,
		Client:     &acme.Client{DirectoryURL: c.acmeDirectory},
	}

	edge := newEdge(c.hosts, c.redirect, newProxy(c.upstream, upstreamTransport(c.upstreamRoots, c.upstreamServerName), log))
	secure := newHTTPSServer(serverTLS(manager.GetCertificate), logged(log, edge))
	plain := &http.Server{
		// HTTP-01 first; anything else is sent to HTTPS.
		Handler:           logged(log, manager.HTTPHandler(redirectToHTTPS(c.hosts))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	var ready atomic.Bool
	probes := &http.Server{
		Addr:              c.healthAddr,
		Handler:           healthHandler(&ready),
		ReadHeaderTimeout: 5 * time.Second,
	}

	secureListener, err := net.Listen("tcp", c.httpsAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", c.httpsAddr, err)
	}
	plainListener, err := net.Listen("tcp", c.httpAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", c.httpAddr, err)
	}

	stopped := make(chan error, 3)
	go func() { stopped <- secure.ServeTLS(secureListener, "", "") }()
	go func() { stopped <- plain.Serve(plainListener) }()
	go func() { stopped <- probes.ListenAndServe() }()
	ready.Store(true)
	log.Info("api edge listening", "version", version,
		"https", secureListener.Addr().String(), "http", plainListener.Addr().String(), "health", c.healthAddr,
		"hosts", c.hosts, "upstream", c.upstream.String(), "upstream_server_name", c.upstreamServerName,
		"acme_directory", c.acmeDirectory)

	interrupted, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-stopped:
		return err
	case <-interrupted.Done():
	}

	// Fail readiness first and keep serving while the load balancer's health
	// check notices, so connections already on their way still land.
	log.Info("draining", "for", c.drain.String())
	ready.Store(false)
	time.Sleep(c.drain)
	shutdown, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()
	// Event streams never finish on their own; Shutdown gives up on them at the
	// deadline and Close cuts them, and the console reconnects.
	_ = plain.Shutdown(shutdown)
	if err := secure.Shutdown(shutdown); err != nil {
		_ = secure.Close()
	}
	_ = probes.Shutdown(shutdown)
	return nil
}

// newHTTPSServer serves handler over TLS with HTTP/2, closing ACME challenge
// connections once their handshake is done.
func newHTTPSServer(tlsConfig *tls.Config, handler http.Handler) *http.Server {
	server := &http.Server{
		Handler:   handler,
		TLSConfig: tlsConfig,
		// A TLS-ALPN-01 connection exists for its handshake, which carries the
		// proof. Nothing is served on it: see challengeConfig.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){
			acme.ALPNProto: func(_ *http.Server, conn *tls.Conn, _ http.Handler) { _ = conn.Close() },
		},
		// No WriteTimeout: the event streams under /v1/ are open for as long as
		// the console is. The upstream transport bounds everything else.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// Refused handshakes are mostly scanners; at debug so they don't bury
		// the request log.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	// TLSNextProto being set would otherwise turn HTTP/2 off.
	server.Protocols = new(http.Protocols)
	server.Protocols.SetHTTP1(true)
	server.Protocols.SetHTTP2(true)
	return server
}

func healthHandler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
