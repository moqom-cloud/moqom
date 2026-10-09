package server_test

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	moqom "github.com/moqom-cloud/moqom-go"
	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

func (h *harness) mintDirect(t *testing.T, room string) *adminpb.DirectGrant {
	t.Helper()
	response, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: room,
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	return response.GetDirect()
}

// A room nobody configured allows direct paths for up to four, says so on
// read, and states it with every token minted for it.
func TestRoomsAllowDirectPathsByDefault(t *testing.T) {
	h := newHarness(t)
	created := h.createRoom(t, "alice")
	if got := created.GetPolicy().GetDirectPaths(); got != adminpb.DirectPaths_DIRECT_PATHS_ALLOWED {
		t.Errorf("a default room's direct_paths is %v, want ALLOWED", got)
	}
	if got := created.GetPolicy().GetMaxDirectParticipants(); got != 4 {
		t.Errorf("a default room's max_direct_participants is %d, want 4", got)
	}
	grant := h.mintDirect(t, roomName)
	if !grant.GetAllowed() || grant.GetMaxParticipants() != 4 {
		t.Errorf("a default room's token states %v, want allowed for 4", grant)
	}
	if grant := h.mintDirect(t, ""); grant != nil {
		t.Errorf("a roomless token states %v, want nothing", grant)
	}
	if stun := grant.GetStun(); stun != nil {
		t.Errorf("a deployment that names no STUN server states %v", stun)
	}
}

func newStunHarness(t *testing.T) *harness {
	t.Helper()
	return newConfiguredHarness(t, func(config *server.Config) {
		config.Stun = &control.StunEndpoint{Host: "relay.moqom.test", Port: 3478}
	})
}

// A deployment's STUN server is named with every token whose room allows
// direct paths, so a device behind a NAT can learn the address to offer.
func TestTokensNameTheRelaysStunServer(t *testing.T) {
	h := newStunHarness(t)
	h.createRoom(t, "alice")
	stun := h.mintDirect(t, roomName).GetStun()
	if stun.GetHost() != "relay.moqom.test" || stun.GetPort() != 3478 {
		t.Errorf("the token names %v, want relay.moqom.test:3478", stun)
	}

	// Nothing to learn an address for in a room that keeps media on the relay.
	if _, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{DirectPaths: adminpb.DirectPaths_DIRECT_PATHS_RELAY_ONLY},
		UpdateMask: []string{"direct_paths"},
	}); err != nil {
		t.Fatalf("keeping the room on the relay: %v", err)
	}
	if stun := h.mintDirect(t, roomName).GetStun(); stun != nil {
		t.Errorf("a relay-only room's token names %v", stun)
	}
}

// The join endpoint's JSON is what MoqomKit parses.
func TestTheTokenResponseSpellsTheStunServer(t *testing.T) {
	encoded, err := json.Marshal(server.TokenDirect{
		Allowed: true, MaxParticipants: 2, Stun: &server.TokenStun{Host: "relay.moqom.test", Port: 3478},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"allowed":true,"maxParticipants":2,"stun":{"host":"relay.moqom.test","port":3478}}`
	if string(encoded) != want {
		t.Errorf("rendered as %s, want %s", encoded, want)
	}
	encoded, _ = json.Marshal(server.TokenDirect{Allowed: false, MaxParticipants: 4})
	if string(encoded) != `{"allowed":false,"maxParticipants":4}` {
		t.Errorf("without a STUN server, rendered as %s", encoded)
	}
}

// A room set to relay only, or to a lower limit, says so in the next token.
func TestRoomPolicyLimitsDirectPaths(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	_, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{MaxDirectParticipants: 2},
		UpdateMask: []string{"max_direct_participants"},
	})
	if err != nil {
		t.Fatalf("lowering the limit: %v", err)
	}
	if grant := h.mintDirect(t, roomName); !grant.GetAllowed() || grant.GetMaxParticipants() != 2 {
		t.Errorf("after lowering the limit the token states %v", grant)
	}

	updated, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{DirectPaths: adminpb.DirectPaths_DIRECT_PATHS_RELAY_ONLY},
		UpdateMask: []string{"direct_paths"},
	})
	if err != nil {
		t.Fatalf("keeping the room on the relay: %v", err)
	}
	if got := updated.GetPolicy().GetDirectPaths(); got != adminpb.DirectPaths_DIRECT_PATHS_RELAY_ONLY {
		t.Errorf("the room reads back as %v", got)
	}
	if grant := h.mintDirect(t, roomName); grant.GetAllowed() {
		t.Errorf("a relay-only room's token states %v", grant)
	}
}

// A limit MoqomKit could not honour is refused rather than clamped.
func TestDirectLimitsOutsideTheRangeAreRefused(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	for _, limit := range []uint32{1, 5, 100} {
		_, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
			Tenant: tenant, Room: roomName,
			Policy:     &adminpb.RoomPolicy{MaxDirectParticipants: limit},
			UpdateMask: []string{"max_direct_participants"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("a limit of %d gave %v, want InvalidArgument", limit, status.Code(err))
		}
	}
}

// The Go SDK writes and reads the policy, and hands a backend the grant to
// pass on with each token.
func TestTheSDKCarriesTheDirectPolicy(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	room, err := client.CreateRoom(context.Background(), moqom.NewRoom{
		Name:   "private-call",
		Policy: moqom.Policy{DirectPaths: moqom.DirectRelayOnly, MaxDirectParticipants: 2},
	})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	if room.Policy.DirectPaths != moqom.DirectRelayOnly || room.Policy.MaxDirectParticipants != 2 {
		t.Fatalf("the room reads back as %q for %d", room.Policy.DirectPaths, room.Policy.MaxDirectParticipants)
	}
	token, err := client.MintToken(context.Background(), moqom.TokenRequest{
		Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: "private-call",
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if token.Direct == nil || token.Direct.Allowed || token.Direct.MaxParticipants != 2 {
		t.Errorf("the token states %+v", token.Direct)
	}
}

// The Go SDK hands a backend the STUN server with the grant.
func TestTheSDKCarriesTheStunServer(t *testing.T) {
	h := newStunHarness(t)
	client := h.sdk(t, ownerKey)
	if _, err := client.CreateRoom(context.Background(), moqom.NewRoom{Name: "call"}); err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	token, err := client.MintToken(context.Background(), moqom.TokenRequest{
		Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: "call",
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if token.Direct == nil || token.Direct.Stun == nil ||
		*token.Direct.Stun != (moqom.StunEndpoint{Host: "relay.moqom.test", Port: 3478}) {
		t.Errorf("the token states %+v", token.Direct)
	}
}
