package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/moqom-cloud/moqom/go/internal/billing"
	"github.com/moqom-cloud/moqom/go/internal/byoc"
	byocpg "github.com/moqom-cloud/moqom/go/internal/byoc/postgres"
	"github.com/moqom-cloud/moqom/go/internal/gceidentity"
	"github.com/moqom-cloud/moqom/go/internal/listen"
	"github.com/moqom-cloud/moqom/go/internal/overlay"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/postgres"
)

// overlayConfig turns on the overlay's management plane when the deployment
// names its management API, and returns nil otherwise.
//
// MOQOM_CONTROL_ZITI_MANAGEMENT_URL is the controller's management API, which
// the control plane logs in to with the identity it binds its services as.
// MOQOM_CONTROL_ZITI_MANAGEMENT_SERVER_NAME is the name its certificate is
// checked against, for an address the certificate does not carry.
//
// Managed relays are minted identities on RelayEnrollPath once
// MOQOM_CONTROL_RELAY_ENROLL_AUDIENCE is set, with MOQOM_CONTROL_RELAY_PROJECT,
// MOQOM_CONTROL_RELAY_INSTANCE_PREFIX and MOQOM_CONTROL_RELAY_SERVICE_ACCOUNTS
// saying which VMs are the fleet. MOQOM_CONTROL_RELAY_OVERLAY_ONLY refuses
// RelayLink over anything but the overlay.
func overlayConfig(backing store.Store) (*server.OverlayConfig, error) {
	managementURL := strings.TrimSpace(os.Getenv("MOQOM_CONTROL_ZITI_MANAGEMENT_URL"))
	if managementURL == "" {
		if envFlag("MOQOM_CONTROL_RELAY_OVERLAY_ONLY") {
			return nil, errors.New("MOQOM_CONTROL_RELAY_OVERLAY_ONLY needs the overlay: set MOQOM_CONTROL_ZITI_MANAGEMENT_URL")
		}
		return nil, nil
	}
	identityFile := os.Getenv(listen.ZitiIdentityVariable)
	if identityFile == "" {
		return nil, fmt.Errorf("MOQOM_CONTROL_ZITI_MANAGEMENT_URL needs %s", listen.ZitiIdentityVariable)
	}
	identity, err := overlay.ReadIdentityFile(identityFile)
	if err != nil {
		return nil, err
	}
	client, err := overlay.NewClient(overlay.Config{
		URL:        managementURL,
		ServerName: os.Getenv("MOQOM_CONTROL_ZITI_MANAGEMENT_SERVER_NAME"),
		Identity:   identity,
	})
	if err != nil {
		return nil, err
	}
	config := &server.OverlayConfig{
		Identities:        client,
		CustomerPrefix:    env("MOQOM_CONTROL_BYOC_PREFIX", "moqom-byoc-"),
		RelaysOverlayOnly: envFlag("MOQOM_CONTROL_RELAY_OVERLAY_ONLY"),
	}

	if subject, ok := backing.(*postgres.Store); ok {
		durable, err := byocpg.New(context.Background(), subject.Pool())
		if err != nil {
			return nil, err
		}
		config.Enrollments = durable
	} else {
		slog.Warn("customer relay enrolments in memory", "consequence", "they are lost on restart, and their identities keep nothing")
		config.Enrollments = byoc.NewMemory()
	}

	if audience := strings.TrimSpace(os.Getenv("MOQOM_CONTROL_RELAY_ENROLL_AUDIENCE")); audience != "" {
		policy := gceidentity.Policy{
			Audience:        audience,
			Project:         os.Getenv("MOQOM_CONTROL_RELAY_PROJECT"),
			NamePrefix:      os.Getenv("MOQOM_CONTROL_RELAY_INSTANCE_PREFIX"),
			ServiceAccounts: billing.ParseAllowList(os.Getenv("MOQOM_CONTROL_RELAY_SERVICE_ACCOUNTS")),
			Keys:            &billing.RemoteKeys{URL: billing.GoogleCertsURL},
		}
		if policy.Project == "" || policy.NamePrefix == "" || len(policy.ServiceAccounts) == 0 {
			return nil, errors.New("MOQOM_CONTROL_RELAY_ENROLL_AUDIENCE needs MOQOM_CONTROL_RELAY_PROJECT, MOQOM_CONTROL_RELAY_INSTANCE_PREFIX and MOQOM_CONTROL_RELAY_SERVICE_ACCOUNTS")
		}
		config.Instances = policy
	}
	slog.Info("overlay management plane on",
		"customer relays", config.Enrollments != nil, "managed relay enrolment", config.Instances != nil,
		"relays overlay only", config.RelaysOverlayOnly)
	return config, nil
}

// overlayAPI serves the overlay's API service: the admin API, RelayLink and
// customer relay enrolments, on its own gRPC server with no TLS of its own.
//
// No TLS because the overlay's circuit is already end-to-end encrypted
// between two authenticated identities, and the callers here -- customers'
// relays and backends -- would otherwise need this deployment's CA and a
// client certificate as well as an overlay identity. Nothing reaches this
// server except through the overlay; it binds no port.
func overlayAPI(instance *server.Server) (*grpc.Server, net.Listener, error) {
	service := strings.TrimSpace(os.Getenv("MOQOM_CONTROL_ZITI_API_SERVICE"))
	if service == "" {
		return nil, nil, nil
	}
	identity := os.Getenv(listen.ZitiIdentityVariable)
	if identity == "" {
		return nil, nil, fmt.Errorf("MOQOM_CONTROL_ZITI_API_SERVICE needs %s", listen.ZitiIdentityVariable)
	}
	listener, err := listen.Open(context.Background(), listen.Spec{
		Kind: listen.KindZiti, ZitiIdentity: identity, ZitiService: service,
	})
	if err != nil {
		return nil, nil, err
	}
	unary, streaming := instance.OverlayInterceptors()
	api := grpc.NewServer(
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(streaming),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 15 * time.Second, PermitWithoutStream: true}),
	)
	instance.Register(api)
	instance.RegisterOverlay(api)
	slog.Info("control plane listening", "overlay", "ziti service "+service, "serves", "admin API, relay link, relay enrolments")
	return api, listener, nil
}

// reconcileRelays deletes managed relays' overlay identities once their VMs
// are gone, every few minutes. Every replica runs it; deleting an identity
// twice is not an error.
func reconcileRelays(ctx context.Context, instance *server.Server) {
	project := os.Getenv("MOQOM_CONTROL_RELAY_PROJECT")
	prefix := os.Getenv("MOQOM_CONTROL_RELAY_INSTANCE_PREFIX")
	if project == "" || prefix == "" || os.Getenv("MOQOM_CONTROL_RELAY_ENROLL_AUDIENCE") == "" {
		return
	}
	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/compute.readonly")
	if err != nil {
		slog.Error("relay identity reconciler off", "error", err)
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		live, err := gceidentity.Instances(ctx, client, "", project, prefix)
		if err != nil {
			slog.Warn("could not list relay VMs; deleting nothing this round", "error", err)
		} else if deleted, err := instance.ReconcileRelayIdentities(ctx, live); err != nil || len(deleted) > 0 {
			slog.Info("relay identities reconciled", "live", len(live), "deleted", deleted, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
