package server_test

// A private relay pool is a promise about where packets go, so what matters
// here is every way it could be broken: a token minted without the pool, a
// token for a pool with nowhere to go, and a pool relay handed something of
// another tenant's.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	sectoken "github.com/moqom-cloud/moqom-security/go/token"
	"github.com/moqom-cloud/moqom/go/internal/byocpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

const (
	poolRelayPrefix = "pool-relay-"
	globexOwnerKey  = "globex-owner-key"
	otherTenant     = "globex"
)

// poolRelayAccounts are relay accounts of the tenant's in whichever pool the
// test created, which is not known until it has.
type poolRelayAccounts struct {
	mu   sync.Mutex
	pool string
}

func (p *poolRelayAccounts) set(pool string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pool = pool
}

func (p *poolRelayAccounts) Lookup(_ context.Context, credential string) (server.ServiceAccount, error) {
	if !strings.HasPrefix(credential, poolRelayPrefix) {
		return server.ServiceAccount{}, server.ErrNoSuchAccount
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return server.ServiceAccount{Name: credential, Tenant: tenant, IsRelay: true, Pool: p.pool}, nil
}

func newPoolHarness(t *testing.T, seal ...sectoken.SealKey) (*harness, *poolRelayAccounts) {
	t.Helper()
	relays := &poolRelayAccounts{}
	return newConfiguredHarness(t, func(config *server.Config) {
		config.SealKeys = seal
		config.Accounts = server.AnyAccounts{config.Accounts, relays, server.NewStaticAccounts(map[string]server.ServiceAccount{
			globexOwnerKey: {Name: "globex-backend", Tenant: otherTenant, Grants: control.Grants{
				Publish: control.PublishFull, Moderation: control.CapAll, IsOwner: true,
			}},
		})}
	}), relays
}

func (h *harness) createPool(t *testing.T, regions ...*adminpb.PoolRegion) *adminpb.RelayPool {
	t.Helper()
	if len(regions) == 0 {
		regions = []*adminpb.PoolRegion{{Region: "us-west1", Relays: 2}}
	}
	pool, err := h.relayPools.CreateRelayPool(as(ownerKey), &adminpb.CreateRelayPoolRequest{
		Tenant: tenant, Name: "production", Regions: regions,
	})
	if err != nil {
		t.Fatalf("creating a pool: %v", err)
	}
	return pool
}

func (h *harness) goPrivate(t *testing.T, pool string) {
	t.Helper()
	if _, err := h.relayPools.SetTenantNetwork(as(ownerKey), &adminpb.SetTenantNetworkRequest{
		Tenant: tenant, Network: &adminpb.TenantNetwork{Pool: pool, Mode: adminpb.NetworkMode_NETWORK_MODE_PRIVATE},
	}); err != nil {
		t.Fatalf("making the tenant private: %v", err)
	}
}

func (h *harness) attachPoolRelay(t *testing.T, id, region, address string) *fakeRelay {
	t.Helper()
	return h.attachRelayWith(t, poolRelayPrefix+id, &relaypb.Hello{
		RelayId: id, Region: region, Version: "0", Address: address,
	})
}

func (h *harness) awaitPoolState(t *testing.T, pool string, want adminpb.PoolState) *adminpb.RelayPool {
	t.Helper()
	var read *adminpb.RelayPool
	waitFor(t, "the pool to be "+want.String(), func() bool {
		var err error
		read, err = h.relayPools.GetRelayPool(as(ownerKey), &adminpb.GetRelayPoolRequest{Tenant: tenant, PoolId: pool})
		return err == nil && read.GetState() == want
	})
	return read
}

// readyPool is a pool with two relays attached in us-west1, and the tenant
// private on it.
func (h *harness) readyPool(t *testing.T, relays *poolRelayAccounts) (*adminpb.RelayPool, []*fakeRelay) {
	t.Helper()
	pool := h.createPool(t)
	relays.set(pool.GetId())
	attached := []*fakeRelay{
		h.attachPoolRelay(t, "pool-a", "us-west1", "198.51.100.2:443"),
		h.attachPoolRelay(t, "pool-b", "us-west1", "198.51.100.1:443"),
	}
	t.Cleanup(func() {
		for _, relay := range attached {
			relay.stop()
		}
	})
	h.goPrivate(t, pool.GetId())
	return h.awaitPoolState(t, pool.GetId(), adminpb.PoolState_POOL_STATE_READY), attached
}

// poolClaim reads the `pool` claim out of an unsealed token.
func poolClaim(t *testing.T, minted string) (string, bool) {
	t.Helper()
	parts := strings.Split(minted, ".")
	if len(parts) != 3 {
		t.Fatalf("a token with %d segments is not a JWS", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("reading the claims: %v", err)
	}
	pool, present := claims["pool"].(string)
	return pool, present
}

func TestOnlyAnOwnerManagesRelayPools(t *testing.T) {
	h, _ := newPoolHarness(t)
	for name, credential := range map[string]string{
		"a moderator's backend": modKey,
		"a join backend":        joinKey,
		"a relay":               relayKey,
	} {
		_, err := h.relayPools.CreateRelayPool(as(credential), &adminpb.CreateRelayPoolRequest{
			Tenant: tenant, Name: "production", Regions: []*adminpb.PoolRegion{{Region: "us-west1", Relays: 2}},
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s creating a pool got %v, want PermissionDenied", name, err)
		}
		_, err = h.relayPools.SetTenantNetwork(as(credential), &adminpb.SetTenantNetworkRequest{
			Tenant: tenant, Network: &adminpb.TenantNetwork{Mode: adminpb.NetworkMode_NETWORK_MODE_SHARED},
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s setting the network got %v, want PermissionDenied", name, err)
		}
	}
	if _, err := h.relayPools.ListRelayPools(as(globexOwnerKey), &adminpb.ListRelayPoolsRequest{Tenant: tenant}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another tenant's owner listing the pools got %v, want PermissionDenied", err)
	}
}

func TestARelayPoolIsSizedWithinTheRules(t *testing.T) {
	h, _ := newPoolHarness(t)
	region := func(name string, relays, most uint32) *adminpb.PoolRegion {
		return &adminpb.PoolRegion{Region: name, Relays: relays, MaxRelays: most}
	}
	for _, c := range []struct {
		name       string
		pool       string
		regions    []*adminpb.PoolRegion
		selfHosted bool
		want       codes.Code
		state      adminpb.PoolState
	}{
		{"a dedicated pool starts provisioning", "production", []*adminpb.PoolRegion{region("us-west1", 2, 0)}, false, codes.OK, adminpb.PoolState_POOL_STATE_PROVISIONING},
		{"a self-hosted pool is ready at once", "on-prem", nil, true, codes.OK, adminpb.PoolState_POOL_STATE_READY},
		{"a self-hosted pool may run one relay a region", "on-prem", []*adminpb.PoolRegion{region("europe-west4", 1, 0)}, true, codes.OK, adminpb.PoolState_POOL_STATE_READY},
		{"a pool needs a name", " ", []*adminpb.PoolRegion{region("us-west1", 2, 0)}, false, codes.InvalidArgument, 0},
		{"a dedicated pool needs a region", "production", nil, false, codes.InvalidArgument, 0},
		{"a dedicated region needs two relays", "production", []*adminpb.PoolRegion{region("us-west1", 1, 0)}, false, codes.InvalidArgument, 0},
		{"a region the fleet does not run in is refused", "production", []*adminpb.PoolRegion{region("mars-north1", 2, 0)}, false, codes.InvalidArgument, 0},
		{"a ceiling below the count is refused", "production", []*adminpb.PoolRegion{region("us-west1", 4, 3)}, false, codes.InvalidArgument, 0},
		{"a region named twice is refused", "production", []*adminpb.PoolRegion{region("us-west1", 2, 0), region("us-west1", 3, 0)}, false, codes.InvalidArgument, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, err := h.relayPools.CreateRelayPool(as(ownerKey), &adminpb.CreateRelayPoolRequest{
				Tenant: tenant, Name: c.pool, Regions: c.regions, SelfHosted: c.selfHosted,
			})
			if status.Code(err) != c.want {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if err != nil {
				return
			}
			if pool.GetState() != c.state || pool.GetAccess() != adminpb.PoolAccess_POOL_ACCESS_PUBLIC {
				t.Errorf("created %v with %v access, want %v and public", pool.GetState(), pool.GetAccess(), c.state)
			}
			for _, sized := range pool.GetRegions() {
				if sized.GetMaxRelays() != sized.GetRelays() {
					t.Errorf("an unset ceiling reads as %d, want the count %d", sized.GetMaxRelays(), sized.GetRelays())
				}
			}
		})
	}
}

func TestResizingAndAccessChangeAPoolButNotADeletedOne(t *testing.T) {
	h, _ := newPoolHarness(t)
	pool := h.createPool(t)

	resized, err := h.relayPools.ResizeRelayPool(as(ownerKey), &adminpb.ResizeRelayPoolRequest{
		Tenant: tenant, PoolId: pool.GetId(), Region: &adminpb.PoolRegion{Region: "us-west1", Relays: 2, MaxRelays: 8},
	})
	if err != nil {
		t.Fatalf("resizing: %v", err)
	}
	if len(resized.GetRegions()) != 1 || resized.GetRegions()[0].GetMaxRelays() != 8 {
		t.Errorf("resized to %v", resized.GetRegions())
	}
	added, err := h.relayPools.ResizeRelayPool(as(ownerKey), &adminpb.ResizeRelayPoolRequest{
		Tenant: tenant, PoolId: pool.GetId(), Region: &adminpb.PoolRegion{Region: "us-east4", Relays: 2},
	})
	if err != nil || len(added.GetRegions()) != 2 {
		t.Errorf("adding a region gave %v (%v)", added.GetRegions(), err)
	}
	access, err := h.relayPools.SetRelayPoolAccess(as(ownerKey), &adminpb.SetRelayPoolAccessRequest{
		Tenant: tenant, PoolId: pool.GetId(), Access: adminpb.PoolAccess_POOL_ACCESS_PRIVATE_SERVICE_CONNECT,
	})
	if err != nil || access.GetAccess() != adminpb.PoolAccess_POOL_ACCESS_PRIVATE_SERVICE_CONNECT {
		t.Errorf("setting access gave %v (%v)", access.GetAccess(), err)
	}

	if _, err := h.relayPools.DeleteRelayPool(as(ownerKey), &adminpb.DeleteRelayPoolRequest{Tenant: tenant, PoolId: pool.GetId()}); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := h.relayPools.ResizeRelayPool(as(ownerKey), &adminpb.ResizeRelayPoolRequest{
		Tenant: tenant, PoolId: pool.GetId(), Region: &adminpb.PoolRegion{Region: "us-west1", Relays: 4},
	}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("resizing a deleted pool got %v, want FailedPrecondition", err)
	}
	if _, err := h.relayPools.GetRelayPool(as(globexOwnerKey), &adminpb.GetRelayPoolRequest{
		Tenant: otherTenant, PoolId: pool.GetId(),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("another tenant reading the pool got %v, want NotFound", err)
	}
}

// A tenant cannot be confined to a pool that is not there, nor have the pool
// it is confined to removed from under it.
func TestATenantNetworkNamesALivePoolOfItsOwn(t *testing.T) {
	h, _ := newPoolHarness(t)
	pool := h.createPool(t)
	theirs, err := h.relayPools.CreateRelayPool(as(globexOwnerKey), &adminpb.CreateRelayPoolRequest{
		Tenant: otherTenant, Name: "theirs", SelfHosted: true,
	})
	if err != nil {
		t.Fatalf("creating another tenant's pool: %v", err)
	}

	set := func(network *adminpb.TenantNetwork) error {
		_, err := h.relayPools.SetTenantNetwork(as(ownerKey), &adminpb.SetTenantNetworkRequest{Tenant: tenant, Network: network})
		return err
	}
	if err := set(&adminpb.TenantNetwork{Pool: theirs.GetId(), Mode: adminpb.NetworkMode_NETWORK_MODE_PRIVATE}); status.Code(err) != codes.NotFound {
		t.Errorf("going private on another tenant's pool got %v, want NotFound", err)
	}
	if err := set(&adminpb.TenantNetwork{Mode: adminpb.NetworkMode_NETWORK_MODE_PRIVATE}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("going private on no pool got %v, want InvalidArgument", err)
	}
	if err := set(&adminpb.TenantNetwork{Pool: pool.GetId()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a network with no mode got %v, want InvalidArgument", err)
	}

	h.goPrivate(t, pool.GetId())
	network, err := h.relayPools.GetTenantNetwork(as(ownerKey), &adminpb.GetTenantNetworkRequest{Tenant: tenant})
	if err != nil || network.GetMode() != adminpb.NetworkMode_NETWORK_MODE_PRIVATE || network.GetPool() != pool.GetId() {
		t.Errorf("the network reads as %v (%v)", network, err)
	}
	_, err = h.relayPools.DeleteRelayPool(as(ownerKey), &adminpb.DeleteRelayPoolRequest{Tenant: tenant, PoolId: pool.GetId()})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "set it to shared") {
		t.Errorf("deleting the pool the tenant is private on got %v, want FailedPrecondition saying what to do", err)
	}

	if err := set(&adminpb.TenantNetwork{Mode: adminpb.NetworkMode_NETWORK_MODE_SHARED}); err != nil {
		t.Fatalf("returning to shared: %v", err)
	}
	deleted, err := h.relayPools.DeleteRelayPool(as(ownerKey), &adminpb.DeleteRelayPoolRequest{Tenant: tenant, PoolId: pool.GetId()})
	if err != nil || deleted.GetState() != adminpb.PoolState_POOL_STATE_DELETED {
		t.Fatalf("deleting once shared gave %v (%v)", deleted.GetState(), err)
	}
	if err := set(&adminpb.TenantNetwork{Pool: pool.GetId(), Mode: adminpb.NetworkMode_NETWORK_MODE_PRIVATE}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("going private on a deleted pool got %v, want FailedPrecondition", err)
	}
}

// There is no fallback: a private tenant whose pool has nowhere to send
// anybody is refused a token, never given one the shared fleet would take.
func TestAPrivateTenantIsNeverMintedASharedFleetToken(t *testing.T) {
	h, relays := newPoolHarness(t)
	h.createRoom(t, "alice")
	pool := h.createPool(t)
	relays.set(pool.GetId())
	h.goPrivate(t, pool.GetId())

	_, err := h.tokens.MintClientToken(as(joinKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: deviceKeyThumbprint, Room: roomName,
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "no relays yet") {
		t.Fatalf("minting for a pool with no relays got %v, want FailedPrecondition", err)
	}

	first := h.attachPoolRelay(t, "pool-a", "us-west1", "198.51.100.2:443")
	defer first.stop()
	if first.welcome.GetPool() != pool.GetId() {
		t.Errorf("the pool relay was welcomed into %q, want %q", first.welcome.GetPool(), pool.GetId())
	}
	waitFor(t, "the first relay to be recorded", func() bool {
		read, err := h.relayPools.GetRelayPool(as(ownerKey), &adminpb.GetRelayPoolRequest{Tenant: tenant, PoolId: pool.GetId()})
		return err == nil && len(read.GetRelays()) == 1
	})
	if read, _ := h.relayPools.GetRelayPool(as(ownerKey), &adminpb.GetRelayPoolRequest{Tenant: tenant, PoolId: pool.GetId()}); read.GetState() != adminpb.PoolState_POOL_STATE_PROVISIONING {
		t.Errorf("one relay of two made the pool %v", read.GetState())
	}

	second := h.attachPoolRelay(t, "pool-b", "us-west1", "198.51.100.1:443")
	defer second.stop()
	h.awaitPoolState(t, pool.GetId(), adminpb.PoolState_POOL_STATE_READY)

	minted := h.mintFor(t, "bob", roomName)
	if claim, _ := poolClaim(t, minted.GetToken()); claim != pool.GetId() {
		t.Errorf("the token names pool %q, want %q", claim, pool.GetId())
	}
	if want := []string{"198.51.100.1:443", "198.51.100.2:443"}; !slices.Equal(minted.GetRelays(), want) {
		t.Errorf("the token lists relays %v, want %v", minted.GetRelays(), want)
	}

	if _, err := h.relayPools.SetTenantNetwork(as(ownerKey), &adminpb.SetTenantNetworkRequest{
		Tenant: tenant, Network: &adminpb.TenantNetwork{Mode: adminpb.NetworkMode_NETWORK_MODE_SHARED},
	}); err != nil {
		t.Fatalf("returning to shared: %v", err)
	}
	shared := h.mintFor(t, "bob", roomName)
	if _, present := poolClaim(t, shared.GetToken()); present || len(shared.GetRelays()) != 0 {
		t.Errorf("a shared tenant's token names a pool or lists relays: %v", shared.GetRelays())
	}
}

// A relay of the shared fleet could read a private room through a parent in
// the pool if it could get a link credential for one.
func TestAPrivateTenantsLinkCredentialIsOnlyForItsPoolsRelays(t *testing.T) {
	h, relays := newPoolHarness(t)
	h.createRoom(t, "alice")
	pool, _ := h.readyPool(t, relays)
	asked := server.RelayLinkRequest{Tenant: tenant, Room: roomName, Key: &deviceKey}

	if code, _, body := askForLink(t, h, relayKey, asked); code != http.StatusForbidden {
		t.Errorf("a shared relay asking for a private room's link got %d %s, want 403", code, body)
	}
	code, response, body := askForLink(t, h, poolRelayPrefix+"pool-a", asked)
	if code != http.StatusOK {
		t.Fatalf("a pool relay asking for its tenant's link got %d %s", code, body)
	}
	if claim, _ := poolClaim(t, response.Token); claim != pool.GetId() || len(response.Relays) != 2 {
		t.Errorf("the link names pool %q and lists %v", claim, response.Relays)
	}
}

// A pool relay serves one tenant, so a session, a snapshot request or a
// moderation action for any other tenant's room never reaches it.
func TestAPoolRelayNeverReceivesAnotherTenantsDirectiveOrSnapshot(t *testing.T) {
	h, relays := newPoolHarness(t)
	_, attached := h.readyPool(t, relays)
	poolRelay := attached[0]
	if _, err := h.rooms.CreateRoom(as(globexOwnerKey), &adminpb.CreateRoomRequest{
		Tenant: otherTenant, Room: "lobby", OwnerUsername: "gina",
	}); err != nil {
		t.Fatalf("creating another tenant's room: %v", err)
	}
	shared := h.attachRelay(t)
	defer shared.stop()

	session := func(relay *fakeRelay, id string) {
		t.Helper()
		if err := relay.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SessionOpened{
			SessionOpened: &relaypb.SessionOpened{Session: &relaypb.SessionState{
				SessionId: id, Tenant: otherTenant, Room: "lobby",
				Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
				OpenedAt: timestamppb.Now(),
			}},
		}}); err != nil {
			t.Fatalf("reporting a session: %v", err)
		}
	}
	session(poolRelay, "smuggled")
	session(shared, "session-1")
	if snapshot := shared.awaitSnapshot(t); snapshot.GetTenant() != otherTenant {
		t.Errorf("the shared relay's snapshot is for %q", snapshot.GetTenant())
	}
	if err := poolRelay.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SnapshotRequest{
		SnapshotRequest: &relaypb.SnapshotRequest{Tenant: otherTenant, Room: "lobby"},
	}}); err != nil {
		t.Fatalf("asking for a snapshot: %v", err)
	}

	if _, err := h.moderation.Kick(as(globexOwnerKey), &adminpb.KickRequest{
		Tenant: otherTenant, Room: "lobby", Target: &adminpb.ParticipantRef{Username: "dave"},
	}); err != nil {
		t.Fatalf("kicking: %v", err)
	}
	if directive := shared.awaitDirective(t); directive.GetTenant() != otherTenant {
		t.Errorf("the shared relay's directive is for %q", directive.GetTenant())
	}
	if _, err := h.rooms.UpdateRoomPolicy(as(globexOwnerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: otherTenant, Room: "lobby", Policy: &adminpb.RoomPolicy{MaxParticipants: 50},
		UpdateMask: []string{"max_participants"},
	}); err != nil {
		t.Fatalf("updating the policy: %v", err)
	}
	shared.awaitSnapshot(t)

	select {
	case snapshot := <-poolRelay.snapshots:
		t.Errorf("the pool relay was sent a snapshot of %s/%s", snapshot.GetTenant(), snapshot.GetRoom())
	case directive := <-poolRelay.directives:
		t.Errorf("the pool relay was sent a directive for %s/%s", directive.GetTenant(), directive.GetRoom())
	case <-time.After(200 * time.Millisecond):
	}
}

// A tenant with a pool has relays of its own, so its tokens are sealed with
// its own key from the moment the pool exists, and its pool relays hold that
// key and nothing else.
func TestATenantWithAPoolHasItsOwnSealKey(t *testing.T) {
	key, err := sectoken.GenerateSealKey()
	if err != nil {
		t.Fatal(err)
	}
	h, relays := newPoolHarness(t, key)
	h.createRoom(t, "alice")

	before := h.mintFor(t, "bob", roomName).GetToken()
	if _, err := sectoken.Open(before, key); err != nil {
		t.Fatalf("before a pool, the deployment key did not open the tenant's token: %v", err)
	}

	pool := h.createPool(t)
	relays.set(pool.GetId())
	own := tenantKey(t, key, tenant)
	minted := h.mintFor(t, "bob", roomName).GetToken()
	if _, err := sectoken.Open(minted, key); err == nil {
		t.Error("the deployment key itself opened a token sealed for the tenant's pool")
	}
	if _, err := sectoken.Open(minted, own); err != nil {
		t.Errorf("the tenant's key did not open its token: %v", err)
	}

	held := firstKeys(t, h.attachPoolRelay(t, "pool-a", "us-west1", "198.51.100.2:443"))
	if len(held.GetSealKeys()) != 1 || !holds(held, own) {
		t.Errorf("the pool relay holds %d seal keys, want its tenant's alone", len(held.GetSealKeys()))
	}
}

// A customer's relay enrolled into a pool is in that pool and no other, and
// the pool must be the tenant's own.
func TestACustomerRelayEnrolledIntoAPoolIsWelcomedIntoIt(t *testing.T) {
	h := newOverlayHarness(t, false)
	pool, err := adminpb.NewRelayPoolsClient(h.plain).CreateRelayPool(as(ownerKey), &adminpb.CreateRelayPoolRequest{
		Tenant: tenant, Name: "on-prem", SelfHosted: true,
	})
	if err != nil {
		t.Fatalf("creating a pool: %v", err)
	}
	enrollments := byocpb.NewRelayEnrollmentsClient(h.plain)
	if _, err := enrollments.Create(as(ownerKey), &byocpb.CreateRequest{
		Tenant: tenant, Name: "on-prem 1", Pool: "no-such-pool",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("enrolling into a pool that is not the tenant's got %v, want NotFound", err)
	}
	created, err := enrollments.Create(as(ownerKey), &byocpb.CreateRequest{Tenant: tenant, Name: "on-prem 1", Pool: pool.GetId()})
	if err != nil {
		t.Fatalf("enrolling into the pool: %v", err)
	}
	if created.GetEnrollment().GetPool() != pool.GetId() {
		t.Errorf("the enrolment is in pool %q, want %q", created.GetEnrollment().GetPool(), pool.GetId())
	}

	identity := created.GetEnrollment().GetIdentityName()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := relaypb.NewRelayLinkClient(h.dial(t, &identity)).Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_Hello{
		Hello: &relaypb.Hello{Region: "on-prem", Address: "relay.example.com:443"},
	}}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetWelcome().GetPool() != pool.GetId() {
		t.Errorf("the customer relay was welcomed into %q, want %q", first.GetWelcome().GetPool(), pool.GetId())
	}
	waitFor(t, "the relay's address to be recorded", func() bool {
		read, err := adminpb.NewRelayPoolsClient(h.plain).GetRelayPool(as(ownerKey), &adminpb.GetRelayPoolRequest{
			Tenant: tenant, PoolId: pool.GetId(),
		})
		return err == nil && len(read.GetRelays()) == 1 && read.GetRelays()[0].GetAddress() == "relay.example.com:443"
	})
}
