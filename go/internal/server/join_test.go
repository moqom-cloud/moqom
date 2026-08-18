package server_test

// The one route a phone may call, and what it must refuse.
//
// This is the only listener in the control plane that faces the internet, so the
// interesting assertions here are all negative. A public endpoint that mints
// signatures is exactly the shape of thing that gets found, and the property
// being defended is that finding it and even stealing its credential buys the
// ability to join rooms in one tenant and nothing else -- no moderation, no
// ownership, no publish rights the credential does not itself hold, no other
// tenant, and not the admin API next door.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

const deviceThumbprint = "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmN"

// The EC key from RFC 7517 §A.1 and the thumbprint RFC 7638 computes over it.
//
// A published pair rather than one generated here, so that these tests assert
// the same agreement the relay and the SDK assert: all three pin this example.
var (
	deviceKey = token.PublicKey{
		Kty: "EC",
		Crv: "P-256",
		X:   "MKBCTNIcKUSDii11ySs3526iDZ8AiTo7Tu6KPAqv7D4",
		Y:   "4Etl6SRW2YiLUrN5vfvVHuhp7x8PxltmWWlbbM4IFyM",
	}
	deviceKeyThumbprint = "cn-I_WNMClehiVp51i_0VpOENW1upEerA8sEam5hn-s"
)

// ask posts a join request and returns the status and decoded body.
func ask(t *testing.T, handler http.Handler, credential string, body any) (int, server.JoinResponse, string) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, server.JoinPath, bytes.NewReader(encoded))
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var decoded server.JoinResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decoding the response: %v", err)
		}
	}
	return recorder.Code, decoded, strings.TrimSpace(recorder.Body.String())
}

// scopeOf reads the claims out of a minted token without verifying it.
//
// Unverified on purpose: what is under test is what the control plane put in the
// token, and the signature over it is tested elsewhere. Verifying here would
// need the signing key, which the harness keeps to itself precisely so that no
// test can quietly mint its own.
func scopeOf(t *testing.T, minted string) struct {
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	MOQT     struct {
		Room         string `json:"room"`
		Publish      uint8  `json:"pub"`
		Capabilities uint32 `json:"cap"`
		Owner        bool   `json:"own"`
	} `json:"moqt"`
	Confirmation struct {
		Thumbprint string `json:"jkt"`
	} `json:"cnf"`
	Expires int64 `json:"exp"`
} {
	t.Helper()

	var claims struct {
		Subject  string `json:"sub"`
		Audience string `json:"aud"`
		MOQT     struct {
			Room         string `json:"room"`
			Publish      uint8  `json:"pub"`
			Capabilities uint32 `json:"cap"`
			Owner        bool   `json:"own"`
		} `json:"moqt"`
		Confirmation struct {
			Thumbprint string `json:"jkt"`
		} `json:"cnf"`
		Expires int64 `json:"exp"`
	}

	parts := strings.Split(minted, ".")
	if len(parts) != 3 {
		t.Fatalf("a token with %d segments is not a JWS", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("reading the claims: %v", err)
	}
	return claims
}

func TestADeviceMintsItsOwnTokenForAnExistingRoom(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	claims := scopeOf(t, response.Token)
	if claims.Subject != "alice" {
		t.Errorf("subject is %q, want alice", claims.Subject)
	}
	if claims.MOQT.Room != roomName {
		t.Errorf("room is %q, want %s", claims.MOQT.Room, roomName)
	}
	if claims.Confirmation.Thumbprint != deviceThumbprint {
		t.Error("the token is not bound to the device that asked for it")
	}
	// The whole point of the endpoint: a credential that is good for a quarter of
	// an hour, fetched seconds before it is used, rather than one configured onto
	// a phone by hand and expired by the time anybody presses Join.
	if remaining := time.Unix(claims.Expires, 0).Sub(h.now()); remaining > 15*time.Minute {
		t.Errorf("token lasts %s, longer than the cap", remaining)
	}
	if response.ExpiresAt.IsZero() {
		t.Error("the response does not say when the token dies, so a client cannot refresh")
	}
}

func TestADeviceGetsThePublishRightsOfItsCredentialAndNoMore(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// The join account holds camera publish only. Nothing in the request names
	// grants at all -- that is deliberate, a device does not get a say -- so what
	// arrives has to be exactly the account's ceiling.
	code, response, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "bob", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	claims := scopeOf(t, response.Token)
	if got := control.PublishScope(claims.MOQT.Publish); got != control.PublishCamera {
		t.Errorf("publish scope is %v, want the account's %v", got, control.PublishCamera)
	}
}

func TestTheJoinEndpointNeverMintsModerationOrOwnership(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// Through the moderator's credential, which holds most of the capability set
	// and full publish. This is the case that matters if a deployment points the
	// endpoint at the wrong secret: the endpoint asks for no capability and no
	// ownership whatever the account behind it is allowed to grant, so the worst a
	// misconfiguration or a leak produces is a participant.
	code, response, body := ask(t, h.server.JoinHandler(false), modKey, server.JoinRequest{
		Tenant: tenant, Username: "carol", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	claims := scopeOf(t, response.Token)
	if claims.MOQT.Capabilities != 0 {
		t.Errorf("token carries capabilities %#x, want none", claims.MOQT.Capabilities)
	}
	if claims.MOQT.Owner {
		t.Error("token claims ownership, which this endpoint must never mint")
	}
}

func TestACredentialThatCouldPublishNothingIsRefusedRatherThanServed(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// An operator's account, in the shape a real deployment writes one: `is_owner`
	// and no publish list, because ownership covers everything the admin API asks
	// about. This endpoint deliberately does not use ownership, so minting against
	// it would produce a token that is valid, admitted, and permitted no media --
	// a phone that joins, looks connected, and carries nothing. That is the exact
	// failure this whole path exists to remove, arriving by a new route, so it is a
	// refusal with the reason attached. Found by pointing the running endpoint at
	// the admin credential and reading `pub = 0` off the token it happily returned.
	code, _, body := ask(t, h.server.JoinHandler(false), barrenKey, server.JoinRequest{
		Tenant: tenant, Username: "carol", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusForbidden {
		t.Fatalf("an account with no publish rights got %d (%s), want 403", code, body)
	}
	// The reason has to name the fix. This one is read by whoever set the
	// deployment up, not by an attacker.
	if !strings.Contains(body, "publish") {
		t.Errorf("the refusal does not say what is wrong: %q", body)
	}
}

func TestJoiningNeedsACredential(t *testing.T) {
	h := newHarness(t)
	handler := h.server.JoinHandler(false)

	for _, header := range []string{"", "Bearer", "Bearer ", "not-a-credential", "Bearer wrong"} {
		request := httptest.NewRequest(http.MethodPost, server.JoinPath, strings.NewReader(`{}`))
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q got %d, want 401", header, recorder.Code)
		}
		// Nothing about why. A caller learning the difference between "no such
		// credential" and "that credential cannot do this" learns which of its
		// guesses was closer.
		if strings.Contains(recorder.Body.String(), "account") {
			t.Errorf("the refusal describes the account: %q", recorder.Body.String())
		}
	}
}

func TestARelayCredentialCannotMintADeviceToken(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// A relay's credential is the one most widely deployed and least like a
	// person's: every relay in the fleet holds it. It is refused on the admin API
	// and it is refused here, rather than being refused here only because a later
	// check happens to run.
	code, _, _ := ask(t, h.server.JoinHandler(false), relayKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusUnauthorized {
		t.Errorf("a relay credential got %d, want 401", code)
	}
}

func TestADeviceCannotReachAnotherTenant(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, _, body := ask(t, h.server.JoinHandler(true), joinKey, server.JoinRequest{
		Tenant: "somebody-else", Username: "alice", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusForbidden {
		t.Errorf("crossing tenants got %d (%s), want 403", code, body)
	}
}

func TestAJoinWithoutADeviceIsRefused(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, _, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName,
	})
	if code != http.StatusBadRequest {
		t.Errorf("a join with no device got %d (%s), want 400", code, body)
	}
}

// The binding that makes a token more than a label: `cnf.jkt` has to be the
// thumbprint of a key the holder can sign with, and the only way to know that
// is to derive it here from the key rather than take a name from the request.
func TestATokenIsBoundToTheKeyTheDeviceSent(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Key: &deviceKey,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	if got := scopeOf(t, response.Token).Confirmation.Thumbprint; got != deviceKeyThumbprint {
		t.Errorf("cnf.jkt = %q, want the thumbprint of the key sent: %q", got, deviceKeyThumbprint)
	}
	// Echoed, so a caller can see which key it was bound to instead of decoding
	// the credential to find out.
	if response.Device != deviceKeyThumbprint {
		t.Errorf("device = %q, want %q", response.Device, deviceKeyThumbprint)
	}
}

// A device may send the truncated thumbprint it knows itself by alongside the
// key, and the two must describe one device.
func TestADeviceMaySendBothWhenTheyAgree(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, response, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName,
		Device: deviceKeyThumbprint[:16], Key: &deviceKey,
	})
	if code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}
	if response.Device != deviceKeyThumbprint {
		t.Errorf("device = %q, want the untruncated %q", response.Device, deviceKeyThumbprint)
	}
}

// The two readings of a disagreement -- a confused client, and one asking for a
// token bound to somebody else's key -- are not distinguishable from here, and
// neither is worth serving.
func TestADeviceThatDisagreesWithItsOwnKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	code, _, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName,
		Device: deviceThumbprint, Key: &deviceKey,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("a device naming another key got %d (%s), want 400", code, body)
	}
	if !strings.Contains(body, "thumbprint") {
		t.Errorf("the refusal does not say what disagrees: %q", body)
	}
}

func TestAKeyThatCannotBeBoundToIsRefused(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	// A curve the Secure Enclave does not do and the relay does not verify. A
	// token bound to this would be one nobody could ever prove possession of.
	wrong := token.PublicKey{Kty: "EC", Crv: "P-384", X: deviceKey.X, Y: deviceKey.Y}
	code, _, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Key: &wrong,
	})
	if code != http.StatusBadRequest {
		t.Errorf("a P-384 key got %d (%s), want 400", code, body)
	}
}

func TestAnUnknownRoomIsRefusedUnlessTheDeploymentSaysOtherwise(t *testing.T) {
	h := newHarness(t)

	// Off: the room has to exist, and a device naming one that does not gets a
	// 404 it can display rather than a room nobody meant to create.
	code, _, body := ask(t, h.server.JoinHandler(false), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: "not-a-room", Device: deviceThumbprint,
	})
	if code != http.StatusNotFound {
		t.Errorf("an unknown room got %d (%s), want 404 with creation off", code, body)
	}

	// On: somebody typed a room name on a phone, which is the demo.
	code, response, body := ask(t, h.server.JoinHandler(true), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: "not-a-room", Device: deviceThumbprint,
	})
	if code != http.StatusOK {
		t.Fatalf("an unknown room got %d (%s), want a created room", code, body)
	}
	if scopeOf(t, response.Token).MOQT.Room != "not-a-room" {
		t.Error("the token is not scoped to the room that was asked for")
	}
}

func TestJoiningAnExistingRoomDoesNotRewriteIt(t *testing.T) {
	h := newHarness(t)
	created := h.createRoom(t, "alice")

	// Creation is allowed here, which is what makes this worth asserting: the
	// path that can create a room must not be a path that can quietly reset the
	// policy or ownership of one that is already running.
	if code, _, body := ask(t, h.server.JoinHandler(true), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "bob", Room: roomName, Device: deviceThumbprint,
	}); code != http.StatusOK {
		t.Fatalf("joining: %d %s", code, body)
	}

	after, err := h.store.GetRoom(context.Background(), tenant, roomName)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	// Read through the store rather than the API because the fields worth
	// checking are the ones a recreation would reset and the API does not return:
	// who owns the room, and when it started existing.
	if after.Owner != "alice" {
		t.Errorf("owner is %q, want alice — the room was rewritten", after.Owner)
	}
	if !after.CreatedAt.Equal(created.GetCreatedAt().AsTime()) {
		t.Errorf("created at %s, was %s — the room was replaced",
			after.CreatedAt, created.GetCreatedAt().AsTime())
	}
}

func TestAClosedRoomIsRefusedRatherThanReopened(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	if _, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{Tenant: tenant, Room: roomName}); err != nil {
		t.Fatalf("closing: %v", err)
	}

	// With creation on, so the interesting failure is possible: a naive
	// implementation looks up the room, does not like what it finds, and makes a
	// new one over the top.
	code, _, body := ask(t, h.server.JoinHandler(true), joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
	})
	if code != http.StatusConflict {
		t.Errorf("a closed room got %d (%s), want 409", code, body)
	}
}

func TestOnlyPostIsAnswered(t *testing.T) {
	h := newHarness(t)
	handler := h.server.JoinHandler(false)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		request := httptest.NewRequest(method, server.JoinPath, nil)
		request.Header.Set("Authorization", "Bearer "+joinKey)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		// A GET in particular: a credential in a query string is a credential in
		// every access log between here and the phone, and answering GET at all
		// invites somebody to build that.
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s got %d, want 405", method, recorder.Code)
		}
	}
}

func TestOneCredentialCannotFloodTheEndpoint(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	handler := h.server.JoinHandler(false)

	// The clock is frozen in the harness, so this is one window by construction
	// and the test is about the ceiling rather than about timing.
	refused := 0
	for i := 0; i < 60; i++ {
		code, _, _ := ask(t, handler, joinKey, server.JoinRequest{
			Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
		})
		if code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("sixty requests in one minute were all served, so nothing bounds the endpoint")
	}

	// And the window reopens, because the alternative is a phone locked out for
	// the rest of the process's life over a retry loop that has since stopped.
	h.advance(time.Minute)
	if code, _, body := ask(t, handler, joinKey, server.JoinRequest{
		Tenant: tenant, Username: "alice", Room: roomName, Device: deviceThumbprint,
	}); code != http.StatusOK {
		t.Errorf("after the window, a join got %d (%s)", code, body)
	}
}

func TestTheResponseIsNeverCached(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")

	request := httptest.NewRequest(http.MethodPost, server.JoinPath,
		strings.NewReader(`{"tenant":"`+tenant+`","username":"alice","room":"`+roomName+`","device":"`+deviceThumbprint+`"}`))
	request.Header.Set("Authorization", "Bearer "+joinKey)
	recorder := httptest.NewRecorder()
	h.server.JoinHandler(false).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("joining: %d %s", recorder.Code, recorder.Body.String())
	}
	// A token in an intermediary's cache is a token handed to whoever asks next.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q, want no-store", got)
	}
}
