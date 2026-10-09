package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// Relay pools.

// poolRegion is control.PoolRegion as stored in relay_pools.regions. Its own
// type, with names of its own, so renaming a Go field is not a migration.
type poolRegion struct {
	Region    string `json:"region"`
	Relays    uint32 `json:"relays"`
	MaxRelays uint32 `json:"max_relays"`
}

func regionsToJSON(regions []control.PoolRegion) ([]byte, error) {
	stored := make([]poolRegion, 0, len(regions))
	for _, region := range regions {
		stored = append(stored, poolRegion(region))
	}
	return json.Marshal(stored)
}

func regionsFromJSON(raw []byte) ([]control.PoolRegion, error) {
	var stored []poolRegion
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	regions := make([]control.PoolRegion, 0, len(stored))
	for _, region := range stored {
		regions = append(regions, control.PoolRegion(region))
	}
	return regions, nil
}

const poolColumns = `id, tenant, name, regions, self_hosted, access, state, created_at`

func scanPool(row pgx.Row) (control.RelayPool, error) {
	var (
		pool          control.RelayPool
		regions       []byte
		access, state string
	)
	if err := row.Scan(&pool.ID, &pool.Tenant, &pool.Name, &regions, &pool.SelfHosted,
		&access, &state, &pool.CreatedAt); err != nil {
		return control.RelayPool{}, err
	}
	decoded, err := regionsFromJSON(regions)
	if err != nil {
		return control.RelayPool{}, fmt.Errorf("reading pool %s's regions: %w", pool.ID, err)
	}
	pool.Regions = decoded
	pool.Access = control.PoolAccess(access)
	pool.State = control.PoolState(state)
	pool.CreatedAt = pool.CreatedAt.UTC()
	return pool, nil
}

func (s *Store) CreateRelayPool(ctx context.Context, pool control.RelayPool) (control.RelayPool, error) {
	if pool.CreatedAt.IsZero() {
		pool.CreatedAt = s.now()
	}
	regions, err := regionsToJSON(pool.Regions)
	if err != nil {
		return control.RelayPool{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO relay_pools (`+poolColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING`,
		pool.ID, pool.Tenant, pool.Name, regions, pool.SelfHosted,
		string(pool.Access), string(pool.State), pool.CreatedAt)
	if err != nil {
		return control.RelayPool{}, fmt.Errorf("creating a relay pool: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return control.RelayPool{}, fmt.Errorf("%w: relay pool %s", control.ErrAlreadyExists, pool.ID)
	}
	pool.Relays = nil
	return pool, nil
}

func (s *Store) GetRelayPool(ctx context.Context, tenant, id string) (control.RelayPool, error) {
	pool, err := scanPool(s.pool.QueryRow(ctx,
		`SELECT `+poolColumns+` FROM relay_pools WHERE id = $1 AND tenant = $2`, id, tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.RelayPool{}, fmt.Errorf("%w: relay pool %s", store.ErrNotFound, id)
	}
	if err != nil {
		return control.RelayPool{}, fmt.Errorf("reading a relay pool: %w", err)
	}
	if pool.Relays, err = s.poolRelays(ctx, id); err != nil {
		return control.RelayPool{}, err
	}
	return pool, nil
}

func (s *Store) poolRelays(ctx context.Context, id string) ([]control.PoolRelay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT relay_id, address, region, state, seen_at FROM pool_relays
		WHERE pool = $1 ORDER BY relay_id COLLATE "C"`, id)
	if err != nil {
		return nil, fmt.Errorf("listing a pool's relays: %w", err)
	}
	defer rows.Close()
	var relays []control.PoolRelay
	for rows.Next() {
		var relay control.PoolRelay
		var state string
		if err := rows.Scan(&relay.RelayID, &relay.Address, &relay.Region, &state, &relay.SeenAt); err != nil {
			return nil, fmt.Errorf("reading a pool relay: %w", err)
		}
		relay.State = control.PoolRelayState(state)
		relay.SeenAt = relay.SeenAt.UTC()
		relays = append(relays, relay)
	}
	return relays, rows.Err()
}

func (s *Store) ListRelayPools(ctx context.Context, tenant string) ([]control.RelayPool, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+poolColumns+` FROM relay_pools
		WHERE tenant = $1 ORDER BY created_at, id COLLATE "C"`, tenant)
	if err != nil {
		return nil, fmt.Errorf("listing relay pools: %w", err)
	}
	pools := []control.RelayPool{}
	for rows.Next() {
		pool, err := scanPool(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading a relay pool: %w", err)
		}
		pools = append(pools, pool)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pools {
		if pools[i].Relays, err = s.poolRelays(ctx, pools[i].ID); err != nil {
			return nil, err
		}
	}
	return pools, nil
}

func (s *Store) SaveRelayPool(ctx context.Context, pool control.RelayPool) (control.RelayPool, error) {
	regions, err := regionsToJSON(pool.Regions)
	if err != nil {
		return control.RelayPool{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_pools SET name = $3, regions = $4, self_hosted = $5, access = $6, state = $7
		WHERE id = $1 AND tenant = $2`,
		pool.ID, pool.Tenant, pool.Name, regions, pool.SelfHosted, string(pool.Access), string(pool.State))
	if err != nil {
		return control.RelayPool{}, fmt.Errorf("saving a relay pool: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return control.RelayPool{}, fmt.Errorf("%w: relay pool %s", store.ErrNotFound, pool.ID)
	}
	return s.GetRelayPool(ctx, pool.Tenant, pool.ID)
}

func (s *Store) PutPoolRelay(ctx context.Context, tenant, id string, relay control.PoolRelay) error {
	if relay.SeenAt.IsZero() {
		relay.SeenAt = s.now()
	}
	// The pool's tenant is checked in the same statement as the write, so a
	// relay of one tenant cannot be recorded into another's pool.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO pool_relays (pool, relay_id, address, region, state, seen_at)
		SELECT id, $3, $4, $5, $6, $7 FROM relay_pools WHERE id = $1 AND tenant = $2
		ON CONFLICT (pool, relay_id) DO UPDATE
		SET address = EXCLUDED.address, region = EXCLUDED.region,
		    state = EXCLUDED.state, seen_at = EXCLUDED.seen_at`,
		id, tenant, relay.RelayID, relay.Address, relay.Region, string(relay.State), relay.SeenAt)
	if err != nil {
		return fmt.Errorf("recording a pool relay: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: relay pool %s", store.ErrNotFound, id)
	}
	return nil
}

func (s *Store) PoolTenants(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT tenant FROM relay_pools
		WHERE state <> $1 ORDER BY tenant COLLATE "C"`, string(control.PoolDeleted))
	if err != nil {
		return nil, fmt.Errorf("listing tenants with pools: %w", err)
	}
	defer rows.Close()
	tenants := []string{}
	for rows.Next() {
		var tenant string
		if err := rows.Scan(&tenant); err != nil {
			return nil, err
		}
		tenants = append(tenants, tenant)
	}
	return tenants, rows.Err()
}

func (s *Store) GetTenantNetwork(ctx context.Context, tenant string) (control.TenantNetwork, error) {
	network := control.TenantNetwork{Tenant: tenant}
	var mode string
	err := s.pool.QueryRow(ctx, `SELECT mode, pool FROM tenant_networks WHERE tenant = $1`, tenant).
		Scan(&mode, &network.Pool)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return control.TenantNetwork{}, fmt.Errorf("reading a tenant's network: %w", err)
	default:
		network.Mode = control.NetworkMode(mode)
	}
	return network.Normalised(), nil
}

func (s *Store) SetTenantNetwork(ctx context.Context, network control.TenantNetwork) (control.TenantNetwork, error) {
	network = network.Normalised()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_networks (tenant, mode, pool) VALUES ($1, $2, $3)
		ON CONFLICT (tenant) DO UPDATE SET mode = EXCLUDED.mode, pool = EXCLUDED.pool`,
		network.Tenant, string(network.Mode), network.Pool); err != nil {
		return control.TenantNetwork{}, fmt.Errorf("setting a tenant's network: %w", err)
	}
	return network, nil
}
