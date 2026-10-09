package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/timecode"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// The whole stack, over a real gRPC connection on an in-memory socket.
//
// Calling the service methods directly would be faster and would skip the
// interceptors, which is where authentication lives -- so a test that did so could
// not tell an unauthenticated call from an authorised one. bufconn keeps the
// transport, the metadata, and the status codes, and costs nothing.

const (
	ownerKey = "owner-key"
	modKey   = "moderator-key"
	joinKey  = "join-only-key"
	relayKey = "relay-key"

	// An owner that names no publish scopes, which is the shape a real
	// deployment's operator account has: `is_owner` covers everything the admin
	// API asks about, so nobody writes a publish list beside it. It matters to the
	// join endpoint, which mints an account's own publish rights and nothing
	// broader -- see TestACredentialThatCouldPublishNothingIsRefusedRatherThanServed.
	barrenKey = "owner-with-no-publish-key"
	tenant    = "acme"
	roomName  = "room-42"
	issuerFor = "moqom-test"
)

type harness struct {
	rooms      adminpb.RoomsClient
	moderation adminpb.ModerationClient
	tokens     adminpb.TokensClient
	events     adminpb.EventsClient
	relay      relaypb.RelayLinkClient
	relayPools adminpb.RelayPoolsClient
	store      store.Store
	now        func() time.Time
	advance    func(time.Duration)

	// server is kept for the parts that are not gRPC. The health endpoints are
	// HTTP because a load balancer and an uptime check cannot speak anything
	// else.
	server *server.Server

	// listener is kept so a test can open a second connection carrying its own
	// credentials, which is what the SDK does.
	listener *bufconn.Listener
}

// newHarness builds a control plane over an in-memory store.
//
// The optional clock is for tests about time itself. Without one the server
// gets the frozen wall clock, which it treats as fallback time — correct for a
// fixture, and worth preserving so that no test accidentally asserts against
// laundered time.
func newHarness(t *testing.T, clocks ...timecode.Clock) *harness {
	t.Helper()
	var clock timecode.Clock
	if len(clocks) > 0 {
		clock = clocks[0]
	}
	return newConfiguredHarness(t, func(config *server.Config) { config.Clock = clock })
}

// newConfiguredHarness is newHarness with the server's configuration adjusted
// by configure before it is built.
func newConfiguredHarness(t *testing.T, configure func(*server.Config)) *harness {
	t.Helper()

	current := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }
	advance := func(d time.Duration) { current = current.Add(d) }

	memoryStore := memory.New(now, nil)
	signing, err := token.GenerateKey()
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}

	accounts := server.NewStaticAccounts(map[string]server.ServiceAccount{
		ownerKey: {Name: "owner-backend", Tenant: tenant, Grants: control.Grants{
			Publish: control.PublishFull, Moderation: control.CapAll, IsOwner: true,
		}},
		modKey: {Name: "mod-backend", Tenant: tenant, Grants: control.Grants{
			Publish: control.PublishFull, Moderation: control.CapChatGroup | control.CapStageGroup | control.CapKick | control.CapBanFromRoom | control.CapChangeRoomPolicy | control.CapManageWatchdogs | control.CapReadModerationLog,
		}},
		joinKey: {Name: "join-backend", Tenant: tenant, Grants: control.Grants{
			Publish: control.PublishCamera,
		}},
		relayKey:  {Name: "relay-1", IsRelay: true},
		barrenKey: {Name: "operator", Tenant: tenant, Grants: control.Grants{IsOwner: true}},
	})

	config := server.Config{
		Store:           memoryStore,
		Accounts:        accounts,
		Issuer:          issuerFor,
		Keys:            []token.Key{signing},
		Now:             now,
		DispatchTimeout: 2 * time.Second,
	}
	configure(&config)
	instance, err := server.New(config)
	if err != nil {
		t.Fatalf("building the control plane: %v", err)
	}

	unary, streaming := instance.Interceptors()
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(streaming),
	)
	instance.Register(grpcServer)

	listener := bufconn.Listen(1 << 20)
	go func() { _ = grpcServer.Serve(listener) }()

	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}

	t.Cleanup(func() {
		_ = connection.Close()
		grpcServer.Stop()
		_ = listener.Close()
	})

	return &harness{
		rooms:      adminpb.NewRoomsClient(connection),
		moderation: adminpb.NewModerationClient(connection),
		tokens:     adminpb.NewTokensClient(connection),
		events:     adminpb.NewEventsClient(connection),
		relay:      relaypb.NewRelayLinkClient(connection),
		relayPools: adminpb.NewRelayPoolsClient(connection),
		store:      memoryStore,
		now:        now,
		advance:    advance,
		server:     instance,
		listener:   listener,
	}
}

func as(credential string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+credential)
}

func (h *harness) createRoom(t *testing.T, owner string) *adminpb.Room {
	t.Helper()
	room, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant:        tenant,
		Room:          roomName,
		OwnerUsername: owner,
	})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	return room
}

func TestUnauthenticatedCallsAreRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.rooms.GetRoom(context.Background(), &adminpb.GetRoomRequest{Tenant: tenant, Room: roomName})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("a call with no credentials gave %v, want Unauthenticated", status.Code(err))
	}

	_, err = h.rooms.GetRoom(as("not-a-key"), &adminpb.GetRoomRequest{Tenant: tenant, Room: roomName})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("a call with a bad credential gave %v, want Unauthenticated", status.Code(err))
	}
}

// An account scoped to one tenant must not be able to reach another by naming it in
// the request body. This is the check that makes multi-tenancy real rather than
// conventional.
func TestAccountsCannotCrossTenants(t *testing.T) {
	h := newHarness(t)

	_, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: "somebody-else", Room: "room-1",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("creating a room in another tenant gave %v, want PermissionDenied", status.Code(err))
	}
}

// Relay credentials reach the relay stream and nothing else. If they were
// interchangeable with an admin key, compromising a relay would mean compromising
// the moderation API.
func TestRelayCredentialsCannotUseTheAdminAPI(t *testing.T) {
	h := newHarness(t)

	_, err := h.rooms.CreateRoom(as(relayKey), &adminpb.CreateRoomRequest{Tenant: tenant, Room: "room-1"})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a relay key reached the admin API: %v", status.Code(err))
	}
}

func TestRoomLifecycle(t *testing.T) {
	h := newHarness(t)

	created := h.createRoom(t, "alice")
	if created.GetNamespace() != "acme/r/room-42" {
		t.Errorf("namespace is %q, want acme/r/room-42", created.GetNamespace())
	}
	if created.GetLifecycle() != adminpb.RoomLifecycle_ROOM_LIFECYCLE_LIVE {
		t.Errorf("a room with no start time is %v, want LIVE", created.GetLifecycle())
	}
	if created.GetPolicy().GetStageSlots() != control.DefaultStageSlots {
		t.Errorf("stage slots default to %d", created.GetPolicy().GetStageSlots())
	}

	_, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{Tenant: tenant, Room: roomName})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("recreating a room gave %v, want AlreadyExists", status.Code(err))
	}

	scheduled, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: "later", StartsAt: timestamppb.New(h.now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("scheduling a room: %v", err)
	}
	if scheduled.GetLifecycle() != adminpb.RoomLifecycle_ROOM_LIFECYCLE_SCHEDULED {
		t.Errorf("a room with a start time is %v, want SCHEDULED", scheduled.GetLifecycle())
	}

	closed, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{
		Tenant: tenant, Room: roomName,
		Code:  adminpb.ModerationCode_MODERATION_CODE_OPERATOR_ACTION,
		Actor: &adminpb.ActorContext{Reason: "the event finished"},
	})
	if err != nil {
		t.Fatalf("closing a room: %v", err)
	}
	if closed.GetLifecycle() != adminpb.RoomLifecycle_ROOM_LIFECYCLE_ENDED {
		t.Errorf("a closed room is %v", closed.GetLifecycle())
	}

	// Idempotent, because a retried shutdown is normal and an error would make a
	// well-behaved caller raise an alarm about a room it successfully closed.
	if _, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{
		Tenant: tenant, Room: roomName,
	}); err != nil {
		t.Errorf("closing a closed room failed: %v", err)
	}
}

func TestResidencyCannotBeEditedThroughTheAPI(t *testing.T) {
	h := newHarness(t)

	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{ResidencyZone: "eu"},
	}); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	_, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{ResidencyZone: "us"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("moving a room's residency gave %v, want InvalidArgument", status.Code(err))
	}

	// A mask means untouched fields keep their values, so an unrelated edit does not
	// trip the residency check by echoing a default back.
	if _, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{MaxParticipants: 50},
		UpdateMask: []string{"max_participants"},
	}); err != nil {
		t.Errorf("a masked policy edit failed: %v", err)
	}
}

// The single most-used call in any backend SDK, and the one whose failure mode is
// worst: a key that can mint anything is a key that owns the tenant.
func TestMintingIsBoundedByTheAccountCeiling(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// A join-only key can mint an ordinary participant.
	minted, err := h.tokens.MintClientToken(as(joinKey), &adminpb.MintClientTokenRequest{
		Tenant:           tenant,
		Username:         "bob",
		DeviceThumbprint: "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM",
		Room:             roomName,
		Grants: &adminpb.Grants{
			Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO},
		},
	})
	if err != nil {
		t.Fatalf("minting an ordinary token: %v", err)
	}
	if minted.GetToken() == "" {
		t.Error("no token was returned")
	}
	if len(minted.GetIdentity().GetDevice()) != control.DeviceIDLength {
		t.Errorf("device is %q, want %d characters derived from the thumbprint",
			minted.GetIdentity().GetDevice(), control.DeviceIDLength)
	}

	// And cannot mint a moderator, however the request is phrased.
	for _, attempt := range []*adminpb.MintClientTokenRequest{
		{
			Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: roomName,
			Grants: &adminpb.Grants{Moderation: []adminpb.Capability{adminpb.Capability_CAPABILITY_KICK}},
		},
		{
			Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: roomName,
			Preset: adminpb.CapabilityPreset_CAPABILITY_PRESET_SUPER_MODERATOR,
		},
		{
			Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: roomName,
			Grants: &adminpb.Grants{IsOwner: true},
		},
	} {
		if _, err := h.tokens.MintClientToken(as(joinKey), attempt); status.Code(err) != codes.PermissionDenied {
			t.Errorf("a join-only key minted elevated rights: %v", status.Code(err))
		}
	}
}

func TestMintedTokensVerifyAndCarryTheirScope(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	response, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant:           tenant,
		Username:         "alice",
		DeviceThumbprint: "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM",
		Room:             roomName,
		Preset:           adminpb.CapabilityPreset_CAPABILITY_PRESET_SUPER_MODERATOR,
		Grants: &adminpb.Grants{
			Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO},
		},
		Lifetime: durationpb.New(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	keys, err := h.tokens.ListVerificationKeys(as(ownerKey), &adminpb.ListVerificationKeysRequest{Tenant: tenant})
	if err != nil {
		t.Fatalf("listing keys: %v", err)
	}
	if len(keys.GetKeys()) != 1 {
		t.Fatalf("%d verification keys published, want 1", len(keys.GetKeys()))
	}
	if keys.GetKeys()[0].GetKeyId() != response.GetKeyId() {
		t.Error("the token names a key that is not published")
	}
	// Every relay requires proof of possession, so a client can refuse a
	// deployment that says otherwise.
	if keys.GetBinding() != adminpb.TokenBinding_TOKEN_BINDING_DPOP {
		t.Errorf("binding is %v, want DPOP", keys.GetBinding())
	}
}

func TestMintingRefusesAClosedRoom(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	if _, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{Tenant: tenant, Room: roomName}); err != nil {
		t.Fatalf("closing: %v", err)
	}

	_, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: roomName,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("minting into a closed room gave %v, want FailedPrecondition", status.Code(err))
	}
}

func TestAgeGatedRoomsRequireAnAssertion(t *testing.T) {
	h := newHarness(t)

	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{AgeGate: &adminpb.AgeGate{MinimumAge: 18, RequireVerification: true}},
	}); err != nil {
		t.Fatalf("creating a gated room: %v", err)
	}

	_, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: roomName,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("minting without an age assertion gave %v, want FailedPrecondition", status.Code(err))
	}

	if _, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ",
		Room: roomName, AgeVerified: true,
	}); err != nil {
		t.Errorf("minting with an age assertion failed: %v", err)
	}
}

// The property that stops a customer's console escalating: routing a chat
// moderator's request through a powerful backend must not make it a ban.
func TestActingOnBehalfIsBoundedByTheHumanStanding(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	// Carol is a chat moderator; dave is an ordinary viewer.
	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "carol", Device: "d1"},
		Grants:   control.Grants{Moderation: control.CapChatGroup},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding carol: %v", err)
	}
	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "dave", Device: "d1"},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}

	// The owner's own backend can kick dave.
	if _, err := h.moderation.Kick(as(ownerKey), &adminpb.KickRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Actor:  &adminpb.ActorContext{Reason: "spam"},
	}); err != nil {
		t.Fatalf("the owner backend could not kick: %v", err)
	}

	// The same backend acting for carol cannot, because carol holds chat powers only.
	_, err := h.moderation.Kick(as(ownerKey), &adminpb.KickRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Actor:  &adminpb.ActorContext{OnBehalfOf: "carol", Reason: "spam"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a chat moderator kicked through a privileged backend: %v", status.Code(err))
	}
}

// Peers cannot act on each other, and this is where it matters: the API is the place
// somebody would try.
func TestModeratorsCannotActOnPeers(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	for _, name := range []string{"carol", "erin"} {
		if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
			Identity: control.Identity{Username: name, Device: "d1"},
			Grants:   control.Grants{Moderation: control.CapChatGroup | control.CapKick},
			Presence: control.PresenceConnected,
		}); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	_, err := h.moderation.Kick(as(ownerKey), &adminpb.KickRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "erin"},
		Actor:  &adminpb.ActorContext{OnBehalfOf: "carol"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("one moderator kicked an equally powerful peer: %v", status.Code(err))
	}
}

func TestOwnersCannotBeKicked(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "alice", Device: "d1"},
		Grants:   control.Grants{IsOwner: true, Publish: control.PublishFull},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding the owner: %v", err)
	}

	_, err := h.moderation.Kick(as(modKey), &adminpb.KickRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "alice"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("the owner was kicked by a moderator backend: %v", status.Code(err))
	}
}

// A ban must be durable before it is pushed, so that a target who reconnects during
// the push is still refused.
func TestBanIsWrittenBeforeItIsPushed(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	entry, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
		Code:    adminpb.ModerationCode_MODERATION_CODE_HARASSMENT,
		Actor:   &adminpb.ActorContext{Reason: "repeated harassment"},
	})
	if err != nil {
		t.Fatalf("banning: %v", err)
	}
	if entry.GetVerb() != adminpb.ModerationVerb_MODERATION_VERB_BAN {
		t.Errorf("entry verb is %v", entry.GetVerb())
	}
	if entry.GetServiceAccount() != "owner-backend" {
		t.Errorf("the entry is attributed to %q", entry.GetServiceAccount())
	}

	bans, err := h.store.ListBans(ctx, tenant, roomName)
	if err != nil {
		t.Fatalf("listing bans: %v", err)
	}
	if len(bans) != 1 || bans[0].Username != "frank" {
		t.Fatalf("the ban was not stored: %+v", bans)
	}
	if !bans[0].ExpiresAt.IsZero() {
		t.Error("a ban with no duration should be permanent")
	}
}

// Widening a ban's scope is a separate power. Without this check, anyone who can ban
// from a room bans from the platform by passing a different enum value.
func TestTenantWideBanNeedsTheWiderCapability(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	_, err := h.moderation.Ban(as(modKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_TENANT,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a room-scoped moderator banned tenant-wide: %v", status.Code(err))
	}

	if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_TENANT,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
	}); err != nil {
		t.Errorf("the owner backend could not ban tenant-wide: %v", err)
	}
}

func TestMalformedAddressBansAreRefused(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	_, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_IpPrefix{IpPrefix: "203.0.113.0/33"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an impossible prefix gave %v, want InvalidArgument", status.Code(err))
	}
}

// A queue that redelivers an action must not apply it twice. This is asserted at the
// API rather than only at the store, because the service does several writes and any
// one of them repeating is a bug.
func TestRetriedActionsApplyOnce(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	request := &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:    adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject:  &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
		Duration: durationpb.New(24 * time.Hour),
		Actor:    &adminpb.ActorContext{IdempotencyKey: "delivery-1", Reason: "spam"},
	}

	first, err := h.moderation.Ban(as(ownerKey), request)
	if err != nil {
		t.Fatalf("first ban: %v", err)
	}
	second, err := h.moderation.Ban(as(ownerKey), request)
	if err != nil {
		t.Fatalf("redelivered ban: %v", err)
	}

	if first.GetId() != second.GetId() {
		t.Errorf("a redelivery produced a second entry: %s then %s", first.GetId(), second.GetId())
	}
	// And the ban did not double in length, which is the concrete harm.
	if !first.GetExpiresAt().AsTime().Equal(second.GetExpiresAt().AsTime()) {
		t.Errorf("the ban's expiry moved from %v to %v",
			first.GetExpiresAt().AsTime(), second.GetExpiresAt().AsTime())
	}

	log, err := h.moderation.ListModerationLog(as(ownerKey), &adminpb.ListModerationLogRequest{Tenant: tenant})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	bans := 0
	for _, entry := range log.GetEntries() {
		if entry.GetVerb() == adminpb.ModerationVerb_MODERATION_VERB_BAN {
			bans++
		}
	}
	if bans != 1 {
		t.Errorf("the log holds %d bans after one retried action", bans)
	}
}

// A retry key must not be usable to discover that an action happened, so the
// idempotency lookup happens after authorisation rather than before.
func TestIdempotencyDoesNotLeakPastActions(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_TENANT,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
		Actor:   &adminpb.ActorContext{IdempotencyKey: "shared-key"},
	}); err != nil {
		t.Fatalf("the owner's ban: %v", err)
	}

	// The same key, replayed by an account that could not have performed it.
	_, err := h.moderation.Ban(as(modKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_TENANT,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
		Actor:   &adminpb.ActorContext{IdempotencyKey: "shared-key"},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("replaying a key returned a result to an unauthorised caller: %v", status.Code(err))
	}
}

func TestForceMuteRecordsScopeAndSurvivesReads(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "dave", Device: "d1"},
		Grants:   control.Grants{Publish: control.PublishFull},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}

	if _, err := h.moderation.ForceMute(as(ownerKey), &adminpb.ForceMuteRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Media:  []adminpb.MediaKind{adminpb.MediaKind_MEDIA_KIND_AUDIO},
		Scope:  adminpb.MuteScope_MUTE_SCOPE_EVERYONE,
	}); err != nil {
		t.Fatalf("force muting: %v", err)
	}
	if _, err := h.moderation.ForceMute(as(ownerKey), &adminpb.ForceMuteRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Media:  []adminpb.MediaKind{adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE},
		Scope:  adminpb.MuteScope_MUTE_SCOPE_AUDIENCE,
	}); err != nil {
		t.Fatalf("audience muting: %v", err)
	}

	participant, err := h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
		Tenant: tenant, Room: roomName,
		Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
	})
	if err != nil {
		t.Fatalf("reading the participant: %v", err)
	}

	if len(participant.GetForcedMutedForEveryone()) != 1 ||
		participant.GetForcedMutedForEveryone()[0] != adminpb.MediaKind_MEDIA_KIND_AUDIO {
		t.Errorf("everyone-mute reads back as %v", participant.GetForcedMutedForEveryone())
	}
	// The two scopes coexist. Collapsing them would make lifting one lift both.
	if len(participant.GetForcedMutedForAudience()) != 1 ||
		participant.GetForcedMutedForAudience()[0] != adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE {
		t.Errorf("audience-mute reads back as %v", participant.GetForcedMutedForAudience())
	}

	if _, err := h.moderation.Unmute(as(ownerKey), &adminpb.UnmuteRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Media:  []adminpb.MediaKind{adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE},
		Scope:  adminpb.MuteScope_MUTE_SCOPE_AUDIENCE,
	}); err != nil {
		t.Fatalf("unmuting: %v", err)
	}

	participant, err = h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
		Tenant: tenant, Room: roomName,
		Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
	})
	if err != nil {
		t.Fatalf("reading again: %v", err)
	}
	if len(participant.GetForcedMutedForAudience()) != 0 {
		t.Errorf("the audience mute survived being lifted: %v", participant.GetForcedMutedForAudience())
	}
	if len(participant.GetForcedMutedForEveryone()) != 1 {
		t.Error("lifting the audience mute also lifted the everyone mute")
	}
}

// Muting a camera says nothing about a microphone, in the durable state a relay
// is handed as well as in the participant record. They used to disagree: the
// record accumulated kinds and the mutes table replaced the row, so a moderator
// who muted audio and then video had turned the microphone back on and could
// read a participant that claimed otherwise.
func TestASecondForceMuteDoesNotLiftTheFirst(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "dave", Device: "d1"},
		Grants:   control.Grants{Publish: control.PublishFull},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}

	mute := func(kind adminpb.MediaKind) {
		t.Helper()
		if _, err := h.moderation.ForceMute(as(ownerKey), &adminpb.ForceMuteRequest{
			Tenant: tenant, Room: roomName,
			Target: &adminpb.ParticipantRef{Username: "dave"},
			Media:  []adminpb.MediaKind{kind},
			Scope:  adminpb.MuteScope_MUTE_SCOPE_EVERYONE,
		}); err != nil {
			t.Fatalf("muting %v: %v", kind, err)
		}
	}
	// What the relay would be handed, as one mask per scope.
	held := func() control.MediaKind {
		t.Helper()
		mutes, err := h.store.ListMutes(ctx, tenant, roomName)
		if err != nil {
			t.Fatalf("listing mutes: %v", err)
		}
		var kinds control.MediaKind
		for _, mute := range mutes {
			if !mute.AudienceOnly {
				kinds |= mute.Media
			}
		}
		return kinds
	}

	mute(adminpb.MediaKind_MEDIA_KIND_AUDIO)
	mute(adminpb.MediaKind_MEDIA_KIND_VIDEO)
	if want := control.MediaAudio | control.MediaVideo; held() != want {
		t.Fatalf("after muting both, the store holds %v, want %v", held(), want)
	}

	if _, err := h.moderation.Unmute(as(ownerKey), &adminpb.UnmuteRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Media:  []adminpb.MediaKind{adminpb.MediaKind_MEDIA_KIND_VIDEO},
		Scope:  adminpb.MuteScope_MUTE_SCOPE_EVERYONE,
	}); err != nil {
		t.Fatalf("lifting the camera: %v", err)
	}
	if held() != control.MediaAudio {
		t.Errorf("lifting the camera left %v muted, want audio", held())
	}

	// And the participant record says the same thing the store does.
	participant, err := h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
		Tenant: tenant, Room: roomName,
		Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
	})
	if err != nil {
		t.Fatalf("reading the participant: %v", err)
	}
	if len(participant.GetForcedMutedForEveryone()) != 1 ||
		participant.GetForcedMutedForEveryone()[0] != adminpb.MediaKind_MEDIA_KIND_AUDIO {
		t.Errorf("the participant reads back as %v, want audio only",
			participant.GetForcedMutedForEveryone())
	}
}

func TestSetGrantsCannotExceedTheGranter(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()

	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: control.Identity{Username: "dave", Device: "d1"},
		Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}

	// The moderator backend holds no appoint_moderators, so it cannot confer it.
	_, err := h.moderation.SetGrants(as(modKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Grants: &adminpb.Grants{Moderation: []adminpb.Capability{adminpb.Capability_CAPABILITY_APPOINT_MODERATORS}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("appoint_moderators was delegated: %v", status.Code(err))
	}

	// Ownership is never grantable by a non-owner.
	_, err = h.moderation.SetGrants(as(modKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Grants: &adminpb.Grants{IsOwner: true},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("ownership was granted: %v", status.Code(err))
	}

	// A promotion within the granter's own powers is fine, and lands as an override
	// so it does not wait for a token reissue.
	if _, err := h.moderation.SetGrants(as(modKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave", Device: "d1"},
		Preset: adminpb.CapabilityPreset_CAPABILITY_PRESET_MODERATOR,
		Grants: &adminpb.Grants{Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO}},
	}); err != nil {
		t.Fatalf("promoting: %v", err)
	}

	overrides, err := h.store.ListGrantOverrides(ctx, tenant, roomName)
	if err != nil {
		t.Fatalf("reading overrides: %v", err)
	}
	dave := control.Identity{Username: "dave", Device: "d1"}
	if overrides[dave].Moderation != control.CapChatGroup {
		t.Errorf("the override holds %s, want the moderator preset", overrides[dave].Moderation)
	}
}

// Reading the log is its own capability. An appeals process needs it without the
// power to act, and someone who can act should not automatically see everyone else's
// decisions.
func TestReadingTheLogNeedsItsOwnCapability(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	_, err := h.moderation.ListModerationLog(as(joinKey), &adminpb.ListModerationLogRequest{Tenant: tenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a key with no capabilities read the log: %v", status.Code(err))
	}
}

func TestWatchdogsAreDisclosedAndLimited(t *testing.T) {
	h := newHarness(t)
	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{WatchdogLimit: 1},
	}); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	attached, err := h.rooms.AttachWatchdog(as(ownerKey), &adminpb.AttachWatchdogRequest{
		Tenant: tenant, Room: roomName,
		Kind:        adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING,
		OperatedBy:  adminpb.WatchdogOperator_WATCHDOG_OPERATOR_TENANT,
		Descriptor_: "Recording for compliance",
	})
	if err != nil {
		t.Fatalf("attaching a recorder: %v", err)
	}
	if attached.GetScope() != "acme/r/room-42" {
		t.Errorf("a watchdog with no scope got %q, want the room prefix", attached.GetScope())
	}

	// A room read says it is being recorded, because being recorded is something the
	// room is told rather than something concealed.
	room, err := h.rooms.GetRoom(as(ownerKey), &adminpb.GetRoomRequest{Tenant: tenant, Room: roomName})
	if err != nil {
		t.Fatalf("reading the room: %v", err)
	}
	if !room.GetIsRecording() {
		t.Error("a room with a recorder attached does not report being recorded")
	}

	if _, err := h.rooms.AttachWatchdog(as(ownerKey), &adminpb.AttachWatchdogRequest{
		Tenant: tenant, Room: roomName, Kind: adminpb.WatchdogKind_WATCHDOG_KIND_SAFETY,
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("exceeding the watchdog limit gave %v", status.Code(err))
	}

	// And a watchdog is never a participant, whatever else it is.
	participants, err := h.rooms.ListParticipants(as(ownerKey), &adminpb.ListParticipantsRequest{
		Tenant: tenant, Room: roomName,
	})
	if err != nil {
		t.Fatalf("listing participants: %v", err)
	}
	if len(participants.GetParticipants()) != 0 {
		t.Errorf("a watchdog appeared in the roster: %v", participants.GetParticipants())
	}
}

func TestRecordingProhibitedBindsTheTenantToo(t *testing.T) {
	h := newHarness(t)
	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{Recording: adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED},
	}); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	_, err := h.rooms.AttachWatchdog(as(ownerKey), &adminpb.AttachWatchdogRequest{
		Tenant: tenant, Room: roomName,
		Kind: adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("recording was attached to a room that prohibits it: %v", status.Code(err))
	}
}

func TestThirdPartyWatchdogsMustBeNameable(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	_, err := h.rooms.AttachWatchdog(as(ownerKey), &adminpb.AttachWatchdogRequest{
		Tenant: tenant, Room: roomName,
		Kind:       adminpb.WatchdogKind_WATCHDOG_KIND_ANALYTICS,
		OperatedBy: adminpb.WatchdogOperator_WATCHDOG_OPERATOR_THIRD_PARTY,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an anonymous third-party observer was attached: %v", status.Code(err))
	}
}

// The relay link, and the thing this whole system exists to do: a moderation call
// becomes a directive on a relay's stream.
func TestKickReachesTheRelayThatHoldsTheSession(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	relay := h.attachRelay(t)
	defer relay.stop()

	relay.openSession(t, "session-1", control.Identity{Username: "dave", Device: "d1"})

	// The control plane learns of the session from the relay rather than from a
	// database write, so the roster reflects reality rather than intent.
	waitFor(t, "the participant to appear", func() bool {
		participants, err := h.rooms.ListParticipants(as(ownerKey), &adminpb.ListParticipantsRequest{
			Tenant: tenant, Room: roomName,
		})
		return err == nil && len(participants.GetParticipants()) == 1
	})

	entry, err := h.moderation.Kick(as(ownerKey), &adminpb.KickRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Code:   adminpb.ModerationCode_MODERATION_CODE_HARASSMENT,
		Actor:  &adminpb.ActorContext{Reason: "abuse", OnBehalfOf: "alice"},
	})
	if err != nil {
		t.Fatalf("kicking: %v", err)
	}

	directive := relay.awaitDirective(t)
	eject := directive.GetEject()
	if eject == nil {
		t.Fatalf("the relay received %T, want an Eject", directive.GetAction())
	}
	if eject.GetTarget().GetUsername() != "dave" {
		t.Errorf("the eject names %q", eject.GetTarget().GetUsername())
	}
	// No device named, so it reaches every device that person is on -- which is what
	// a kick means and what a per-device eject would get wrong.
	if eject.GetTarget().GetDevice() != "" {
		t.Errorf("the eject was narrowed to device %q", eject.GetTarget().GetDevice())
	}
	if eject.GetCode() != adminpb.ModerationCode_MODERATION_CODE_HARASSMENT {
		t.Errorf("the client will be told %v", eject.GetCode())
	}

	// The relay confirmed, so the entry says so. This is what lets a moderator's
	// tool distinguish "done" from "recorded, not yet applied".
	if !entry.GetEnforced() {
		t.Error("the entry reports unenforced despite the relay confirming")
	}
	if entry.GetOnBehalfOf() != "alice" {
		t.Errorf("the entry is attributed to %q", entry.GetOnBehalfOf())
	}
}

// A relay that reports a session in a room it has not served before must be given
// that room's bans, or it would admit somebody the control plane has already
// refused.
func TestNewRoomOnARelayGetsASnapshot(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "frank"}},
	}); err != nil {
		t.Fatalf("banning: %v", err)
	}

	relay := h.attachRelay(t)
	defer relay.stop()
	relay.openSession(t, "session-1", control.Identity{Username: "dave", Device: "d1"})

	snapshot := relay.awaitSnapshot(t)
	if snapshot.GetRoom() != roomName {
		t.Errorf("the snapshot is for %q", snapshot.GetRoom())
	}
	if len(snapshot.GetBans()) != 1 {
		t.Fatalf("the snapshot carries %d bans, want the one in force", len(snapshot.GetBans()))
	}
	if snapshot.GetBans()[0].GetUsername() != "frank" {
		t.Errorf("the snapshot bans %q", snapshot.GetBans()[0].GetUsername())
	}
	if snapshot.GetClosed() {
		t.Error("a live room was reported closed")
	}
}

// A lost transport is not a departure. Someone in a tunnel is coming back, and a
// room that forgot them would have to be rebuilt on their return.
func TestLostTransportMarksReconnectingRatherThanLeft(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	relay := h.attachRelay(t)
	defer relay.stop()
	relay.openSession(t, "session-1", control.Identity{Username: "dave", Device: "d1"})

	waitFor(t, "the participant to appear", func() bool {
		_, err := h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
			Tenant: tenant, Room: roomName,
			Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
		})
		return err == nil
	})

	relay.closeSession(t, "session-1", relaypb.SessionClosed_CAUSE_TRANSPORT_LOST)

	waitFor(t, "presence to become reconnecting", func() bool {
		participant, err := h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
			Tenant: tenant, Room: roomName,
			Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
		})
		return err == nil && participant.GetPresence() == adminpb.Presence_PRESENCE_RECONNECTING
	})

	// And the record is still there, so "who was in the room" survives the tunnel.
	if _, err := h.rooms.GetParticipant(as(ownerKey), &adminpb.GetParticipantRequest{
		Tenant: tenant, Room: roomName,
		Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
	}); err != nil {
		t.Errorf("the participant was forgotten: %v", err)
	}
}

func TestEventsStreamCarriesModerationAndResumes(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// Two actions before anyone is listening, so resumption has something to catch
	// up on.
	for _, name := range []string{"frank", "grace"} {
		if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
			Tenant: tenant, Room: roomName,
			Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
			Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: name}},
		}); err != nil {
			t.Fatalf("banning %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(as(ownerKey), 10*time.Second)
	defer cancel()

	stream, err := h.events.StreamRoomEvents(ctx, &adminpb.StreamRoomEventsRequest{
		Tenant: tenant, Room: roomName, AfterSequence: 0,
	})
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	// Opening a stream returns before the server has subscribed, so an action taken
	// now could be published to nobody. Headers arrive once the subscription exists,
	// which is the only synchronisation point a live consumer has.
	if _, err := stream.Header(); err != nil {
		t.Fatalf("waiting for the stream to become live: %v", err)
	}

	// A live action, which the stream should carry.
	if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: "heidi"}},
	}); err != nil {
		t.Fatalf("banning heidi: %v", err)
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if event.GetKind() != adminpb.RoomEventKind_ROOM_EVENT_KIND_MODERATION_APPLIED {
		t.Errorf("first live event is %v", event.GetKind())
	}
	applied := event.GetModerationApplied()
	if applied.GetBanSubject().GetUsername() != "heidi" {
		t.Errorf("the event describes %q", applied.GetBanSubject().GetUsername())
	}
	// Sequences are contiguous per room, so a consumer can tell a gap from quiet. The
	// two earlier bans took 1 and 2.
	if event.GetSequence() != 3 {
		t.Errorf("sequence is %d, want 3", event.GetSequence())
	}

	// Resuming from the start replays what was missed, in order.
	replay, err := h.events.StreamRoomEvents(ctx, &adminpb.StreamRoomEventsRequest{
		Tenant: tenant, Room: roomName, AfterSequence: 1,
	})
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	second, err := replay.Recv()
	if err != nil {
		t.Fatalf("reading the replay: %v", err)
	}
	if second.GetSequence() != 2 {
		t.Errorf("resuming after 1 delivered sequence %d", second.GetSequence())
	}
}

func TestTenantWideStreamCannotResume(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	ctx, cancel := context.WithTimeout(as(ownerKey), 5*time.Second)
	defer cancel()

	stream, err := h.events.StreamRoomEvents(ctx, &adminpb.StreamRoomEventsRequest{
		Tenant: tenant, AfterSequence: 5,
	})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("resuming a tenant-wide stream gave %v, want InvalidArgument", status.Code(err))
	}
}

// Readiness carries the clock's own account of itself, and does not fail on it.
//
// The question this answers is the one an operator has after reading "clock
// fell back to local time" in a log: how wide is the bound now, how wide is it
// allowed to be, and how many stamps have gone out untrusted. Before this, that
// needed a build. It stays out of the verdict deliberately -- a replica whose
// clock has drifted can still mint tokens and answer moderators, and taking it
// out of the load balancer would turn a measurement problem into an outage.
func TestReadinessReportsTheClock(t *testing.T) {
	h := newHarness(t, timecode.NewHost())

	recorder := httptest.NewRecorder()
	h.server.HealthHandler().ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, server.ReadinessPath, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("readiness = %d, want 200: a clock is not a reason to leave the load balancer", recorder.Code)
	}
	var body struct {
		Status string `json:"status"`
		Clock  *struct {
			Trust         string `json:"trust"`
			CeilingMs     int64  `json:"ceilingMs"`
			FallbackCount uint64 `json:"fallbackCount"`
		} `json:"clock"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("readiness body: %v", err)
	}
	if body.Status != "ready" {
		t.Errorf("status = %q, want ready", body.Status)
	}
	if body.Clock == nil {
		t.Fatal("no clock in the readiness body")
	}
	if body.Clock.Trust == "" {
		t.Error("the clock reported no trust")
	}
	if body.Clock.CeilingMs == 0 {
		t.Error("the ceiling is what the bound is being measured against and has to be in the report")
	}
}

// A clock that cannot say -- a fixed one, or an injected wall clock -- reports
// nothing rather than reporting zeroes, which would read as a perfect clock.
func TestReadinessOmitsAClockThatCannotReportOnItself(t *testing.T) {
	h := newHarness(t)

	recorder := httptest.NewRecorder()
	h.server.HealthHandler().ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, server.ReadinessPath, nil))

	if bytes.Contains(recorder.Body.Bytes(), []byte(`"clock"`)) {
		t.Errorf("body claims a clock report it cannot make: %s", recorder.Body.String())
	}
}

func TestEventsCarryTheServersOwnClockQuality(t *testing.T) {
	// A host that knows it is well disciplined. The interesting part is not
	// that a number arrives, it is that the number brought its error bound
	// with it: a consumer merging this room's events with a relay's in another
	// region has no way to order them honestly otherwise.
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, timecode.Fixed{
		Timecode: timecode.New(at, 3*time.Millisecond, timecode.TrustSynchronised),
	})
	h.createRoom(t, "alice")

	ctx, cancel := context.WithTimeout(as(ownerKey), 5*time.Second)
	defer cancel()

	event := h.liveEvent(t, ctx, "mallory")

	stamp := event.GetTimecode()
	if stamp == nil {
		t.Fatal("event carries no timecode")
	}
	if got := stamp.GetTrust(); got != adminpb.TimeTrust_TIME_TRUST_SYNCHRONISED {
		t.Errorf("trust = %v, want synchronised", got)
	}
	if got := stamp.GetUncertaintyNs(); got != int64(3*time.Millisecond) {
		t.Errorf("uncertainty = %dns, want the clock's 3ms", got)
	}

	// The legacy field and the timecode must never drift apart, or a consumer
	// reading one and a consumer reading the other will disagree about what
	// the server said.
	if !stamp.GetAt().AsTime().Equal(event.GetAt().AsTime()) {
		t.Errorf("timecode.at = %v but at = %v", stamp.GetAt().AsTime(), event.GetAt().AsTime())
	}
}

func TestAnInjectedClockIsNotLaunderedIntoTrustedTime(t *testing.T) {
	// The default fixture freezes the wall clock and says nothing about its
	// quality. Recording that as synchronised would make every test a place
	// where fake time looks like good time.
	h := newHarness(t)
	h.createRoom(t, "alice")

	ctx, cancel := context.WithTimeout(as(ownerKey), 5*time.Second)
	defer cancel()

	event := h.liveEvent(t, ctx, "mallory")

	if got := event.GetTimecode().GetTrust(); got != adminpb.TimeTrust_TIME_TRUST_FALLBACK {
		t.Errorf("trust = %v, want fallback for an unchecked clock", got)
	}
}

// liveEvent subscribes, provokes one moderation event, and returns it.
//
// The subscribe-then-act order is not incidental: a stream opened with
// `after_sequence` zero carries only what happens next, and headers are the
// only signal that the subscription actually exists.
func (h *harness) liveEvent(
	t *testing.T,
	ctx context.Context,
	target string,
) *adminpb.RoomEvent {
	t.Helper()

	stream, err := h.events.StreamRoomEvents(ctx, &adminpb.StreamRoomEventsRequest{
		Tenant: tenant, Room: roomName,
	})
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	if _, err := stream.Header(); err != nil {
		t.Fatalf("waiting for the stream to become live: %v", err)
	}

	if _, err := h.moderation.Ban(as(ownerKey), &adminpb.BanRequest{
		Tenant: tenant, Room: roomName,
		Scope:   adminpb.BanScope_BAN_SCOPE_ROOM,
		Subject: &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: target}},
	}); err != nil {
		t.Fatalf("banning %s: %v", target, err)
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return event
}

// A fake relay: enough of one to prove directives arrive and snapshots are sent.

type fakeRelay struct {
	stream     relaypb.RelayLink_AttachClient
	cancel     context.CancelFunc
	directives chan *relaypb.Directive
	snapshots  chan *relaypb.RoomSnapshot
	welcomes   chan *relaypb.Welcome
	keys       chan *relaypb.VerificationKeys

	// welcome is the Welcome the control plane attached it with.
	welcome *relaypb.Welcome
}

func (h *harness) attachRelay(t *testing.T) *fakeRelay {
	t.Helper()
	return h.attachRelayAs(t, relayKey, "relay-1")
}

func (h *harness) attachRelayAs(t *testing.T, credential, id string) *fakeRelay {
	t.Helper()
	return h.attachRelayWith(t, credential, &relaypb.Hello{RelayId: id, Region: "test", Version: "0"})
}

func (h *harness) attachRelayWith(t *testing.T, credential string, hello *relaypb.Hello) *fakeRelay {
	t.Helper()

	ctx, cancel := context.WithCancel(as(credential))
	stream, err := h.relay.Attach(ctx)
	if err != nil {
		cancel()
		t.Fatalf("attaching a relay: %v", err)
	}

	if err := stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_Hello{
		Hello: hello,
	}}); err != nil {
		cancel()
		t.Fatalf("sending hello: %v", err)
	}

	relay := &fakeRelay{
		stream:     stream,
		cancel:     cancel,
		directives: make(chan *relaypb.Directive, 8),
		snapshots:  make(chan *relaypb.RoomSnapshot, 8),
		welcomes:   make(chan *relaypb.Welcome, 8),
		keys:       make(chan *relaypb.VerificationKeys, 8),
	}
	go relay.read()

	// The relay is not attached until the control plane has said so, and a directive
	// sent before then would go to a registry entry that does not exist yet.
	select {
	case relay.welcome = <-relay.welcomes:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no welcome from the control plane")
	}
	return relay
}

func (r *fakeRelay) read() {
	for {
		message, err := r.stream.Recv()
		if err != nil {
			return
		}
		switch body := message.GetBody().(type) {
		case *relaypb.ControlMessage_Welcome:
			select {
			case r.welcomes <- body.Welcome:
			default:
			}
		case *relaypb.ControlMessage_RoomSnapshot:
			select {
			case r.snapshots <- body.RoomSnapshot:
			default:
			}
		case *relaypb.ControlMessage_VerificationKeys:
			select {
			case r.keys <- body.VerificationKeys:
			default:
			}
		case *relaypb.ControlMessage_Directive:
			select {
			case r.directives <- body.Directive:
			default:
			}
			// Answered immediately, which is what a real relay does after applying it.
			// Without an answer the control plane would report every action unenforced.
			_ = r.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_DirectiveResult{
				DirectiveResult: &relaypb.DirectiveResult{
					DirectiveId:      body.Directive.GetDirectiveId(),
					AffectedSessions: 1,
				},
			}})
		}
	}
}

func (r *fakeRelay) openSession(t *testing.T, id string, identity control.Identity) {
	t.Helper()
	if err := r.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SessionOpened{
		SessionOpened: &relaypb.SessionOpened{Session: &relaypb.SessionState{
			SessionId: id,
			Tenant:    tenant,
			Room:      roomName,
			Identity:  &adminpb.Identity{Username: identity.Username, Device: identity.Device},
			Grants: &adminpb.Grants{
				Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO},
			},
			RemoteAddress: "203.0.113.9:51234",
			OpenedAt:      timestamppb.Now(),
		}},
	}}); err != nil {
		t.Fatalf("reporting a session: %v", err)
	}
}

func (r *fakeRelay) closeSession(t *testing.T, id string, cause relaypb.SessionClosed_Cause) {
	t.Helper()
	if err := r.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SessionClosed{
		SessionClosed: &relaypb.SessionClosed{SessionId: id, Cause: cause, At: timestamppb.Now()},
	}}); err != nil {
		t.Fatalf("reporting a close: %v", err)
	}
}

func (r *fakeRelay) awaitDirective(t *testing.T) *relaypb.Directive {
	t.Helper()
	select {
	case directive := <-r.directives:
		return directive
	case <-time.After(5 * time.Second):
		t.Fatal("no directive reached the relay")
		return nil
	}
}

func (r *fakeRelay) awaitSnapshot(t *testing.T) *relaypb.RoomSnapshot {
	t.Helper()
	select {
	case snapshot := <-r.snapshots:
		return snapshot
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot reached the relay")
		return nil
	}
}

func (r *fakeRelay) stop() {
	r.cancel()
	_ = r.stream.CloseSend()
}

// waitFor polls a condition that depends on another goroutine having run.
//
// Polling rather than sleeping a fixed interval: the relay's reports are processed
// asynchronously, and a fixed sleep is either slower than necessary or flaky on a
// loaded machine.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAnAgentTokenIsMintedOnlyForAnAttachedWatchdogAndOnlyWatches(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	mint := func(username string) (*adminpb.MintClientTokenResponse, error) {
		return h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
			Tenant: tenant, Username: username, Room: roomName,
			DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ",
			Preset:           adminpb.CapabilityPreset_CAPABILITY_PRESET_SUPER_MODERATOR,
			Grants: &adminpb.Grants{
				Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_VIDEO},
			},
		})
	}

	if _, err := mint("watchdog.rec-1"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an agent for a watchdog nobody attached gave %v, want FailedPrecondition", status.Code(err))
	}

	attached, err := h.rooms.AttachWatchdog(as(ownerKey), &adminpb.AttachWatchdogRequest{
		Tenant: tenant, Room: roomName,
		Kind:        adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING,
		OperatedBy:  adminpb.WatchdogOperator_WATCHDOG_OPERATOR_TENANT,
		Descriptor_: "Recording for compliance",
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	response, err := mint("watchdog." + attached.GetId())
	if err != nil {
		t.Fatalf("minting for an attached watchdog: %v", err)
	}

	// Whatever the request asked for, an agent watches.
	parts := strings.Split(response.GetToken(), ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		MOQT struct {
			Publish      uint8  `json:"pub"`
			Capabilities uint32 `json:"cap"`
			Owner        bool   `json:"own"`
		} `json:"moqt"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.MOQT.Publish != 0 || claims.MOQT.Capabilities != 0 || claims.MOQT.Owner {
		t.Errorf("an agent token grants %+v, want nothing but watching", claims.MOQT)
	}

	// And an agent token is always for one room.
	if _, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "watchdog." + attached.GetId(),
		DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ",
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an agent token with no room gave %v, want InvalidArgument", status.Code(err))
	}
}
