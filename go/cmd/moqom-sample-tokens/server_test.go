package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"
	"github.com/moqom-cloud/moqom-security/go/proof"
	appleid "github.com/moqom-cloud/moqom/go/internal/apple"
)

const audience = "cloud.moqom.room"

var b64 = base64.RawURLEncoding

type fakeApple struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newFakeApple(t *testing.T) *fakeApple {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	apple := &fakeApple{key: key}
	apple.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "apple-1", "alg": "RS256", "use": "sig",
			"n": b64.EncodeToString(key.N.Bytes()),
			"e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(apple.server.Close)
	return apple
}

func (a *fakeApple) identityToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "apple-1"})
	payload, _ := json.Marshal(claims)
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64.EncodeToString(signature)
}

type fakeMinter struct {
	asked []moqom.TokenRequest
	err   error
}

func (m *fakeMinter) MintToken(_ context.Context, request moqom.TokenRequest) (moqom.Token, error) {
	m.asked = append(m.asked, request)
	if m.err != nil {
		return moqom.Token{}, m.err
	}
	return moqom.Token{
		Token:     "sealed-token",
		ExpiresAt: time.Unix(2_000_000_900, 0).UTC(),
		Identity:  moqom.Identity{Username: request.Username, Device: request.DeviceThumbprint[:16]},
		KeyID:     "k1",
	}, nil
}

func deviceKey(t *testing.T) (proof.JWK, string) {
	t.Helper()
	pub := make([]byte, mldsa.MLDSA87PublicKeySize)
	_, _ = rand.Read(pub)
	key := proof.JWK{Kty: "AKP", Alg: "ML-DSA-87", Pub: b64.EncodeToString(pub)}
	thumbprint, err := key.Thumbprint()
	if err != nil {
		t.Fatal(err)
	}
	return key, thumbprint
}

type fixture struct {
	apple  *fakeApple
	minter *fakeMinter
	server *server
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	apple := newFakeApple(t)
	verifier := newAppleVerifier([]string{audience})
	verifier.inner.KeysURL = apple.server.URL
	sealer, err := newEnrollmentSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	minter := &fakeMinter{}
	s := newServer(minter, verifier, sealer, "moqom", []string{"demo"})
	f := &fixture{apple: apple, minter: minter, server: s, now: time.Unix(2_000_000_000, 0)}
	s.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) claims(nonce string) map[string]any {
	return map[string]any{
		"iss": appleid.Issuer, "aud": audience, "sub": "001234.abcdef",
		"iat": f.now.Unix(), "exp": f.now.Add(10 * time.Minute).Unix(), "nonce": nonce,
	}
}

func (f *fixture) post(t *testing.T, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	f.server.routes().ServeHTTP(recorder, request)
	return recorder
}

func TestSignInThenRenew(t *testing.T) {
	f := newFixture(t)
	key, thumbprint := deviceKey(t)

	signedIn := f.post(t, "/v1/sign-in", "", map[string]any{
		"identityToken": f.apple.identityToken(t, f.claims(thumbprint)), "key": key, "room": "demo",
	})
	if signedIn.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", signedIn.Code, signedIn.Body)
	}
	var first tokenResponse
	if err := json.Unmarshal(signedIn.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Token != "sealed-token" || first.Tenant != "moqom" || first.Device != thumbprint || first.Enrollment == "" || first.EnrollmentExpiresAt == nil {
		t.Fatalf("sign-in response: %+v", first)
	}
	if len(first.Publish) != 4 {
		t.Fatalf("publish: %v", first.Publish)
	}
	asked := f.minter.asked[0]
	if asked.Publish != moqom.PublishFull || asked.Moderation != 0 || asked.Owner || asked.Room != "demo" {
		t.Fatalf("minted with %+v", asked)
	}
	if strings.Contains(asked.Username, "001234") || !strings.HasPrefix(asked.Username, "apple-") {
		t.Fatalf("username %q leaks or misses the prefix", asked.Username)
	}

	renewed := f.post(t, "/v1/token", first.Enrollment, map[string]any{"key": key, "room": "demo"})
	if renewed.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", renewed.Code, renewed.Body)
	}
	var second tokenResponse
	_ = json.Unmarshal(renewed.Body.Bytes(), &second)
	if second.Enrollment != "" || second.Username != first.Username {
		t.Fatalf("renew response: %+v", second)
	}
}

func TestSignInRefusals(t *testing.T) {
	f := newFixture(t)
	key, thumbprint := deviceKey(t)
	other, _ := deviceKey(t)

	cases := map[string]struct {
		claims map[string]any
		key    proof.JWK
		room   string
		token  func(string) string
		want   int
	}{
		"nonce names another key": {claims: f.claims("someone-else"), key: key, room: "demo", want: http.StatusUnauthorized},
		"key differs from nonce":  {claims: f.claims(thumbprint), key: other, room: "demo", want: http.StatusUnauthorized},
		"wrong audience": {claims: func() map[string]any {
			c := f.claims(thumbprint)
			c["aud"] = "com.example.other"
			return c
		}(), key: key, room: "demo", want: http.StatusUnauthorized},
		"wrong issuer": {claims: func() map[string]any {
			c := f.claims(thumbprint)
			c["iss"] = "https://example.com"
			return c
		}(), key: key, room: "demo", want: http.StatusUnauthorized},
		"expired": {claims: func() map[string]any {
			c := f.claims(thumbprint)
			c["exp"] = f.now.Add(-time.Hour).Unix()
			return c
		}(), key: key, room: "demo", want: http.StatusUnauthorized},
		"tampered": {claims: f.claims(thumbprint), key: key, room: "demo", want: http.StatusUnauthorized,
			token: func(token string) string { return token[:len(token)-4] + "AAAA" }},
		"room not on the list": {claims: f.claims(thumbprint), key: key, room: "elsewhere", want: http.StatusNotFound},
		"not an ML-DSA key":    {claims: f.claims(thumbprint), key: proof.JWK{Kty: "EC"}, room: "demo", want: http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			token := f.apple.identityToken(t, c.claims)
			if c.token != nil {
				token = c.token(token)
			}
			got := f.post(t, "/v1/sign-in", "", map[string]any{"identityToken": token, "key": c.key, "room": c.room})
			if got.Code != c.want {
				t.Fatalf("got %d %s, want %d", got.Code, got.Body, c.want)
			}
		})
	}
}

func TestTokenRefusals(t *testing.T) {
	f := newFixture(t)
	key, thumbprint := deviceKey(t)
	other, _ := deviceKey(t)

	enrolled, err := f.server.sealer.seal(enrollment{Subject: "s", Thumbprint: thumbprint, Expires: f.now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := f.server.sealer.seal(enrollment{Subject: "s", Thumbprint: thumbprint, Expires: f.now.Add(-time.Second).Unix()})
	foreign, _ := newEnrollmentSealer(bytes.Repeat([]byte{8}, 32))
	forged, _ := foreign.seal(enrollment{Subject: "s", Thumbprint: thumbprint, Expires: f.now.Add(time.Hour).Unix()})

	for name, c := range map[string]struct {
		bearer string
		key    proof.JWK
		want   int
	}{
		"no enrollment":      {bearer: "", key: key, want: http.StatusUnauthorized},
		"expired enrollment": {bearer: expired, key: key, want: http.StatusUnauthorized},
		"forged enrollment":  {bearer: forged, key: key, want: http.StatusUnauthorized},
		"another device key": {bearer: enrolled, key: other, want: http.StatusUnauthorized},
		"garbage":            {bearer: "not-base64!", key: key, want: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			got := f.post(t, "/v1/token", c.bearer, map[string]any{"key": c.key, "room": "demo"})
			if got.Code != c.want {
				t.Fatalf("got %d %s, want %d", got.Code, got.Body, c.want)
			}
		})
	}
	if len(f.minter.asked) != 0 {
		t.Fatalf("minted %d tokens for refused requests", len(f.minter.asked))
	}
}

func TestRateLimitPerPerson(t *testing.T) {
	f := newFixture(t)
	key, thumbprint := deviceKey(t)
	enrolled, _ := f.server.sealer.seal(enrollment{Subject: "s", Thumbprint: thumbprint, Expires: f.now.Add(time.Hour).Unix()})

	for range mintsPerWindow {
		if got := f.post(t, "/v1/token", enrolled, map[string]any{"key": key, "room": "demo"}); got.Code != http.StatusOK {
			t.Fatalf("got %d before the limit", got.Code)
		}
	}
	if got := f.post(t, "/v1/token", enrolled, map[string]any{"key": key, "room": "demo"}); got.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d past the limit", got.Code)
	}
	f.now = f.now.Add(mintWindow)
	if got := f.post(t, "/v1/token", enrolled, map[string]any{"key": key, "room": "demo"}); got.Code != http.StatusOK {
		t.Fatalf("got %d in the next window", got.Code)
	}
}

func TestMissingRoomAtControlPlane(t *testing.T) {
	f := newFixture(t)
	f.minter.err = moqom.ErrNotFound
	key, thumbprint := deviceKey(t)
	enrolled, _ := f.server.sealer.seal(enrollment{Subject: "s", Thumbprint: thumbprint, Expires: f.now.Add(time.Hour).Unix()})
	if got := f.post(t, "/v1/token", enrolled, map[string]any{"key": key, "room": "demo"}); got.Code != http.StatusNotFound {
		t.Fatalf("got %d", got.Code)
	}
}
