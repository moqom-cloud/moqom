package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	sectoken "github.com/moqom-cloud/moqom-security/go/token"
	"github.com/moqom-cloud/moqom/go/internal/apple"
	"github.com/moqom-cloud/moqom/go/internal/apple/appletest"
	"github.com/moqom-cloud/moqom/go/internal/applesignin"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	appleTeam   = "ABCDE12345"
	appleBundle = "cloud.moqom.room"
	appleApp    = appleTeam + "." + appleBundle
	appleUser   = "001234.abcdef"
)

type appleFixture struct {
	*harness
	seal      sectoken.SealKey
	authority appletest.Authority
	signIn    *appletest.SignIn
	device    appletest.Device
	public    http.Handler
}

func newAppleFixture(t *testing.T) *appleFixture {
	t.Helper()
	seal, err := sectoken.GenerateSealKey()
	if err != nil {
		t.Fatal(err)
	}
	signIn := appletest.NewSignIn(t)
	identity := apple.NewIdentityVerifier()
	identity.KeysURL = signIn.KeysURL
	f := &appleFixture{seal: seal, signIn: signIn, device: appletest.NewDevice(t)}
	f.harness = newConfiguredHarness(t, func(config *server.Config) {
		f.authority = appletest.NewAuthority(t, config.Now())
		config.SealKeys = []sectoken.SealKey{seal}
		config.AppleSignIn = &server.AppleSignInConfig{
			Settings: applesignin.NewMemory(),
			Identity: identity,
			Roots:    f.authority.Roots,
		}
	})
	f.public = f.server.PublicHandler()
	f.createRoom(t, "alice")
	if _, err := f.tokens.SetAppleSignIn(as(ownerKey), &adminpb.SetAppleSignInRequest{
		Tenant: tenant, TeamId: appleTeam, BundleIds: []string{appleBundle}, Rooms: []string{roomName},
		Publish: []adminpb.PublishScope{adminpb.PublishScope_PUBLISH_SCOPE_AUDIO, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO},
	}); err != nil {
		t.Fatalf("turning Apple sign-in on: %v", err)
	}
	return f
}

func (f *appleFixture) identityToken(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": apple.Issuer, "aud": appleBundle, "sub": appleUser,
		"iat": f.now().Unix(), "exp": f.now().Add(10 * time.Minute).Unix(), "nonce": deviceKeyThumbprint,
	}
	if change != nil {
		change(claims)
	}
	return f.signIn.IdentityToken(t, claims)
}

func (f *appleFixture) signInRequest(t *testing.T, identityToken string, spoil appletest.Spoil) server.AppleSignInRequest {
	t.Helper()
	challenge := sha256.Sum256([]byte(identityToken))
	key := deviceKey
	return server.AppleSignInRequest{
		Tenant:        tenant,
		IdentityToken: identityToken,
		Attestation:   base64.StdEncoding.EncodeToString(f.authority.Attest(t, f.device, challenge[:], appleApp, spoil)),
		AttestKeyID:   f.device.KeyID,
		Key:           &key,
		Room:          roomName,
	}
}

func (f *appleFixture) post(t *testing.T, path string, body any, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	for name, values := range header {
		request.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	f.public.ServeHTTP(recorder, request)
	return recorder
}

func (f *appleFixture) signInOK(t *testing.T) server.AppleSignInResponse {
	t.Helper()
	recorder := f.post(t, server.AppleSignInPath, f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{}), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("signing in: %d %s", recorder.Code, recorder.Body)
	}
	var signedIn server.AppleSignInResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &signedIn); err != nil {
		t.Fatal(err)
	}
	return signedIn
}

// renewal is a renewal body and the headers that go with it, asserted by
// device at counter.
func (f *appleFixture) renewal(t *testing.T, enrollment string, device appletest.Device, counter uint32) ([]byte, http.Header) {
	t.Helper()
	key := deviceKey
	body, err := json.Marshal(server.AppleTokenRequest{Key: &key, Room: roomName})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return body, http.Header{
		"Authorization":             {"Bearer " + enrollment},
		server.AppleAssertionHeader: {base64.StdEncoding.EncodeToString(device.Assert(t, digest[:], appleApp, counter))},
	}
}

// A person signs in once with Apple and App Attest, and renews with the
// enrollment and an assertion from the same app on the same device. What they
// get is a participant in a room that exists, with the publish rights the
// owner chose and nothing more.
func TestAppleSignInThenRenew(t *testing.T) {
	f := newAppleFixture(t)

	signedIn := f.signInOK(t)
	if signedIn.Enrollment == "" || !signedIn.EnrollmentExpiresAt.After(f.now().Add(29*24*time.Hour)) {
		t.Errorf("enrollment %q until %v", signedIn.Enrollment, signedIn.EnrollmentExpiresAt)
	}
	opened, err := sectoken.Open(signedIn.Token, f.seal)
	if err != nil {
		t.Fatalf("the token is not sealed for the deployment: %v", err)
	}
	scope := scopeOf(t, opened)
	if !strings.HasPrefix(scope.Subject, "apple-") || scope.Subject != signedIn.Username || strings.Contains(scope.Subject, appleUser) {
		t.Errorf("username %q, response %q", scope.Subject, signedIn.Username)
	}
	if scope.MOQT.Room != roomName || scope.MOQT.Publish != 0b11 || scope.MOQT.Capabilities != 0 || scope.MOQT.Owner {
		t.Errorf("granted %+v", scope.MOQT)
	}
	if scope.Confirmation.Thumbprint != deviceKeyThumbprint {
		t.Errorf("bound to %q", scope.Confirmation.Thumbprint)
	}

	body, header := f.renewal(t, signedIn.Enrollment, f.device, 1)
	if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusOK {
		t.Fatalf("renewing: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusUnauthorized {
		t.Errorf("a replayed renewal gave %d", recorder.Code)
	}
	body, header = f.renewal(t, signedIn.Enrollment, f.device, 2)
	if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusOK {
		t.Errorf("the next renewal: %d %s", recorder.Code, recorder.Body)
	}
}

func TestAppleSignInRefusals(t *testing.T) {
	f := newAppleFixture(t)

	for name, test := range map[string]struct {
		request func() server.AppleSignInRequest
		want    int
	}{
		"a nonce that is not the device key": {func() server.AppleSignInRequest {
			return f.signInRequest(t, f.identityToken(t, func(c map[string]any) { c["nonce"] = deviceThumbprint }), appletest.Spoil{})
		}, http.StatusUnauthorized},
		"an identity token for another app": {func() server.AppleSignInRequest {
			return f.signInRequest(t, f.identityToken(t, func(c map[string]any) { c["aud"] = "com.example.other" }), appletest.Spoil{})
		}, http.StatusUnauthorized},
		"an attestation over another identity token": {func() server.AppleSignInRequest {
			other := sha256.Sum256([]byte("another identity token"))
			return f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{NonceFor: other[:]})
		}, http.StatusUnauthorized},
		"a development key in production": {func() server.AppleSignInRequest {
			return f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{AAGUID: appletest.AAGUIDDevelopment})
		}, http.StatusUnauthorized},
		"a room the owner did not name": {func() server.AppleSignInRequest {
			request := f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{})
			request.Room = "backstage"
			return request
		}, http.StatusForbidden},
		"a tenant without Apple sign-in": {func() server.AppleSignInRequest {
			request := f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{})
			request.Tenant = "globex"
			return request
		}, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if recorder := f.post(t, server.AppleSignInPath, test.request(), nil); recorder.Code != test.want {
				t.Errorf("got %d %s, want %d", recorder.Code, recorder.Body, test.want)
			}
		})
	}
}

func TestAppleRenewalRefusals(t *testing.T) {
	f := newAppleFixture(t)
	signedIn := f.signInOK(t)

	t.Run("no assertion", func(t *testing.T) {
		body, header := f.renewal(t, signedIn.Enrollment, f.device, 1)
		header.Del(server.AppleAssertionHeader)
		if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusUnauthorized {
			t.Errorf("got %d", recorder.Code)
		}
	})
	t.Run("asserted by another device", func(t *testing.T) {
		body, header := f.renewal(t, signedIn.Enrollment, appletest.NewDevice(t), 1)
		if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusUnauthorized {
			t.Errorf("got %d", recorder.Code)
		}
	})
	t.Run("a forged enrollment", func(t *testing.T) {
		forged := []byte(signedIn.Enrollment)
		forged[len(forged)/2] ^= 'A' ^ 'B'
		body, header := f.renewal(t, string(forged), f.device, 1)
		if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusUnauthorized {
			t.Errorf("got %d", recorder.Code)
		}
	})
	t.Run("another device key", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"key": map[string]string{"kty": "AKP", "alg": "ML-DSA-87", "pub": "AAAA"}, "room": roomName})
		digest := sha256.Sum256(raw)
		header := http.Header{
			"Authorization":             {"Bearer " + signedIn.Enrollment},
			server.AppleAssertionHeader: {base64.StdEncoding.EncodeToString(f.device.Assert(t, digest[:], appleApp, 1))},
		}
		if recorder := f.post(t, server.AppleTokenPath, raw, header); recorder.Code < 400 {
			t.Errorf("got %d", recorder.Code)
		}
	})
	t.Run("after the enrollment expires", func(t *testing.T) {
		f.advance(31 * 24 * time.Hour)
		body, header := f.renewal(t, signedIn.Enrollment, f.device, 5)
		if recorder := f.post(t, server.AppleTokenPath, body, header); recorder.Code != http.StatusUnauthorized {
			t.Errorf("got %d", recorder.Code)
		}
	})
}

// Only an owner key decides who strangers become in the tenant's rooms, and
// only for rooms that exist.
func TestAppleSignInSettingsAreForOwners(t *testing.T) {
	f := newAppleFixture(t)
	request := &adminpb.SetAppleSignInRequest{
		Tenant: tenant, TeamId: appleTeam, BundleIds: []string{appleBundle}, Rooms: []string{roomName},
	}
	for _, key := range []string{joinKey, modKey} {
		if _, err := f.tokens.SetAppleSignIn(as(key), request); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s gave %v", key, err)
		}
	}
	request.Rooms = []string{"nowhere"}
	if _, err := f.tokens.SetAppleSignIn(as(ownerKey), request); status.Code(err) != codes.NotFound {
		t.Errorf("a room that does not exist gave %v", err)
	}
	request.Rooms, request.TeamId = []string{roomName}, "nope"
	if _, err := f.tokens.SetAppleSignIn(as(ownerKey), request); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed team gave %v", err)
	}

	got, err := f.tokens.GetAppleSignIn(as(modKey), &adminpb.GetAppleSignInRequest{Tenant: tenant})
	if err != nil || got.GetTeamId() != appleTeam || got.GetSignInPath() != server.AppleSignInPath {
		t.Errorf("reading the settings: %+v, %v", got, err)
	}

	if _, err := f.tokens.SetAppleSignIn(as(ownerKey), &adminpb.SetAppleSignInRequest{Tenant: tenant}); err != nil {
		t.Fatalf("turning it off: %v", err)
	}
	recorder := f.post(t, server.AppleSignInPath, f.signInRequest(t, f.identityToken(t, nil), appletest.Spoil{}), nil)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("signing in after it was turned off gave %d", recorder.Code)
	}
}
