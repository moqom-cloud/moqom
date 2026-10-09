package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	sectoken "github.com/moqom-cloud/moqom-security/go/token"
	"github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// Which key seals a tenant's tokens, and which relays hold which keys.
//
// The deployment's seal keys open every tenant's tokens, so only relays MOQOM
// runs for every tenant hold them. A tenant with relays of its own has its
// tokens sealed with its own key (token.TenantSealKey), which its relays hold
// alone; the shared fleet holds it as well, so the same token works on both.
//
// The set of tenants with relays of their own is read again every
// sealRefreshInterval. When it changes, the shared fleet is sent the new keys
// before any token is sealed with them.

// sealRefreshInterval bounds how long a tenant's newly enrolled relay may wait
// before tokens it can open are minted.
const sealRefreshInterval = 30 * time.Second

// SealTenantSource names tenants that run relays of their own.
type SealTenantSource interface {
	ActiveTenants(ctx context.Context) ([]string, error)
}

type sealState struct {
	// own is every tenant whose tokens are sealed with its own key.
	own map[string]bool
	// shared is what the shared fleet and the control plane open tokens with:
	// the deployment keys, and every own tenant's key under each of them.
	shared []sectoken.SealKey
}

type sealer struct {
	// keys are the deployment's, newest first; the first seals.
	keys []sectoken.SealKey
	// static are tenants known to run relays from configuration: tenant-scoped
	// relay accounts.
	static []string
	// sources are read for the rest.
	sources []SealTenantSource
	// since is when this process started sealing, and so when tokens minted
	// unsealed before it stop mattering: MaxLifetime later.
	since time.Time

	state atomic.Pointer[sealState]
}

func newSealer(keys []sectoken.SealKey, static []string, sources []SealTenantSource, since time.Time) *sealer {
	s := &sealer{keys: keys, static: static, sources: sources, since: since}
	s.state.Store(s.compute(context.Background()))
	return s
}

func (s *sealer) enabled() bool { return s != nil && len(s.keys) > 0 }

// current is the state minting uses, or nil without seal keys.
func (s *sealer) current() *sealState {
	if !s.enabled() {
		return nil
	}
	return s.state.Load()
}

// unsealedUntil is when an unsealed token stops being accepted.
func (s *sealer) unsealedUntil() time.Time { return s.since.Add(token.MaxLifetime) }

// sealFor seals a signed token for whoever serves tenant. Without seal keys the
// token goes out as signed.
func (s *sealer) seal(tenant, signed string) (string, error) {
	if !s.enabled() {
		return signed, nil
	}
	key := s.keys[0]
	if s.state.Load().own[tenant] {
		derived, err := token.TenantSealKey(key, tenant)
		if err != nil {
			return "", err
		}
		key = derived
	}
	return key.Seal(signed)
}

// keysFor is what a relay serving tenant is given; "" is the shared fleet.
func (s *sealer) keysFor(tenant string) []sectoken.SealKey {
	if !s.enabled() {
		return nil
	}
	if tenant == "" {
		return s.state.Load().shared
	}
	keys := make([]sectoken.SealKey, 0, len(s.keys))
	for _, master := range s.keys {
		derived, err := token.TenantSealKey(master, tenant)
		if err != nil {
			continue
		}
		keys = append(keys, derived)
	}
	return keys
}

// compute reads the tenants with relays of their own. A source that cannot be
// read keeps the tenants it last named, so an outage of the store does not
// switch a tenant's tokens to a key its relays do not hold.
func (s *sealer) compute(ctx context.Context) *sealState {
	own := map[string]bool{}
	for _, tenant := range s.static {
		own[tenant] = true
	}
	previous := s.state.Load()
	for _, source := range s.sources {
		tenants, err := source.ActiveTenants(ctx)
		if err != nil {
			slog.WarnContext(ctx, "could not read tenants with relays of their own", "error", err)
			if previous != nil {
				for tenant := range previous.own {
					own[tenant] = true
				}
			}
			continue
		}
		for _, tenant := range tenants {
			own[tenant] = true
		}
	}

	tenants := make([]string, 0, len(own))
	for tenant := range own {
		tenants = append(tenants, tenant)
	}
	slices.Sort(tenants)
	shared := slices.Clone(s.keys)
	for _, master := range s.keys {
		for _, tenant := range tenants {
			derived, err := token.TenantSealKey(master, tenant)
			if err != nil {
				continue
			}
			shared = append(shared, derived)
		}
	}
	return &sealState{own: own, shared: shared}
}

// mint signs a token with the newest key and seals it for whoever serves its
// tenant. Every token the control plane issues goes through here.
func (c *core) mint(claims token.Claims, lifetime time.Duration) (string, time.Time, error) {
	signed, expires, err := token.Mint(c.keys[0], claims, lifetime, c.now())
	if err != nil {
		return "", time.Time{}, err
	}
	sealed, err := c.sealer.seal(claims.Tenant, signed)
	if err != nil {
		return "", time.Time{}, err
	}
	return sealed, expires, nil
}

// sealKeysToProto renders keys for the relay link.
func sealKeysToProto(keys []sectoken.SealKey) []*relaypb.SealKey {
	out := make([]*relaypb.SealKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, &relaypb.SealKey{KeyId: key.ID(), Secret: key.Secret()})
	}
	return out
}

// refreshSealing recomputes the tenants with relays of their own, sends every
// relay whose keys changed its new set, and only then lets minting use them.
func (c *core) refreshSealing(ctx context.Context) {
	if !c.sealer.enabled() {
		return
	}
	next := c.sealer.compute(ctx)
	for _, connection := range c.relays.all() {
		c.sendVerificationKeys(connection, next)
	}
	c.sealer.state.Store(next)
	c.verifier.SetSealKeys(next.shared, c.sealer.unsealedUntil())
}

// runSealRefresh refreshes until ctx ends.
func (c *core) runSealRefresh(ctx context.Context) {
	if !c.sealer.enabled() {
		return
	}
	ticker := time.NewTicker(sealRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshSealing(ctx)
		}
	}
}

// sendVerificationKeys sends a relay its keys if they differ from what it was
// last sent. A send that does not fit the queue is tried again next refresh.
func (c *core) sendVerificationKeys(connection *relayConn, state *sealState) {
	keys := c.verificationKeysFor(connection.tenant, state)
	if keys == nil {
		return
	}
	fingerprint := keysFingerprint(keys)
	connection.mu.Lock()
	same := bytes.Equal(connection.sentKeys, fingerprint)
	connection.mu.Unlock()
	if same {
		return
	}
	if connection.enqueue(&relaypb.ControlMessage{Body: &relaypb.ControlMessage_VerificationKeys{
		VerificationKeys: keys,
	}}) {
		connection.mu.Lock()
		connection.sentKeys = fingerprint
		connection.mu.Unlock()
	}
}

func keysFingerprint(keys *relaypb.VerificationKeys) []byte {
	b := sha256.New()
	for _, key := range keys.GetKeys() {
		b.Write([]byte(key.GetKeyId()))
		b.Write([]byte{0})
	}
	b.Write([]byte{1})
	for _, key := range keys.GetSealKeys() {
		b.Write(key.GetKeyId())
		b.Write(key.GetSecret())
	}
	if keys.GetRequireSealed() {
		b.Write([]byte{2})
	}
	return b.Sum(nil)
}
