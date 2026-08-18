package token

import (
	"errors"
	"strings"
	"testing"
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

func TestThumbprintRejectsWhatItCannotBind(t *testing.T) {
	for name, key := range map[string]PublicKey{
		"another curve": {Kty: "EC", Crv: "P-384", X: coord32, Y: coord32},
		"another kind":  {Kty: "OKP", Crv: "P-256", X: coord32, Y: coord32},
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
