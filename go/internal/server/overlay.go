package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/moqom-cloud/moqom/go/internal/byoc"
	"github.com/moqom-cloud/moqom/go/internal/byocpb"
	"github.com/moqom-cloud/moqom/go/internal/gceidentity"
	"github.com/moqom-cloud/moqom/go/internal/listen"
	"github.com/moqom-cloud/moqom/go/internal/overlay"
)

// The management plane on the OpenZiti overlay: who a connection is when it
// arrives without a port, and which identities the control plane mints.
//
// Two kinds of relay reach the control plane over the overlay. A managed relay
// is one of MOQOM's own VMs: it still presents its relay credential and mutual
// TLS, and the overlay only replaces the internal TCP path. Its identity is
// minted here at boot, against a token only that VM can produce, and deleted
// when the VM is gone. A customer's relay presents nothing but its overlay
// identity, which an enrolment in byoc.Store maps to one tenant.

// OverlayIdentities is the part of the overlay's management API the control
// plane uses. *overlay.Client implements it.
type OverlayIdentities interface {
	Create(ctx context.Context, asked overlay.NewIdentity) (overlay.Enrolment, error)
	Find(ctx context.Context, name string) (overlay.Identity, error)
	WithRole(ctx context.Context, role string) ([]overlay.Identity, error)
	Delete(ctx context.Context, id string) error
}

// InstanceVerifier checks a managed relay VM's instance identity token.
type InstanceVerifier interface {
	Verify(ctx context.Context, raw string) (gceidentity.Instance, error)
}

// OverlayConfig turns on the overlay's management plane.
type OverlayConfig struct {
	Identities OverlayIdentities
	// Enrollments, if set, turns on customer relays.
	Enrollments byoc.Store
	// Instances, if set, turns on RelayEnrollPath for managed relays.
	Instances InstanceVerifier
	// CustomerPrefix begins every customer relay identity's name, so an
	// operator reading the overlay's identity list can tell them apart.
	CustomerPrefix string
	// CustomerTokenLifetime is how long a customer's one-time token lasts.
	CustomerTokenLifetime time.Duration
	// RelaysOverlayOnly refuses RelayLink unless it arrived over the overlay.
	RelaysOverlayOnly bool
}

// Overlay role attributes. The overlay's service policies are written against
// these; see deploy/tools/ziti-provision.sh.
const (
	RoleManagedRelay   = "moqom-relay"
	RoleMintedRelay    = "moqom-relay-managed"
	RoleCustomerRelay  = "moqom-byoc-relay"
	customerRelayIDTag = "byoc:"
)

// RelayEnrollPath is where a managed relay VM asks for its overlay identity.
const RelayEnrollPath = "/relay/enroll"

const defaultCustomerTokenLifetime = 24 * time.Hour

// customerRelayCheck is how often a customer relay's stream rechecks its
// enrolment, which is what cuts it on a replica other than the one that
// handled the revoke.
const customerRelayCheck = 30 * time.Second

func (c *OverlayConfig) customerTokenLifetime() time.Duration {
	if c.CustomerTokenLifetime > 0 {
		return c.CustomerTokenLifetime
	}
	return defaultCustomerTokenLifetime
}

// OverlayInterceptors authenticate calls on the overlay's API service.
//
// A connection from an enrolled customer relay is that relay, scoped to its
// tenant, with no credential asked for: the overlay has authenticated it, and
// the enrolment says what it may do. A revoked one is refused outright rather
// than falling through to a bearer credential, so revoking cannot be routed
// around. Anything else -- an operator, a customer backend -- authenticates
// with a bearer credential as it would over TCP.
func (s *Server) OverlayInterceptors() (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	bearer := Authenticator{Accounts: s.core.accounts}
	authenticate := func(ctx context.Context) (context.Context, error) {
		account, found, err := s.core.customerRelay(ctx)
		if err != nil {
			return nil, err
		}
		if found {
			return WithPrincipal(ctx, account), nil
		}
		return bearer.authenticate(ctx)
	}
	unary := func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		authenticated, err := authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return handler(authenticated, request)
	}
	stream := func(service any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authenticated, err := authenticate(ss.Context())
		if err != nil {
			return err
		}
		return handler(service, wrappedStream{ServerStream: ss, ctx: authenticated})
	}
	return unary, stream
}

// customerRelay resolves the overlay peer behind a call to a customer relay
// account, if it is one.
func (c *core) customerRelay(ctx context.Context) (ServiceAccount, bool, error) {
	if c.overlay == nil || c.overlay.Enrollments == nil {
		return ServiceAccount{}, false, nil
	}
	caller, ok := listen.PeerOf(ctx)
	if !ok {
		return ServiceAccount{}, false, nil
	}
	enrollment, err := c.overlay.Enrollments.ByIdentity(ctx, caller.Name)
	switch {
	case errors.Is(err, byoc.ErrNotFound):
		return ServiceAccount{}, false, nil
	case err != nil:
		return ServiceAccount{}, false, status.Error(codes.Unavailable, "could not check the relay's enrolment")
	case !enrollment.Active():
		return ServiceAccount{}, false, status.Error(codes.Unauthenticated, "this relay's enrolment has been revoked")
	}
	return customerAccount(enrollment), true, nil
}

func customerAccount(enrollment byoc.Enrollment) ServiceAccount {
	id := customerRelayIDTag + enrollment.IdentityName
	return ServiceAccount{
		Name:       id,
		Tenant:     enrollment.Tenant,
		IsRelay:    true,
		RelayID:    id,
		Enrollment: enrollment.ID,
		Pool:       enrollment.Pool,
	}
}

// RegisterOverlay attaches the services the overlay's API service serves
// beyond the ordinary ones: customer relay enrolments.
func (s *Server) RegisterOverlay(grpcServer grpc.ServiceRegistrar) {
	if s.core.overlay != nil && s.core.overlay.Enrollments != nil {
		byocpb.RegisterRelayEnrollmentsServer(grpcServer, &enrollmentsService{core: s.core})
	}
}

type enrollmentsService struct {
	byocpb.UnimplementedRelayEnrollmentsServer
	core *core
}

// requireEnroller is who may hand out a licence to run a relay for a tenant:
// an account with the owner ceiling on that tenant, or a platform account.
func requireEnroller(ctx context.Context, tenant string) (ServiceAccount, error) {
	account, err := requireTenant(ctx, tenant)
	if err != nil {
		return ServiceAccount{}, err
	}
	if account.Tenant != "" && !account.Grants.IsOwner {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "enrolling a relay needs an account with the owner ceiling")
	}
	return account, nil
}

func (e *enrollmentsService) Create(ctx context.Context, request *byocpb.CreateRequest) (*byocpb.CreateResponse, error) {
	account, err := requireEnroller(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.GetName())
	if name == "" || len(name) > 100 {
		return nil, status.Error(codes.InvalidArgument, "a relay needs a name of at most 100 characters")
	}
	if request.GetPool() != "" {
		pool, err := e.core.store.GetRelayPool(ctx, request.GetTenant(), request.GetPool())
		if err != nil {
			return nil, statusFor(err)
		}
		if !pool.Live() {
			return nil, status.Errorf(codes.FailedPrecondition, "pool %s has been deleted", pool.ID)
		}
	}
	settings := e.core.overlay
	id := e.core.ids()
	identityName := settings.CustomerPrefix + request.GetTenant() + "-" + id
	created, err := settings.Identities.Create(ctx, overlay.NewIdentity{
		Name:           identityName,
		RoleAttributes: []string{RoleCustomerRelay},
		Tags:           map[string]string{"tenant": request.GetTenant(), "enrollment": id, "minted_by": "moqom-control"},
		TokenExpires:   e.core.now().Add(settings.customerTokenLifetime()),
	})
	if err != nil {
		slog.ErrorContext(ctx, "could not mint a customer relay identity", "tenant", request.GetTenant(), "error", err)
		return nil, status.Error(codes.Unavailable, "could not create the overlay identity")
	}
	enrollment := byoc.Enrollment{
		ID: id, Tenant: request.GetTenant(), Name: name, Pool: request.GetPool(),
		IdentityID: created.ID, IdentityName: created.Name,
		CreatedBy: account.Name, CreatedAt: e.core.now(),
	}
	if err := settings.Enrollments.Create(ctx, enrollment); err != nil {
		// Not left behind: an identity with no enrolment would be a relay
		// the overlay admits and the control plane does not recognise.
		_ = settings.Identities.Delete(context.WithoutCancel(ctx), created.ID)
		return nil, statusFor(err)
	}
	slog.InfoContext(ctx, "customer relay enrolled", "tenant", enrollment.Tenant,
		"enrollment", enrollment.ID, "identity", enrollment.IdentityName, "by", account.Name)
	return &byocpb.CreateResponse{
		Enrollment:      enrollmentToProto(enrollment),
		EnrollmentToken: created.JWT,
		TokenExpiresAt:  timestamppb.New(created.ExpiresAt),
	}, nil
}

func (e *enrollmentsService) List(ctx context.Context, request *byocpb.ListRequest) (*byocpb.ListResponse, error) {
	if _, err := requireEnroller(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	listed, err := e.core.overlay.Enrollments.List(ctx, request.GetTenant())
	if err != nil {
		return nil, statusFor(err)
	}
	response := &byocpb.ListResponse{}
	for _, enrollment := range listed {
		response.Enrollments = append(response.Enrollments, enrollmentToProto(enrollment))
	}
	return response, nil
}

func (e *enrollmentsService) Revoke(ctx context.Context, request *byocpb.RevokeRequest) (*byocpb.RevokeResponse, error) {
	account, err := requireEnroller(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	settings := e.core.overlay
	existing, err := settings.Enrollments.Get(ctx, request.GetId())
	if errors.Is(err, byoc.ErrNotFound) || (err == nil && existing.Tenant != request.GetTenant()) {
		return nil, status.Error(codes.NotFound, "no such enrolment")
	}
	if err != nil {
		return nil, statusFor(err)
	}
	// The record first: from this moment the control plane refuses the
	// identity whatever the overlay does.
	revoked, err := settings.Enrollments.Revoke(ctx, existing.ID, e.core.now())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := settings.Identities.Delete(ctx, existing.IdentityID); err != nil {
		slog.ErrorContext(ctx, "could not delete a revoked relay's overlay identity",
			"enrollment", existing.ID, "identity", existing.IdentityName, "error", err)
		return nil, status.Error(codes.Unavailable, "revoked, but the overlay identity is not yet deleted; revoke again")
	}
	cut := e.core.relays.cut(customerAccount(existing).RelayID)
	slog.InfoContext(ctx, "customer relay revoked", "tenant", existing.Tenant,
		"enrollment", existing.ID, "identity", existing.IdentityName, "by", account.Name, "stream cut", cut)
	return &byocpb.RevokeResponse{Enrollment: enrollmentToProto(revoked)}, nil
}

func enrollmentToProto(e byoc.Enrollment) *byocpb.Enrollment {
	wire := &byocpb.Enrollment{
		Id: e.ID, Tenant: e.Tenant, Name: e.Name, IdentityName: e.IdentityName, Pool: e.Pool,
		CreatedBy: e.CreatedBy, CreatedAt: timestamppb.New(e.CreatedAt),
	}
	if !e.RevokedAt.IsZero() {
		wire.RevokedAt = timestamppb.New(e.RevokedAt)
	}
	return wire
}

// RelayEnrollRequest is what a managed relay VM sends to RelayEnrollPath.
type RelayEnrollRequest struct {
	// InstanceToken is the VM's identity token, "full" format, minted for the
	// deployment's audience.
	InstanceToken string `json:"instance_token"`
}

// RelayEnrollResponse is its overlay identity's one-time token.
type RelayEnrollResponse struct {
	Identity        string    `json:"identity"`
	EnrollmentToken string    `json:"enrollment_token"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// relayEnroll mints a managed relay's overlay identity.
//
// Two proofs, and both are needed: the relay credential says this is the
// fleet, and the instance token says which VM, signed by Google. The identity
// is named after the instance, so an operator dials a relay by its VM name.
// Asking again replaces the identity, which is how a VM that lost its
// enrolled file, or a recreated VM reusing a name, gets a fresh one.
func (s *Server) relayEnroll(limiter *publicLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := s.core.overlay
		if settings == nil || settings.Instances == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
			return
		}
		account, err := s.relayAccountFor(r)
		if err != nil || account.RelayID != "" {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		var asked RelayEnrollRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*gceTokenLimit)).Decode(&asked); err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		instance, err := settings.Instances.Verify(r.Context(), asked.InstanceToken)
		if err != nil {
			slog.WarnContext(r.Context(), "relay enrolment refused", "reason", "instance token", "account", account.Name)
			http.Error(w, "the instance token is not trusted", http.StatusForbidden)
			return
		}
		if !limiter.permit("relay-enroll:" + instance.Name) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		if existing, err := settings.Identities.Find(r.Context(), instance.Name); err == nil {
			if err := settings.Identities.Delete(r.Context(), existing.ID); err != nil {
				http.Error(w, "could not replace the overlay identity", http.StatusServiceUnavailable)
				return
			}
		} else if !errors.Is(err, overlay.ErrNotFound) {
			http.Error(w, "could not reach the overlay", http.StatusServiceUnavailable)
			return
		}
		created, err := settings.Identities.Create(r.Context(), overlay.NewIdentity{
			Name:           instance.Name,
			RoleAttributes: []string{RoleManagedRelay, RoleMintedRelay},
			Tags: map[string]string{
				"instance_id": instance.ID, "zone": instance.Zone,
				"project": instance.Project, "minted_by": "moqom-control",
			},
		})
		if err != nil {
			slog.ErrorContext(r.Context(), "could not mint a relay identity", "instance", instance.Name, "error", err)
			http.Error(w, "could not create the overlay identity", http.StatusServiceUnavailable)
			return
		}
		slog.InfoContext(r.Context(), "relay identity minted", "instance", instance.Name,
			"instance_id", instance.ID, "zone", instance.Zone)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(RelayEnrollResponse{
			Identity: created.Name, EnrollmentToken: created.JWT, ExpiresAt: created.ExpiresAt,
		})
	}
}

const gceTokenLimit = 8 << 10

// ReconcileRelayIdentities deletes the overlay identity of every managed relay
// whose VM is gone. live maps each running instance's name to its ID; an
// identity whose instance ID no longer matches is a VM that was recreated
// under the same name, and goes too.
//
// An empty live set deletes nothing: a fleet with no VMs at all is far less
// likely than a listing that came back wrong, and the cost of waiting one more
// round is an identity that admits nothing.
func (s *Server) ReconcileRelayIdentities(ctx context.Context, live map[string]string) (deleted []string, err error) {
	if s.core.overlay == nil || len(live) == 0 {
		return nil, nil
	}
	minted, err := s.core.overlay.Identities.WithRole(ctx, RoleMintedRelay)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, identity := range minted {
		if id, ok := live[identity.Name]; ok && id == identity.Tags["instance_id"] {
			continue
		}
		if err := s.core.overlay.Identities.Delete(ctx, identity.ID); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", identity.Name, err))
			continue
		}
		deleted = append(deleted, identity.Name)
	}
	return deleted, errors.Join(failures...)
}
