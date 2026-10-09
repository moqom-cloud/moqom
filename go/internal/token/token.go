// Package token is the control plane's half of the credential a client joins
// with: the rules about what may be minted into one, on top of moqom-token.
//
// The token itself -- ML-DSA-87 over JSON claims in the compact JWS form, the claims'
// wire format, the fifteen-minute cap, and every check a verifier makes -- is
// github.com/moqom-cloud/moqom-security/go/token, which the relay (Rust) and
// MoqomKit (Swift) share through the same library's vectors. What is here is
// what only a MOQOM control plane decides: which capability bits exist, that a
// token's encryption policy is "required", what provenance may say, and the
// room control log signed with the same keys (roomlog.go).
//
// What the format still lacks is the encoding, not the binding. `cnf.jkt` is
// derived from a key the device sends -- see PublicKey.Thumbprint -- and every
// relay demands a signature from that key at every point it checks a token, so
// a copy of one is worth nothing without the Secure Enclave it was bound to.
// CBOR/COSE remains the target; the encoding is behind the library on every
// side, so swapping it is a library change rather than a protocol migration.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"time"

	sectoken "github.com/moqom-cloud/moqom-security/go/token"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// The library's failures, so callers can tell them apart without importing it.
var (
	ErrMalformed   = sectoken.ErrMalformed
	ErrUnsupported = sectoken.ErrUnsupported
	ErrUnknownKey  = sectoken.ErrUnknownKey
	ErrSignature   = sectoken.ErrBadSignature
	ErrExpired     = sectoken.ErrExpired
	ErrNotYetValid = sectoken.ErrNotYetValid
	ErrLifetime    = sectoken.ErrLifetime
	ErrWrongIssuer = sectoken.ErrWrongIssuer
	ErrIncomplete  = sectoken.ErrIncomplete
)

const (
	// AlgorithmMLDSA87 is the JOSE name for ML-DSA-87, which every token is
	// signed with.
	AlgorithmMLDSA87 = string(sectoken.MLDSA87)
)

// MaxLifetime caps how long a token is good for; a longer request is refused
// rather than clamped, because silently issuing something weaker than asked
// for is how a security property gets lost.
const MaxLifetime = sectoken.MaxLifetime

// DefaultLifetime applies when a caller does not say.
const DefaultLifetime = 10 * time.Minute

// Claims, Confirmation and Scope are the library's: the wire format is
// moqom-token's, and these names are only where the control plane's code reads
// them.
type (
	Claims       = sectoken.Claims
	Confirmation = sectoken.Confirmation
	Scope        = sectoken.Grant
)

// Grants converts a scope into the domain type.
//
// Unknown capability bits are refused rather than dropped. Dropping them means a
// token minted by a newer control plane silently grants less than it says, which
// surfaces as a moderator whose buttons stopped working and no error anywhere.
func Grants(s Scope) (control.Grants, error) {
	capabilities := control.Capability(s.Capabilities)
	if unknown := capabilities.Unknown(); unknown != 0 {
		return control.Grants{}, fmt.Errorf("%w: capability bits %#x", ErrUnsupported, uint32(unknown))
	}
	return control.Grants{
		Publish:    control.PublishScope(s.Publish),
		Moderation: capabilities,
		IsOwner:    s.Owner,
	}, nil
}

// ScopeFrom is the inverse, used when minting.
//
// It never sets Relay: that is minted only by the relay-link route, for relay
// service accounts, and a customer able to set it could watch unmetered.
func ScopeFrom(grants control.Grants, room string, backstage, ageVerified bool) Scope {
	return Scope{
		Room:         room,
		Publish:      uint8(grants.Publish),
		Capabilities: uint32(grants.Moderation),
		Owner:        grants.IsOwner,
		Backstage:    backstage,
		AgeVerified:  ageVerified,
	}
}

// Identity is who claims say the holder is.
//
// Derived rather than carried, so the two cannot disagree: a token that named a
// device other than the one its thumbprint implies would be a token whose device
// ban does not match it.
func Identity(c Claims) (control.Identity, error) {
	device, err := control.DeviceFromThumbprint(c.Confirmation.Thumbprint)
	if err != nil {
		return control.Identity{}, err
	}
	return control.Identity{Username: c.Subject, Device: device}, nil
}

// Key is a token key with an identifier: an ML-DSA-87 key, which signs when
// the control plane holds its seed and verifies either way.
type Key struct {
	ID        string
	Algorithm string

	signer *sectoken.Signer
	public sectoken.PublicKey
}

// CanSign reports whether Mint will sign with this key.
func (k Key) CanSign() bool { return k.signer != nil }

// PublicBytes is the raw public key: 2592 bytes for ML-DSA-87.
func (k Key) PublicBytes() []byte { return k.public.Bytes() }

// PublicKey is the verification half, as the library holds it.
func (k Key) PublicKey() sectoken.PublicKey { return k.public }

// PublicJWK renders the verification half in the form ListVerificationKeys
// serves.
func (k Key) PublicJWK() (string, error) {
	encoded, err := k.public.JWK()
	return string(encoded), err
}

// GenerateKey makes an ML-DSA-87 signing key, named by its public half.
func GenerateKey() (Key, error) {
	signer, err := sectoken.GenerateSigner()
	if err != nil {
		return Key{}, err
	}
	return signingKey(signer), nil
}

// KeyFromSeed loads an ML-DSA-87 signing key from its 32-byte FIPS 204 seed, as
// when one is read from a secret. The identifier is a function of the key, so a
// deployment that reloads its secrets keeps verifying the tokens it signed.
func KeyFromSeed(seed []byte) (Key, error) {
	signer, err := sectoken.NewSigner(seed)
	if err != nil {
		return Key{}, err
	}
	return signingKey(signer), nil
}

func signingKey(signer *sectoken.Signer) Key {
	return Key{ID: signer.ID(), Algorithm: AlgorithmMLDSA87, signer: signer, public: signer.PublicKey()}
}

// ValidThumbprint refuses anything but an RFC 7638 SHA-256 thumbprint. A token
// whose cnf is any other string is bound to no key at all, since no relay will
// accept a proof for it, so it is refused here rather than issued and refused
// at the door.
func ValidThumbprint(thumbprint string) error {
	if thumbprint == "" {
		return fmt.Errorf("%w: token needs a device thumbprint", control.ErrInvalid)
	}
	if !sectoken.ValidThumbprint(thumbprint) {
		return fmt.Errorf("%w: device thumbprint is not an RFC 7638 SHA-256 thumbprint", control.ErrInvalid)
	}
	return nil
}

// Mint signs a token.
//
// The lifetime is validated rather than clamped, and the times are filled here
// rather than by the caller, so nothing upstream can issue a token with no expiry
// by forgetting to set one.
func Mint(key Key, claims Claims, lifetime time.Duration, now time.Time) (string, time.Time, error) {
	if !key.CanSign() {
		return "", time.Time{}, fmt.Errorf("key %s is %s and only verifies; tokens are signed with %s",
			key.ID, key.Algorithm, AlgorithmMLDSA87)
	}
	if lifetime == 0 {
		lifetime = DefaultLifetime
	}
	if lifetime < 0 || lifetime > MaxLifetime {
		return "", time.Time{}, fmt.Errorf("%w: lifetime %s, maximum %s", control.ErrInvalid, lifetime, MaxLifetime)
	}
	if claims.Subject == "" {
		return "", time.Time{}, fmt.Errorf("%w: token needs a subject", control.ErrInvalid)
	}
	if err := ValidThumbprint(claims.Confirmation.Thumbprint); err != nil {
		return "", time.Time{}, err
	}
	if _, err := Grants(claims.Grant); err != nil {
		return "", time.Time{}, err
	}
	// Refused rather than dropped. A caller that asked for attribution and got a
	// token without it would produce a viewer counted as organic, and the
	// discrepancy would surface as a cohost disputing a revenue split months
	// later with nothing left to reconstruct.
	if claims.Via == claims.Subject {
		return "", time.Time{}, fmt.Errorf(
			"%w: token is attributed to its own subject %q", control.ErrInvalid, claims.Subject)
	}
	if claims.Via != "" && claims.Grant.Room == "" {
		return "", time.Time{}, fmt.Errorf(
			"%w: token is attributed to %q but names no room", control.ErrInvalid, claims.Via)
	}
	expires := now.Add(lifetime).Truncate(time.Second)
	claims.IssuedAt = now.Unix()
	claims.Expires = expires.Unix()
	if claims.ID == "" {
		identifier, err := randomID()
		if err != nil {
			return "", time.Time{}, err
		}
		claims.ID = identifier
	}
	signed, err := key.signer.Sign(claims)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %w", control.ErrInvalid, err)
	}
	return signed, expires, nil
}

// Verifier holds the public keys tokens may be signed with, the issuer they
// must name, and the keys sealed tokens are opened with.
type Verifier struct {
	inner *sectoken.Verifier
	seal  atomic.Pointer[sealing]
}

type sealing struct {
	keys []sectoken.SealKey
	// unsealedUntil is when a token that is signed and not sealed stops being
	// accepted: the overlap after sealing is switched on, during which tokens
	// minted before it are still in hands.
	unsealedUntil time.Time
}

// SetSealKeys replaces the keys sealed tokens are opened with. With none, only
// unsealed tokens verify; with some, an unsealed token is refused after
// unsealedUntil.
func (v *Verifier) SetSealKeys(keys []sectoken.SealKey, unsealedUntil time.Time) {
	v.seal.Store(&sealing{keys: keys, unsealedUntil: unsealedUntil})
}

// NewVerifier builds a verifier from keys, each found by its identifier.
func NewVerifier(issuer string, keys []Key) *Verifier {
	public := make([]sectoken.PublicKey, 0, len(keys))
	for _, key := range keys {
		public = append(public, key.public)
	}
	return &Verifier{inner: sectoken.NewVerifier(issuer, public...)}
}

// Verify checks a token and returns its claims: the library's checks, then the
// capability bits, so an unrecognised bit cannot reach a decision about what
// somebody may do.
func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	if seal := v.seal.Load(); seal != nil && len(seal.keys) > 0 {
		switch {
		case sectoken.IsSealed(token):
			opened, err := sectoken.Open(token, seal.keys...)
			if err != nil {
				return Claims{}, err
			}
			token = opened
		case !now.Before(seal.unsealedUntil):
			return Claims{}, fmt.Errorf("%w: token is not sealed", sectoken.ErrUnsupported)
		}
	}
	claims, err := v.inner.Verify(token, now)
	if err != nil {
		return Claims{}, err
	}
	if _, err := Grants(claims.Grant); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func randomID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	// Encoded rather than formatted as a UUID: it is compared, never parsed.
	return base64.RawURLEncoding.EncodeToString(buffer[:]), nil
}
