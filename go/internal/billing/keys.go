package billing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

// KeyKind is what a key may be used for.
type KeyKind string

const (
	// KeyBackend is a tenant's backend calling the admin API: rooms,
	// moderation, token minting, billing.
	KeyBackend KeyKind = "backend"
	// KeyRelay is a self-hosted relay reporting usage. It can do nothing else,
	// and in particular cannot reach the relay link, which carries directives
	// for every tenant.
	KeyRelay KeyKind = "relay"
	// KeyMint is for an agent or MCP server that lets people into rooms: it
	// mints viewer and publisher tokens and reads rooms. It cannot mint a
	// moderator or an owner, change a room, or touch keys or billing, so a
	// leaked one lets strangers join rooms and no more.
	KeyMint KeyKind = "mint"
	// KeyReadOnly reads rooms, participants, the moderation log and the
	// balance, and changes nothing.
	KeyReadOnly KeyKind = "readonly"
)

func (k KeyKind) valid() bool {
	switch k {
	case KeyBackend, KeyRelay, KeyMint, KeyReadOnly:
		return true
	}
	return false
}

// Key is an API key, minus the secret, which is never stored.
type Key struct {
	ID     string
	Tenant string
	Name   string
	Kind   KeyKind
	Hash   string
	// PepperVersion names the pepper Hash was keyed with. Empty is a bare
	// SHA-256 from before peppering, rehashed at the key's next use.
	PepperVersion string
	CreatedAt     time.Time
	// RevokedAt may be in the future: a rotated key keeps working until then,
	// so the replacement can be rolled out without a gap.
	RevokedAt time.Time
	// ExpiresAt, if set, is fixed at creation; unlike RevokedAt nothing moves
	// it, so a key issued for a week cannot be quietly kept for a year.
	ExpiresAt time.Time
	// AllowedNetworks, if any, are the only source addresses the key works
	// from.
	AllowedNetworks []netip.Prefix
	// RateLimit is requests a minute per control-plane replica. Zero is the
	// kind's default; see Key.Limit.
	RateLimit int
}

// Revoked reports whether the key has been revoked.
func (k Key) Revoked() bool { return k.RevokedAsOf(time.Now()) }

// RevokedAsOf reports whether the key no longer works at now.
func (k Key) RevokedAsOf(now time.Time) bool {
	return !k.RevokedAt.IsZero() && !now.Before(k.RevokedAt)
}

// Retiring reports whether the key still works at now but is scheduled to stop.
func (k Key) Retiring(now time.Time) bool {
	return !k.RevokedAt.IsZero() && now.Before(k.RevokedAt)
}

// ExpiredAsOf reports whether the key's fixed lifetime is over.
func (k Key) ExpiredAsOf(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

// WorksAt reports whether the key authenticates at all at now.
func (k Key) WorksAt(now time.Time) bool { return !k.RevokedAsOf(now) && !k.ExpiredAsOf(now) }

// Limit is the effective requests a minute, zero for none. Keys meant for
// agents are limited by default, because an agent in a loop is the likeliest
// thing to hammer the API; a tenant's own backend and relays are not, so
// turning this on changed nothing for them.
func (k Key) Limit() int {
	if k.RateLimit > 0 {
		return k.RateLimit
	}
	if k.Kind == KeyMint || k.Kind == KeyReadOnly {
		return DefaultScopedRateLimit
	}
	return 0
}

// Rate limit bounds, in requests a minute per replica.
const (
	DefaultScopedRateLimit = 600
	MaxRateLimit           = 100_000
)

// keyPrefix makes a leaked key recognisable to secret scanners, which match on
// a fixed prefix; GitHub's push protection can be registered for it.
const keyPrefix = "mqk_"

// NewKey generates a key. The plaintext is returned once and never again.
//
// The ID is embedded in the key so lookup is one indexed read rather than a
// scan, and only the secret half is hashed. A fast hash rather than a password
// hash because the secret is 256 random bits: there is no dictionary to slow
// down. The hash here is the bare SHA-256; Peppers.Seal replaces it with the
// keyed one before the key is stored.
func NewKey(random io.Reader, tenant, name string, kind KeyKind, now time.Time) (Key, string, error) {
	if random == nil {
		random = rand.Reader
	}
	var id [8]byte
	var secret [32]byte
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return Key{}, "", fmt.Errorf("generating a key id: %w", err)
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return Key{}, "", fmt.Errorf("generating a key secret: %w", err)
	}
	key := Key{
		ID:        hex.EncodeToString(id[:]),
		Tenant:    tenant,
		Name:      name,
		Kind:      kind,
		CreatedAt: now,
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret[:])
	key.Hash = hashSecret(encoded)
	return key, keyPrefix + key.ID + "_" + encoded, nil
}

// ParseKey splits a presented key into its ID and secret.
func ParseKey(presented string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(presented, keyPrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || len(id) != 16 || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

func hashSecret(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Peppers are the HMAC keys stored hashes are made with, newest first: the
// first hashes new keys, every one verifies. Held outside the database (Secret
// Manager, mounted as a file), so a dump of billing_keys alone cannot be used
// to test a guessed secret, and the hashes in it are worth nothing to whoever
// does not also hold the pepper.
//
// A version's ID is derived from its secret rather than configured, so two
// different peppers can never be stored under one name, and a hash keyed with
// a pepper that has since been removed fails rather than verifying against the
// wrong one. Rotation is adding a new pepper at the top: keys are rehashed
// with it at their next use, and an old pepper can be dropped once no key
// names its version.
type Peppers struct {
	versions []pepper
}

type pepper struct {
	id     string
	secret []byte
}

// shortestPepper is the HMAC-SHA-256 block's worth of entropy halved: a pepper
// shorter than the secrets it protects would be the weaker of the two.
const shortestPepper = 32

// ParsePeppers reads one base64 pepper per line, newest first. Blank lines and
// lines starting with # are skipped. Nothing at all is no pepper, which keeps
// bare SHA-256 hashes.
func ParsePeppers(contents string) (Peppers, error) {
	var peppers Peppers
	seen := map[string]bool{}
	for number, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		secret, err := decodePepper(line)
		if err != nil {
			return Peppers{}, fmt.Errorf("pepper on line %d: %w", number+1, err)
		}
		if len(secret) < shortestPepper {
			return Peppers{}, fmt.Errorf("pepper on line %d is %d bytes, want at least %d", number+1, len(secret), shortestPepper)
		}
		id := pepperID(secret)
		if seen[id] {
			return Peppers{}, fmt.Errorf("pepper on line %d is a repeat", number+1)
		}
		seen[id] = true
		peppers.versions = append(peppers.versions, pepper{id: id, secret: secret})
	}
	return peppers, nil
}

func decodePepper(line string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(line); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("not base64")
}

func pepperID(secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("moqom api key pepper id"))
	return hex.EncodeToString(mac.Sum(nil))[:12]
}

// Current is the version new hashes use, empty when there is no pepper.
func (p Peppers) Current() string {
	if len(p.versions) == 0 {
		return ""
	}
	return p.versions[0].id
}

// Versions lists every configured version, newest first.
func (p Peppers) Versions() []string {
	ids := make([]string, len(p.versions))
	for i, v := range p.versions {
		ids[i] = v.id
	}
	return ids
}

func (p Peppers) find(version string) ([]byte, bool) {
	for _, v := range p.versions {
		if v.id == version {
			return v.secret, true
		}
	}
	return nil, false
}

func keyedHash(pepper []byte, secret string) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// hash is secret under the current pepper, or bare with none.
func (p Peppers) hash(secret string) (hash, version string) {
	if len(p.versions) == 0 {
		return hashSecret(secret), ""
	}
	return keyedHash(p.versions[0].secret, secret), p.versions[0].id
}

// Seal replaces a new key's hash with the keyed one. plaintext is the whole
// key as NewKey returned it.
func (p Peppers) Seal(key *Key, plaintext string) error {
	_, secret, ok := ParseKey(plaintext)
	if !ok {
		return errors.New("sealing a key that does not parse")
	}
	key.Hash, key.PepperVersion = p.hash(secret)
	return nil
}

// verify checks secret against key's stored hash, under the version the key
// names. stale reports a match under something other than the current pepper,
// which the caller rehashes. A version no longer configured never matches:
// failing closed is the only safe reading of a hash nobody can check.
func (p Peppers) verify(key Key, secret string) (ok, stale bool) {
	computed, known := hashSecret(secret), true
	if key.PepperVersion != "" {
		pepper, found := p.find(key.PepperVersion)
		if !found {
			// Still hashed and compared, so an unknown version costs what a
			// wrong secret does.
			pepper, known = make([]byte, shortestPepper), false
		}
		computed = keyedHash(pepper, secret)
	}
	ok = subtle.ConstantTimeCompare([]byte(computed), []byte(key.Hash)) == 1 && known
	return ok, ok && key.PepperVersion != p.Current()
}

// ErrInvalidKey is every way a presented key can fail before its secret is
// proven, as one error: which part was wrong is not something an
// unauthenticated caller learns.
var ErrInvalidKey = errors.New("invalid api key")

// Refusals of a key whose secret was right. The caller has proven it holds
// the key, so telling it why is no help to anyone who does not.
var (
	ErrKeyExpired     = fmt.Errorf("%w: expired", ErrInvalidKey)
	ErrOutOfScope     = errors.New("this api key's kind does not allow this call")
	ErrAddressRefused = errors.New("this api key is not allowed from this address")
	ErrKeyRateLimited = errors.New("this api key is over its rate limit")
)

// ResolveKey checks a presented key against an unpeppered store and returns
// what it is, without scope, address, rate or audit. For tests and tools; the
// service authorises through KeyAuth.
func ResolveKey(ctx context.Context, keys Store, presented string) (Key, error) {
	id, secret, ok := ParseKey(presented)
	if !ok {
		return Key{}, ErrInvalidKey
	}
	key, err := keys.GetKey(ctx, id)
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	if matched, _ := (Peppers{}).verify(key, secret); !matched || !key.WorksAt(time.Now()) {
		return Key{}, ErrInvalidKey
	}
	return key, nil
}

// Accounts lets the control plane's gRPC interceptor, and the HTTP routes
// that share its accounts, accept API keys.
//
// A backend key is an owner of its tenant: the customer administers their own
// rooms with it. The narrower kinds carry narrower ceilings, and KeyAuth also
// refuses them any method outside their scope before the handler runs. Relay
// keys are refused here outright.
type Accounts struct {
	Store Store
	// Keys is the service's authoriser, which holds the peppers, the rate
	// limits and the audit buffer. Nil builds one per call with none of them,
	// for tests.
	Keys *KeyAuth
}

func (a Accounts) Lookup(ctx context.Context, credential string) (server.ServiceAccount, error) {
	if _, _, ok := ParseKey(credential); !ok {
		return server.ServiceAccount{}, server.ErrNoSuchAccount
	}
	keys := a.Keys
	if keys == nil {
		keys = &KeyAuth{Store: a.Store}
	}
	key, err := keys.Authorize(ctx, credential, UseFrom(ctx))
	switch {
	case errors.Is(err, ErrOutOfScope), errors.Is(err, ErrAddressRefused):
		return server.ServiceAccount{}, fmt.Errorf("%w: %w", server.ErrNotPermitted, err)
	case errors.Is(err, ErrKeyRateLimited):
		return server.ServiceAccount{}, fmt.Errorf("%w: %w", server.ErrRateLimited, err)
	case err != nil:
		return server.ServiceAccount{}, server.ErrNoSuchAccount
	}
	return server.ServiceAccount{
		Name:   "key:" + key.ID,
		Tenant: key.Tenant,
		Grants: grantsFor(key.Kind),
	}, nil
}

// grantsFor is each kind's ceiling. A mint key holds every publish right and
// no moderation, so the token ceiling in MintClientToken lets it mint viewers
// and publishers and refuses moderators and owners whatever is asked.
func grantsFor(kind KeyKind) control.Grants {
	switch kind {
	case KeyBackend:
		return control.Grants{IsOwner: true}
	case KeyMint:
		return control.Grants{Publish: control.PublishFull}
	case KeyReadOnly:
		return control.Grants{Moderation: control.CapReadModerationLog}
	}
	return control.Grants{}
}
