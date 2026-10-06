// Package postgres keeps the MLS delivery service's state in Postgres, beside
// the control plane's tables, so that every replica orders a room's commits
// the same way.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/mls"
)

//go:embed schema.sql
var schema string

// Store is mls.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ mls.Store = (*Store)(nil)

// New applies the schema and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	// Its own lock, as billing has: independent migrations need not wait on
	// each other.
	const lock = 0x6d716d6c73 // "mqmls"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return nil, fmt.Errorf("taking the mls migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	if _, err := connection.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying the mls schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) PutKeyPackages(ctx context.Context, room mls.Room, device string, oneTime [][]byte, lastResort []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mls_key_packages WHERE tenant = $1 AND room = $2 AND device = $3`,
			room.Tenant, room.Room, device); err != nil {
			return err
		}
		for _, data := range oneTime {
			if _, err := tx.Exec(ctx, `INSERT INTO mls_key_packages (tenant, room, device, last_resort, data)
				VALUES ($1, $2, $3, false, $4)`, room.Tenant, room.Room, device, data); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO mls_key_packages (tenant, room, device, last_resort, data)
			VALUES ($1, $2, $3, true, $4)`, room.Tenant, room.Room, device, lastResort)
		return err
	})
}

// ClaimKeyPackage deletes the oldest one-time package in the same statement
// that reads it, with SKIP LOCKED so two committers claiming at once each get
// a different one rather than one waiting for the other and then failing.
func (s *Store) ClaimKeyPackage(ctx context.Context, room mls.Room, device string) ([]byte, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, `DELETE FROM mls_key_packages WHERE id = (
			SELECT id FROM mls_key_packages
			WHERE tenant = $1 AND room = $2 AND device = $3 AND NOT last_resort
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
		) RETURNING data`, room.Tenant, room.Room, device).Scan(&data)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `SELECT data FROM mls_key_packages
		WHERE tenant = $1 AND room = $2 AND device = $3 AND last_resort
		ORDER BY id DESC LIMIT 1`, room.Tenant, room.Room, device).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, mls.ErrNotFound
	}
	return data, err
}

func (s *Store) Touch(ctx context.Context, room mls.Room, device string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO mls_devices (tenant, room, device, last_seen) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant, room, device) DO UPDATE SET last_seen = GREATEST(mls_devices.last_seen, EXCLUDED.last_seen)`,
		room.Tenant, room.Room, device, at.UTC())
	return err
}

func (s *Store) SetMember(ctx context.Context, room mls.Room, device string, member bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO mls_devices (tenant, room, device, member, last_seen)
		VALUES ($1, $2, $3, $4, 'epoch')
		ON CONFLICT (tenant, room, device) DO UPDATE SET member = EXCLUDED.member`,
		room.Tenant, room.Room, device, member)
	return err
}

func (s *Store) Leave(ctx context.Context, room mls.Room, device string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mls_devices WHERE tenant = $1 AND room = $2 AND device = $3`,
			room.Tenant, room.Room, device); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM mls_key_packages WHERE tenant = $1 AND room = $2 AND device = $3`,
			room.Tenant, room.Room, device)
		return err
	})
}

func (s *Store) Presence(ctx context.Context, room mls.Room) ([]mls.Presence, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.device, d.member, d.last_seen,
			EXISTS (SELECT 1 FROM mls_key_packages k
				WHERE k.tenant = d.tenant AND k.room = d.room AND k.device = d.device)
		FROM mls_devices d WHERE d.tenant = $1 AND d.room = $2 ORDER BY d.device`, room.Tenant, room.Room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mls.Presence
	for rows.Next() {
		var entry mls.Presence
		if err := rows.Scan(&entry.Device, &entry.Member, &entry.LastSeen, &entry.HasKeyPackages); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) Group(ctx context.Context, room mls.Room) (mls.Group, error) {
	var group mls.Group
	err := s.pool.QueryRow(ctx, `SELECT group_id, epoch FROM mls_rooms
		WHERE tenant = $1 AND room = $2 AND group_id IS NOT NULL`, room.Tenant, room.Room).Scan(&group.ID, &group.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return mls.Group{}, mls.ErrNotFound
	}
	return group, err
}

func (s *Store) CreateGroup(ctx context.Context, room mls.Room, id []byte, founder string, at time.Time) (mls.Group, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The upsert only takes a row whose group is empty, so two founders
		// racing on the same room each see exactly one outcome.
		tag, err := tx.Exec(ctx, `INSERT INTO mls_rooms (tenant, room, group_id, epoch, founded_at)
			VALUES ($1, $2, $3, 0, $4)
			ON CONFLICT (tenant, room) DO UPDATE SET group_id = EXCLUDED.group_id, epoch = 0, founded_at = EXCLUDED.founded_at
			WHERE mls_rooms.group_id IS NULL`, room.Tenant, room.Room, id, at.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return mls.ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO mls_devices (tenant, room, device, member, last_seen) VALUES ($1, $2, $3, true, $4)
			ON CONFLICT (tenant, room, device) DO UPDATE SET member = true,
				last_seen = GREATEST(mls_devices.last_seen, EXCLUDED.last_seen)`,
			room.Tenant, room.Room, founder, at.UTC())
		return err
	})
	if err != nil {
		return mls.Group{}, err
	}
	return mls.Group{ID: id}, nil
}

func (s *Store) DropGroup(ctx context.Context, room mls.Room, id []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mls_rooms SET group_id = NULL, epoch = 0
			WHERE tenant = $1 AND room = $2 AND group_id = $3`, room.Tenant, room.Room, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mls_messages WHERE tenant = $1 AND room = $2`, room.Tenant, room.Room); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mls_devices SET member = false WHERE tenant = $1 AND room = $2`, room.Tenant, room.Room)
		return err
	})
}

// AppendCommit locks the room's row, so the epoch check and the advance are
// one step on every replica: this is the whole of "first commit wins".
func (s *Store) AppendCommit(ctx context.Context, room mls.Room, commit mls.Commit, at time.Time) (uint64, error) {
	var first uint64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id []byte
		var epoch, last uint64
		err := tx.QueryRow(ctx, `SELECT group_id, epoch, last_seq FROM mls_rooms
			WHERE tenant = $1 AND room = $2 FOR UPDATE`, room.Tenant, room.Room).Scan(&id, &epoch, &last)
		if errors.Is(err, pgx.ErrNoRows) {
			return mls.ErrStale
		} else if err != nil {
			return err
		}
		if id == nil || string(id) != string(commit.GroupID) || epoch != commit.Epoch {
			return mls.ErrStale
		}

		if _, err := tx.Exec(ctx, `DELETE FROM mls_messages WHERE tenant = $1 AND room = $2 AND at < $3`,
			room.Tenant, room.Room, at.Add(-mls.MessageRetention).UTC()); err != nil {
			return err
		}
		first = last + 1
		if _, err := tx.Exec(ctx, `INSERT INTO mls_messages (tenant, room, seq, kind, epoch, sender, data, at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			room.Tenant, room.Room, first, string(mls.KindCommit), commit.Epoch, commit.Sender, commit.Data, at.UTC()); err != nil {
			return err
		}
		last = first
		if len(commit.Welcome) > 0 {
			last++
			if _, err := tx.Exec(ctx, `INSERT INTO mls_messages (tenant, room, seq, kind, epoch, sender, recipients, data, at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				room.Tenant, room.Room, last, string(mls.KindWelcome), commit.Epoch+1, commit.Sender,
				commit.Joiners, commit.Welcome, at.UTC()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE mls_rooms SET epoch = epoch + 1, last_seq = $3
			WHERE tenant = $1 AND room = $2`, room.Tenant, room.Room, last); err != nil {
			return err
		}
		// Removed before joined, so a device replaced in one commit ends up in.
		if len(commit.Removed) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE mls_devices SET member = false
				WHERE tenant = $1 AND room = $2 AND device = ANY($3)`, room.Tenant, room.Room, commit.Removed); err != nil {
				return err
			}
		}
		for _, joiner := range commit.Joiners {
			if _, err := tx.Exec(ctx, `INSERT INTO mls_devices (tenant, room, device, member, last_seen)
				VALUES ($1, $2, $3, true, $4)
				ON CONFLICT (tenant, room, device) DO UPDATE SET member = true`,
				room.Tenant, room.Room, joiner, at.UTC()); err != nil {
				return err
			}
		}
		return nil
	})
	return first, err
}

func (s *Store) Messages(ctx context.Context, room mls.Room, after uint64, device string, limit int) ([]mls.Message, uint64, error) {
	var last uint64
	err := s.pool.QueryRow(ctx, `SELECT last_seq FROM mls_rooms WHERE tenant = $1 AND room = $2`,
		room.Tenant, room.Room).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, kind, epoch, sender, recipients, data, at FROM mls_messages
		WHERE tenant = $1 AND room = $2 AND seq > $3 AND (kind = $4 OR $5 = ANY(recipients))
		ORDER BY seq LIMIT $6`, room.Tenant, room.Room, after, string(mls.KindCommit), device, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []mls.Message{}
	for rows.Next() {
		var message mls.Message
		var kind string
		if err := rows.Scan(&message.Seq, &kind, &message.Epoch, &message.Sender, &message.Recipients,
			&message.Data, &message.At); err != nil {
			return nil, 0, err
		}
		message.Kind = mls.MessageKind(kind)
		out = append(out, message)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) == limit {
		last = out[len(out)-1].Seq
	}
	return out, last, nil
}
