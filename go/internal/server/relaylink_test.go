package server_test

// The relay-link credential is worth not paying for -- a parent relay does not
// meter a session holding one -- so what matters here is who cannot get one.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

func askForLink(t *testing.T, h *harness, credential string, body any) (int, server.JoinResponse, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, server.RelayLinkPath, bytes.NewReader(encoded))
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	h.server.JoinHandler(false).ServeHTTP(recorder, request)
	var decoded server.JoinResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decoding the response: %v", err)
		}
	}
	return recorder.Code, decoded, strings.TrimSpace(recorder.Body.String())
}

// moqtOf reads the scope out of a minted token, unverified for the reason
// scopeOf is.
func moqtOf(t *testing.T, minted string) map[string]any {
	t.Helper()
	parts := strings.Split(minted, ".")
	if len(parts) != 3 {
		t.Fatalf("a token with %d segments is not a JWS", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	var claims struct {
		MOQT map[string]any `json:"moqt"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("reading the claims: %v", err)
	}
	return claims.MOQT
}

func TestARelayAccountMintsAReadOnlyLinkCredential(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := askForLink(t, h, relayKey, server.RelayLinkRequest{
		Tenant: tenant, Room: roomName, Key: &deviceKey,
	})
	if code != http.StatusOK {
		t.Fatalf("minting a link: %d %s", code, body)
	}
	claims := scopeOf(t, response.Token)
	if claims.Subject != server.RelaySubject || claims.Audience != tenant || claims.MOQT.Room != roomName {
		t.Errorf("link claims %+v", claims)
	}
	if claims.Confirmation.Thumbprint != deviceKeyThumbprint {
		t.Error("the link is not bound to the key that asked for it")
	}
	moqt := moqtOf(t, response.Token)
	if moqt["rly"] != true {
		t.Errorf("the link does not carry the relay grant: %v", moqt)
	}
	// Read-only: the relay refuses a link claim beside any of these, so a
	// minter that set one would produce a credential no relay accepts.
	if moqt["pub"] != float64(0) || moqt["cap"] != float64(0) || moqt["own"] != nil || moqt["bs"] != nil {
		t.Errorf("the link carries powers: %v", moqt)
	}
}

func TestOnlyARelayAccountCanMintALink(t *testing.T) {
	h := newHarness(t)

	for name, credential := range map[string]string{
		"an owner's backend": ownerKey,
		"a join backend":     joinKey,
		"nobody":             "",
		"a guess":            "not-a-key",
	} {
		code, _, _ := askForLink(t, h, credential, server.RelayLinkRequest{
			Tenant: tenant, Room: roomName, Key: &deviceKey,
		})
		if code != http.StatusUnauthorized {
			t.Errorf("%s got %d, want 401", name, code)
		}
	}
	if code, _, _ := askForLink(t, h, relayKey, server.RelayLinkRequest{Room: roomName, Device: deviceThumbprint}); code != http.StatusBadRequest {
		t.Errorf("a link with no tenant got %d, want 400", code)
	}
}

func TestTheBackendPathsNeverMintARelayLink(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Key: &deviceKey,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}
	if moqt := moqtOf(t, response.Token); moqt["rly"] != nil {
		t.Errorf("a device token carries the relay grant: %v", moqt)
	}

	// Nor can a backend mint somebody who looks like one on a roster.
	code, _, _ = ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: server.RelaySubject, Room: roomName, Key: &deviceKey,
	})
	if code != http.StatusBadRequest {
		t.Errorf("a device named %s got %d, want 400", server.RelaySubject, code)
	}
}

// One room per credential, bound to a key. A link naming no room would read the
// whole tenant, and one naming only a thumbprint would bind the credential to a
// key the edge never showed.
func TestALinkCredentialNamesOneRoomAndAKey(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	for name, asked := range map[string]server.RelayLinkRequest{
		"no room":           {Tenant: tenant, Key: &deviceKey},
		"only a thumbprint": {Tenant: tenant, Room: roomName, Device: deviceThumbprint},
	} {
		if code, _, body := askForLink(t, h, relayKey, asked); code != http.StatusBadRequest {
			t.Errorf("%s got %d %s, want 400", name, code, body)
		}
	}
}

// Ending a room reaches the edges at their next renewal, which is refused; a
// room that never existed is refused from the start.
func TestALinkIsRefusedForARoomThatIsMissingOrEnded(t *testing.T) {
	h := newHarness(t)
	asked := server.RelayLinkRequest{Tenant: tenant, Room: roomName, Key: &deviceKey}

	if code, _, body := askForLink(t, h, relayKey, asked); code != http.StatusNotFound {
		t.Errorf("a link into no room got %d %s, want 404", code, body)
	}

	h.createRoom(t, "alice")
	if code, _, body := askForLink(t, h, relayKey, asked); code != http.StatusOK {
		t.Fatalf("a link into an open room: %d %s", code, body)
	}

	if _, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{Tenant: tenant, Room: roomName}); err != nil {
		t.Fatalf("closing the room: %v", err)
	}
	if code, _, body := askForLink(t, h, relayKey, asked); code != http.StatusGone {
		t.Errorf("renewing a link into an ended room got %d %s, want 410", code, body)
	}
}

// The lifetime is how long a revocation takes to reach a parent, so it is
// shorter than a device's.
func TestALinkCredentialIsShortLived(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := askForLink(t, h, relayKey, server.RelayLinkRequest{
		Tenant: tenant, Room: roomName, Key: &deviceKey,
	})
	if code != http.StatusOK {
		t.Fatalf("minting a link: %d %s", code, body)
	}
	if lasts := response.ExpiresAt.Sub(h.now()); lasts != 5*time.Minute {
		t.Errorf("a link credential lasts %v, want 5m", lasts)
	}
}
