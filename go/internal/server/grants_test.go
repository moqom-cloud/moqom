package server_test

import (
	"context"
	"testing"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
)

// A real thumbprint, because the device a token is minted for is derived from it
// and an override can name that device.
const davesPhone = "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM"

func davesDevice(t *testing.T) string {
	t.Helper()
	device, err := control.DeviceFromThumbprint(davesPhone)
	if err != nil {
		t.Fatalf("deriving the device: %v", err)
	}
	return device
}

// What a token minted for dave says he may do, asked for as a cohost.
func (h *harness) mintCohost(t *testing.T) (publish, capabilities float64) {
	t.Helper()
	response, err := h.tokens.MintClientToken(as(ownerKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "dave", DeviceThumbprint: davesPhone, Room: roomName,
		Preset: adminpb.CapabilityPreset_CAPABILITY_PRESET_MODERATOR,
		Grants: &adminpb.Grants{Publish: []adminpb.PublishScope{
			adminpb.PublishScope_PUBLISH_SCOPE_AUDIO, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO,
		}},
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	moqt, ok := claimsOf(t, response.GetToken())["moqt"].(map[string]any)
	if !ok {
		t.Fatal("the token carries no moqt scope")
	}
	publish, _ = moqt["pub"].(float64)
	capabilities, _ = moqt["cap"].(float64)
	return publish, capabilities
}

// A demotion reaches the relay at once, without waiting for the client to renew,
// and the next token the backend mints for the same person agrees with it. The
// relay holds the line either way; the token agreeing is what lets the app show
// the right controls instead of offering a camera it will be refused.
func TestADemotionReachesTheRelayAndTheNextToken(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	relay := h.attachRelay(t)
	defer relay.stop()

	if publish, _ := h.mintCohost(t); publish == 0 {
		t.Fatal("a cohost token with nothing overridden carries no publishing")
	}

	dave := control.Identity{Username: "dave", Device: davesDevice(t)}
	relay.openSession(t, "session-1", dave)
	waitFor(t, "dave to appear", func() bool {
		participants, err := h.rooms.ListParticipants(as(ownerKey), &adminpb.ListParticipantsRequest{
			Tenant: tenant, Room: roomName,
		})
		return err == nil && len(participants.GetParticipants()) == 1
	})

	if _, err := h.moderation.SetGrants(as(ownerKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Grants: &adminpb.Grants{},
	}); err != nil {
		t.Fatalf("demoting: %v", err)
	}

	apply := relay.awaitDirective(t).GetApplyGrants()
	if apply == nil {
		t.Fatal("the relay was not told to apply the new grants")
	}
	if apply.GetTarget().GetUsername() != "dave" || apply.GetTarget().GetDevice() != "" {
		t.Errorf("the directive names %v, want every device dave is on", apply.GetTarget())
	}
	if len(apply.GetGrants().GetPublish()) != 0 || len(apply.GetGrants().GetModeration()) != 0 {
		t.Errorf("the directive carries %v, want nothing", apply.GetGrants())
	}

	publish, capabilities := h.mintCohost(t)
	if publish != 0 || capabilities != 0 {
		t.Errorf("a token minted after the demotion carries pub=%v cap=%v, want neither", publish, capabilities)
	}
}

// Every override that names somebody applies at once, so a promotion for the
// person has to replace a demotion that named only their phone. Otherwise the
// phone stays demoted, and the backend mints a guest token the relay refuses.
func TestAPersonWidePromotionReplacesADeviceDemotion(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	ctx := context.Background()
	dave := control.Identity{Username: "dave", Device: davesDevice(t)}
	if err := h.store.SaveParticipant(ctx, tenant, roomName, control.Participant{
		Identity: dave, Presence: control.PresenceConnected,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}

	if _, err := h.moderation.SetGrants(as(ownerKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave", Device: dave.Device},
		Grants: &adminpb.Grants{},
	}); err != nil {
		t.Fatalf("demoting the phone: %v", err)
	}
	if publish, _ := h.mintCohost(t); publish != 0 {
		t.Fatalf("the demoted phone was minted pub=%v", publish)
	}

	guest := []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO}
	if _, err := h.moderation.SetGrants(as(ownerKey), &adminpb.SetGrantsRequest{
		Tenant: tenant, Room: roomName,
		Target: &adminpb.ParticipantRef{Username: "dave"},
		Grants: &adminpb.Grants{Publish: guest},
	}); err != nil {
		t.Fatalf("promoting: %v", err)
	}

	overrides, err := h.store.ListGrantOverrides(ctx, tenant, roomName)
	if err != nil {
		t.Fatalf("reading overrides: %v", err)
	}
	want := control.PublishAudio | control.PublishVideo
	if overrides[dave].Publish != want {
		t.Errorf("the phone's override still holds %v, want %v", overrides[dave].Publish, want)
	}
	// A guest, not the cohost the backend asked for: the promotion set the most
	// a token may carry, and the backend asking for more does not get it.
	publish, capabilities := h.mintCohost(t)
	if publish != float64(want) || capabilities != 0 {
		t.Errorf("the promoted phone was minted pub=%v cap=%v, want pub=%v and no powers", publish, capabilities, want)
	}
}
