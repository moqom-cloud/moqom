package server_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	moqom "github.com/moqom-cloud/moqom-go"
	"github.com/moqom-cloud/moqom/go/internal/control"
)

// The SDK against the real control plane.
//
// The SDK has its own tests, but they run against a stub, so they can only check that
// the SDK agrees with itself. These check the things that need both halves present: that
// a capability set means the same thing on each side of the wire, that a kick issued
// through the SDK produces the audit entry the server would have produced anyway, and
// that a stream opened by the SDK sees what happens next.

// sdk opens an SDK client authenticated as one service account.
func (h *harness) sdk(t *testing.T, credential string) *moqom.Client {
	t.Helper()

	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return h.listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(testCredential(credential)),
	)
	if err != nil {
		t.Fatalf("dialling for the SDK: %v", err)
	}

	client, err := moqom.Dial("ignored",
		moqom.WithTenant(tenant),
		moqom.WithConnection(connection),
	)
	if err != nil {
		t.Fatalf("building the SDK client: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = connection.Close()
	})
	return client
}

// testCredential presents a service account credential the way the SDK does.
//
// The SDK's own bearer credential is not exported, and reaching for it would be testing
// the wrong thing: what matters is that the server accepts what arrives in this header,
// which the SDK's tests already assert it sends.
type testCredential string

func (c testCredential) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(c)}, nil
}

func (c testCredential) RequireTransportSecurity() bool { return false }

// The SDK duplicates the capability bitmask, because a public SDK cannot import an
// internal package and its types are its API. Duplication is fine; drift is not, and
// drift here is a moderator granted a power somebody meant to withhold.
//
// The positions are also what a token carries and what MoqomKit uses, so this test is
// guarding a three-way agreement and not a two-way one.
func TestTheSDKAndTheServerAgreeOnCapabilityBits(t *testing.T) {
	pairs := []struct {
		name string
		sdk  moqom.Capability
		core control.Capability
	}{
		{"delete_message", moqom.CapDeleteMessage, control.CapDeleteMessage},
		{"timeout_user", moqom.CapTimeoutUser, control.CapTimeoutUser},
		{"ban_from_chat", moqom.CapBanFromChat, control.CapBanFromChat},
		{"set_chat_mode", moqom.CapSetChatMode, control.CapSetChatMode},
		{"manage_filters", moqom.CapManageFilters, control.CapManageFilters},
		{"promote_to_guest", moqom.CapPromoteToGuest, control.CapPromoteToGuest},
		{"promote_to_cohost", moqom.CapPromoteToCohost, control.CapPromoteToCohost},
		{"demote", moqom.CapDemote, control.CapDemote},
		{"force_mute", moqom.CapForceMute, control.CapForceMute},
		{"spotlight", moqom.CapSpotlight, control.CapSpotlight},
		{"manage_queue", moqom.CapManageQueue, control.CapManageQueue},
		{"lock_stage", moqom.CapLockStage, control.CapLockStage},
		{"kick", moqom.CapKick, control.CapKick},
		{"ban_from_room", moqom.CapBanFromRoom, control.CapBanFromRoom},
		{"ban_from_tenant", moqom.CapBanFromTenant, control.CapBanFromTenant},
		{"close_room", moqom.CapCloseRoom, control.CapCloseRoom},
		{"change_room_policy", moqom.CapChangeRoomPolicy, control.CapChangeRoomPolicy},
		{"manage_watchdogs", moqom.CapManageWatchdogs, control.CapManageWatchdogs},
		{"appoint_moderators", moqom.CapAppointModerators, control.CapAppointModerators},
		{"read_moderation_log", moqom.CapReadModerationLog, control.CapReadModerationLog},
		{"handle_appeals", moqom.CapHandleAppeals, control.CapHandleAppeals},
	}

	for _, pair := range pairs {
		if uint32(pair.sdk) != uint32(pair.core) {
			t.Errorf("%s is %#x in the SDK and %#x in the control plane", pair.name, uint32(pair.sdk), uint32(pair.core))
		}
	}

	// And the aggregate, which catches a capability added to one side only. A new
	// power that the SDK cannot express is a smaller problem than one it can express
	// and the server does not recognise, but both are caught here.
	if uint32(moqom.CapAll) != uint32(control.CapAll) {
		t.Errorf("the full set is %#x in the SDK and %#x in the control plane", uint32(moqom.CapAll), uint32(control.CapAll))
	}
	if len(pairs) != 21 {
		t.Errorf("this table lists %d capabilities; add the new one here too", len(pairs))
	}

	for _, pair := range []struct {
		name string
		sdk  moqom.PublishScope
		core control.PublishScope
	}{
		{"audio", moqom.PublishAudio, control.PublishAudio},
		{"video", moqom.PublishVideo, control.PublishVideo},
		{"screen_share", moqom.PublishScreenShare, control.PublishScreenShare},
		{"data", moqom.PublishData, control.PublishData},
	} {
		if uint8(pair.sdk) != uint8(pair.core) {
			t.Errorf("publish %s is %#x in the SDK and %#x in the control plane", pair.name, uint8(pair.sdk), uint8(pair.core))
		}
	}
}

// The whole point of the SDK, end to end: a backend creates a room, hands a user a
// token, and moderates.
func TestTheSDKRunsARoom(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	ctx := context.Background()

	room, err := client.CreateRoom(ctx, moqom.NewRoom{
		Name:  "sdk-room",
		Owner: "alice",
		Policy: moqom.Policy{
			ResidencyZone: "eu",
			StageSlots:    4,
			AgeGate:       moqom.AgeGate{MinimumAge: 18},
		},
		Actor: moqom.Actor{OnBehalfOf: "alice", Reason: "scheduled show"},
	})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	if room.Namespace != tenant+"/r/sdk-room" {
		t.Fatalf("the namespace was not returned: %+v", room)
	}
	if room.Policy.StageSlots != 4 || room.Policy.ResidencyZone != "eu" {
		t.Fatalf("the policy did not survive: %+v", room.Policy)
	}
	if room.Lifecycle != moqom.LifecycleLive {
		t.Fatalf("expected a live room, got %q", room.Lifecycle)
	}

	handle := client.Room("sdk-room")

	// The call a backend makes most. The identity comes back with the device
	// already derived, so the backend can record what it handed out without
	// reimplementing the derivation.
	minted, err := client.MintToken(ctx, moqom.TokenRequest{
		Username:         "bob",
		DeviceThumbprint: "thumbprint-of-bobs-phonethumbprint-of-bobsA",
		Room:             "sdk-room",
		Publish:          moqom.PublishCamera,
		Preset:           moqom.PresetModerator,
		Lifetime:         5 * time.Minute,
		AgeVerified:      true,
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if minted.Token == "" || minted.KeyID == "" {
		t.Fatalf("an empty token came back: %+v", minted)
	}
	if minted.Identity.Username != "bob" || minted.Identity.Device == "" {
		t.Fatalf("the identity was not derived: %+v", minted.Identity)
	}
	if !minted.ExpiresAt.After(h.now()) {
		t.Fatalf("the token expires at %v, which is not in the future", minted.ExpiresAt)
	}

	// The keys a relay verifies that token with.
	keys, err := client.VerificationKeys(ctx)
	if err != nil {
		t.Fatalf("reading verification keys: %v", err)
	}
	if len(keys.Keys) != 1 || keys.Keys[0].KeyID != minted.KeyID {
		t.Fatalf("the signing key is not published: %+v", keys)
	}
	if keys.Binding != moqom.BindingDPoP {
		t.Fatalf("expected the dpop binding, got %q", keys.Binding)
	}

	// Moderation, and the entry that records it.
	entry, err := handle.Ban(ctx, moqom.BanRequest{
		Subject:  moqom.BanAccount("carol"),
		Duration: 24 * time.Hour,
		Code:     moqom.CodeHarassment,
		Actor:    moqom.Actor{OnBehalfOf: "alice", Reason: "slurs in chat"},
	})
	if err != nil {
		t.Fatalf("banning: %v", err)
	}
	if entry.Verb != moqom.VerbBan || entry.BanScope != moqom.BanFromRoom {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if entry.BanSubject.Username != "carol" {
		t.Fatalf("the subject did not survive: %+v", entry.BanSubject)
	}
	if entry.OnBehalfOf != "alice" || entry.Reason != "slurs in chat" {
		t.Fatalf("attribution was lost: %+v", entry)
	}
	if entry.ServiceAccount != "owner-backend" {
		t.Fatalf("the service account was not filled from the credentials: %q", entry.ServiceAccount)
	}
	if !entry.ExpiresAt.After(h.now()) {
		t.Fatalf("a 24-hour ban expires at %v", entry.ExpiresAt)
	}
	// No relay holds a session here, so there is nothing to enforce against and the
	// call still reports success. That is the honest answer rather than a
	// convenient one.
	if !entry.Enforced {
		t.Fatal("a ban with no affected sessions reported incomplete enforcement")
	}

	// And the audit trail reads back through the SDK.
	log, err := handle.Log(ctx, moqom.LogQuery{Verb: []moqom.ModerationVerb{moqom.VerbBan}})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(log.Items) != 1 || log.Items[0].ID != entry.ID {
		t.Fatalf("expected the ban in the log, got %d entries", len(log.Items))
	}

	// Closing ends it for everyone, and is idempotent.
	closed, err := handle.Close(ctx, moqom.CodeOperatorAction, moqom.Actor{OnBehalfOf: "alice"})
	if err != nil {
		t.Fatalf("closing: %v", err)
	}
	if closed.Lifecycle != moqom.LifecycleEnded {
		t.Fatalf("expected an ended room, got %q", closed.Lifecycle)
	}

	// And a token into a closed room is refused, with an error the caller can
	// branch on without importing gRPC.
	_, err = client.MintToken(ctx, moqom.TokenRequest{
		Username:         "dave",
		DeviceThumbprint: "thumbprint-of-daves-phonethumbprint-of-davA",
		Room:             "sdk-room",
	})
	if !errors.Is(err, moqom.ErrClosed) {
		t.Fatalf("expected ErrClosed minting into a closed room, got %v", err)
	}
}

// A service account's ceiling has to bind the SDK as well, since the SDK is just a
// caller. Worth asserting from this side: a client library that quietly widened what a
// key could mint would be the most convenient possible security hole.
func TestTheSDKCannotExceedItsAccountsCeiling(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, joinKey)
	ctx := context.Background()

	// The join-only account can mint a plain viewer.
	if _, err := client.MintToken(ctx, moqom.TokenRequest{
		Username:         "bob",
		DeviceThumbprint: "thumbthumbthumbthumbthumbthumbthumbthumbthA",
		Publish:          moqom.PublishAudio,
	}); err != nil {
		t.Fatalf("minting within the ceiling failed: %v", err)
	}

	// It cannot mint a moderator.
	_, err := client.MintToken(ctx, moqom.TokenRequest{
		Username:         "bob",
		DeviceThumbprint: "thumbthumbthumbthumbthumbthumbthumbthumbthA",
		Moderation:       moqom.CapKick,
	})
	if !errors.Is(err, moqom.ErrDenied) {
		t.Fatalf("expected ErrDenied minting above the ceiling, got %v", err)
	}

	// Nor an owner.
	_, err = client.MintToken(ctx, moqom.TokenRequest{
		Username:         "bob",
		DeviceThumbprint: "thumbthumbthumbthumbthumbthumbthumbthumbthA",
		Owner:            true,
	})
	if !errors.Is(err, moqom.ErrDenied) {
		t.Fatalf("expected ErrDenied minting an owner, got %v", err)
	}

	// And it cannot moderate at all.
	h.createRoom(t, "alice")
	_, err = client.Room(roomName).Kick(ctx, moqom.KickRequest{Target: moqom.User("carol")})
	if !errors.Is(err, moqom.ErrDenied) {
		t.Fatalf("expected ErrDenied kicking without the capability, got %v", err)
	}
}

// The event stream is a backend's substitute for being in the room, so what matters is
// that an action taken after the stream opens arrives on it -- and that a consumer which
// records the sequence number can resume from it.
func TestTheSDKFollowsARoom(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	h.createRoom(t, "alice")
	ctx := context.Background()

	handle := client.Room(roomName)

	stream, err := handle.Events(ctx, moqom.EventQuery{})
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer stream.Close()

	first, err := handle.Kick(ctx, moqom.KickRequest{
		Target: moqom.User("carol"),
		Code:   moqom.CodeOperatorAction,
		Actor:  moqom.Actor{OnBehalfOf: "alice"},
	})
	if err != nil {
		t.Fatalf("kicking: %v", err)
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if event.Kind != moqom.EventModerationApplied {
		t.Fatalf("expected a moderation event, got %q", event.Kind)
	}
	if event.Moderation == nil || event.Moderation.ID != first.ID {
		t.Fatalf("the event did not carry the entry: %+v", event.Moderation)
	}
	if event.Sequence == 0 {
		t.Fatal("the event has no sequence number, so a consumer cannot resume")
	}
	cursor := event.Sequence
	stream.Close()

	// Something happens while nobody is listening.
	second, err := handle.Kick(ctx, moqom.KickRequest{
		Target: moqom.User("dave"),
		Actor:  moqom.Actor{OnBehalfOf: "alice"},
	})
	if err != nil {
		t.Fatalf("kicking again: %v", err)
	}

	// A consumer that persisted its cursor sees it anyway, which is the difference
	// between a dashboard and something that must not miss an event.
	resumed, err := handle.Events(ctx, moqom.EventQuery{After: cursor})
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	defer resumed.Close()

	missed, err := resumed.Recv()
	if err != nil {
		t.Fatalf("reading after resuming: %v", err)
	}
	if missed.Moderation == nil || missed.Moderation.ID != second.ID {
		t.Fatalf("resuming did not replay the missed action: %+v", missed)
	}
	if missed.Sequence <= cursor {
		t.Fatalf("resuming replayed something already seen: %d after %d", missed.Sequence, cursor)
	}
}

// A retry of a moderation action must not act twice, and the SDK is where the key comes
// from. This is the same property the service tests assert, checked through the surface
// a backend actually calls -- including that a caller-supplied key is honoured, which is
// what a queue worker needs.
func TestTheSDKMakesRetriesSafe(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	h.createRoom(t, "alice")
	ctx := context.Background()

	request := moqom.BanRequest{
		Subject:  moqom.BanAccount("carol"),
		Duration: time.Hour,
		Actor:    moqom.Actor{OnBehalfOf: "alice", IdempotencyKey: "queue-message-17"},
	}

	first, err := client.Room(roomName).Ban(ctx, request)
	if err != nil {
		t.Fatalf("banning: %v", err)
	}

	h.advance(30 * time.Minute)

	second, err := client.Room(roomName).Ban(ctx, request)
	if err != nil {
		t.Fatalf("redelivering the ban: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("the redelivery created a second entry: %s then %s", first.ID, second.ID)
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("the redelivery extended the ban from %v to %v", first.ExpiresAt, second.ExpiresAt)
	}

	// Without a key of its own, a second call is a second action -- which is
	// correct, and worth pinning down so nobody assumes the SDK deduplicates by
	// content.
	third, err := client.Room(roomName).Ban(ctx, moqom.BanRequest{
		Subject:  moqom.BanAccount("carol"),
		Duration: time.Hour,
		Actor:    moqom.Actor{OnBehalfOf: "alice"},
	})
	if err != nil {
		t.Fatalf("banning again: %v", err)
	}
	if third.ID == first.ID {
		t.Fatal("two calls without an idempotency key were deduplicated, which they cannot be")
	}
}

// A pool made private through the SDK confines the tenant: tokens are refused
// until the pool has relays, then name it and list where to dial.
func TestTheSDKMakesATenantPrivateOnAPool(t *testing.T) {
	h, relays := newPoolHarness(t)
	h.createRoom(t, "alice")
	client := h.sdk(t, ownerKey)
	ctx := context.Background()

	pool, err := client.CreateRelayPool(ctx, moqom.NewRelayPool{
		Name:    "production",
		Regions: []moqom.PoolRegion{{Region: "us-west1", Relays: 2}},
	})
	if err != nil {
		t.Fatalf("creating a pool: %v", err)
	}
	if pool.State != moqom.PoolProvisioning || pool.Regions[0].MaxRelays != 2 {
		t.Errorf("created %+v", pool)
	}
	relays.set(pool.ID)
	if err := client.SetTenantNetwork(ctx, moqom.TenantNetwork{Pool: pool.ID, Mode: moqom.NetworkPrivate}); err != nil {
		t.Fatalf("going private: %v", err)
	}
	request := moqom.TokenRequest{Username: "bob", DeviceThumbprint: deviceKeyThumbprint, Room: roomName}
	if _, err := client.MintToken(ctx, request); err == nil {
		t.Fatal("a private tenant was minted a token before its pool had relays")
	}

	for _, relay := range []*fakeRelay{
		h.attachPoolRelay(t, "pool-a", "us-west1", "198.51.100.2:443"),
		h.attachPoolRelay(t, "pool-b", "us-west1", "198.51.100.1:443"),
	} {
		defer relay.stop()
	}
	waitFor(t, "the pool to be ready", func() bool {
		read, err := client.GetRelayPool(ctx, pool.ID)
		return err == nil && read.State == moqom.PoolReady && len(read.Relays) == 2
	})
	minted, err := client.MintToken(ctx, request)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if len(minted.Relays) != 2 {
		t.Errorf("the token lists %v", minted.Relays)
	}
	if claim, _ := poolClaim(t, minted.Token); claim != pool.ID {
		t.Errorf("the token names pool %q, want %q", claim, pool.ID)
	}

	if _, err := client.DeleteRelayPool(ctx, pool.ID); err == nil {
		t.Error("the pool the tenant is private on was deleted")
	}
	network, err := client.TenantNetwork(ctx)
	if err != nil || network.Mode != moqom.NetworkPrivate || network.Pool != pool.ID {
		t.Errorf("the network reads as %+v (%v)", network, err)
	}
}
