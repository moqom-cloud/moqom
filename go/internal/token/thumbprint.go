package token

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// Failures a caller can tell apart, because they are different mistakes: a key
// of the wrong kind is a client built for another curve, and a malformed one is
// a client that has its encoding wrong.
var (
	ErrUnsupportedKey = errors.New("device key is not an EC P-256 key")
	ErrMalformedKey   = errors.New("device key coordinates are malformed")
)

// PublicKey is the public half of a device's signing key, as a JWK.
//
// Sent by a device at join so the control plane can bind a token to it. Only the
// four members RFC 7638 calls required for an EC key are modelled, because they
// are the only four the thumbprint is computed over -- anything else a client
// sends would be ignored by the hash and is therefore not worth accepting into
// a struct that exists to be hashed.
type PublicKey struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Thumbprint is the RFC 7638 JWK thumbprint, base64url without padding.
//
// This is the value that goes in `cnf.jkt`, and the point of computing it here
// rather than accepting one from the device is that a thumbprint supplied by a
// client is a claim about which key it holds. The relay will later demand a
// signature from the key named in `cnf.jkt`, so a client that could name any
// thumbprint it liked could have a token minted for a key it does not have --
// which would be a token nobody can use, arriving as an authentication failure
// at the relay with nothing there to explain it.
//
// Every part of the canonical form is load-bearing: the required members and no
// others, ordered lexicographically, no whitespace, and the coordinates
// re-encoded from what was decoded rather than copied from the request. Copying
// them would let a device pad its base64 and produce a second thumbprint for one
// key -- two names for one device, which is a way to sidestep a ban.
//
// The relay computes this in Rust and the SDK in Swift, and all three are pinned
// to the same worked example so they cannot drift apart.
func (k PublicKey) Thumbprint() (string, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return "", fmt.Errorf("%w: kty %q, crv %q", ErrUnsupportedKey, k.Kty, k.Crv)
	}

	x, err := coordinate(k.X)
	if err != nil {
		return "", err
	}
	y, err := coordinate(k.Y)
	if err != nil {
		return "", err
	}

	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, x, y)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// coordinate decodes one coordinate and re-encodes it canonically.
//
// The length check is not pedantry. Base64url decodes a shorter value happily,
// and a coordinate left-padded or truncated is a different point -- so without
// this the control plane would mint a token naming a key nobody holds, and the
// failure would surface as a refused session a long way from the cause.
func coordinate(value string) (string, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("%w: not base64url", ErrMalformedKey)
	}
	if len(bytes) != 32 {
		return "", fmt.Errorf("%w: %d bytes, want 32", ErrMalformedKey, len(bytes))
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
