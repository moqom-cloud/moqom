package billing

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/server"
)

// KeySpec is what a tenant asks for when it issues a key.
type KeySpec struct {
	Name string
	Kind KeyKind
	// ExpiresAt is zero for a key that lasts until revoked.
	ExpiresAt       time.Time
	AllowedNetworks []netip.Prefix
	// RateLimit is requests a minute per replica; zero is the kind's default.
	RateLimit int
}

// Key lifetime bounds. The floor is so an expiry that is already past, or a
// typo of seconds for days, fails at issue rather than producing a key that
// stops before it is deployed.
const (
	MinKeyLifetime  = time.Hour
	MaxKeyLifetime  = 3 * 365 * 24 * time.Hour
	mostNetworks    = 20
	longestKeyName  = 100
	keyAuditPage    = 200
	largestKeyAudit = 1000
)

// ParseNetworks reads an allowlist. A bare address is that one address.
// Prefixes are stored masked, so 203.0.113.7/24 reads back as 203.0.113.0/24
// and what the console shows is what is enforced.
func ParseNetworks(raw []string) ([]netip.Prefix, error) {
	if len(raw) > mostNetworks {
		return nil, fmt.Errorf("%w: at most %d allowed networks", ErrInvalid, mostNetworks)
	}
	var networks []netip.Prefix
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		var prefix netip.Prefix
		if address, err := netip.ParseAddr(entry); err == nil {
			address = address.Unmap()
			prefix = netip.PrefixFrom(address, address.BitLen())
		} else if parsed, err := netip.ParsePrefix(entry); err == nil {
			prefix = netip.PrefixFrom(parsed.Addr().Unmap(), parsed.Bits()).Masked()
		} else {
			return nil, fmt.Errorf("%w: %q is not an address or CIDR prefix", ErrInvalid, entry)
		}
		if !slices.Contains(networks, prefix) {
			networks = append(networks, prefix)
		}
	}
	return networks, nil
}

// IssueKeyWith creates another key for a tenant.
func (s *Service) IssueKeyWith(ctx context.Context, tenant string, spec KeySpec) (Key, string, error) {
	now := s.now()
	spec.Name = strings.TrimSpace(spec.Name)
	switch {
	case !spec.Kind.valid():
		return Key{}, "", fmt.Errorf("%w: key kind must be backend, relay, mint or readonly", ErrInvalid)
	case spec.Name == "" || len(spec.Name) > longestKeyName:
		return Key{}, "", fmt.Errorf("%w: a key name of 1 to 100 characters is required", ErrInvalid)
	case !spec.ExpiresAt.IsZero() && spec.ExpiresAt.Before(now.Add(MinKeyLifetime)):
		return Key{}, "", fmt.Errorf("%w: expires_at must be at least an hour from now", ErrInvalid)
	case !spec.ExpiresAt.IsZero() && spec.ExpiresAt.After(now.Add(MaxKeyLifetime)):
		return Key{}, "", fmt.Errorf("%w: expires_at must be within three years", ErrInvalid)
	case len(spec.AllowedNetworks) > mostNetworks:
		return Key{}, "", fmt.Errorf("%w: at most %d allowed networks", ErrInvalid, mostNetworks)
	case spec.RateLimit < 0 || spec.RateLimit > MaxRateLimit:
		return Key{}, "", fmt.Errorf("%w: rate_limit_per_minute must be between 0 and %d", ErrInvalid, MaxRateLimit)
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Key{}, "", err
	}
	if record.Status == StatusArchived || record.Status == StatusDeleted {
		return Key{}, "", ErrSuspended
	}
	return s.issueSpec(ctx, tenant, spec)
}

func (s *Service) issueSpec(ctx context.Context, tenant string, spec KeySpec) (Key, string, error) {
	key, plaintext, err := NewKey(s.random, tenant, spec.Name, spec.Kind, s.now())
	if err != nil {
		return Key{}, "", err
	}
	key.ExpiresAt = spec.ExpiresAt
	key.AllowedNetworks = slices.Clone(spec.AllowedNetworks)
	key.RateLimit = spec.RateLimit
	if err := s.keys.Peppers.Seal(&key, plaintext); err != nil {
		return Key{}, "", err
	}
	if err := s.store.PutKey(ctx, key); err != nil {
		return Key{}, "", err
	}
	return key, plaintext, nil
}

// replacementFor is what a rotation issues: the same name, kind, allowlist and
// rate, and, for a key that expires, the same lifetime counted from now, so
// rotating is how an expiring key is renewed without its terms changing.
func replacementFor(old Key, now time.Time) KeySpec {
	spec := KeySpec{Name: old.Name, Kind: old.Kind, AllowedNetworks: old.AllowedNetworks, RateLimit: old.RateLimit}
	if !old.ExpiresAt.IsZero() {
		spec.ExpiresAt = now.Add(min(max(old.ExpiresAt.Sub(old.CreatedAt), MinKeyLifetime), MaxKeyLifetime))
	}
	return spec
}

// Keys is the authoriser every key use goes through.
func (s *Service) Keys() *KeyAuth { return s.keys }

// Accounts lets the control plane's interceptor and HTTP routes accept this
// service's keys, sharing its peppers, rate limits and audit.
func (s *Service) Accounts() server.Accounts { return Accounts{Store: s.store, Keys: s.keys} }

// RunKeyAudit writes the key audit buffer every few seconds until ctx ends.
func (s *Service) RunKeyAudit(ctx context.Context) { s.keys.Run(ctx) }

// KeyAudit is a tenant's key use, newest first, for one key or all of them.
// This replica's buffer is written first, so a tenant sees its own last call;
// other replicas' are at most KeyAuditFlushInterval behind.
func (s *Service) KeyAudit(ctx context.Context, tenant, key string, limit int) ([]KeyUse, error) {
	if key != "" {
		found, err := s.store.GetKey(ctx, key)
		if err != nil || found.Tenant != tenant {
			return nil, ErrNotFound
		}
	}
	if err := s.keys.Flush(ctx); err != nil {
		s.log.WarnContext(ctx, "key audit read without this replica's latest uses", "error", err)
	}
	return s.store.ListKeyUse(ctx, tenant, key, limit)
}
