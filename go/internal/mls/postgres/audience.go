package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moqom-cloud/moqom/go/internal/mls"
)

// PutAudienceKey keeps the envelope only while the key is unchanged, since a
// new key cannot open it.
func (s *Store) PutAudienceKey(ctx context.Context, room mls.Room, device string, key []byte, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO mls_audience (tenant, room, device, public_key, last_seen)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant, room, device) DO UPDATE SET
			last_seen = GREATEST(mls_audience.last_seen, EXCLUDED.last_seen),
			sealed_group = CASE WHEN mls_audience.public_key = EXCLUDED.public_key THEN mls_audience.sealed_group END,
			sealed_epoch = CASE WHEN mls_audience.public_key = EXCLUDED.public_key THEN mls_audience.sealed_epoch END,
			envelope = CASE WHEN mls_audience.public_key = EXCLUDED.public_key THEN mls_audience.envelope END,
			public_key = EXCLUDED.public_key`,
		room.Tenant, room.Room, device, key, at.UTC())
	return err
}

func (s *Store) LeaveAudience(ctx context.Context, room mls.Room, device string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM mls_audience WHERE tenant = $1 AND room = $2 AND device = $3`,
		room.Tenant, room.Room, device)
	return err
}

func (s *Store) Audience(ctx context.Context, room mls.Room) ([]mls.AudienceSeat, error) {
	rows, err := s.pool.Query(ctx, `SELECT device, public_key, last_seen, sealed_group, sealed_epoch
		FROM mls_audience WHERE tenant = $1 AND room = $2 ORDER BY device`, room.Tenant, room.Room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []mls.AudienceSeat{}
	for rows.Next() {
		var seat mls.AudienceSeat
		var group []byte
		var epoch *int64
		if err := rows.Scan(&seat.Device, &seat.Key, &seat.LastSeen, &group, &epoch); err != nil {
			return nil, err
		}
		if group != nil && epoch != nil {
			seat.Sealed = &mls.EpochRef{GroupID: group, Epoch: uint64(*epoch)}
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

func (s *Store) AudienceCount(ctx context.Context, room mls.Room, ref mls.EpochRef, since time.Time) (int, int, error) {
	var present, waiting int
	err := s.pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE sealed_group IS DISTINCT FROM $3 OR sealed_epoch IS DISTINCT FROM $4)
		FROM mls_audience WHERE tenant = $1 AND room = $2 AND last_seen >= $5`,
		room.Tenant, room.Room, ref.GroupID, int64(ref.Epoch), since.UTC()).Scan(&present, &waiting)
	return present, waiting, err
}

func (s *Store) PutEnvelopes(ctx context.Context, room mls.Room, envelopes []mls.Envelope) error {
	if len(envelopes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, envelope := range envelopes {
		batch.Queue(`UPDATE mls_audience SET sealed_group = $4, sealed_epoch = $5, envelope = $6
			WHERE tenant = $1 AND room = $2 AND device = $3`,
			room.Tenant, room.Room, envelope.Device, envelope.GroupID, int64(envelope.Epoch), envelope.Data)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *Store) Envelope(ctx context.Context, room mls.Room, device string) (mls.Envelope, error) {
	found := mls.Envelope{Device: device}
	var epoch int64
	err := s.pool.QueryRow(ctx, `SELECT sealed_group, sealed_epoch, envelope FROM mls_audience
		WHERE tenant = $1 AND room = $2 AND device = $3 AND envelope IS NOT NULL`,
		room.Tenant, room.Room, device).Scan(&found.GroupID, &epoch, &found.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return mls.Envelope{}, mls.ErrNotFound
	}
	found.Epoch = uint64(epoch)
	return found, err
}
