// Package postgres keeps hosted recordings in Postgres, beside the room
// store's tables, so every replica hands a recording to one worker at a time.
package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/recording"
)

//go:embed schema.sql
var schema string

// Store is recording.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ recording.Store = (*Store)(nil)

// New applies the schema and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	// Its own lock, as billing and key delivery have.
	const lock = 0x6d71726563 // "mqrec"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return nil, fmt.Errorf("taking the recording migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	if _, err := connection.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying the recording schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

const active = `state IN ('pending', 'recording', 'stopping')`

type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func write(ctx context.Context, q querier, r recording.Recording, insert bool) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var lease *time.Time
	if !r.LeaseUntil.IsZero() {
		lease = &r.LeaseUntil
	}
	if insert {
		_, err = q.Exec(ctx, `INSERT INTO recordings (id, tenant, room, state, created_at, worker, lease_until, doc)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			r.ID, r.Tenant, r.Room, string(r.State), r.CreatedAt, r.Worker, lease, doc)
	} else {
		_, err = q.Exec(ctx, `UPDATE recordings SET state = $2, worker = $3, lease_until = $4, doc = $5 WHERE id = $1`,
			r.ID, string(r.State), r.Worker, lease, doc)
	}
	var violation *pgconn.PgError
	if errors.As(err, &violation) && violation.Code == "23505" {
		if violation.ConstraintName == "recordings_one_active" {
			return recording.ErrActive
		}
		return fmt.Errorf("%w: recording %s", control.ErrAlreadyExists, r.ID)
	}
	return err
}

func scan(row pgx.Row) (recording.Recording, error) {
	var doc []byte
	if err := row.Scan(&doc); err != nil {
		return recording.Recording{}, err
	}
	var r recording.Recording
	if err := json.Unmarshal(doc, &r); err != nil {
		return recording.Recording{}, fmt.Errorf("decoding a recording: %w", err)
	}
	return r, nil
}

func (s *Store) Create(ctx context.Context, r recording.Recording) (recording.Recording, error) {
	// To the column's precision, so the document and the column a page token
	// is compared against hold the same instant.
	r.CreatedAt = r.CreatedAt.Truncate(time.Microsecond)
	if err := write(ctx, s.pool, r, true); err != nil {
		return recording.Recording{}, err
	}
	return r, nil
}

func (s *Store) Get(ctx context.Context, tenant, id string) (recording.Recording, error) {
	found, err := scan(s.pool.QueryRow(ctx, `SELECT doc FROM recordings WHERE id = $1 AND tenant = $2`, id, tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return recording.Recording{}, fmt.Errorf("%w: recording %s", control.ErrNotFound, id)
	}
	return found, err
}

func (s *Store) List(ctx context.Context, query recording.Query) (recording.Page, error) {
	first := query.PageToken == ""
	var after time.Time
	afterID := ""
	if !first {
		at, id, err := recording.ParseCursor(query.PageToken)
		if err != nil {
			return recording.Page{}, err
		}
		after, afterID = at, id
	}
	size := query.Size()
	rows, err := s.pool.Query(ctx, `SELECT doc FROM recordings
		WHERE tenant = $1 AND ($2 = '' OR room = $2)
		  AND ($3 OR (created_at, id) < ($4, $5))
		ORDER BY created_at DESC, id DESC
		LIMIT $6`, query.Tenant, query.Room, first, after, afterID, size+1)
	if err != nil {
		return recording.Page{}, err
	}
	defer rows.Close()
	var page recording.Page
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return recording.Page{}, err
		}
		page.Recordings = append(page.Recordings, r)
	}
	if err := rows.Err(); err != nil {
		return recording.Page{}, err
	}
	if len(page.Recordings) > size {
		page.Recordings = page.Recordings[:size]
		page.NextPageToken = recording.Cursor(page.Recordings[size-1])
	}
	return page, nil
}

func (s *Store) Active(ctx context.Context, tenant, room string) ([]recording.Recording, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM recordings WHERE tenant = $1 AND room = $2 AND `+active, tenant, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []recording.Recording
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	return found, rows.Err()
}

func (s *Store) Update(ctx context.Context, id string, change func(*recording.Recording) error) (recording.Recording, error) {
	var updated recording.Recording
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		found, err := scan(tx.QueryRow(ctx, `SELECT doc FROM recordings WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: recording %s", control.ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if err := change(&found); err != nil {
			return err
		}
		updated = found
		return write(ctx, tx, found, false)
	})
	return updated, err
}

// Claim locks the oldest claimable row with SKIP LOCKED, so two workers
// claiming at once each get a different recording rather than one waiting
// for the other and then finding it taken.
func (s *Store) Claim(ctx context.Context, worker string, now time.Time, lease time.Duration) (recording.Recording, bool, error) {
	var claimed recording.Recording
	found := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := scan(tx.QueryRow(ctx, `SELECT doc FROM recordings
			WHERE `+active+` AND (worker = '' OR lease_until IS NULL OR lease_until <= $1)
			ORDER BY created_at, id
			LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		recording.Claimed(&r, worker, now, lease)
		claimed, found = r, true
		return write(ctx, tx, r, false)
	})
	return claimed, found, err
}

// StorageDue reads the billing watermark from the document, which is where
// it lives, and checks each row with the same test the memory store uses.
func (s *Store) StorageDue(ctx context.Context, before time.Time, limit int) ([]recording.Recording, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM recordings
		WHERE jsonb_array_length(COALESCE(doc->'artifacts', '[]'::jsonb)) > 0
		  AND NOT COALESCE((doc->>'storage_done')::boolean, false)
		  AND COALESCE((doc->>'storage_billed_through')::timestamptz, '-infinity') < $1
		ORDER BY created_at, id
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []recording.Recording
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if recording.StorageOwed(r, before) {
			due = append(due, r)
		}
	}
	return due, rows.Err()
}
