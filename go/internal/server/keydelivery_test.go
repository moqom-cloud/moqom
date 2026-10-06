package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/mls"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

// The delivery service is reached with the token a device joined with, and
// sees the room and device that token names.
func TestKeyDeliveryIsAuthenticatedByTheParticipantToken(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	code, joined, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	store := mls.NewMemory()
	handler := h.server.KeyDeliveryHandler(mls.NewService(store, h.now))
	sync := func(credential string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, mls.PathPrefix+"sync?wait=0", nil)
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	for name, credential := range map[string]string{
		"no credential":          "",
		"a backend key":          joinKey,
		"a token with a bad sig": joined.Token[:len(joined.Token)-4] + "AAAA",
	} {
		if got := sync(credential); got.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, got.Code)
		}
	}

	got := sync(joined.Token)
	if got.Code != http.StatusOK {
		t.Fatalf("sync with the participant token: %d %s", got.Code, got.Body)
	}
	var snapshot struct {
		Group *struct{} `json:"group"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &snapshot); err != nil || snapshot.Group != nil {
		t.Fatalf("a first sync should find no group: %s", got.Body)
	}
	device, err := control.DeviceFromThumbprint(deviceThumbprint)
	if err != nil {
		t.Fatal(err)
	}
	identity := control.Identity{Username: "alice", Device: device}
	presence, _ := store.Presence(context.Background(), mls.Room{Tenant: tenant, Room: roomName})
	if len(presence) != 1 || presence[0].Device != identity.String() {
		t.Fatalf("the service saw %+v, want %s in %s/%s", presence, identity, tenant, roomName)
	}

	// Banned after joining, with the token still valid: refused from then on,
	// so the device drops out of presence and the next commit removes it.
	if _, err := h.store.PutBan(context.Background(), control.Ban{
		ID: "ban-1", Tenant: tenant, Room: roomName, Kind: control.BanByDevice,
		Username: "alice", Device: device, CreatedAt: h.now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := sync(joined.Token); got.Code != http.StatusUnauthorized {
		t.Fatalf("a banned device synced: %d", got.Code)
	}
}
