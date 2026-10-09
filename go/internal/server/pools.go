package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// Private relay pools: where a tenant's network is decided, and where the
// decision is written into every token minted for it.
//
// The pool is enforced by the relays, offline, from the token's `pool` claim,
// so the control plane's part is to put the claim in every token a private
// tenant is issued -- a participant's, a recorder's, a relay link's -- and to
// refuse to issue one rather than issue it without. There is no fallback to the
// shared fleet anywhere in this file, on purpose: a pool with no relays yet is a
// tenant told so, not a tenant quietly put somewhere it asked not to be.
//
// Provisioning is not here. A pool MOQOM runs is recorded as provisioning, and
// becomes ready when as many relays as each region asked for have attached
// naming it; what starts those relays is outside the control plane.

// errPoolSealPending is a private tenant whose relays' seal key this replica
// cannot yet confirm is in force. Retryable: it clears at the next refresh.
var errPoolSealPending = errors.New("the tenant's private relay pool is not yet sealed for; try again shortly")

// poolTenants names the tenants with a relay pool to the sealer: a pool's
// relays hold their tenant's seal key alone, so its tokens must be sealed with it.
type poolTenants struct {
	store interface {
		PoolTenants(ctx context.Context) ([]string, error)
	}
}

func (p poolTenants) ActiveTenants(ctx context.Context) ([]string, error) {
	return p.store.PoolTenants(ctx)
}

type relayPoolsService struct {
	adminpb.UnimplementedRelayPoolsServer
	core *core
}

// requirePoolOwner is who may change where a tenant's media goes: an account
// with the owner ceiling on that tenant, or a platform account.
func requirePoolOwner(ctx context.Context, tenant string) (ServiceAccount, error) {
	account, err := requireTenant(ctx, tenant)
	if err != nil {
		return ServiceAccount{}, err
	}
	if account.Tenant != "" && !account.Grants.IsOwner {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "relay pools need an account with the owner ceiling")
	}
	return account, nil
}

func (s *relayPoolsService) CreateRelayPool(ctx context.Context, request *adminpb.CreateRelayPoolRequest) (*adminpb.RelayPool, error) {
	account, err := requirePoolOwner(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.GetName())
	if name == "" || len(name) > 100 {
		return nil, status.Error(codes.InvalidArgument, "a relay pool needs a name of at most 100 characters")
	}
	regions, err := control.ValidatePoolRegions(poolRegionsFromProto(request.GetRegions()), request.GetSelfHosted())
	if err != nil {
		return nil, statusFor(err)
	}
	pool := control.RelayPool{
		ID:         s.core.ids(),
		Tenant:     request.GetTenant(),
		Name:       name,
		Regions:    regions,
		SelfHosted: request.GetSelfHosted(),
		Access:     control.PoolAccessPublic,
		State:      control.PoolProvisioning,
		CreatedAt:  s.core.now(),
	}
	// A self-hosted pool's relays are whatever the tenant enrols, whenever it
	// does; there is nothing for it to wait for.
	if pool.SelfHosted {
		pool.State = control.PoolReady
	}
	created, err := s.core.store.CreateRelayPool(ctx, pool)
	if err != nil {
		return nil, statusFor(err)
	}
	slog.InfoContext(ctx, "relay pool created", "tenant", created.Tenant, "pool", created.ID,
		"self_hosted", created.SelfHosted, "by", account.Name)
	// This replica seals for the tenant from now rather than at the next
	// refresh, so the pool's first relays can open the first tokens minted.
	s.core.refreshSealing(ctx)
	return poolToProto(created), nil
}

func (s *relayPoolsService) GetRelayPool(ctx context.Context, request *adminpb.GetRelayPoolRequest) (*adminpb.RelayPool, error) {
	if _, err := requirePoolOwner(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	pool, err := s.pool(ctx, request.GetTenant(), request.GetPoolId())
	if err != nil {
		return nil, err
	}
	return poolToProto(pool), nil
}

func (s *relayPoolsService) ListRelayPools(ctx context.Context, request *adminpb.ListRelayPoolsRequest) (*adminpb.ListRelayPoolsResponse, error) {
	if _, err := requirePoolOwner(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	pools, err := s.core.store.ListRelayPools(ctx, request.GetTenant())
	if err != nil {
		return nil, statusFor(err)
	}
	response := &adminpb.ListRelayPoolsResponse{}
	for _, pool := range pools {
		response.Pools = append(response.Pools, poolToProto(pool))
	}
	return response, nil
}

func (s *relayPoolsService) ResizeRelayPool(ctx context.Context, request *adminpb.ResizeRelayPoolRequest) (*adminpb.RelayPool, error) {
	if _, err := requirePoolOwner(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	if request.GetRegion() == nil {
		return nil, status.Error(codes.InvalidArgument, "region is required")
	}
	pool, err := s.livePool(ctx, request.GetTenant(), request.GetPoolId())
	if err != nil {
		return nil, err
	}
	region, err := control.ValidatePoolRegion(poolRegionFromProto(request.GetRegion()), pool.SelfHosted)
	if err != nil {
		return nil, statusFor(err)
	}
	saved, err := s.core.store.SaveRelayPool(ctx, pool.Resized(region))
	if err != nil {
		return nil, statusFor(err)
	}
	return poolToProto(saved), nil
}

func (s *relayPoolsService) DeleteRelayPool(ctx context.Context, request *adminpb.DeleteRelayPoolRequest) (*adminpb.RelayPool, error) {
	account, err := requirePoolOwner(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	pool, err := s.pool(ctx, request.GetTenant(), request.GetPoolId())
	if err != nil {
		return nil, err
	}
	if !pool.Live() {
		return poolToProto(pool), nil
	}
	network, err := s.core.store.GetTenantNetwork(ctx, request.GetTenant())
	if err != nil {
		return nil, statusFor(err)
	}
	if network.Private() && network.Pool == pool.ID {
		return nil, status.Errorf(codes.FailedPrecondition,
			"the tenant's network is private on pool %s; set it to shared, or to another pool, before deleting this one", pool.ID)
	}
	pool.State = control.PoolDeleted
	saved, err := s.core.store.SaveRelayPool(ctx, pool)
	if err != nil {
		return nil, statusFor(err)
	}
	slog.InfoContext(ctx, "relay pool deleted", "tenant", saved.Tenant, "pool", saved.ID, "by", account.Name)
	return poolToProto(saved), nil
}

func (s *relayPoolsService) SetRelayPoolAccess(ctx context.Context, request *adminpb.SetRelayPoolAccessRequest) (*adminpb.RelayPool, error) {
	if _, err := requirePoolOwner(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	access, ok := poolAccessFromProto[request.GetAccess()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "access must be PUBLIC, PRIVATE_SERVICE_CONNECT or OVERLAY")
	}
	pool, err := s.livePool(ctx, request.GetTenant(), request.GetPoolId())
	if err != nil {
		return nil, err
	}
	pool.Access = access
	saved, err := s.core.store.SaveRelayPool(ctx, pool)
	if err != nil {
		return nil, statusFor(err)
	}
	return poolToProto(saved), nil
}

func (s *relayPoolsService) SetTenantNetwork(ctx context.Context, request *adminpb.SetTenantNetworkRequest) (*adminpb.TenantNetwork, error) {
	account, err := requirePoolOwner(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	wire := request.GetNetwork()
	network := control.TenantNetwork{Tenant: request.GetTenant()}
	switch wire.GetMode() {
	case adminpb.NetworkMode_NETWORK_MODE_SHARED:
		network.Mode = control.NetworkShared
	case adminpb.NetworkMode_NETWORK_MODE_PRIVATE:
		if wire.GetPool() == "" {
			return nil, status.Error(codes.InvalidArgument, "a private network names its pool")
		}
		if _, err := s.livePool(ctx, request.GetTenant(), wire.GetPool()); err != nil {
			return nil, err
		}
		network.Mode, network.Pool = control.NetworkPrivate, wire.GetPool()
	default:
		return nil, status.Error(codes.InvalidArgument, "mode must be SHARED or PRIVATE")
	}
	saved, err := s.core.store.SetTenantNetwork(ctx, network)
	if err != nil {
		return nil, statusFor(err)
	}
	slog.InfoContext(ctx, "tenant network set", "tenant", saved.Tenant, "mode", saved.Mode, "pool", saved.Pool, "by", account.Name)
	return networkToProto(saved), nil
}

func (s *relayPoolsService) GetTenantNetwork(ctx context.Context, request *adminpb.GetTenantNetworkRequest) (*adminpb.TenantNetwork, error) {
	if _, err := requirePoolOwner(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	network, err := s.core.store.GetTenantNetwork(ctx, request.GetTenant())
	if err != nil {
		return nil, statusFor(err)
	}
	return networkToProto(network), nil
}

func (s *relayPoolsService) pool(ctx context.Context, tenant, id string) (control.RelayPool, error) {
	if id == "" {
		return control.RelayPool{}, status.Error(codes.InvalidArgument, "pool_id is required")
	}
	pool, err := s.core.store.GetRelayPool(ctx, tenant, id)
	if err != nil {
		return control.RelayPool{}, statusFor(err)
	}
	return pool, nil
}

// livePool is a pool that may still be changed or used.
func (s *relayPoolsService) livePool(ctx context.Context, tenant, id string) (control.RelayPool, error) {
	pool, err := s.pool(ctx, tenant, id)
	if err != nil {
		return control.RelayPool{}, err
	}
	if !pool.Live() {
		return control.RelayPool{}, status.Errorf(codes.FailedPrecondition, "pool %s has been deleted", id)
	}
	return pool, nil
}

// confine names the tenant's pool in claims if the tenant is private, and
// returns the addresses a token for it must dial. Every token minted for a
// tenant passes through here before it is signed.
//
// A private tenant whose pool cannot take anybody is refused, never handed a
// shared-fleet token instead: that would be exactly the traffic the tenant
// asked never to touch a shared relay.
func (c *core) confine(ctx context.Context, claims *token.Claims) ([]string, error) {
	network, err := c.store.GetTenantNetwork(ctx, claims.Tenant)
	if err != nil {
		return nil, err
	}
	if !network.Private() {
		return nil, nil
	}
	pool, err := c.store.GetRelayPool(ctx, claims.Tenant, network.Pool)
	if errors.Is(err, control.ErrNotFound) {
		return nil, control.ErrPoolNotReady
	}
	if err != nil {
		return nil, err
	}
	addresses := pool.Addresses()
	if pool.State != control.PoolReady || len(addresses) == 0 {
		return nil, control.ErrPoolNotReady
	}
	if c.sealer.enabled() && !c.sealer.state.Load().own[claims.Tenant] {
		c.refreshSealing(ctx)
		if !c.sealer.state.Load().own[claims.Tenant] {
			return nil, errPoolSealPending
		}
	}
	claims.Pool = pool.ID
	return addresses, nil
}

// notePoolRelay records a pool relay as it attaches, and marks its pool ready
// once every region has as many as it asked for.
//
// Best effort, and logged rather than fatal: a relay that cannot be recorded
// still serves its pool, it is only missing from the addresses handed out.
func (c *core) notePoolRelay(ctx context.Context, account ServiceAccount, relayID string, hello *relaypb.Hello) {
	if account.Pool == "" || account.Tenant == "" {
		return
	}
	pool, err := c.store.GetRelayPool(ctx, account.Tenant, account.Pool)
	if err != nil || !pool.Live() {
		slog.WarnContext(ctx, "a relay attached naming a pool its tenant does not have",
			"relay", relayID, "tenant", account.Tenant, "pool", account.Pool, "error", err)
		return
	}
	address := strings.TrimSpace(hello.GetAddress())
	if address == "" {
		slog.WarnContext(ctx, "a pool relay attached without an address; clients will not be sent to it",
			"relay", relayID, "pool", pool.ID)
		return
	}
	if err := c.store.PutPoolRelay(ctx, pool.Tenant, pool.ID, control.PoolRelay{
		RelayID: relayID, Address: address, Region: hello.GetRegion(),
		State: control.PoolRelayReady, SeenAt: c.now(),
	}); err != nil {
		slog.WarnContext(ctx, "could not record a pool relay", "relay", relayID, "pool", pool.ID, "error", err)
		return
	}
	if pool.State != control.PoolProvisioning {
		return
	}
	pool, err = c.store.GetRelayPool(ctx, pool.Tenant, pool.ID)
	if err != nil || !pool.Provisioned() {
		return
	}
	pool.State = control.PoolReady
	if _, err := c.store.SaveRelayPool(ctx, pool); err != nil {
		slog.WarnContext(ctx, "could not mark a pool ready", "pool", pool.ID, "error", err)
		return
	}
	slog.InfoContext(ctx, "relay pool ready", "tenant", pool.Tenant, "pool", pool.ID)
}

var (
	poolAccessFromProto = map[adminpb.PoolAccess]control.PoolAccess{
		adminpb.PoolAccess_POOL_ACCESS_PUBLIC:                  control.PoolAccessPublic,
		adminpb.PoolAccess_POOL_ACCESS_PRIVATE_SERVICE_CONNECT: control.PoolAccessPrivateServiceConnect,
		adminpb.PoolAccess_POOL_ACCESS_OVERLAY:                 control.PoolAccessOverlay,
	}
	poolAccessToProto = map[control.PoolAccess]adminpb.PoolAccess{
		control.PoolAccessPublic:                adminpb.PoolAccess_POOL_ACCESS_PUBLIC,
		control.PoolAccessPrivateServiceConnect: adminpb.PoolAccess_POOL_ACCESS_PRIVATE_SERVICE_CONNECT,
		control.PoolAccessOverlay:               adminpb.PoolAccess_POOL_ACCESS_OVERLAY,
	}
	poolStateToProto = map[control.PoolState]adminpb.PoolState{
		control.PoolProvisioning: adminpb.PoolState_POOL_STATE_PROVISIONING,
		control.PoolReady:        adminpb.PoolState_POOL_STATE_READY,
		control.PoolDraining:     adminpb.PoolState_POOL_STATE_DRAINING,
		control.PoolDeleted:      adminpb.PoolState_POOL_STATE_DELETED,
	}
	poolRelayStateToProto = map[control.PoolRelayState]adminpb.PoolRelayState{
		control.PoolRelayStarting: adminpb.PoolRelayState_POOL_RELAY_STATE_STARTING,
		control.PoolRelayReady:    adminpb.PoolRelayState_POOL_RELAY_STATE_READY,
		control.PoolRelayDraining: adminpb.PoolRelayState_POOL_RELAY_STATE_DRAINING,
	}
)

func poolRegionFromProto(wire *adminpb.PoolRegion) control.PoolRegion {
	return control.PoolRegion{Region: wire.GetRegion(), Relays: wire.GetRelays(), MaxRelays: wire.GetMaxRelays()}
}

func poolRegionsFromProto(wire []*adminpb.PoolRegion) []control.PoolRegion {
	regions := make([]control.PoolRegion, 0, len(wire))
	for _, region := range wire {
		regions = append(regions, poolRegionFromProto(region))
	}
	return regions
}

func poolToProto(pool control.RelayPool) *adminpb.RelayPool {
	wire := &adminpb.RelayPool{
		Id:         pool.ID,
		Tenant:     pool.Tenant,
		Name:       pool.Name,
		SelfHosted: pool.SelfHosted,
		Access:     poolAccessToProto[pool.Access],
		State:      poolStateToProto[pool.State],
		CreatedAt:  timeToProto(pool.CreatedAt),
	}
	for _, region := range pool.Regions {
		wire.Regions = append(wire.Regions, &adminpb.PoolRegion{
			Region: region.Region, Relays: region.Relays, MaxRelays: region.MaxRelays,
		})
	}
	for _, relay := range pool.Relays {
		wire.Relays = append(wire.Relays, &adminpb.PoolRelay{
			Address: relay.Address, Region: relay.Region, State: poolRelayStateToProto[relay.State],
		})
	}
	return wire
}

func networkToProto(network control.TenantNetwork) *adminpb.TenantNetwork {
	mode := adminpb.NetworkMode_NETWORK_MODE_SHARED
	if network.Private() {
		mode = adminpb.NetworkMode_NETWORK_MODE_PRIVATE
	}
	return &adminpb.TenantNetwork{Pool: network.Pool, Mode: mode}
}
