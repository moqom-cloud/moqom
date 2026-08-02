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
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/moqom-cloud/moqom/go/internal/control"
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
	accounts, err := serviceAccounts()
	if err != nil {
		return err
	}
	// One clock for the process, disciplined by the host's NTP daemon and
	// honest about how well that is going. See internal/timecode.
	clock := timecode.NewHost()
	reportClockHealth(clock)

	backing, err := backingStore(func() time.Time { return clock.Now().At })
	if err != nil {
		return err
	}

	instance, err := server.New(server.Config{
		Store:    backing,
		Accounts: accounts,
		Issuer:   issuer,
		Keys:     keys,
		Clock:    clock,
	})
	if err != nil {
		return err
	}

	unary, streaming := instance.Interceptors()
	grpcServer := grpc.NewServer(
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
	)
	instance.Register(grpcServer)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	// No TLS here, deliberately: this expects to sit behind a terminator that has the
	// certificate -- Cloud Run, or an internal load balancer. Exposing this port
	// directly to anything but localhost or a mesh is a misconfiguration, and it is
	// worth saying so in the log that an operator will read while doing it.
	//
	// The address logged is the one the socket actually has, not the one that was
	// requested. They differ whenever the port is 0, which is how a test finds the
	// server it just started without racing to reserve a port first.
	slog.Info("control plane listening",
		"address", listener.Addr().String(), "issuer", issuer,
		"keys", len(keys), "note", "expects TLS termination in front")

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

	stopped := make(chan error, 1)
	go func() { stopped <- grpcServer.Serve(listener) }()

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
		// Graceful, so relays see a clean close and reattach rather than timing out.
		// Their grace period covers the gap, which is the whole reason it exists.
		grpcServer.GracefulStop()
		return nil
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// signingKeys reads the token signing keys.
//
// MOQOM_CONTROL_SIGNING_KEYS is a comma-separated list of base64url Ed25519 seeds,
// newest first: the first signs, all of them verify. That ordering is what makes
// rotation a deployment rather than an outage.
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
		jwk, err := generated.PublicJWK()
		if err != nil {
			return nil, err
		}
		slog.Warn("no signing key configured, generated an ephemeral one",
			"key", generated.ID, "jwk", jwk,
			"consequence", "every token stops verifying when this process restarts")
		return []token.Key{generated}, nil
	}

	var keys []token.Key
	for _, encoded := range strings.Split(raw, ",") {
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			continue
		}
		seed, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
		if err != nil {
			return nil, fmt.Errorf("signing key is not base64url: %w", err)
		}
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("signing key is %d bytes, want a %d-byte seed", len(seed), ed25519.SeedSize)
		}
		private := ed25519.NewKeyFromSeed(seed)
		// The identifier is derived from the public half rather than configured, so a
		// key cannot be published twice under different names as if it were two.
		derived, err := token.KeyFrom(private)
		if err != nil {
			return nil, err
		}
		keys = append(keys, derived)
	}
	if len(keys) == 0 {
		return nil, errors.New("MOQOM_CONTROL_SIGNING_KEYS was set but held no keys")
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
func serviceAccounts() (server.Accounts, error) {
	path := os.Getenv("MOQOM_CONTROL_ACCOUNTS")
	if path == "" {
		return nil, errors.New("MOQOM_CONTROL_ACCOUNTS is required: a path to the service account file")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading service accounts: %w", err)
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
		} `json:"accounts"`
	}
	if err := json.Unmarshal(contents, &file); err != nil {
		return nil, fmt.Errorf("parsing service accounts: %w", err)
	}

	byCredential := map[string]server.ServiceAccount{}
	for _, entry := range file.Accounts {
		if entry.Name == "" || entry.Credential == "" {
			return nil, errors.New("every service account needs a name and a credential")
		}
		grants := control.Grants{IsOwner: entry.IsOwner}
		if entry.Preset != "" {
			expanded, ok := control.Preset(entry.Preset).Expand()
			if !ok {
				return nil, fmt.Errorf("account %s names an unknown preset %q", entry.Name, entry.Preset)
			}
			grants.Moderation = expanded
		}
		for _, name := range entry.Moderation {
			capability, ok := capabilityByName[name]
			if !ok {
				return nil, fmt.Errorf("account %s names an unknown capability %q", entry.Name, name)
			}
			grants.Moderation |= capability
		}
		for _, name := range entry.Publish {
			scope, ok := publishByName[name]
			if !ok {
				return nil, fmt.Errorf("account %s names an unknown publish scope %q", entry.Name, name)
			}
			grants.Publish |= scope
		}
		byCredential[entry.Credential] = server.ServiceAccount{
			Name:    entry.Name,
			Tenant:  entry.Tenant,
			Grants:  grants,
			IsRelay: entry.IsRelay,
		}
	}
	if len(byCredential) == 0 {
		return nil, errors.New("the service account file holds no accounts")
	}

	slog.Info("service accounts loaded", "count", len(byCredential))
	return server.NewStaticAccounts(byCredential), nil
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
