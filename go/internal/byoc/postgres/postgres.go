// Package postgres keeps customer relay enrolments in Postgres, beside the room
// store's tables.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/byoc"
)

//go:embed schema.sql
var schema string

// Store is byoc.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ byoc.Store = (*Store)(nil)

// New applies the schema and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	// Its own lock, as recordings and billing have.
	const lock = 0x6d7162796f63 // "mqbyoc"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return nil, fmt.Errorf("taking the byoc migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	if _, err := connection.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying the byoc schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

const columns = `id, tenant, name, identity_id, identity_name, created_by, created_at, revoked_at`

func scan(row pgx.Row) (byoc.Enrollment, error) {
	var (
		e       byoc.Enrollment
		revoked *time.Time
	)
	err := row.Scan(&e.ID, &e.Tenant, &e.Name, &e.IdentityID, &e.IdentityName, &e.CreatedBy, &e.CreatedAt, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return byoc.Enrollment{}, byoc.ErrNotFound
	}
	if err != nil {
		return byoc.Enrollment{}, err
	}
	e.CreatedAt = e.CreatedAt.UTC()
	if revoked != nil {
		e.RevokedAt = revoked.UTC()
	}
	return e, nil
}

func (s *Store) Create(ctx context.Context, e byoc.Enrollment) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO byoc_enrollments (`+columns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)`,
		e.ID, e.Tenant, e.Name, e.IdentityID, e.IdentityName, e.CreatedBy, e.CreatedAt)
	var violation *pgconn.PgError
	if errors.As(err, &violation) && violation.Code == "23505" {
		return byoc.ErrExists
	}
	return err
}

func (s *Store) Get(ctx context.Context, id string) (byoc.Enrollment, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM byoc_enrollments WHERE id = $1`, id))
}

func (s *Store) ByIdentity(ctx context.Context, identityName string) (byoc.Enrollment, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM byoc_enrollments WHERE identity_name = $1`, identityName))
}

func (s *Store) List(ctx context.Context, tenant string) ([]byoc.Enrollment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM byoc_enrollments
		WHERE tenant = $1 ORDER BY created_at DESC, id DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var listed []byoc.Enrollment
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		listed = append(listed, e)
	}
	return listed, rows.Err()
}

func (s *Store) Revoke(ctx context.Context, id string, at time.Time) (byoc.Enrollment, error) {
	return scan(s.pool.QueryRow(ctx, `UPDATE byoc_enrollments
		SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1 RETURNING `+columns, id, at))
}
