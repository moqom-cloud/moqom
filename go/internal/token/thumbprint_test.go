package token

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/moqom-cloud/moqom-security/go/proof"
)

// A P-256 key is refused: device keys are ML-DSA-87 and nothing else, so a
// token is never bound to a key whose signature a quantum computer could forge.
func TestThumbprintRefusesAnECKey(t *testing.T) {
	var key PublicKey
	if err := json.Unmarshal([]byte(`{"kty":"EC","crv":"P-256",`+
		`"x":"MKBCTNIcKUSDii11ySs3526iDZ8AiTo7Tu6KPAqv7D4",`+
		`"y":"4Etl6SRW2YiLUrN5vfvVHuhp7x8PxltmWWlbbM4IFyM"}`), &key); err != nil {
		t.Fatal(err)
	}
	if _, err := key.Thumbprint(); !errors.Is(err, ErrUnsupportedKey) {
		t.Errorf("Thumbprint() error = %v, want ErrUnsupportedKey", err)
	}
}

// Padding the key must not produce a second thumbprint for one key, or a
// device could pick which identity a token appears to name -- and a banned
// device could come back under the other spelling.
func TestThumbprintRejectsANonCanonicalSpelling(t *testing.T) {
	device, err := proof.GenerateProver(proof.MLDSA87)
	if err != nil {
		t.Fatal(err)
	}
	padded := device.JWK()
	padded.Pub += "="
	if _, err := padded.Thumbprint(); !errors.Is(err, ErrMalformedKey) {
		t.Errorf("Thumbprint() error = %v, want ErrMalformedKey", err)
	}
}

// The key every SDK now sends. The thumbprint is over alg, kty and pub, per
// draft-ietf-cose-dilithium, so a key reaching the control plane as JSON names
// the device its own prover says it is.
func TestThumbprintOfAnMLDSAKey(t *testing.T) {
	device, err := proof.GenerateProver(proof.MLDSA87)
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
		"another kind": {Kty: "OKP", Alg: "ML-DSA-87", Pub: coord32},
		"ml-dsa-44":    {Kty: "AKP", Alg: "ML-DSA-44", Pub: coord32},
		"ml-dsa-65":    {Kty: "AKP", Alg: "ML-DSA-65", Pub: coord32},
		"short":        {Kty: "AKP", Alg: "ML-DSA-87", Pub: strings.Repeat("A", 42)},
		"absent":       {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := key.Thumbprint(); err == nil {
				t.Error("Thumbprint() accepted a key it cannot have bound a token to")
			}
		})
	}
}

// 32 zero bytes, base64url: well-formed, and the wrong size for any key here.
const coord32 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
