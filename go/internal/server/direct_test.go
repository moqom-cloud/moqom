package server_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
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
