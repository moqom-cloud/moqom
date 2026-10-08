package token

import "github.com/moqom-cloud/moqom-security/go/proof"

// Failures a caller can tell apart, because they are different mistakes: a key
// of the wrong kind is a client built for another algorithm, and a malformed
// one is a client that has its encoding wrong.
var (
	ErrUnsupportedKey = proof.ErrUnsupported
	ErrMalformedKey   = proof.ErrMalformed
)

// PublicKey is the public half of a device's signing key, as a JWK: kty "AKP"
// with alg "ML-DSA-65" and pub, or kty "EC" with crv "P-256", x and y.
//
// Sent by a device at join so the control plane can bind a token to it. Its
// Thumbprint is the value that goes in `cnf.jkt`, and the point of computing it
// here rather than accepting one from the device is that a thumbprint supplied
// by a client is a claim about which key it holds. The relay will later demand
// a signature from the key named in `cnf.jkt`, so a client that could name any
// thumbprint it liked could have a token minted for a key it does not have.
//
// The canonical form is moqom-proof's, shared with the relay and the SDKs, and
// rebuilt from the decoded key rather than copied from the request: copying
// would let a device pad its base64 and produce a second thumbprint for one
// key, two names for one device, which is a way to sidestep a ban.
type PublicKey = proof.JWK
