package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/moqom-cloud/moqom/go/internal/pqtls"
)

const apiHost = "api.moqom.cloud"

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &authority{cert: cert, key: key, pool: pool}
}

func (a *authority) leaf(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// logs collects the JSON lines the edge writes, from any goroutine.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// request waits for the request line for path, since it is written after the
// response has gone.
func (l *logs) request(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		scanner := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
		l.mu.Unlock()
		for scanner.Scan() {
			var line map[string]any
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line["msg"] == "request" && line["path"] == path {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no request log for %s", path)
	return nil
}

type running struct {
	addr  string
	roots *x509.CertPool
	logs  *logs
}

// startEdge serves handler the way run does, with a fixed certificate in
// place of ACME.
func startEdge(t *testing.T, proxy http.Handler) running {
	t.Helper()
	ca := newAuthority(t)
	cert := ca.leaf(t, apiHost)
	collected := &logs{}
	log := slog.New(slog.NewJSONHandler(collected, nil))
	server := newHTTPSServer(
		serverTLS(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }),
		logged(log, newEdge([]string{apiHost}, "https://moqom.cloud/", proxy)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	return running{addr: listener.Addr().String(), roots: ca.pool, logs: collected}
}

func (r running) client(config *tls.Config) *http.Client {
	config.RootCAs = r.roots
	config.ServerName = apiHost
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   config,
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, r.addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

var answer = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

func TestKeyExchange(t *testing.T) {
	edge := startEdge(t, answer)
	cases := []struct {
		name   string
		config *tls.Config
		group  string // empty means refused
	}{
		{"classical only", &tls.Config{CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}}, ""},
		{"TLS 1.2", &tls.Config{MaxVersion: tls.VersionTLS12}, ""},
		{"X25519MLKEM768, which browsers offer", &tls.Config{CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519}}, "X25519MLKEM768"},
		{"ML-KEM-1024", &tls.Config{CurvePreferences: []tls.CurveID{tls.MLKEM1024}}, "MLKEM1024"},
		{"hybrid ML-KEM-1024", &tls.Config{CurvePreferences: pqtls.LinkCurves}, "SecP384r1MLKEM1024"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := "/v1/case/" + string(rune('a'+i))
			response, err := edge.client(c.config).Get("https://" + apiHost + path)
			if c.group == "" {
				if err == nil {
					response.Body.Close()
					t.Fatal("handshake succeeded; want it refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("handshake refused: %v", err)
			}
			defer response.Body.Close()
			if response.TLS.CurveID.String() != c.group {
				t.Fatalf("negotiated %s, want %s", response.TLS.CurveID, c.group)
			}
			if response.Proto != "HTTP/2.0" {
				t.Fatalf("protocol %s, want HTTP/2.0", response.Proto)
			}
			line := edge.logs.request(t, path)
			if line["tls_group"] != c.group || line["tls_version"] != "TLS 1.3" || line["host"] != apiHost ||
				line["status"] != float64(200) || line["bytes"] != float64(2) {
				t.Fatalf("log line %v", line)
			}
		})
	}
}

// A classical ClientHello offering only acme-tls/1 is a CA validating a
// TLS-ALPN-01 challenge. It completes a handshake and is then closed with
// nothing served.
func TestChallengeConnectionServesNothing(t *testing.T) {
	edge := startEdge(t, answer)
	conn, err := tls.Dial("tcp", edge.addr, &tls.Config{
		RootCAs: edge.roots, ServerName: apiHost,
		NextProtos:       []string{acme.ALPNProto},
		CurvePreferences: []tls.CurveID{tls.X25519},
	})
	if err != nil {
		t.Fatalf("challenge handshake: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().NegotiatedProtocol; got != acme.ALPNProto {
		t.Fatalf("negotiated %q", got)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write([]byte("GET /v1/rates HTTP/1.1\r\nHost: " + apiHost + "\r\n\r\n"))
	if n, err := conn.Read(make([]byte, 64)); n != 0 || err == nil {
		t.Fatalf("read %d bytes, %v; want the connection closed", n, err)
	}
}

func TestRouting(t *testing.T) {
	var proxied []string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	edge := newEdge([]string{apiHost}, "https://moqom.cloud/", proxy)
	cases := []struct {
		method, host, target string
		status               int
		location             string
	}{
		{"GET", apiHost, "/v1/rates", http.StatusTeapot, ""},
		{"POST", apiHost, "/v1/signup", http.StatusTeapot, ""},
		{"OPTIONS", apiHost, "/v1/public/stats", http.StatusTeapot, ""},
		{"GET", "API.moqom.cloud:443", "/v1/rates", http.StatusTeapot, ""},
		{"GET", apiHost, "/", http.StatusFound, "https://moqom.cloud/"},
		{"GET", apiHost, "/pricing?plan=pro", http.StatusFound, "https://moqom.cloud/"},
		{"GET", apiHost, "/v1", http.StatusFound, "https://moqom.cloud/"},
		{"GET", apiHost, "/mls/v1/sync", http.StatusFound, "https://moqom.cloud/"},
		{"GET", "moqom.cloud", "/v1/rates", http.StatusMisdirectedRequest, ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.host+c.target, func(t *testing.T) {
			proxied = nil
			r := httptest.NewRequest(c.method, "https://"+c.host+c.target, nil)
			r.Host = c.host
			w := httptest.NewRecorder()
			edge.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("status %d, want %d", w.Code, c.status)
			}
			if got := w.Header().Get("Location"); got != c.location {
				t.Fatalf("Location %q, want %q", got, c.location)
			}
			if got := w.Header().Get("Strict-Transport-Security"); got != hsts {
				t.Fatalf("HSTS %q", got)
			}
			if (c.status == http.StatusTeapot) != (len(proxied) == 1) {
				t.Fatalf("proxied %v", proxied)
			}
		})
	}
}

func TestPortEighty(t *testing.T) {
	manager := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(apiHost),
		Cache: autocert.DirCache(t.TempDir())}
	handler := manager.HTTPHandler(redirectToHTTPS([]string{apiHost}))
	cases := []struct {
		host, target string
		status       int
		location     string
	}{
		{apiHost, "/v1/rates?x=1&y=2", http.StatusMovedPermanently, "https://api.moqom.cloud/v1/rates?x=1&y=2"},
		{apiHost, "/", http.StatusMovedPermanently, "https://api.moqom.cloud/"},
		{"elsewhere.example", "/", http.StatusNotFound, ""},
		// ACME's HTTP-01 path is answered by autocert, never redirected; an
		// unknown token is a 404.
		{apiHost, "/.well-known/acme-challenge/unknown", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://"+c.host+c.target, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != c.status || w.Header().Get("Location") != c.location {
			t.Errorf("%s%s: %d %q, want %d %q", c.host, c.target, w.Code, w.Header().Get("Location"), c.status, c.location)
		}
	}
}

type seen struct {
	forwardedFor, forwardedProto, host, body string
	group                                    tls.CurveID
}

// startUpstream is the control plane's public listener: a private
// certificate naming moqom-control, pqtls.Public, HTTP/2.
func startUpstream(t *testing.T, ca *authority, handler http.Handler) *url.URL {
	t.Helper()
	upstream := httptest.NewUnstartedServer(handler)
	upstream.EnableHTTP2 = true
	upstream.TLS = pqtls.Public(&tls.Config{Certificates: []tls.Certificate{ca.leaf(t, "moqom-control")}})
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	return target
}

func TestProxy(t *testing.T) {
	ca := newAuthority(t)
	got := make(chan seen, 1)
	release := make(chan struct{})
	upstream := startUpstream(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/live/connections/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: first\n\n"))
			_ = http.NewResponseController(w).Flush()
			<-release
		default:
			body, _ := io.ReadAll(r.Body)
			got <- seen{r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.Host, string(body), r.TLS.CurveID}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
		}
	}))
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	edge := startEdge(t, newProxy(upstream, upstreamTransport(ca.pool, "moqom-control"), log))
	client := edge.client(&tls.Config{})

	t.Run("forwarded for the client, whatever it claims", func(t *testing.T) {
		request, _ := http.NewRequest("POST", "https://"+apiHost+"/v1/signup", strings.NewReader(`{"email":"a@b.c"}`))
		request.Header.Set("X-Forwarded-For", "6.6.6.6")
		request.Header.Set("Forwarded", "for=6.6.6.6")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusCreated || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d, Cache-Control %q", response.StatusCode, response.Header.Get("Cache-Control"))
		}
		s := <-got
		if s.forwardedFor != "127.0.0.1" || s.forwardedProto != "https" || s.host != apiHost || s.body != `{"email":"a@b.c"}` {
			t.Fatalf("upstream saw %+v", s)
		}
		if s.group != tls.SecP384r1MLKEM1024 {
			t.Fatalf("upstream negotiated %s, want SecP384r1MLKEM1024", s.group)
		}
	})

	t.Run("streams before the response ends", func(t *testing.T) {
		defer close(release)
		response, err := client.Get("https://" + apiHost + "/v1/live/connections/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		line := make(chan string, 1)
		go func() {
			text, _ := bufio.NewReader(response.Body).ReadString('\n')
			line <- text
		}()
		select {
		case text := <-line:
			if text != "data: first\n" {
				t.Fatalf("read %q", text)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("first event not delivered while the stream is open")
		}
	})
}

func TestProxyVerifiesUpstream(t *testing.T) {
	ca := newAuthority(t)
	upstream := startUpstream(t, ca, answer)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := map[string]*http.Transport{
		"another authority": upstreamTransport(newAuthority(t).pool, "moqom-control"),
		"another name":      upstreamTransport(ca.pool, "somebody-else"),
	}
	for name, transport := range cases {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "https://"+apiHost+"/v1/rates", nil)
			newProxy(upstream, transport, log).ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502", w.Code)
			}
		})
	}
}

func TestUpstreamAllowsSignup(t *testing.T) {
	transport := upstreamTransport(x509.NewCertPool(), "moqom-control")
	if transport.ResponseHeaderTimeout < 90*time.Second {
		t.Fatalf("ResponseHeaderTimeout %s; signup needs 90s", transport.ResponseHeaderTimeout)
	}
	if got := transport.TLSClientConfig.CurvePreferences; len(got) != len(pqtls.LinkCurves) || got[0] != pqtls.LinkCurves[0] {
		t.Fatalf("upstream curves %v", got)
	}
}
