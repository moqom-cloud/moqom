// Package postgres keeps Apple sign-in settings and App Attest counters in
// Postgres, beside the room store's tables.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/applesignin"
	"github.com/moqom-cloud/moqom/go/internal/control"
)

//go:embed schema.sql
var schema string

// Store is applesignin.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ applesignin.Store = (*Store)(nil)

// New applies the schema and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	const lock = 0x6d7161706c65 // "mqaple"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return nil, fmt.Errorf("taking the Apple sign-in migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	if _, err := connection.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying the Apple sign-in schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Get(ctx context.Context, tenant string) (applesignin.Settings, error) {
	var (
		settings applesignin.Settings
		publish  int16
	)
	err := s.pool.QueryRow(ctx, `SELECT tenant, team_id, bundle_ids, rooms, publish, allow_development, updated_at
		FROM apple_sign_in WHERE tenant = $1`, tenant).Scan(
		&settings.Tenant, &settings.TeamID, &settings.BundleIDs, &settings.Rooms,
		&publish, &settings.AllowDevelopment, &settings.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return applesignin.Settings{}, applesignin.ErrNotFound
	}
	if err != nil {
		return applesignin.Settings{}, err
	}
	settings.Publish = control.PublishScope(publish)
	settings.UpdatedAt = settings.UpdatedAt.UTC()
	return settings, nil
}

func (s *Store) Put(ctx context.Context, settings applesignin.Settings) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO apple_sign_in
		(tenant, team_id, bundle_ids, rooms, publish, allow_development, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant) DO UPDATE SET team_id = EXCLUDED.team_id,
			bundle_ids = EXCLUDED.bundle_ids, rooms = EXCLUDED.rooms, publish = EXCLUDED.publish,
			allow_development = EXCLUDED.allow_development, updated_at = EXCLUDED.updated_at`,
		settings.Tenant, settings.TeamID, settings.BundleIDs, settings.Rooms,
		int16(settings.Publish), settings.AllowDevelopment, settings.UpdatedAt)
	return err
}

func (s *Store) Advance(ctx context.Context, keyID string, counter uint32) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO apple_attest_counters (key_id, counter) VALUES ($1, $2)
		ON CONFLICT (key_id) DO UPDATE SET counter = EXCLUDED.counter
		WHERE apple_attest_counters.counter < EXCLUDED.counter`, keyID, int64(counter))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) Counter(ctx context.Context, keyID string) (uint32, error) {
	var counter int64
	err := s.pool.QueryRow(ctx, `SELECT counter FROM apple_attest_counters WHERE key_id = $1`, keyID).Scan(&counter)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return uint32(counter), err
}
