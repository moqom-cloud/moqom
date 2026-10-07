package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

func testKey(t *testing.T) (Key, *Verifier) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return key, NewVerifier("moqom", []Key{key})
}

func claimsFor(grants control.Grants) Claims {
	return Claims{
		Issuer:       "moqom",
		Subject:      "alice",
		Audience:     "acme",
		Confirmation: Confirmation{Thumbprint: "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM"},
		MOQT:         ScopeFrom(grants, "room-42", false, false),
	}
}

func TestRoundTrip(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	grants := control.Grants{Publish: control.PublishCamera, Moderation: control.CapChatGroup}

	minted, expires, err := Mint(key, claimsFor(grants), 5*time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if !expires.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("expiry is %s, want %s", expires, now.Add(5*time.Minute))
	}

	claims, err := verifier.Verify(minted, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}

	recovered, err := claims.MOQT.Grants()
	if err != nil {
		t.Fatalf("reading grants: %v", err)
	}
	if recovered != grants {
		t.Errorf("grants came back as %+v, want %+v", recovered, grants)
	}

	// The device is derived, never carried, so a token cannot name a device its
	// key does not imply -- which is what makes a device ban match it.
	identity, err := claims.Identity()
	if err != nil {
		t.Fatalf("deriving identity: %v", err)
	}
	if identity.Username != "alice" {
		t.Errorf("username is %q", identity.Username)
	}
	if len(identity.Device) != control.DeviceIDLength {
		t.Errorf("device is %q, want %d characters", identity.Device, control.DeviceIDLength)
	}
	if !strings.HasPrefix(claims.Confirmation.Thumbprint, identity.Device) {
		t.Error("the device is not a prefix of the thumbprint it was derived from")
	}
}

// The relay-link grant round-trips through the strict decoder, and a scope that
// does not hold it says nothing about it at all: every token minted before the
// field existed is byte for byte what it was.
func TestARelayLinkGrantRoundTripsAndIsAbsentOtherwise(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	link := claimsFor(control.Grants{})
	link.MOQT.Relay = true
	minted, _, err := Mint(key, link, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	claims, err := verifier.Verify(minted, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !claims.MOQT.Relay {
		t.Error("the relay-link grant did not survive verification")
	}

	ordinary, _, err := Mint(key, claimsFor(control.Grants{Publish: control.PublishCamera}), 5*time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(ordinary, ".")[1])
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if strings.Contains(string(payload), `"rly"`) {
		t.Errorf("an ordinary token mentions the relay-link grant: %s", payload)
	}
	if ScopeFrom(control.Grants{IsOwner: true}, "room-42", false, false).Relay {
		t.Error("ScopeFrom, which the backend path mints through, produced a relay link")
	}
}

// The classic JWS forgery: change `alg` to something the verifier will accept on
// weaker terms. A verifier that reads the algorithm out of the token and dispatches
// on it can be told to use "none", or to verify an HMAC with the public key as the
// shared secret. Both validate. Pinning one algorithm is the fix.
func TestAlgorithmConfusionIsRefused(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Now()

	minted, _, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	parts := strings.Split(minted, ".")

	for _, algorithm := range []string{"none", "HS256", "RS256", ""} {
		forged := encodeSegment(t, header{Algorithm: algorithm, Type: tokenType, KeyID: key.ID})
		_, err := verifier.Verify(forged+"."+parts[1]+"."+parts[2], now)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("alg=%q gave %v, want ErrUnsupported", algorithm, err)
		}
	}
}

// A token minted for some other purpose in the same deployment must not be
// replayable at a relay, so the type is checked as well as the algorithm.
func TestForeignTokenTypeIsRefused(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Now()

	minted, _, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	parts := strings.Split(minted, ".")
	forged := encodeSegment(t, header{Algorithm: key.Algorithm, Type: "JWT", KeyID: key.ID})

	if _, err := verifier.Verify(forged+"."+parts[1]+"."+parts[2], now); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a non-MOQT token gave %v, want ErrUnsupported", err)
	}
}

// Editing the claims must invalidate the signature. This is the property
// everything else rests on, so it is asserted directly rather than assumed.
func TestTamperedClaimsFailVerification(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Now()

	minted, _, err := Mint(key, claimsFor(control.Grants{Publish: control.PublishAudio}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	parts := strings.Split(minted, ".")

	var claims Claims
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		t.Fatalf("parsing claims: %v", err)
	}

	// Promote themselves to owner, which is the escalation an attacker wants.
	claims.MOQT.Owner = true
	claims.MOQT.Capabilities = uint32(control.CapAll)
	tampered := encodeSegment(t, claims)

	if _, err := verifier.Verify(parts[0]+"."+tampered+"."+parts[2], now); !errors.Is(err, ErrSignature) {
		t.Errorf("a self-promotion gave %v, want ErrSignature", err)
	}
}

func TestUnknownKeyIsRefused(t *testing.T) {
	key, _ := testKey(t)
	_, otherVerifier := testKey(t)
	now := time.Now()

	minted, _, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if _, err := otherVerifier.Verify(minted, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a token from an unknown key gave %v, want ErrUnknownKey", err)
	}
}

// Rotation needs an overlap: the retiring key must stay verifiable until the last
// token it signed has expired, or rolling a key is an outage.
func TestBothKeysVerifyDuringRotation(t *testing.T) {
	retiring, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	incoming, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	verifier := NewVerifier("moqom", []Key{retiring, incoming})
	now := time.Now()

	for _, key := range []Key{retiring, incoming} {
		minted, _, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
		if err != nil {
			t.Fatalf("minting with %s: %v", key.ID, err)
		}
		if _, err := verifier.Verify(minted, now); err != nil {
			t.Errorf("a token from %s did not verify: %v", key.ID, err)
		}
	}
}

func TestExpiryIsEnforcedAndSkewIsBounded(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	minted, expires, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	// Just past expiry, inside the skew allowance: still accepted, because that
	// is two machines disagreeing about the time.
	if _, err := verifier.Verify(minted, expires.Add(clockSkew/2)); err != nil {
		t.Errorf("a token inside the skew allowance was refused: %v", err)
	}
	// Well past it: refused. Skew tolerance is not an extension.
	if _, err := verifier.Verify(minted, expires.Add(clockSkew*2)); !errors.Is(err, ErrExpired) {
		t.Errorf("an expired token gave %v, want ErrExpired", err)
	}
}

// A lifetime beyond the cap is refused rather than clamped. Quietly issuing
// something weaker than asked for is how a caller ends up believing it has an
// hour-long token and refreshing every hour.
func TestOverlongLifetimeIsRefusedNotClamped(t *testing.T) {
	key, _ := testKey(t)
	now := time.Now()

	if _, _, err := Mint(key, claimsFor(control.Grants{}), 24*time.Hour, now); !errors.Is(err, control.ErrInvalid) {
		t.Errorf("a day-long lifetime gave %v, want ErrInvalid", err)
	}
	if _, _, err := Mint(key, claimsFor(control.Grants{}), MaxLifetime, now); err != nil {
		t.Errorf("the maximum lifetime was refused: %v", err)
	}
}

func TestTokenNeedsSubjectAndDevice(t *testing.T) {
	key, _ := testKey(t)
	now := time.Now()

	anonymous := claimsFor(control.Grants{})
	anonymous.Subject = ""
	if _, _, err := Mint(key, anonymous, time.Minute, now); !errors.Is(err, control.ErrInvalid) {
		t.Error("a token with no subject was minted")
	}

	unbound := claimsFor(control.Grants{})
	unbound.Confirmation.Thumbprint = ""
	if _, _, err := Mint(key, unbound, time.Minute, now); !errors.Is(err, control.ErrInvalid) {
		t.Error("a token with no device binding was minted")
	}
}

// An unrecognised capability bit is refused on both paths. Accepted and dropped,
// a token from a newer control plane would grant less than it says, with no error
// anywhere to explain why a power went missing.
func TestUnknownCapabilityBitsAreRefused(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Now()

	future := claimsFor(control.Grants{})
	future.MOQT.Capabilities = uint32(control.CapKick) | (1 << 31)

	if _, _, err := Mint(key, future, time.Minute, now); !errors.Is(err, ErrUnsupported) {
		t.Errorf("minting with an unknown bit gave %v, want ErrUnsupported", err)
	}

	// And on the way in, for a token minted elsewhere.
	valid := claimsFor(control.Grants{})
	minted, _, err := Mint(key, valid, time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	parts := strings.Split(minted, ".")
	var claims Claims
	decoded, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(decoded, &claims); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	claims.MOQT.Capabilities = 1 << 31
	resigned, _, err := Mint(key, claims, time.Minute, now)
	if err == nil {
		if _, err := verifier.Verify(resigned, now); !errors.Is(err, ErrUnsupported) {
			t.Errorf("verifying an unknown bit gave %v, want ErrUnsupported", err)
		}
	}
}

func TestMalformedTokensAreRejectedNotPanicked(t *testing.T) {
	_, verifier := testKey(t)
	now := time.Now()

	for _, malformed := range []string{
		"",
		"not-a-token",
		"one.two",
		"one.two.three.four",
		"!!!.###.$$$",
		strings.Repeat("a", 10000),
	} {
		if _, err := verifier.Verify(malformed, now); err == nil {
			t.Errorf("%q verified", truncate(malformed))
		}
	}
}

// A token from another deployment that happens to share a key identifier must not
// be accepted, so the issuer is checked rather than merely recorded.
func TestForeignIssuerIsRefused(t *testing.T) {
	key, _ := testKey(t)
	verifier := NewVerifier("moqom", []Key{key})
	now := time.Now()

	foreign := claimsFor(control.Grants{})
	foreign.Issuer = "somebody-else"
	minted, _, err := Mint(key, foreign, time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if _, err := verifier.Verify(minted, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("a token from another issuer gave %v, want a rejection", err)
	}
}

func TestPublicJWKIsWellFormed(t *testing.T) {
	key, _ := testKey(t)

	encoded, err := key.PublicJWK()
	if err != nil {
		t.Fatalf("rendering the JWK: %v", err)
	}
	var jwk map[string]string
	if err := json.Unmarshal([]byte(encoded), &jwk); err != nil {
		t.Fatalf("the JWK is not JSON: %v", err)
	}
	if jwk["kty"] != "AKP" || jwk["alg"] != "ML-DSA-65" {
		t.Errorf("unexpected key type: %v", jwk)
	}
	if jwk["kid"] != key.ID {
		t.Errorf("the JWK names key %q, want %q", jwk["kid"], key.ID)
	}
	raw, err := base64.RawURLEncoding.DecodeString(jwk["pub"])
	if err != nil {
		t.Fatalf("the public key is not base64url: %v", err)
	}
	if len(raw) != mldsa.MLDSA65().PublicKeySize() {
		t.Errorf("the public key is %d bytes, want %d", len(raw), mldsa.MLDSA65().PublicKeySize())
	}
}

func legacyKey(t *testing.T) (Key, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	key, err := LegacyEd25519(public)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	return key, private
}

// signLegacy produces what Mint produced before ML-DSA, so the rollover can be
// tested without Mint being able to make one.
func signLegacy(t *testing.T, key Key, private ed25519.PrivateKey, claims Claims, now time.Time) string {
	t.Helper()
	claims.IssuedAt, claims.Expires, claims.ID = now.Unix(), now.Add(time.Minute).Unix(), "legacy"
	signing := encodeSegment(t, header{Algorithm: AlgorithmEdDSA, Type: tokenType, KeyID: key.ID}) +
		"." + encodeSegment(t, claims)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signing)))
}

// Tokens signed with Ed25519 before the move still verify until they expire,
// and an Ed25519 key never signs another.
func TestEd25519KeysOnlyVerify(t *testing.T) {
	legacy, private := legacyKey(t)
	signer, _ := testKey(t)
	verifier := NewVerifier("moqom", []Key{signer, legacy})
	now := time.Now()

	if _, err := verifier.Verify(signLegacy(t, legacy, private, claimsFor(control.Grants{}), now), now); err != nil {
		t.Errorf("a token signed before the move was refused: %v", err)
	}
	if _, _, err := Mint(legacy, claimsFor(control.Grants{}), time.Minute, now); err == nil {
		t.Error("an Ed25519 key minted a token")
	}
}

// The algorithm belongs to the key, so a token naming EdDSA for an ML-DSA key
// is refused before any signature is checked, and the reverse likewise.
func TestATokenMustNameItsKeysAlgorithm(t *testing.T) {
	legacy, private := legacyKey(t)
	signer, _ := testKey(t)
	verifier := NewVerifier("moqom", []Key{signer, legacy})
	now := time.Now()

	minted, _, err := Mint(signer, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	parts := strings.Split(minted, ".")
	parts[0] = encodeSegment(t, header{Algorithm: AlgorithmEdDSA, Type: tokenType, KeyID: signer.ID})
	if _, err := verifier.Verify(strings.Join(parts, "."), now); !errors.Is(err, ErrUnsupported) {
		t.Errorf("EdDSA named for an ML-DSA key gave %v, want ErrUnsupported", err)
	}

	legacyToken := strings.Split(signLegacy(t, legacy, private, claimsFor(control.Grants{}), now), ".")
	legacyToken[0] = encodeSegment(t, header{Algorithm: AlgorithmMLDSA65, Type: tokenType, KeyID: legacy.ID})
	if _, err := verifier.Verify(strings.Join(legacyToken, "."), now); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ML-DSA-65 named for an Ed25519 key gave %v, want ErrUnsupported", err)
	}
}

func TestASeedAlwaysNamesTheSameKey(t *testing.T) {
	seed := make([]byte, 32)
	one, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	two, _ := KeyFromSeed(seed)
	if one.ID != two.ID || !one.CanSign() {
		t.Errorf("a seed gave %s then %s", one.ID, two.ID)
	}
}

func encodeSegment(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func truncate(value string) string {
	if len(value) <= 32 {
		return value
	}
	return value[:32] + "..."
}

func TestProvenanceSurvivesTheRoundTripAndIsSigned(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	claims := claimsFor(control.Grants{})
	claims.ArrivedVia = "bea"

	minted, _, err := Mint(key, claims, time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	verified, err := verifier.Verify(minted, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if verified.ArrivedVia != "bea" {
		t.Errorf("provenance should survive, got %q", verified.ArrivedVia)
	}

	// The property the whole design rests on: these counts divide revenue between
	// cohosts, so a holder who could edit their own provenance could inflate
	// somebody's audience for nothing.
	segments := strings.Split(minted, ".")
	var edited Claims
	if err := decodeJSON(segments[1], &edited); err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	edited.ArrivedVia = "ana"
	forged, err := encodeJSON(edited)
	if err != nil {
		t.Fatalf("re-encoding: %v", err)
	}
	if _, err := verifier.Verify(segments[0]+"."+forged+"."+segments[2], now); !errors.Is(err, ErrSignature) {
		t.Errorf("a rewritten provenance claim should fail the signature, got %v", err)
	}
}

func TestProvenanceIsRefusedWhenItCannotMeanAnything(t *testing.T) {
	key, _ := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// Attributed to itself: nobody brings themselves, and counting it would put a
	// cohost in their own audience.
	itself := claimsFor(control.Grants{})
	itself.ArrivedVia = itself.Subject
	if _, _, err := Mint(key, itself, time.Minute, now); !errors.Is(err, control.ErrInvalid) {
		t.Errorf("a self-attributed token should be refused, got %v", err)
	}

	// Attributed but naming no room: there is no audience for it to belong to.
	roomless := claimsFor(control.Grants{})
	roomless.MOQT = ScopeFrom(control.Grants{}, "", false, false)
	roomless.ArrivedVia = "bea"
	if _, _, err := Mint(key, roomless, time.Minute, now); !errors.Is(err, control.ErrInvalid) {
		t.Errorf("attribution with no room should be refused, got %v", err)
	}

	// Refused rather than quietly dropped, which is the same rule the lifetime
	// follows: issuing something weaker than requested is how a fact gets lost
	// with nobody to notice.
}

func TestATokenWithNoProvenanceIsNormal(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	minted, _, err := Mint(key, claimsFor(control.Grants{}), time.Minute, now)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	verified, err := verifier.Verify(minted, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if verified.ArrivedVia != "" {
		t.Errorf("nobody brought them, got %q", verified.ArrivedVia)
	}
	// And the claim is absent from the wire rather than present and empty, so a
	// token for a viewer who arrived alone is no larger than it was. Checked
	// against the decoded claims, since the compact form is base64 and would
	// contain the substring by coincidence or not at all.
	segments := strings.Split(minted, ".")
	body, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("claims are not base64url: %v", err)
	}
	if bytes.Contains(body, []byte(`"via"`)) {
		t.Errorf("an empty provenance claim should be omitted, got %s", body)
	}
}

// The encryption claim round-trips as required, and anything else -- including
// the "optional" earlier versions allowed -- is refused at minting.
func TestE2EEClaim(t *testing.T) {
	key, verifier := testKey(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	for _, policy := range []control.E2EEPolicy{"", control.E2EERequired} {
		claims := claimsFor(control.Grants{})
		claims.E2EE = policy
		minted, _, err := Mint(key, claims, time.Minute, now)
		if err != nil {
			t.Fatalf("minting e2ee=%q: %v", policy, err)
		}
		verified, err := verifier.Verify(minted, now)
		if err != nil {
			t.Fatalf("verifying e2ee=%q: %v", policy, err)
		}
		if verified.E2EE != policy {
			t.Errorf("e2ee came back as %q, want %q", verified.E2EE, policy)
		}
	}

	for _, policy := range []control.E2EEPolicy{"off", "optional"} {
		relaxed := claimsFor(control.Grants{})
		relaxed.E2EE = policy
		if _, _, err := Mint(key, relaxed, time.Minute, now); !errors.Is(err, control.ErrInvalid) {
			t.Errorf("minting e2ee=%q gave %v, want ErrInvalid", policy, err)
		}
	}
}
