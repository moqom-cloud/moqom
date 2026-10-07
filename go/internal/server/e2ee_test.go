package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	moqom "github.com/moqom-cloud/moqom-go"
	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// The claims of a minted token, read without verifying it. Verification has its
// own tests; these are about what the control plane chose to say.
func claimsOf(t *testing.T, minted string) map[string]any {
	t.Helper()
	parts := strings.Split(minted, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parsing claims: %v", err)
	}
	return claims
}

func (h *harness) mint(t *testing.T, room string) map[string]any {
	t.Helper()
	response, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", Room: room,
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	return claimsOf(t, response.GetToken())
}

// A room nobody configured is an encrypted room, and says so on read and in
// every token minted for it.
func TestRoomsRequireEndToEndEncryptionByDefault(t *testing.T) {
	h := newHarness(t)
	created := h.createRoom(t, "alice")
	if created.GetPolicy().GetE2Ee() != adminpb.E2EEPolicy_E2EE_POLICY_REQUIRED {
		t.Errorf("a default room's e2ee is %v, want REQUIRED", created.GetPolicy().GetE2Ee())
	}
	if got := h.mint(t, roomName)["e2ee"]; got != "required" {
		t.Errorf("a token for a default room carries e2ee=%v, want required", got)
	}

	// A token that names no room has no room policy to relax, and is silent,
	// which the SDK reads as required.
	if got, present := h.mint(t, "")["e2ee"]; present {
		t.Errorf("a roomless token carries e2ee=%v, want no claim", got)
	}
}

// There is no turning encryption off: not on update, with or without a reason,
// not at creation, and not through the SDK. The room stays required and so do
// its tokens.
func TestEncryptionCannotBeTurnedOff(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	for _, reason := range []string{"", "server-side transcription for accessibility"} {
		_, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
			Tenant: tenant, Room: roomName,
			Policy:     &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
			UpdateMask: []string{"e2ee"},
			Actor:      &adminpb.ActorContext{Reason: reason},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("turning e2ee off (reason %q) gave %v, want InvalidArgument", reason, status.Code(err))
		}
	}
	if got := h.mint(t, roomName)["e2ee"]; got != "required" {
		t.Fatalf("a refused change reached the token: e2ee=%v", got)
	}

	_, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: "other-room",
		Policy: &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
		Actor:  &adminpb.ActorContext{Reason: "recorded webinar"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("creating an unencrypted room gave %v, want InvalidArgument", status.Code(err))
	}

	// An edit that does not mention e2ee, masked or not, leaves it required.
	for _, mask := range [][]string{{"max_participants"}, nil} {
		updated, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
			Tenant: tenant, Room: roomName,
			Policy:     &adminpb.RoomPolicy{MaxParticipants: 10},
			UpdateMask: mask,
		})
		if err != nil {
			t.Fatalf("an edit with mask %v: %v", mask, err)
		}
		if updated.GetPolicy().GetE2Ee() != adminpb.E2EEPolicy_E2EE_POLICY_REQUIRED {
			t.Errorf("an edit with mask %v left e2ee at %v", mask, updated.GetPolicy().GetE2Ee())
		}
	}
}

// The Go SDK reads every room as required.
func TestTheSDKReadsEveryRoomAsEncrypted(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	room, err := client.CreateRoom(context.Background(), moqom.NewRoom{Name: "sdk-room"})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	if room.Policy.E2EE != moqom.E2EERequired {
		t.Fatalf("a default room reads back as e2ee %q", room.Policy.E2EE)
	}
}
