package token

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/moqom-cloud/moqom-security/go/proof"
)

// Pinned against RFC 7638, which is the only way to know this agrees with the
// relay's Rust and the SDK's Swift rather than merely with itself. The same key
// and the same expected digest appear in `moqom-relay/src/possession.rs` and in
// `Tests/MoqomKitTests/DeviceKeyTests.swift`; a change that broke the agreement
// fails in all three places.
func TestThumbprintFollowsRFC7638(t *testing.T) {
	key := PublicKey{
		Kty: "EC",
		Crv: "P-256",
		X:   "MKBCTNIcKUSDii11ySs3526iDZ8AiTo7Tu6KPAqv7D4",
		Y:   "4Etl6SRW2YiLUrN5vfvVHuhp7x8PxltmWWlbbM4IFyM",
	}

	got, err := key.Thumbprint()
	if err != nil {
		t.Fatalf("Thumbprint() = %v", err)
	}
	if want := "cn-I_WNMClehiVp51i_0VpOENW1upEerA8sEam5hn-s"; got != want {
		t.Errorf("Thumbprint() = %q, want %q", got, want)
	}
}

// Padding the coordinates must not produce a second thumbprint for one key, or
// a device could pick which identity a token appears to name -- and a banned
// device could come back under the other spelling.
func TestThumbprintRejectsANonCanonicalSpelling(t *testing.T) {
	padded := PublicKey{
		Kty: "EC",
		Crv: "P-256",
		X:   "MKBCTNIcKUSDii11ySs3526iDZ8AiTo7Tu6KPAqv7D4=",
		Y:   "4Etl6SRW2YiLUrN5vfvVHuhp7x8PxltmWWlbbM4IFyM",
	}

	if _, err := padded.Thumbprint(); !errors.Is(err, ErrMalformedKey) {
		t.Errorf("Thumbprint() error = %v, want ErrMalformedKey", err)
	}
}

// The key every SDK now sends. The thumbprint is over alg, kty and pub, per
// draft-ietf-cose-dilithium, so a key reaching the control plane as JSON names
// the device its own prover says it is.
func TestThumbprintOfAnMLDSAKey(t *testing.T) {
	device, err := proof.GenerateProver(proof.MLDSA65)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(device.JWK())
	if err != nil {
		t.Fatal(err)
	}
	var sent PublicKey
	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatal(err)
	}
	got, err := sent.Thumbprint()
	if err != nil {
		t.Fatalf("Thumbprint() = %v", err)
	}
	if got != device.Thumbprint() {
		t.Errorf("Thumbprint() = %q, want %q", got, device.Thumbprint())
	}

	short := sent
	short.Pub = short.Pub[:len(short.Pub)-4]
	if _, err := short.Thumbprint(); !errors.Is(err, ErrMalformedKey) {
		t.Errorf("a truncated pub: Thumbprint() error = %v, want ErrMalformedKey", err)
	}
}

func TestThumbprintRejectsWhatItCannotBind(t *testing.T) {
	for name, key := range map[string]PublicKey{
		"another curve": {Kty: "EC", Crv: "P-384", X: coord32, Y: coord32},
		"another kind":  {Kty: "OKP", Crv: "P-256", X: coord32, Y: coord32},
		"ml-dsa-44":     {Kty: "AKP", Alg: "ML-DSA-44", Pub: coord32},
		"short":         {Kty: "EC", Crv: "P-256", X: strings.Repeat("A", 42), Y: coord32},
		"absent":        {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := key.Thumbprint(); err == nil {
				t.Error("Thumbprint() accepted a key it cannot have bound a token to")
			}
		})
	}
}

// 32 zero bytes, base64url: a well-formed coordinate for cases testing
// something other than the coordinates.
const coord32 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
