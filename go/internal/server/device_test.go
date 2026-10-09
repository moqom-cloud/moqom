package server_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom-security/go/proof"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// deviceThumbprint is a well-formed thumbprint of no key anybody holds.
const deviceThumbprint = "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM"

// deviceKey is an ML-DSA-87 key generated once for the package's tests.
var deviceKey, deviceKeyThumbprint = func() (token.PublicKey, string) {
	device, err := proof.GenerateProver(proof.MLDSA87)
	if err != nil {
		panic(err)
	}
	return device.JWK(), device.Thumbprint()
}()

// mintFor mints a participant token for deviceKey through the admin API, as a
// customer's backend does.
func (h *harness) mintFor(t *testing.T, username, room string) *adminpb.MintClientTokenResponse {
	t.Helper()
	minted, err := h.tokens.MintClientToken(as(joinKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: username, DeviceThumbprint: deviceKeyThumbprint, Room: room,
		Grants: &adminpb.Grants{Publish: []adminpb.PublishScope{
			adminpb.PublishScope_PUBLISH_SCOPE_AUDIO, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO,
		}},
	})
	if err != nil {
		t.Fatalf("minting for %s: %v", username, err)
	}
	return minted
}

// scopeOf reads the claims out of a minted token without verifying it: what is
// under test is what the control plane put in the token.
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
