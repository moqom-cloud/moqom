package control

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A relay pool is a set of relays that carries one tenant's traffic and nobody
// else's, and a tenant's network says whether its tokens are confined to one.
//
// The confinement is a claim in every token rather than a routing decision,
// because a routing decision is advice a client can ignore and a claim is
// something every relay checks. A pool relay refuses a token that does not name
// its pool; a shared relay refuses one that names any pool. Which is why there is
// no fallback anywhere here: a private tenant whose pool cannot take a session
// is told so, and is never handed a token the shared fleet would accept.

// ErrPoolNotReady is a private tenant whose pool has nowhere to send anybody
// yet. Minting is refused rather than falling back to the shared fleet.
var ErrPoolNotReady = errors.New("the tenant's private relay pool has no relays yet")

// PoolRegions are the MOQOM Cloud regions a pool may run relays in: the
// regions the fleet itself runs in, by their Google Cloud names.
var PoolRegions = []string{
	"asia-northeast1",
	"asia-southeast1",
	"australia-southeast1",
	"europe-west1",
	"europe-west4",
	"us-central1",
	"us-east1",
	"us-east4",
	"us-west1",
}

// MinPoolRelays is the fewest relays a MOQOM-run pool may run in a region, so
// one can restart without dropping the region.
const MinPoolRelays = 2

// PoolAccess is how a pool's relays are reached.
type PoolAccess string

const (
	PoolAccessPublic                PoolAccess = "public"
	PoolAccessPrivateServiceConnect PoolAccess = "private_service_connect"
	PoolAccessOverlay               PoolAccess = "overlay"
)

// PoolState is where a pool is in its life.
type PoolState string

const (
	PoolProvisioning PoolState = "provisioning"
	PoolReady        PoolState = "ready"
	PoolDraining     PoolState = "draining"
	PoolDeleted      PoolState = "deleted"
)

// PoolRelayState is where one relay of a pool is.
type PoolRelayState string

const (
	PoolRelayStarting PoolRelayState = "starting"
	PoolRelayReady    PoolRelayState = "ready"
	PoolRelayDraining PoolRelayState = "draining"
)

// PoolRegion is how many relays a pool runs in one region. MaxRelays above
// Relays is autoscaling up to it.
type PoolRegion struct {
	Region    string
	Relays    uint32
	MaxRelays uint32
}

// PoolRelay is one relay of a pool, as it last attached.
type PoolRelay struct {
	// RelayID is the relay's ID on the relay link, which is what a relay that
	// reattaches is recognised by.
	RelayID string
	// Address is where clients dial it, `host:port`.
	Address string
	Region  string
	State   PoolRelayState
	SeenAt  time.Time
}

// RelayPool is one tenant's pool.
type RelayPool struct {
	ID         string
	Tenant     string
	Name       string
	Regions    []PoolRegion
	SelfHosted bool
	Access     PoolAccess
	State      PoolState
	// Relays is read with the pool and written separately, with PutPoolRelay,
	// because relays attach on their own schedule and a pool's settings are
	// changed by its owner.
	Relays    []PoolRelay
	CreatedAt time.Time
}

// NetworkMode is whether a tenant runs on the shared fleet or its own pool.
type NetworkMode string

const (
	NetworkShared  NetworkMode = "shared"
	NetworkPrivate NetworkMode = "private"
)

// TenantNetwork is which relays a tenant's tokens are good for. The zero value
// reads as the shared fleet, which is every tenant that never said otherwise.
type TenantNetwork struct {
	Tenant string
	Mode   NetworkMode
	// Pool is the pool a private tenant is confined to. Empty when shared.
	Pool string
}

// Private reports whether the tenant is confined to a pool.
func (n TenantNetwork) Private() bool { return n.Mode == NetworkPrivate }

// Normalised fills the default: a network nobody set is shared, and a shared
// network names no pool.
func (n TenantNetwork) Normalised() TenantNetwork {
	if n.Mode != NetworkPrivate {
		n.Mode, n.Pool = NetworkShared, ""
	}
	return n
}

// ValidatePoolRegion checks one region's sizing.
//
// MaxRelays of zero is taken to mean Relays, and is returned that way so what
// is stored is always the bound in force.
func ValidatePoolRegion(region PoolRegion, selfHosted bool) (PoolRegion, error) {
	region.Region = strings.TrimSpace(region.Region)
	if !slices.Contains(PoolRegions, region.Region) {
		return PoolRegion{}, fmt.Errorf("%w: %q is not a region relay pools run in (one of %s)",
			ErrInvalid, region.Region, strings.Join(PoolRegions, ", "))
	}
	least := uint32(MinPoolRelays)
	if selfHosted {
		least = 1
	}
	if region.Relays < least {
		return PoolRegion{}, fmt.Errorf("%w: %s needs at least %d relays, not %d",
			ErrInvalid, region.Region, least, region.Relays)
	}
	if region.MaxRelays == 0 {
		region.MaxRelays = region.Relays
	}
	if region.MaxRelays < region.Relays {
		return PoolRegion{}, fmt.Errorf("%w: %s has max_relays %d below its %d relays",
			ErrInvalid, region.Region, region.MaxRelays, region.Relays)
	}
	return region, nil
}

// ValidatePoolRegions checks a pool's regions and returns them normalised.
// A MOQOM-run pool needs at least one; a self-hosted pool's relays are
// wherever the tenant enrols them, so it may name none.
func ValidatePoolRegions(regions []PoolRegion, selfHosted bool) ([]PoolRegion, error) {
	if len(regions) == 0 && !selfHosted {
		return nil, fmt.Errorf("%w: a pool MOQOM runs needs at least one region", ErrInvalid)
	}
	validated := make([]PoolRegion, 0, len(regions))
	for _, region := range regions {
		checked, err := ValidatePoolRegion(region, selfHosted)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(validated, func(seen PoolRegion) bool { return seen.Region == checked.Region }) {
			return nil, fmt.Errorf("%w: %s is named twice", ErrInvalid, checked.Region)
		}
		validated = append(validated, checked)
	}
	return validated, nil
}

// Resized returns the pool with one region's sizing replaced, or added if the
// pool has no relays there.
func (p RelayPool) Resized(region PoolRegion) RelayPool {
	regions := slices.Clone(p.Regions)
	index := slices.IndexFunc(regions, func(existing PoolRegion) bool { return existing.Region == region.Region })
	if index < 0 {
		regions = append(regions, region)
	} else {
		regions[index] = region
	}
	p.Regions = regions
	return p
}

// Live reports whether the pool still exists.
func (p RelayPool) Live() bool { return p.State != PoolDeleted }

// Addresses are where a client may dial the pool: every ready relay's address,
// sorted so two tokens minted a moment apart list them the same way.
func (p RelayPool) Addresses() []string {
	var addresses []string
	for _, relay := range p.Relays {
		if relay.State == PoolRelayReady && relay.Address != "" && !slices.Contains(addresses, relay.Address) {
			addresses = append(addresses, relay.Address)
		}
	}
	slices.Sort(addresses)
	return addresses
}

// Provisioned reports whether every region has as many ready relays as it
// asked for, which is when a pool MOQOM runs is ready for sessions.
func (p RelayPool) Provisioned() bool {
	ready := map[string]uint32{}
	for _, relay := range p.Relays {
		if relay.State == PoolRelayReady && relay.Address != "" {
			ready[relay.Region]++
		}
	}
	for _, region := range p.Regions {
		if ready[region.Region] < region.Relays {
			return false
		}
	}
	return true
}
