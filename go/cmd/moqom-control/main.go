// Command moqom-control is the control plane.
//
// It owns everything that is a request/response over a database -- identity, tokens,
// rooms, moderation state, the audit log -- and nothing on the media path. No byte of
// anybody's video passes through this process, which is what lets it be deployed,
// restarted, and scaled on its own schedule.
//
// Configuration is environment variables rather than a file, because the first
// deployment target is Cloud Run and that is what Cloud Run gives you.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"golang.org/x/oauth2/google"

	sectoken "github.com/moqom-cloud/moqom-security/go/token"

	"github.com/moqom-cloud/moqom/go/internal/billing"
	billingpg "github.com/moqom-cloud/moqom/go/internal/billing/postgres"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/listen"
	"github.com/moqom-cloud/moqom/go/internal/mls"
	mlspg "github.com/moqom-cloud/moqom/go/internal/mls/postgres"
	"github.com/moqom-cloud/moqom/go/internal/pqtls"
	"github.com/moqom-cloud/moqom/go/internal/recording"
	recordingpg "github.com/moqom-cloud/moqom/go/internal/recording/postgres"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/store/postgres"
	"github.com/moqom-cloud/moqom/go/internal/timecode"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// version is stamped by the container build with `-X main.version`.
//
// A running process has to be able to say which commit it is. A container has no
// git directory to infer it from, and matching a misbehaving replica to a change
// during an incident is not the moment to be guessing from a deployment
// timestamp.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("moqom control plane starting", "version", version)

	if err := run(); err != nil {
		logger.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	address := env("MOQOM_CONTROL_ADDR", ":8443")
	issuer := env("MOQOM_CONTROL_ISSUER", "moqom.cloud")

	keys, err := signingKeys()
	if err != nil {
		return err
	}
	accounts, relayTenants, err := serviceAccounts()
	if err != nil {
		return err
	}
	sealKeys, err := sealKeys()
	if err != nil {
		return err
	}
	// One clock for the process, disciplined by the host's NTP daemon and
	// honest about how well that is going. See internal/timecode.
	//
	// A wider ceiling than a relay's, because this runs on a node whose time
	// daemon belongs to whoever built the node and polls on a schedule we cannot
	// set. Not a lowered standard: the kernel's bound grows at a fixed 500ppm
	// between corrections, so on a long poll interval it exceeds a relay's
	// ceiling while the clock is behaving perfectly. See the constant.
	clock := timecode.NewHost()
	clock.Ceiling = timecode.CeilingWhereTheDaemonIsNotOurs
	reportClockHealth(clock)

	backing, err := backingStore(func() time.Time { return clock.Now().At })
	if err != nil {
		return err
	}

	payments, err := billingService(backing)
	if err != nil {
		return err
	}
	config := server.Config{
		Store:    backing,
		Accounts: accounts,
		Issuer:   issuer,
		Keys:     keys,
		Clock:    clock,

		SealKeys:    sealKeys,
		SealTenants: relayTenants,
	}
	// The key audit is buffered in memory and written every few seconds; the
	// last write happens on the way out, after the listeners have drained.
	keyAudit, stopKeyAudit := context.WithCancel(context.Background())
	keyAuditDone := make(chan struct{})
	defer func() {
		stopKeyAudit()
		<-keyAuditDone
	}()
	if payments != nil {
		config.Accounts = server.AnyAccounts{accounts, payments.Accounts()}
		config.Credits = payments
		go func() {
			defer close(keyAuditDone)
			payments.RunKeyAudit(keyAudit)
		}()
	} else {
		close(keyAuditDone)
	}
	if config.Recording, err = recordingConfig(backing); err != nil {
		return err
	}
	storageBilling, stopStorageBilling := context.WithCancel(context.Background())
	defer stopStorageBilling()
	if err := billRecordingStorage(storageBilling, config.Recording, payments); err != nil {
		return err
	}
	if config.Overlay, err = overlayConfig(backing); err != nil {
		return err
	}
	if config.Stun, err = stunEndpoint(); err != nil {
		return err
	}
	if config.AppleSignIn, err = appleSignIn(backing, len(sealKeys) > 0); err != nil {
		return err
	}
	instance, err := server.New(config)
	if err != nil {
		return err
	}

	// Billing rides on the public listener, so the deployment keeps exactly
	// one internet-facing listener. Its own address is for a laptop running
	// without one.
	var billingRoutes http.Handler
	if payments != nil {
		billingRoutes = billing.CORS(payments.Handler(accounts), strings.Split(os.Getenv("MOQOM_BILLING_CORS_ORIGINS"), ","))
		operatorRoutes, err := operatorAPI(payments)
		if err != nil {
			return err
		}
		if operatorRoutes != nil {
			// Its own subtree and its own CORS, so the public site's origins
			// never reach it and its origin never reaches the tenant API.
			combined := http.NewServeMux()
			combined.Handle("/v1/operator/", operatorRoutes)
			combined.Handle("/", billingRoutes)
			billingRoutes = combined
		}
	}
	var billingAPI *http.Server
	if address := os.Getenv("MOQOM_BILLING_ADDR"); billingRoutes != nil && address != "" {
		billingAPI = &http.Server{
			Addr:              address,
			Handler:           billingRoutes,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
		}
		go func() {
			if err := billingAPI.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("billing endpoint stopped", "error", err)
			}
		}()
		slog.Info("billing endpoint listening", "address", billingAPI.Addr)
	}

	transport, described, err := transportCredentials()
	if err != nil {
		return err
	}

	unary, streaming := instance.Interceptors()
	options := []grpc.ServerOption{
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(streaming),
		// Relays hold one stream open indefinitely and may be idle for hours in a
		// quiet region. Without keepalives, a middlebox drops the connection and the
		// relay does not find out until it has a directive to miss.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	if transport != nil {
		options = append(options, grpc.Creds(transport))
	}
	grpcServer := grpc.NewServer(options...)
	instance.Register(grpcServer)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	// The address logged is the one the socket actually has, not the one that was
	// requested. They differ whenever the port is 0, which is how a test finds the
	// server it just started without racing to reserve a port first.
	slog.Info("control plane listening",
		"address", listener.Addr().String(), "issuer", issuer,
		"keys", len(keys), "transport", described)

	// The same gRPC server, also bound as an OpenZiti service when the
	// deployment has enrolled one. Same credentials on top: the overlay
	// decides who may reach the port, and mutual TLS still decides who the
	// caller is to this deployment. See internal/listen.
	overlaySpec, err := listen.Overlay(os.Getenv)
	if err != nil {
		return err
	}
	var overlay net.Listener
	if overlaySpec != nil {
		overlay, err = listen.Open(context.Background(), *overlaySpec)
		if err != nil {
			return err
		}
		slog.Info("control plane listening", "overlay", overlaySpec.String())
	}
	overlayServer, overlayAPIListener, err := overlayAPI(instance)
	if err != nil {
		return err
	}
	reconciling, stopReconciling := context.WithCancel(context.Background())
	defer stopReconciling()
	if config.Overlay != nil {
		go reconcileRelays(reconciling, instance)
	}
	go instance.RunSealRefresh(reconciling)

	// A second listener for the probes, on a port of its own.
	//
	// Separate from the gRPC port because the things that read it cannot speak
	// gRPC -- a Google Cloud uptime check is HTTP over TCP, and so is the
	// simplest load balancer health check -- and because that port can then stay
	// closed to everything but the orchestrator while the API port is public.
	probes := &http.Server{
		Addr:              env("MOQOM_CONTROL_HEALTH_ADDR", ":8080"),
		Handler:           instance.HealthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := probes.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Not fatal. A control plane that is serving media control but has
			// lost its health port should be reported and left running: killing
			// it would turn a monitoring fault into an outage.
			slog.Error("health endpoint stopped", "error", err)
		}
	}()
	slog.Info("health endpoint listening", "address", probes.Addr,
		"liveness", server.LivenessPath, "readiness", server.ReadinessPath)

	// A third listener, and the only one a phone or a customer's relay ever
	// reaches.
	//
	// Off unless an address is given, because it is the one port here that faces
	// the internet and a surface like that should be something a deployment
	// turned on rather than something it inherited. Nothing on it mints a token
	// from a credential a device carries; see internal/server/public.go.
	keyRoutes, err := keyDelivery(instance, backing, clock)
	if err != nil {
		return err
	}
	public, err := publicEndpoint(instance, billingRoutes, keyRoutes)
	if err != nil {
		return err
	}
	if public != nil {
		go func() {
			err := servePublic(public)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Fatal to the feature and not to the process. A control plane
				// that cannot deliver keys is still running rooms for every
				// session already connected, and killing it would take those
				// down to fix the ones that cannot start.
				slog.Error("public listener stopped", "error", err)
			}
		}()
		slog.Info("public listener listening", "address", public.Addr,
			"transport", publicTransport())
	}

	stopped := make(chan error, 3)
	go func() { stopped <- grpcServer.Serve(listener) }()
	if overlay != nil {
		go func() { stopped <- grpcServer.Serve(overlay) }()
	}
	if overlayServer != nil {
		go func() { stopped <- overlayServer.Serve(overlayAPIListener) }()
	}

	interrupted, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-stopped:
		return err
	case <-interrupted.Done():
		slog.Info("draining")
		// The probes go first, and the wait is the point: an orchestrator needs
		// a moment to notice this replica is leaving and stop sending it work.
		// Closing both at once means the requests already in flight towards it
		// arrive at a socket that is gone.
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = probes.Shutdown(shutdown)
		if public != nil {
			_ = public.Shutdown(shutdown)
		}
		if billingAPI != nil {
			_ = billingAPI.Shutdown(shutdown)
		}
		// Graceful, so relays see a clean close and reattach rather than timing out.
		// Their grace period covers the gap, which is the whole reason it exists.
		if overlayServer != nil {
			overlayServer.GracefulStop()
		}
		grpcServer.GracefulStop()
		return nil
	}
}

// stunEndpoint reads MOQOM_CONTROL_STUN, the relays' STUN responder as
// "host" or "host:port" (port 3478 if omitted), which every token whose room
// allows direct paths names so devices behind NATs can learn their public
// address. The relays' edge name is the usual value: any relay answers.
// Unset names none, and devices offer only their own interface addresses.
func stunEndpoint() (*control.StunEndpoint, error) {
	value := os.Getenv("MOQOM_CONTROL_STUN")
	if value == "" {
		slog.Info("no MOQOM_CONTROL_STUN, so tokens name no STUN server")
		return nil, nil
	}
	endpoint, err := control.ParseStunEndpoint(value)
	if err != nil {
		return nil, fmt.Errorf("MOQOM_CONTROL_STUN: %w", err)
	}
	slog.Info("tokens name a STUN server", "host", endpoint.Host, "port", endpoint.Port)
	return &endpoint, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// publicEndpoint builds the internet-facing listener, or nil for a deployment
// that has not asked for one.
//
// Its own port rather than a route on an existing one, because the three
// listeners here need three different answers to "who may reach this". The gRPC
// port wants a client certificate and an internal address; the probe port wants
// to be visible to the orchestrator and nobody else; this one has to be
// reachable from a phone on a mobile network, which is to say from anywhere, and
// putting it behind the same socket as either of the others would mean opening
// that socket too.
//
// Its own certificate is not needed: it serves the same leaf as the gRPC port,
// which is already issued for this deployment and whose hash the clients already
// pin. What it must not do is ask for one back. A client CA here would mean
// every phone needed an issued certificate before it could ask for a token,
// which is the bootstrapping problem a certificate cannot solve.
func publicEndpoint(instance *server.Server, billingRoutes, keyRoutes http.Handler) (*http.Server, error) {
	address := os.Getenv("MOQOM_CONTROL_PUBLIC_ADDR")
	if address == "" {
		return nil, nil
	}

	endpoint := &http.Server{
		Addr:    address,
		Handler: instance.PublicHandler(),
		// Short, because the work behind it is a signature and a row read. A
		// request that has not finished sending its headers in five seconds is
		// not a phone on a bad network, it is a socket being held open.
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	// The console's live connections map. Authenticated by a tenant's backend
	// key in the Authorization header and allowed from the same browser origins
	// as billing; see internal/server/livemap.go. The stream clears the
	// listener's write timeout for itself.
	live := billing.CORS(instance.LiveHandler(server.LiveOptionsFromEnv()),
		strings.Split(os.Getenv("MOQOM_BILLING_CORS_ORIGINS"), ","))
	routes := http.NewServeMux()
	// Annotated so an API key used on these routes is scoped, allowlisted and
	// audited against the same trusted client address the billing routes use.
	// A child relay's credential for its parent, for relay service accounts
	// only; see internal/server/relaylinktoken.go.
	routes.Handle(server.RelayLinkPath, billing.Annotate(endpoint.Handler))
	// A managed relay VM's overlay identity; see internal/server/overlay.go.
	routes.Handle(server.RelayEnrollPath, billing.Annotate(endpoint.Handler))
	// Apple sign-in for apps with no backend; see internal/server/apple.go.
	routes.Handle(server.AppleSignInPath, billing.Annotate(endpoint.Handler))
	routes.Handle(server.AppleTokenPath, billing.Annotate(endpoint.Handler))
	live = billing.Annotate(live)
	routes.Handle(server.LivePath, live)
	routes.Handle(server.LiveStreamPath, live)
	// The console's read-only rooms list and room event stream, authenticated
	// and allowed from the same origins as the live map; see
	// internal/server/roomsweb.go.
	rooms := billing.Annotate(billing.CORS(instance.RoomsHandler(server.RoomsOptions{}),
		strings.Split(os.Getenv("MOQOM_BILLING_CORS_ORIGINS"), ",")))
	routes.Handle(server.RoomsPath, rooms)
	routes.Handle(server.RoomEventsStreamPath, rooms)
	// MLS key delivery for end-to-end encrypted rooms, authenticated by the
	// device's participant token; see internal/mls. Here because this is the
	// port a phone can reach. The sync long poll extends the write timeout for
	// itself.
	routes.Handle(mls.PathPrefix, keyRoutes)
	// The host third-party webhooks are sent to, which serves nothing else;
	// see hooks.go.
	stripeSenders, err := parseNetworks(os.Getenv("MOQOM_CONTROL_HOOKS_STRIPE_IPS"))
	if err != nil {
		return nil, fmt.Errorf("MOQOM_CONTROL_HOOKS_STRIPE_IPS: %w", err)
	}
	endpoint.Handler = hooksOnly(os.Getenv("MOQOM_CONTROL_HOOKS_HOST"), stripeSenders, billing.ClientAddress, routes)
	if billingRoutes != nil {
		routes.Handle("/v1/", billingRoutes)
		// Signup makes several Stripe calls in a row, each allowed thirty seconds.
		endpoint.WriteTimeout = 90 * time.Second
	}

	certificate := os.Getenv("MOQOM_CONTROL_TLS_CERT")
	key := os.Getenv("MOQOM_CONTROL_TLS_KEY")
	if certificate == "" || key == "" {
		// Reachable only with MOQOM_CONTROL_ALLOW_PLAINTEXT set, because
		// transportCredentials runs first and refuses the same missing pair
		// without it. A credential travelling in a header over plaintext to an
		// address on the internet is a credential anyone on the path collects,
		// so the log line has to say which of the two this is -- see
		// publicTransport, which is printed at startup.
		return endpoint, nil
	}

	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, fmt.Errorf("public listener certificate: %w", err)
	}
	endpoint.TLSConfig = pqtls.Public(&tls.Config{
		Certificates: []tls.Certificate{pair},
	})
	return endpoint, nil
}

// servePublic starts the listener with whatever transport it was given.
func servePublic(endpoint *http.Server) error {
	if endpoint.TLSConfig == nil {
		return endpoint.ListenAndServe()
	}
	return endpoint.ListenAndServeTLS("", "")
}

func publicTransport() string {
	if os.Getenv("MOQOM_CONTROL_TLS_CERT") == "" || os.Getenv("MOQOM_CONTROL_TLS_KEY") == "" {
		return "plaintext — do not expose"
	}
	return "tls, no client certificate"
}

// envFlag reads a boolean switch. Anything other than an explicit yes is no,
// because every switch read this way turns a protection off.
func envFlag(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// transportCredentials builds the gRPC port's TLS, and returns nil for none.
//
// # Why a control plane holds a certificate at all
//
// It used to sit behind a terminator that had one, which is right for the admin
// API -- that is a request/response surface reached through a load balancer or a
// port-forward. It is not right for RelayLink. Over that stream the control
// plane sends a relay the keys it will authenticate every session with, and a
// relay taking keys from an unauthenticated peer is a relay that will take them
// from whatever answers to the address. Terminating TLS in front does not help:
// what needs authenticating is the process, to the relay, and a terminator can
// only prove it is the terminator.
//
// So both directions. The client certificate is what proves a peer is one of
// this deployment's relays; the bearer credential it also presents is what says
// which one, and whether it may use the admin API at all. Two mechanisms because
// they answer different questions, and the certificate answers the one that a
// stolen credential would otherwise settle on its own.
//
// # Strict unless told otherwise
//
// Mutual TLS is the default, and the process refuses to start without it. A
// laptop, the test suites, and a deployment that terminates TLS in front of the
// container (Cloud Run) each have to say so: MOQOM_CONTROL_ALLOW_PLAINTEXT for
// no TLS at all, MOQOM_CONTROL_ALLOW_NO_CLIENT_CERT for TLS that does not ask
// relays for a certificate. A missing variable is how a deployment ends up
// insecure without anybody having decided it should be, so a missing variable
// is an error, and the decision is a line somebody wrote in the deployment's
// configuration. The log says which of these this process is, because "the
// relays cannot attach" and "the relays attached to something else" look the
// same from here.
func transportCredentials() (credentials.TransportCredentials, string, error) {
	certificate := os.Getenv("MOQOM_CONTROL_TLS_CERT")
	key := os.Getenv("MOQOM_CONTROL_TLS_KEY")
	clientCA := os.Getenv("MOQOM_CONTROL_CLIENT_CA")

	if certificate == "" && key == "" {
		if clientCA != "" {
			return nil, "", errors.New(
				"MOQOM_CONTROL_CLIENT_CA is set without MOQOM_CONTROL_TLS_CERT, " +
					"so client certificates could never be presented, let alone verified")
		}
		if !envFlag("MOQOM_CONTROL_ALLOW_PLAINTEXT") {
			return nil, "", errors.New(
				"MOQOM_CONTROL_TLS_CERT and MOQOM_CONTROL_TLS_KEY are unset; the control plane " +
					"serves mutual TLS by default. Set them (and MOQOM_CONTROL_CLIENT_CA), or set " +
					"MOQOM_CONTROL_ALLOW_PLAINTEXT=true where TLS is terminated in front of this " +
					"process or on a laptop")
		}
		slog.Warn("serving without TLS because MOQOM_CONTROL_ALLOW_PLAINTEXT is set")
		return nil, "plaintext", nil
	}
	if certificate == "" || key == "" {
		return nil, "", errors.New("MOQOM_CONTROL_TLS_CERT and MOQOM_CONTROL_TLS_KEY go together")
	}

	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, "", fmt.Errorf("reading the server certificate: %w", err)
	}

	// Both ends are ours and both are current: TLS 1.3 and ML-KEM-1024 only.
	config := pqtls.Link(&tls.Config{
		Certificates: []tls.Certificate{pair},
	})
	described := "tls"

	if clientCA == "" {
		if !envFlag("MOQOM_CONTROL_ALLOW_NO_CLIENT_CERT") {
			return nil, "", errors.New(
				"MOQOM_CONTROL_CLIENT_CA is unset; the control plane requires relays to present " +
					"a client certificate by default. Set it, or set " +
					"MOQOM_CONTROL_ALLOW_NO_CLIENT_CERT=true to serve TLS without one")
		}
		slog.Warn("serving TLS without verifying client certificates because MOQOM_CONTROL_ALLOW_NO_CLIENT_CERT is set")
	} else {
		pool := x509.NewCertPool()
		pem, err := os.ReadFile(clientCA)
		if err != nil {
			return nil, "", fmt.Errorf("reading the client CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, "", fmt.Errorf("the client CA at %s holds no certificates", clientCA)
		}
		config.ClientCAs = pool
		// Required rather than requested. Verifying a certificate when one turns
		// up, and shrugging when it does not, is the configuration that reads as
		// mutual TLS in a diagram and authenticates nobody in practice.
		config.ClientAuth = tls.RequireAndVerifyClientCert
		described = "mutual tls"
	}
	return credentials.NewTLS(config), described, nil
}

// signingKeys reads the token keys.
//
// MOQOM_CONTROL_SIGNING_KEYS is a comma-separated list, newest first: the first
// signs, all of them verify. That ordering is what makes rotation a deployment
// rather than an outage. Each entry is `mldsa87:` and a base64url 32-byte
// ML-DSA-87 seed. An `mldsa65:`, `ed25519:` or unprefixed entry, from before
// the move, is skipped with a warning: tokens signed with those are no longer
// accepted anywhere, and leaving one in the list must not stop the control plane.
//
// With none set, one is generated and its public half logged. Useful on a laptop and
// wrong everywhere else, because tokens stop verifying at every restart -- so it says
// so loudly rather than working quietly until it does not.
func signingKeys() ([]token.Key, error) {
	raw := os.Getenv("MOQOM_CONTROL_SIGNING_KEYS")
	if raw == "" {
		generated, err := token.GenerateKey()
		if err != nil {
			return nil, err
		}
		slog.Warn("no signing key configured, generated an ephemeral one",
			"key", generated.ID, "algorithm", generated.Algorithm,
			"consequence", "every token stops verifying when this process restarts")
		return []token.Key{generated}, nil
	}

	var keys []token.Key
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kind, encoded, prefixed := strings.Cut(entry, ":")
		if !prefixed || kind == "ed25519" || kind == "mldsa65" {
			slog.Warn("skipping a signing key from before ML-DSA-87: its tokens are no longer accepted",
				"kind", kind)
			continue
		}
		seed, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
		if err != nil {
			return nil, fmt.Errorf("signing key is not base64url: %w", err)
		}
		// The identifier is derived from the public half rather than configured, so a
		// key cannot be published twice under different names as if it were two.
		var key token.Key
		switch kind {
		case "mldsa87":
			key, err = token.KeyFromSeed(seed)
		default:
			return nil, fmt.Errorf("signing key kind %q, want mldsa87", kind)
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("MOQOM_CONTROL_SIGNING_KEYS was set but held no keys")
	}
	if !keys[0].CanSign() {
		return nil, fmt.Errorf("the first MOQOM_CONTROL_SIGNING_KEYS entry is %s, which only verifies: "+
			"prepend an mldsa87: key to sign with", keys[0].Algorithm)
	}
	return keys, nil
}

// serviceAccounts reads the credentials backends authenticate with.
//
// MOQOM_CONTROL_ACCOUNTS is a path to JSON. A file rather than an inline variable
// because these are secrets and a process's environment is readable by more things
// than its files are.
//
// Keeping them in a file at all is the temporary part: rotation, revocation, and
// per-account rate limits all want a table. What is not temporary is that the ceiling
// is declared here -- an account's grants bound what it can ever mint, which is what
// keeps a leaked integration key from becoming an owner of every room.
//
// It also returns the tenants that tenant-scoped relay accounts serve, whose
// tokens are sealed with a key of their own.
func serviceAccounts() (server.Accounts, []string, error) {
	path := os.Getenv("MOQOM_CONTROL_ACCOUNTS")
	if path == "" {
		return nil, nil, errors.New("MOQOM_CONTROL_ACCOUNTS is required: a path to the service account file")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading service accounts: %w", err)
	}

	var file struct {
		Accounts []struct {
			Name       string   `json:"name"`
			Credential string   `json:"credential"`
			Tenant     string   `json:"tenant"`
			Publish    []string `json:"publish"`
			Moderation []string `json:"moderation"`
			Preset     string   `json:"preset"`
			IsOwner    bool     `json:"is_owner"`
			IsRelay    bool     `json:"is_relay"`
			IsRecorder bool     `json:"is_recorder"`
			Pool       string   `json:"pool"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(contents, &file); err != nil {
		return nil, nil, fmt.Errorf("parsing service accounts: %w", err)
	}

	byCredential := map[string]server.ServiceAccount{}
	var relayTenants []string
	for _, entry := range file.Accounts {
		if entry.Name == "" || entry.Credential == "" {
			return nil, nil, errors.New("every service account needs a name and a credential")
		}
		// A pool is one tenant's, and its relays are sent only that tenant's
		// keys and rooms; a relay serving every tenant cannot be in one.
		if entry.Pool != "" && (!entry.IsRelay || entry.Tenant == "") {
			return nil, nil, fmt.Errorf("account %s names pool %q but is not a relay scoped to a tenant", entry.Name, entry.Pool)
		}
		if entry.IsRelay && entry.Tenant != "" && !slices.Contains(relayTenants, entry.Tenant) {
			relayTenants = append(relayTenants, entry.Tenant)
		}
		grants := control.Grants{IsOwner: entry.IsOwner}
		if entry.Preset != "" {
			expanded, ok := control.Preset(entry.Preset).Expand()
			if !ok {
				return nil, nil, fmt.Errorf("account %s names an unknown preset %q", entry.Name, entry.Preset)
			}
			grants.Moderation = expanded
		}
		for _, name := range entry.Moderation {
			capability, ok := capabilityByName[name]
			if !ok {
				return nil, nil, fmt.Errorf("account %s names an unknown capability %q", entry.Name, name)
			}
			grants.Moderation |= capability
		}
		for _, name := range entry.Publish {
			scope, ok := publishByName[name]
			if !ok {
				return nil, nil, fmt.Errorf("account %s names an unknown publish scope %q", entry.Name, name)
			}
			grants.Publish |= scope
		}
		byCredential[entry.Credential] = server.ServiceAccount{
			Name:       entry.Name,
			Tenant:     entry.Tenant,
			Grants:     grants,
			IsRelay:    entry.IsRelay,
			IsRecorder: entry.IsRecorder,
			Pool:       entry.Pool,
		}
	}
	if len(byCredential) == 0 {
		return nil, nil, errors.New("the service account file holds no accounts")
	}

	slog.Info("service accounts loaded", "count", len(byCredential))
	return server.NewStaticAccounts(byCredential), relayTenants, nil
}

// sealKeys reads MOQOM_CONTROL_SEAL_KEYS, a path to a JWK set of AES-256-GCM
// keys ({"keys":[{"kty":"oct","alg":"A256GCM","kid":…,"k":…}]}), newest first.
// The first seals every token minted; all of them open, which is how a key is
// rotated: prepend the new one, and remove the old one fifteen minutes later.
//
// Unset mints tokens that are signed and not sealed, which is a laptop: every
// relay still verifies them, and anybody holding one can read who it names.
func sealKeys() ([]sectoken.SealKey, error) {
	path := os.Getenv("MOQOM_CONTROL_SEAL_KEYS")
	if path == "" {
		slog.Warn("no MOQOM_CONTROL_SEAL_KEYS, so tokens are signed and not sealed")
		return nil, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading seal keys: %w", err)
	}
	keys, err := sectoken.ParseSealKeySet(contents)
	if err != nil {
		return nil, fmt.Errorf("MOQOM_CONTROL_SEAL_KEYS: %w", err)
	}
	if len(keys) == 0 {
		return nil, errors.New("MOQOM_CONTROL_SEAL_KEYS holds no keys")
	}
	slog.Info("tokens are sealed", "keys", len(keys), "sealing with", keys[0].KeyID())
	return keys, nil
}

// billingService builds prepaid billing, or nil when MOQOM_BILLING is not "on".
//
// Off by default so a self-hosted control plane never starts refusing joins
// because it has no Stripe account. On, every join and minted token needs a
// tenant in credit, and customers authenticate with the API keys billing issues.
//
//	MOQOM_BILLING=on
//	MOQOM_BILLING_ADDR           a listener of its own; default is the public listener's port
//	MOQOM_BILLING_PUBLIC_URL     where checkout returns to (https://moqom.cloud)
//	MOQOM_BILLING_CORS_ORIGINS   comma-separated origins allowed to call it from a browser
//	MOQOM_STRIPE_SECRET_KEY      file path, not the key: environments leak
//	MOQOM_STRIPE_WEBHOOK_SECRET  file path
//	MOQOM_KEY_PEPPER             file path: API key hash peppers, see keyPeppers
//	MOQOM_SMTP_*                 alert and notice email, see billingNotifier
//	MOQOM_BILLING_EXEMPT         comma-separated tenants never gated (MOQOM's own)
//	MOQOM_BILLING_MARKUP_BP      markup on cost in basis points (default 1500)
//	MOQOM_BILLING_FEE_PER_GIB    platform fee in micro-dollars per GiB (default 10000)
//	MOQOM_BILLING_EGRESS_PER_GIB negotiated egress cost in micro-dollars per GiB
//	MOQOM_BILLING_DIRECT_FREE_MINUTES      direct participant-minutes free per tenant per month (default 1000)
//	MOQOM_BILLING_DIRECT_FEE_PER_1000_MIN  direct fee in micro-dollars per 1,000 minutes past that (default 500000)
//	MOQOM_BILLING_TRUSTED_PROXIES          comma-separated CIDRs whose last X-Forwarded-For entry is the
//	                                       client: the cluster's pod range, where cmd/moqom-api-edge runs.
//	                                       Read even with billing off, since the other public routes'
//	                                       key allowlists use the same address.
func billingService(backing store.Store) (*billing.Service, error) {
	if raw := strings.TrimSpace(os.Getenv("MOQOM_BILLING_TRUSTED_PROXIES")); raw != "" {
		proxies, err := billing.ParseNetworks(strings.Split(raw, ","))
		if err != nil {
			return nil, fmt.Errorf("MOQOM_BILLING_TRUSTED_PROXIES: %w", err)
		}
		billing.TrustProxies(proxies)
		slog.Info("trusting X-Forwarded-For from", "proxies", raw)
	}
	if os.Getenv("MOQOM_BILLING") != "on" {
		return nil, nil
	}
	ledger, err := billingStore(backing)
	if err != nil {
		return nil, err
	}
	config := billing.Config{
		Store:     ledger,
		Pricing:   billing.DefaultPricing(),
		PublicURL: env("MOQOM_BILLING_PUBLIC_URL", "https://moqom.cloud"),
		Exempt:    map[string]bool{},
	}
	for _, tenant := range strings.Split(os.Getenv("MOQOM_BILLING_EXEMPT"), ",") {
		if tenant = strings.TrimSpace(tenant); tenant != "" {
			config.Exempt[tenant] = true
		}
	}
	for name, into := range map[string]*int64{
		"MOQOM_BILLING_MARKUP_BP":               &config.Pricing.MarkupBasisPoints,
		"MOQOM_BILLING_FEE_PER_GIB":             (*int64)(&config.Pricing.PlatformFeePerGiB),
		"MOQOM_BILLING_SELF_HOSTED_FEE_PER_GIB": (*int64)(&config.Pricing.SelfHostedFeePerGiB),
		"MOQOM_BILLING_EGRESS_PER_GIB":          (*int64)(&config.Pricing.Costs.EgressPerGiB),
		"MOQOM_BILLING_DIRECT_FREE_MINUTES":     &config.Pricing.DirectFreeMinutesPerMonth,
		"MOQOM_BILLING_DIRECT_FEE_PER_1000_MIN": (*int64)(&config.Pricing.DirectFeePer1000Minutes),
	} {
		if raw := os.Getenv(name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return nil, fmt.Errorf("%s must be a non-negative integer", name)
			}
			*into = value
		}
	}
	// Recording storage is priced where the bucket is, which DefaultPricing
	// assumes is a North American region.
	if location := strings.TrimSpace(os.Getenv("MOQOM_RECORDING_BUCKET_LOCATION")); location != "" {
		prices, ok := billing.GCSStorageListPrice(location)
		if !ok {
			return nil, fmt.Errorf("MOQOM_RECORDING_BUCKET_LOCATION %q has no storage price; add it to billing.GCSStorageListPrice", location)
		}
		config.Pricing.Costs.Storage = prices
	}

	// The key file is absent until tools/sync-secrets.sh finds the key in Secret
	// Manager, and billing runs without payments until it is there, rather than
	// the control plane refusing to start.
	secretKey, err := secretFile("MOQOM_STRIPE_SECRET_KEY")
	if errors.Is(err, os.ErrNotExist) {
		secretKey, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	if secretKey != "" {
		config.Stripe = &billing.Stripe{SecretKey: secretKey}
		if config.WebhookSecret, err = secretFile("MOQOM_STRIPE_WEBHOOK_SECRET"); err != nil {
			return nil, err
		}
	} else {
		slog.Warn("billing is on without Stripe",
			"consequence", "nobody can pay, so only exempt tenants can join")
	}
	if config.Peppers, err = keyPeppers(); err != nil {
		return nil, err
	}
	if config.Notifier, err = billingNotifier(config.PublicURL); err != nil {
		return nil, err
	}
	payments, err := billing.New(config)
	if err != nil {
		return nil, err
	}
	slog.Info("billing on", "stripe", config.Stripe != nil, "exempt", len(config.Exempt),
		"key_pepper", config.Peppers.Current(), "key_pepper_versions", len(config.Peppers.Versions()),
		"markup_bp", config.Pricing.MarkupBasisPoints, "fee_per_gib", config.Pricing.PlatformFeePerGiB.String(),
		"self_hosted_fee_per_gib", config.Pricing.SelfHostedFeePerGiB.String(),
		"egress_per_gib", config.Pricing.Costs.EgressPerGiB.String(),
		"direct_free_minutes", config.Pricing.DirectFreeMinutesPerMonth,
		"direct_fee_per_1000_min", config.Pricing.DirectFeePer1000Minutes.String())
	return payments, nil
}

// keyPeppers reads MOQOM_KEY_PEPPER, a file of base64 peppers, newest first,
// mounted from Secret Manager. A missing or empty file keeps bare SHA-256
// hashes, so the secret can be created empty and filled later without a
// deploy breaking anything. Once filled, removing a version breaks every key
// still hashed under it: add a new one at the top instead, and drop the old
// only when no key's pepper_version names it.
func keyPeppers() (billing.Peppers, error) {
	contents, err := secretFile("MOQOM_KEY_PEPPER")
	if errors.Is(err, os.ErrNotExist) {
		contents, err = "", nil
	}
	if err != nil {
		return billing.Peppers{}, err
	}
	peppers, err := billing.ParsePeppers(contents)
	if err != nil {
		return billing.Peppers{}, fmt.Errorf("MOQOM_KEY_PEPPER: %w", err)
	}
	if peppers.Current() == "" {
		slog.Warn("API key hashes are not peppered",
			"consequence", "a copy of billing_keys alone is enough to test a guessed key",
			"remedy", "add a version to the api-key-pepper secret and rerun sync-secrets.sh")
	}
	return peppers, nil
}

// billingNotifier emails spend alerts and leaked-key notices, or logs them
// when no SMTP server is configured.
//
//	MOQOM_SMTP_HOST            the submission server; unset logs instead of sending
//	MOQOM_SMTP_PORT            587 (STARTTLS, default) or 465 (implicit TLS)
//	MOQOM_SMTP_USERNAME        for AUTH PLAIN, over TLS only
//	MOQOM_SMTP_PASSWORD        file path, not the password
//	MOQOM_BILLING_EMAIL_FROM   sender (billing@moqom.cloud)
//	MOQOM_BILLING_CONSOLE_URL  linked from each message (<public URL>/account)
func billingNotifier(publicURL string) (billing.Notifier, error) {
	host := strings.TrimSpace(os.Getenv("MOQOM_SMTP_HOST"))
	if host == "" {
		slog.Warn("no SMTP server configured", "consequence", "spend alerts and leaked-key notices are logged, not emailed")
		return billing.LogNotifier{Log: slog.Default()}, nil
	}
	password, err := secretFile("MOQOM_SMTP_PASSWORD")
	if errors.Is(err, os.ErrNotExist) {
		password, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	notifier := &billing.SMTPNotifier{
		Host:       host,
		Port:       env("MOQOM_SMTP_PORT", "587"),
		Username:   strings.TrimSpace(os.Getenv("MOQOM_SMTP_USERNAME")),
		Password:   password,
		From:       env("MOQOM_BILLING_EMAIL_FROM", "MOQOM Billing <billing@moqom.cloud>"),
		ConsoleURL: env("MOQOM_BILLING_CONSOLE_URL", strings.TrimRight(publicURL, "/")+"/account"),
	}
	if notifier.Username != "" && notifier.Password == "" {
		// Logged rather than fatal: a password secret not filled yet should not
		// stop billing, and every send would fail authentication anyway.
		slog.Warn("MOQOM_SMTP_USERNAME is set but the MOQOM_SMTP_PASSWORD file is missing or empty",
			"consequence", "spend alerts and leaked-key notices are logged, not emailed")
		return billing.LogNotifier{Log: slog.Default()}, nil
	}
	slog.Info("billing email on", "host", host, "port", notifier.Port, "from", notifier.From)
	return notifier, nil
}

// operatorAPI builds /v1/operator/*, MOQOM staff's view across tenants, or nil
// when it is not configured. See internal/billing/operator.go and
// docs/operations/admin-console.md.
//
//	MOQOM_OPERATOR_GOOGLE_CLIENT_ID  the admin console's OAuth client; the only ID token audience accepted
//	MOQOM_OPERATOR_EMAILS            comma-separated operator addresses; nobody else is let in
//	MOQOM_OPERATOR_HOSTED_DOMAIN     Workspace domain the account must belong to (moqom.cloud)
//	MOQOM_OPERATOR_ORIGINS           browser origins allowed to call it (https://admin.moqom.cloud)
func operatorAPI(payments *billing.Service) (http.Handler, error) {
	clientID := strings.TrimSpace(os.Getenv("MOQOM_OPERATOR_GOOGLE_CLIENT_ID"))
	allowed := billing.ParseAllowList(os.Getenv("MOQOM_OPERATOR_EMAILS"))
	if clientID == "" && len(allowed) == 0 {
		return nil, nil
	}
	if clientID == "" || len(allowed) == 0 {
		return nil, errors.New("the operator API needs both MOQOM_OPERATOR_GOOGLE_CLIENT_ID and MOQOM_OPERATOR_EMAILS")
	}
	handler, err := payments.OperatorHandler(billing.OperatorOptions{
		Verifier: &billing.GoogleIdentity{
			ClientID:     clientID,
			Allowed:      allowed,
			HostedDomain: strings.TrimSpace(os.Getenv("MOQOM_OPERATOR_HOSTED_DOMAIN")),
			Keys:         &billing.RemoteKeys{URL: billing.GoogleCertsURL},
		},
		Origins: strings.Split(os.Getenv("MOQOM_OPERATOR_ORIGINS"), ","),
	})
	if err != nil {
		return nil, err
	}
	slog.Info("operator API on", "operators", len(allowed), "hosted_domain", os.Getenv("MOQOM_OPERATOR_HOSTED_DOMAIN"))
	return handler, nil
}

// billingStore keeps the ledger in the same database as rooms, so a backup of
// one is a backup of both.
func billingStore(backing store.Store) (billing.Store, error) {
	if subject, ok := backing.(*postgres.Store); ok {
		return billingpg.New(context.Background(), subject.Pool())
	}
	if os.Getenv("MOQOM_BILLING_ALLOW_MEMORY") != "true" {
		return nil, errors.New("billing on the in-memory store would lose every payment at restart; " +
			"use MOQOM_CONTROL_STORE=postgres, or set MOQOM_BILLING_ALLOW_MEMORY=true on a laptop")
	}
	slog.Warn("billing ledger in memory", "consequence", "balances and keys are lost on restart")
	return billing.NewMemory(), nil
}

// keyDelivery builds the MLS delivery service on the same database as rooms.
//
// In memory without the opt-in billing needs, because losing it costs every
// live encrypted room one re-founding of its group and nothing else: it holds
// no secrets and no history anybody needs back.
func keyDelivery(instance *server.Server, backing store.Store, clock timecode.Clock) (http.Handler, error) {
	var keys mls.Store = mls.NewMemory()
	if subject, ok := backing.(*postgres.Store); ok {
		durable, err := mlspg.New(context.Background(), subject.Pool())
		if err != nil {
			return nil, err
		}
		keys = durable
	}
	service := mls.NewService(keys, func() time.Time { return clock.Now().At })
	return instance.KeyDeliveryHandler(service), nil
}

// recordingConfig turns hosted recording on when a bucket is named.
//
// Download links are signed through IAM as MOQOM_RECORDING_SIGNER_ACCOUNT,
// which the control plane's own account needs roles/iam.serviceAccountTokenCreator
// on; without it, GetRecording answers with the artifacts and no links.
func recordingConfig(backing store.Store) (*server.RecordingConfig, error) {
	bucket := strings.TrimSpace(os.Getenv("MOQOM_RECORDING_BUCKET"))
	if bucket == "" {
		return nil, nil
	}
	thumbprint := strings.TrimSpace(os.Getenv("MOQOM_RECORDER_THUMBPRINT"))
	if thumbprint == "" {
		return nil, errors.New("MOQOM_RECORDING_BUCKET needs MOQOM_RECORDER_THUMBPRINT, the JWK thumbprint of the recorder's key")
	}
	config := &server.RecordingConfig{Bucket: bucket, RecorderThumbprint: thumbprint}

	if subject, ok := backing.(*postgres.Store); ok {
		durable, err := recordingpg.New(context.Background(), subject.Pool())
		if err != nil {
			return nil, err
		}
		config.Store = durable
	} else {
		slog.Warn("recordings in memory", "consequence", "the list of recordings is lost on restart; the files are not")
		config.Store = recording.NewMemory()
	}

	if account := strings.TrimSpace(os.Getenv("MOQOM_RECORDING_SIGNER_ACCOUNT")); account != "" {
		client, err := google.DefaultClient(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("credentials for signing recording links: %w", err)
		}
		config.Signer = recording.GCSSigner{Signer: recording.IAMSigner{Account: account, Client: client}}
	}
	slog.Info("hosted recording on", "bucket", bucket, "signed_links", config.Signer != nil)
	return config, nil
}

// billRecordingStorage charges tenants for what their recordings hold in the
// bucket, hourly, a closed day at a time. MOQOM_RECORDING_LIFECYCLE must be
// the bucket's lifecycle rules, which Terraform sets beside them.
func billRecordingStorage(ctx context.Context, config *server.RecordingConfig, payments *billing.Service) error {
	if config == nil {
		return nil
	}
	lifecycle, err := recording.ParseLifecycle(os.Getenv("MOQOM_RECORDING_LIFECYCLE"))
	if err != nil {
		return err
	}
	if payments == nil {
		slog.Warn("recording storage not billed", "reason", "billing is off")
		return nil
	}
	job := &recording.StorageBilling{Store: config.Store, Biller: storageBiller{payments}, Lifecycle: lifecycle}
	go job.Loop(ctx, time.Hour)
	slog.Info("recording storage billed", "lifecycle", fmt.Sprintf("%+v", lifecycle))
	return nil
}

// storageBiller is billing.Service as recording.StorageBiller.
type storageBiller struct{ service *billing.Service }

func (b storageBiller) BillStorage(ctx context.Context, charges []recording.StorageCharge) error {
	records := make([]billing.StorageRecord, 0, len(charges))
	for _, c := range charges {
		records = append(records, billing.StorageRecord{
			ID: c.ID, Tenant: c.Tenant, Room: c.Room, Recording: c.Recording,
			Class: billing.StorageClass(c.Class), Bytes: c.Bytes, Seconds: c.Seconds,
		})
	}
	_, err := b.service.RecordStorage(ctx, records)
	return err
}

func secretFile(name string) (string, error) {
	path := os.Getenv(name)
	if path == "" {
		return "", nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(contents)), nil
}

// reportClockHealth says at startup whether this host can timestamp anything.
//
// Worth a line in the log rather than leaving it to be inferred from the width
// of timecodes much later. A control plane whose chrony never started produces
// a complete, entirely plausible-looking audit log in which nothing can be
// ordered against another machine, and the cheapest moment to find that out is
// before it has recorded anything.
func reportClockHealth(clock *timecode.Host) {
	health, ok := clock.Healthy()
	switch {
	case !ok:
		slog.Warn("cannot read the host clock's discipline",
			"consequence", "every event will be stamped with fallback time",
			"note", "expected off Linux; production hosts read it via adjtimex")
	case !health.Synchronised:
		slog.Error("the host clock is not disciplined",
			"consequence", "events cannot be ordered against other machines",
			"remedy", "check chrony is running and can reach time.cloudflare.com")
	default:
		slog.Info("host clock disciplined",
			"maxErrorMs", health.MaxError.Milliseconds(),
			"ceilingMs", clock.Ceiling.Milliseconds(),
			"leapPending", health.LeapPending)
	}
}

func backingStore(now func() time.Time) (store.Store, error) {
	switch kind := env("MOQOM_CONTROL_STORE", "memory"); kind {
	case "memory":
		// Honest about what this is. A single-node deployment or a laptop can run
		// against it; a restart loses every room, ban, and audit entry.
		slog.Warn("using the in-memory store",
			"consequence", "all state, including the moderation log, is lost on restart")
		return memory.New(now, nil), nil
	case "postgres":
		settings, err := databaseConfig()
		if err != nil {
			return nil, err
		}
		settings.Now = now

		// Bounded, because a database that cannot be reached at startup is a
		// misconfiguration and the useful thing to do about it is to fail
		// visibly rather than sit at a health check that never passes.
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()

		subject, err := postgres.Open(ctx, settings)
		if err != nil {
			return nil, fmt.Errorf("opening the postgres store: %w", err)
		}
		slog.Info("using the postgres store", "instance", settings.Instance)
		return subject, nil
	default:
		return nil, fmt.Errorf("unknown store %q, want memory or postgres", kind)
	}
}

// databaseConfig assembles how to reach Postgres.
//
// Two ways in, and the deployment uses the first. MOQOM_CONTROL_SQL_INSTANCE
// names a Cloud SQL instance and connects as the runtime's own Google identity:
// no password is configured, stored, or rotated, because there is not one.
// MOQOM_CONTROL_DATABASE_URL is the ordinary libpq route, for a Postgres that is
// not Cloud SQL and for a developer running against a proxy.
func databaseConfig() (postgres.Config, error) {
	instance := os.Getenv("MOQOM_CONTROL_SQL_INSTANCE")
	if instance == "" {
		dsn := os.Getenv("MOQOM_CONTROL_DATABASE_URL")
		if dsn == "" {
			return postgres.Config{}, errors.New(
				"the postgres store needs MOQOM_CONTROL_SQL_INSTANCE (Cloud SQL, IAM auth) " +
					"or MOQOM_CONTROL_DATABASE_URL (anything else)")
		}
		return postgres.Config{DSN: dsn}, nil
	}

	// Cloud SQL derives the Postgres role from a service account's email by
	// stripping `.gserviceaccount.com`, and connecting as the full email fails
	// with "password authentication failed" for an account that has no password.
	// The deployment injects the truncated form; saying so here is cheaper than
	// debugging it there.
	user := os.Getenv("PGUSER")
	if user == "" {
		return postgres.Config{}, errors.New(
			"MOQOM_CONTROL_SQL_INSTANCE needs PGUSER: the service account email " +
				"with .gserviceaccount.com removed")
	}

	return postgres.Config{
		DSN:      fmt.Sprintf("user=%s dbname=%s sslmode=disable", user, env("PGDATABASE", "moqom")),
		Instance: instance,
		// The instance has no public address, so this is not a preference.
		PrivateIP: env("MOQOM_CONTROL_SQL_PRIVATE_IP", "true") == "true",
	}, nil
	// sslmode=disable is not a downgrade: the connector dials a socket it has
	// already wrapped in mutual TLS against the instance's own certificate, and
	// asking pgx to negotiate TLS again inside that would fail.
}

var capabilityByName = map[string]control.Capability{
	"delete_message":      control.CapDeleteMessage,
	"timeout_user":        control.CapTimeoutUser,
	"ban_from_chat":       control.CapBanFromChat,
	"set_chat_mode":       control.CapSetChatMode,
	"manage_filters":      control.CapManageFilters,
	"promote_to_guest":    control.CapPromoteToGuest,
	"promote_to_cohost":   control.CapPromoteToCohost,
	"demote":              control.CapDemote,
	"force_mute":          control.CapForceMute,
	"spotlight":           control.CapSpotlight,
	"manage_queue":        control.CapManageQueue,
	"lock_stage":          control.CapLockStage,
	"kick":                control.CapKick,
	"ban_from_room":       control.CapBanFromRoom,
	"ban_from_tenant":     control.CapBanFromTenant,
	"close_room":          control.CapCloseRoom,
	"change_room_policy":  control.CapChangeRoomPolicy,
	"manage_watchdogs":    control.CapManageWatchdogs,
	"appoint_moderators":  control.CapAppointModerators,
	"read_moderation_log": control.CapReadModerationLog,
	"handle_appeals":      control.CapHandleAppeals,
}

var publishByName = map[string]control.PublishScope{
	"audio":        control.PublishAudio,
	"video":        control.PublishVideo,
	"screen_share": control.PublishScreenShare,
	"data":         control.PublishData,
}
