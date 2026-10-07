// Package token mints and verifies the credential a client joins with.
//
// # What this is, and what it is not yet
//
// The design target is CAT/CWT: CBOR claims, COSE signatures, and DPoP
// proof-of-possession so the token is useless without the device key it was bound
// to. What is implemented here is a subset -- Ed25519 over JSON claims in the
// compact JWS form -- and the gap is worth stating plainly rather than burying.
//
// What the subset keeps: asymmetric signing, so a relay verifies without holding
// anything that could mint; key rotation by identifier; the device thumbprint in
// a `cnf` claim; short lifetimes.
//
// What it lacks is now the encoding and not the binding, and this comment used
// to say otherwise. `cnf.jkt` is derived here from a key the device sends -- see
// PublicKey.Thumbprint -- and the relay demands an ES256 signature from that key
// at every point it checks a token, so a token in flight is no longer a bearer
// credential and a copy of one is worth nothing without the Secure Enclave it
// was bound to. Lifetimes stay capped in minutes anyway: a relay may be
// configured not to require proofs while a fleet is still being upgraded, and a
// short life is what covers that window.
//
// `ListVerificationKeys` reports the binding in force, so a client that wants to
// know whether it is talking to a deployment that enforces this can find out
// without inspecting a token.
//
// Why JSON rather than CBOR, given the target: the relay has to parse this on
// every session, and adding a CBOR/COSE stack to the media-path binary for a
// four-hundred-byte credential is a dependency for no measurable gain. The
// encoding is behind this package on both sides, so swapping it is a change in
// two files rather than a protocol migration.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// Failures a caller can distinguish, because they mean different things: a
// malformed token is a client bug, an expired one is routine, and a bad signature
// is worth counting.
var (
	ErrMalformed   = errors.New("token is malformed")
	ErrUnsupported = errors.New("token algorithm is not supported")
	ErrUnknownKey  = errors.New("token was signed by an unknown key")
	ErrSignature   = errors.New("token signature does not verify")
	ErrExpired     = errors.New("token has expired")
	ErrNotYetValid = errors.New("token is not valid yet")
)

// The only algorithm accepted, named explicitly in the header and checked on the
// way in.
//
// Checked rather than trusted, because the classic JWS attack is algorithm
// confusion: a verifier that reads `alg` from the token and picks its code path
// accordingly can be told to use "none", or to verify an HMAC using the public
// key as the shared secret. Both are forgeries that validate. Pinning one
// algorithm removes the choice.
const algorithm = "EdDSA"

// Type distinguishes these from any other JWS a deployment handles, so a token
// minted for something else cannot be replayed at a relay.
const tokenType = "MOQT"

// MaxLifetime caps how long a token is good for.
//
// Fifteen minutes, because this is a bearer credential: the window in which a
// leaked token is useful is exactly its remaining lifetime, and the client can
// refresh. A caller asking for longer is refused rather than quietly clamped --
// silently issuing something weaker than requested is how a security property
// gets lost.
const MaxLifetime = 15 * time.Minute

// DefaultLifetime applies when a caller does not say.
const DefaultLifetime = 10 * time.Minute

// clockSkew is how much disagreement between the minting and verifying machines
// is tolerated.
//
// Small and one-directional: a token from slightly in the future is accepted,
// because that is a clock, but an expired one is not extended, because that is a
// security boundary.
const clockSkew = 30 * time.Second

// Claims is what a token asserts.
//
// Field names are short because they are on the wire, and JSON-tagged rather than
// inferred so renaming a Go field cannot silently change the format.
type Claims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	ID       string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`

	// Confirmation: what the token is bound to. The thumbprint is recorded now
	// and enforced when DPoP lands; until then it is what makes a device ban
	// possible against a stolen token.
	Confirmation Confirmation `json:"cnf"`

	// MOQT is the part the relay reads.
	MOQT Scope `json:"moqt"`

	// ArrivedVia is the username whose following this holder came in through,
	// empty when they arrived by themselves.
	//
	// Provenance, not authorisation. It grants nothing, withholds nothing, and no
	// decision about what the holder may do reads it -- which is what makes it
	// safe for the relay to parse leniently while the scope above is parsed
	// strictly.
	//
	// It is in the token rather than passed at join because it must be
	// unforgeable and it must survive. Unforgeable, because these counts divide
	// revenue between cohosts, and a client that could assert its own provenance
	// could inflate somebody's audience for free. Surviving, because a viewer
	// reconnects and refreshes tokens for hours: provenance is a fact about the
	// arrival, so it has to be reasserted with every credential rather than
	// remembered in one server's memory, and a signed claim is the only carrier
	// that reaches a relay in a different region without a lookup on the join
	// path.
	//
	// A username rather than an Identity, because a following belongs to a person
	// and not to one of their phones.
	//
	// Adding this was a compatible change for the relay and an incompatible one
	// here: the decoder below refuses unknown claims, so a control plane older
	// than this field rejects a token carrying it. Rolling out the verifier ahead
	// of the minter is the order that has no window.
	ArrivedVia string `json:"via,omitempty"`

	// E2EE is the end-to-end encryption policy of the room in MOQT.Room, which
	// is always "required"; absent means the same. Minting refuses anything else.
	//
	// For the client, not the relay. The relay never sees plaintext and has
	// nothing to enforce; MoqomKit refuses to publish without a key whatever
	// this says.
	//
	// Top-level rather than inside `moqt` for the reason ArrivedVia is: the relay
	// parses the scope strictly and the top level leniently, so a relay that
	// predates this claim admits a token carrying it. The verifier below is strict,
	// so the same rollout order applies -- verifier before minter -- and here they
	// are one binary.
	E2EE control.E2EEPolicy `json:"e2ee,omitempty"`
}

type Confirmation struct {
	// JWK thumbprint, base64url. The device half of every Identity is derived
	// from this, so the token cannot claim a device its key does not match.
	Thumbprint string `json:"jkt"`
}

// Scope is what the holder may do, in the form the relay enforces.
type Scope struct {
	Room string `json:"room,omitempty"`

	// Publish and Capabilities are bitmasks, and their bit positions are
	// protocol: the same numbers appear in control.Capability, in MoqomKit's
	// Capability, and in the Rust relay. A bitmask rather than a list because
	// this is the field a relay reads on every session and comparing two integers
	// is the cheapest form the check can take.
	Publish      uint8  `json:"pub"`
	Capabilities uint32 `json:"cap"`

	Owner       bool `json:"own,omitempty"`
	Backstage   bool `json:"bs,omitempty"`
	AgeVerified bool `json:"age,omitempty"`

	// Relay marks another relay's link to the one this is presented to: a
	// child in a tier reading from its parent. The parent does not meter it,
	// because the child meters the people it serves and both would bill them.
	//
	// Minted only by the relay-link route, for relay service accounts, and
	// never by ScopeFrom or anything a backend can reach -- a customer able to
	// set it could watch unmetered. The relay refuses it beside any publish
	// right or power, so it cannot carry anything else.
	//
	// Omitted when false, so every other token is unchanged. The relay parses
	// the scope strictly, so relays must learn this field before a control
	// plane mints it.
	Relay bool `json:"rly,omitempty"`
}

// Grants converts the scope into the domain type.
//
// Unknown capability bits are refused rather than dropped. Dropping them means a
// token minted by a newer control plane silently grants less than it says, which
// surfaces as a moderator whose buttons stopped working and no error anywhere.
func (s Scope) Grants() (control.Grants, error) {
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

// Identity is who the token says its holder is.
//
// Derived rather than carried, so the two cannot disagree: a token that named a
// device other than the one its thumbprint implies would be a token whose device
// ban does not match it.
func (c Claims) Identity() (control.Identity, error) {
	device, err := control.DeviceFromThumbprint(c.Confirmation.Thumbprint)
	if err != nil {
		return control.Identity{}, err
	}
	return control.Identity{Username: c.Subject, Device: device}, nil
}

// Key is a signing key with an identifier.
type Key struct {
	ID      string
	Private ed25519.PrivateKey
}

// PublicJWK renders the verification half in the form ListVerificationKeys
// serves.
func (k Key) PublicJWK() (string, error) {
	public, ok := k.Private.Public().(ed25519.PublicKey)
	if !ok {
		return "", errors.New("key is not Ed25519")
	}
	jwk := map[string]string{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": k.ID,
		"x":   base64.RawURLEncoding.EncodeToString(public),
	}
	encoded, err := json.Marshal(jwk)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// GenerateKey makes a signing key with an identifier derived from the public
// half.
//
// Derived rather than random so the identifier is a function of the key: two
// deployments that somehow share a key agree on its name, and a key cannot be
// republished under a second identifier as if it were a rotation.
func GenerateKey() (Key, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	return KeyFrom(private)
}

// KeyFrom names a key that already exists, as when one is loaded from a secret.
//
// Same derivation as GenerateKey, which is the point: a key has one identifier no
// matter which side of a restart it was created on, so a deployment that reloads its
// secrets keeps verifying the tokens it signed before.
func KeyFrom(private ed25519.PrivateKey) (Key, error) {
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return Key{}, errors.New("key is not Ed25519")
	}
	digest := sha256.Sum256(public)
	return Key{
		ID:      base64.RawURLEncoding.EncodeToString(digest[:8]),
		Private: private,
	}, nil
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

// Mint signs a token.
//
// The lifetime is validated rather than clamped, and the claims are filled here
// rather than by the caller, so nothing upstream can issue a token with no expiry
// by forgetting to set one.
// ValidThumbprint refuses anything but an RFC 7638 SHA-256 thumbprint: 43
// base64url characters decoding to 32 bytes. A token whose cnf is any other
// string is bound to no key at all, since no relay will accept a proof for it,
// so it is refused here rather than issued and refused at the door.
func ValidThumbprint(thumbprint string) error {
	if thumbprint == "" {
		return fmt.Errorf("%w: token needs a device thumbprint", control.ErrInvalid)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(thumbprint)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%w: device thumbprint is not an RFC 7638 SHA-256 thumbprint", control.ErrInvalid)
	}
	return nil
}

func Mint(key Key, claims Claims, lifetime time.Duration, now time.Time) (string, time.Time, error) {
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
	if _, err := claims.MOQT.Grants(); err != nil {
		return "", time.Time{}, err
	}
	// Refused rather than dropped. A caller that asked for attribution and got a
	// token without it would produce a viewer counted as organic, and the
	// discrepancy would surface as a cohost disputing a revenue split months
	// later with nothing left to reconstruct.
	if claims.ArrivedVia == claims.Subject {
		return "", time.Time{}, fmt.Errorf(
			"%w: token is attributed to its own subject %q", control.ErrInvalid, claims.Subject)
	}
	if claims.ArrivedVia != "" && claims.MOQT.Room == "" {
		return "", time.Time{}, fmt.Errorf(
			"%w: token is attributed to %q but names no room", control.ErrInvalid, claims.ArrivedVia)
	}
	if err := (control.Policy{E2EE: claims.E2EE}).ValidateE2EE(); err != nil {
		return "", time.Time{}, err
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

	encodedHeader, err := encodeJSON(header{Algorithm: algorithm, Type: tokenType, KeyID: key.ID})
	if err != nil {
		return "", time.Time{}, err
	}
	encodedClaims, err := encodeJSON(claims)
	if err != nil {
		return "", time.Time{}, err
	}

	signing := encodedHeader + "." + encodedClaims
	signature := ed25519.Sign(key.Private, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

// Verifier holds the public keys tokens may be signed with.
//
// A set rather than one key, because rotation needs an overlap: the new key is
// published, minting switches to it, and the old one stays verifiable until the
// last token it signed has expired. With a single key that sequence has a window
// where every session fails.
type Verifier struct {
	keys map[string]ed25519.PublicKey
	// Issuer, checked against the claim. A token from another deployment that
	// happens to share a key identifier is refused rather than accepted.
	issuer string
}

// NewVerifier builds a verifier from public keys by identifier.
func NewVerifier(issuer string, keys map[string]ed25519.PublicKey) *Verifier {
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		copied[id] = key
	}
	return &Verifier{keys: copied, issuer: issuer}
}

// Verify checks a token and returns its claims.
//
// The order matters. Signature first, then the registered claims: reading a
// claim from an unverified token and acting on it -- even to produce an error
// message -- is how a parser becomes an attack surface.
func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: %d segments, want 3", ErrMalformed, len(parts))
	}

	var parsed header
	if err := decodeJSON(parts[0], &parsed); err != nil {
		return Claims{}, fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	if parsed.Algorithm != algorithm {
		return Claims{}, fmt.Errorf("%w: %q", ErrUnsupported, parsed.Algorithm)
	}
	if parsed.Type != tokenType {
		return Claims{}, fmt.Errorf("%w: type %q is not a MOQT token", ErrUnsupported, parsed.Type)
	}
	public, ok := v.keys[parsed.KeyID]
	if !ok {
		return Claims{}, fmt.Errorf("%w: %q", ErrUnknownKey, parsed.KeyID)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature is not base64url", ErrMalformed)
	}
	if !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrSignature
	}

	var claims Claims
	if err := decodeJSON(parts[1], &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %w", ErrMalformed, err)
	}
	if v.issuer != "" && claims.Issuer != v.issuer {
		return Claims{}, fmt.Errorf("%w: issued by %q", ErrMalformed, claims.Issuer)
	}
	if claims.Expires == 0 {
		return Claims{}, fmt.Errorf("%w: no expiry", ErrMalformed)
	}
	if now.Add(-clockSkew).After(time.Unix(claims.Expires, 0)) {
		return Claims{}, fmt.Errorf("%w: expired at %s", ErrExpired, time.Unix(claims.Expires, 0).UTC())
	}
	if claims.IssuedAt != 0 && now.Add(clockSkew).Before(time.Unix(claims.IssuedAt, 0)) {
		return Claims{}, fmt.Errorf("%w: issued at %s", ErrNotYetValid, time.Unix(claims.IssuedAt, 0).UTC())
	}
	// Rejected here rather than where the grants are used, so an unrecognised
	// bit cannot reach a decision about what somebody may do.
	if _, err := claims.MOQT.Grants(); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func encodeJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeJSON(segment string, into any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return errors.New("segment is not base64url")
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	// Unknown fields are refused. A token carrying a claim this build does not
	// understand may be asserting something that matters, and guessing which is
	// not a decision to make on an authorisation path.
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func randomID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	// Encoded rather than formatted as a UUID: it is compared, never parsed.
	return base64.RawURLEncoding.EncodeToString(buffer[:]), nil
}
