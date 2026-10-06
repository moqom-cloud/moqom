package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrst", Room: room,
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

// Turning encryption off is possible and never silent: it needs a reason, it is
// written to the moderation log with what changed, and the next token says so.
func TestTurningEncryptionOffIsReasonedAndLogged(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	loosen := &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
		UpdateMask: []string{"e2ee"},
	}
	_, err := h.rooms.UpdateRoomPolicy(as(ownerKey), loosen)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("turning e2ee off with no reason gave %v, want InvalidArgument", status.Code(err))
	}
	if got := h.mint(t, roomName)["e2ee"]; got != "required" {
		t.Fatalf("a refused change still reached the token: e2ee=%v", got)
	}

	loosen.Actor = &adminpb.ActorContext{Reason: "server-side transcription for accessibility"}
	updated, err := h.rooms.UpdateRoomPolicy(as(ownerKey), loosen)
	if err != nil {
		t.Fatalf("turning e2ee off with a reason: %v", err)
	}
	if updated.GetPolicy().GetE2Ee() != adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL {
		t.Errorf("the room's e2ee is %v after the change", updated.GetPolicy().GetE2Ee())
	}
	if got := h.mint(t, roomName)["e2ee"]; got != "optional" {
		t.Errorf("a token minted after the change carries e2ee=%v, want optional", got)
	}

	log, err := h.moderation.ListModerationLog(as(ownerKey), &adminpb.ListModerationLogRequest{Tenant: tenant})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	var found *adminpb.ModerationEntry
	for _, entry := range log.GetEntries() {
		if entry.GetVerb() == adminpb.ModerationVerb_MODERATION_VERB_UPDATE_ROOM_POLICY {
			found = entry
		}
	}
	if found == nil {
		t.Fatal("no policy entry in the moderation log")
	}
	if found.GetPolicyChange() != "e2ee: required -> optional" {
		t.Errorf("the entry records %q", found.GetPolicyChange())
	}
	if found.GetReason() != loosen.Actor.Reason {
		t.Errorf("the entry lost why: %q", found.GetReason())
	}
	if found.GetServiceAccount() != "owner-backend" {
		t.Errorf("the entry names service account %q", found.GetServiceAccount())
	}

	// Tightening needs no reason. Making a room safer is never the thing anybody
	// has to justify afterwards.
	if _, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_REQUIRED},
		UpdateMask: []string{"e2ee"},
	}); err != nil {
		t.Fatalf("turning e2ee back on: %v", err)
	}
	if got := h.mint(t, roomName)["e2ee"]; got != "required" {
		t.Errorf("a token minted after tightening carries e2ee=%v", got)
	}
}

// An unrelated masked edit leaves an optional room optional, and an unmasked one
// that does not mention e2ee puts it back to required -- the absent field means
// the strict answer, as it does everywhere else.
func TestUnmaskedPolicyWritesFallBackToRequired(t *testing.T) {
	h := newHarness(t)
	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
		Actor:  &adminpb.ActorContext{Reason: "a public broadcast with server-side captions"},
	}); err != nil {
		t.Fatalf("creating an optional room: %v", err)
	}

	masked, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{MaxParticipants: 10},
		UpdateMask: []string{"max_participants"},
	})
	if err != nil {
		t.Fatalf("a masked edit: %v", err)
	}
	if masked.GetPolicy().GetE2Ee() != adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL {
		t.Errorf("a masked edit moved e2ee to %v", masked.GetPolicy().GetE2Ee())
	}

	whole, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{MaxParticipants: 10},
	})
	if err != nil {
		t.Fatalf("an unmasked edit: %v", err)
	}
	if whole.GetPolicy().GetE2Ee() != adminpb.E2EEPolicy_E2EE_POLICY_REQUIRED {
		t.Errorf("an unmasked edit left e2ee at %v", whole.GetPolicy().GetE2Ee())
	}
}

// Creating a room unencrypted is held to the same rule as making one so later,
// or deleting and recreating would be the way around the audit trail.
func TestCreatingAnUnencryptedRoomNeedsAReason(t *testing.T) {
	h := newHarness(t)
	_, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("creating an optional room with no reason gave %v, want InvalidArgument", status.Code(err))
	}

	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: roomName,
		Policy: &adminpb.RoomPolicy{E2Ee: adminpb.E2EEPolicy_E2EE_POLICY_OPTIONAL},
		Actor:  &adminpb.ActorContext{Reason: "recorded webinar"},
	}); err != nil {
		t.Fatalf("creating an optional room with a reason: %v", err)
	}
	log, err := h.moderation.ListModerationLog(as(ownerKey), &adminpb.ListModerationLogRequest{Tenant: tenant})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(log.GetEntries()) != 1 || log.GetEntries()[0].GetPolicyChange() != "e2ee: required -> optional" {
		t.Errorf("creating an optional room logged %v", log.GetEntries())
	}
}

// The Go SDK models the field, defaults it to required on read, and carries the
// audit detail back.
func TestTheSDKTurnsEncryptionOffOnlyWithAReason(t *testing.T) {
	h := newHarness(t)
	client := h.sdk(t, ownerKey)
	ctx := context.Background()

	room, err := client.CreateRoom(ctx, moqom.NewRoom{Name: "sdk-room"})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	if room.Policy.E2EE != moqom.E2EERequired {
		t.Fatalf("a default room reads back as e2ee %q", room.Policy.E2EE)
	}

	handle := client.Room("sdk-room")
	update := moqom.PolicyUpdate{
		Policy: moqom.Policy{E2EE: moqom.E2EEOptional},
		Fields: []string{"e2ee"},
	}
	if _, err := handle.UpdatePolicy(ctx, update); !errors.Is(err, moqom.ErrInvalid) {
		t.Fatalf("turning e2ee off with no reason gave %v, want ErrInvalid", err)
	}

	update.Actor = moqom.Actor{Reason: "live captions"}
	updated, err := handle.UpdatePolicy(ctx, update)
	if err != nil {
		t.Fatalf("turning e2ee off with a reason: %v", err)
	}
	if updated.Policy.E2EE != moqom.E2EEOptional {
		t.Fatalf("the room reads back as e2ee %q", updated.Policy.E2EE)
	}

	log, err := handle.Log(ctx, moqom.LogQuery{})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(log.Items) != 1 || log.Items[0].PolicyChange != "e2ee: required -> optional" ||
		log.Items[0].Reason != "live captions" {
		t.Fatalf("the log reads %+v", log.Items)
	}
}
