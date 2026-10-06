// Package postgres is the durable Store.
//
// The in-memory store is what the services are developed against and what a
// laptop runs; this is what a deployment runs, and the difference is that a
// restart here does not lose the moderation log. Everything either of them is
// relied upon to do is asserted once, in storetest, and run against both --
// because the risk worth designing for is not that one of them is broken but
// that they differ, and a difference found in production is found by somebody
// whose ban stopped working.
//
// Three things in here are load-bearing and easy to miss.
//
// Ordering is `COLLATE "C"`, not the database's collation. Page tokens are keys
// rather than offsets, so paging is only correct if the comparison that made the
// token is the comparison that consumes it -- and the other side of that
// comparison is Go's byte-wise string ordering. A database created with
// en_US.UTF-8 sorts punctuation differently, which would silently skip and
// repeat rows around any name containing a slash or a dash.
//
// Time comes from the injected clock, never from `now()`. A ban's expiry has to
// be judged against the same clock the rest of the control plane uses, and a
// test that advances a fake clock has to see the effect.
//
// A scope of "everywhere" is the empty string, never NULL. Two NULLs are not
// equal in SQL, so a nullable room would let one person be banned tenant-wide
// twice and leave one of those bans in force after an unban.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

//go:embed schema.sql
var schema string

// Config is what a Store needs to open.
type Config struct {
	// DSN is a libpq connection string or URL.
	//
	// With Instance set it carries the user and the database and nothing else,
	// because the connector supplies the address, the transport and the
	// credential: `user=... dbname=... sslmode=disable`.
	DSN string

	// Instance is a Cloud SQL connection name, `project:region:instance`.
	//
	// Setting it switches on IAM database authentication, which is the reason no
	// database password exists anywhere in this deployment -- not in Terraform
	// state, not in Secret Manager, not in a process's environment. The
	// credential is a short-lived access token the connector fetches from the
	// runtime's own identity, and revoking access is removing a role rather than
	// rotating a secret and redeploying everything that held it.
	Instance string

	// PrivateIP reaches the instance over the peered range rather than a public
	// address. Required when the instance has no public address, which is how
	// the deployment builds it.
	PrivateIP bool

	// Schema is the Postgres schema to use, empty for the default. Tests give
	// each case its own so they can run against one database without sharing
	// tables.
	Schema string

	// Now is the control plane's clock. See the package comment.
	Now func() time.Time

	// IDs generates identifiers for rows that need one and were not given one.
	IDs func() string
}

// Store is a Postgres implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
	ids  func() string

	// Held so it can be closed. The dialer keeps a refresh loop per instance,
	// and dropping the reference without closing leaks it for the life of the
	// process -- which matters in tests, where stores are opened per case.
	dialer io.Closer
}

// Open connects, applies the schema, and returns a ready store.
func Open(ctx context.Context, config Config) (*Store, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.IDs == nil {
		config.IDs = randomIDs
	}

	settings, err := pgxpool.ParseConfig(config.DSN)
	if err != nil {
		return nil, fmt.Errorf("parsing the database URL: %w", err)
	}

	var dialer *cloudsqlconn.Dialer
	if config.Instance != "" {
		options := []cloudsqlconn.Option{cloudsqlconn.WithIAMAuthN()}
		dialer, err = cloudsqlconn.NewDialer(ctx, options...)
		if err != nil {
			return nil, fmt.Errorf("building the Cloud SQL dialer: %w", err)
		}
		var reach []cloudsqlconn.DialOption
		if config.PrivateIP {
			reach = append(reach, cloudsqlconn.WithPrivateIP())
		}
		// Replacing the dial function rather than pointing at an address is what
		// makes this work without a sidecar: the connector resolves the
		// instance, verifies its certificate, and hands back a socket that is
		// already authenticated.
		settings.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.Dial(ctx, config.Instance, reach...)
		}
	}

	if config.Schema != "" {
		// Set per connection rather than in the DSN: the pool opens more
		// connections as load arrives, and one that came up without the search
		// path would read an empty database rather than fail.
		settings.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, "SET search_path TO "+pgx.Identifier{config.Schema}.Sanitize())
			return err
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, settings)
	if err != nil {
		if dialer != nil {
			_ = dialer.Close()
		}
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	subject := &Store{pool: pool, now: config.Now, ids: config.IDs}
	if dialer != nil {
		subject.dialer = dialer
	}

	if err := subject.migrate(ctx, config.Schema); err != nil {
		subject.Close()
		return nil, err
	}
	return subject, nil
}

// Pool is the connection pool, for stores that share this database.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool and the connector.
func (s *Store) Close() {
	s.pool.Close()
	if s.dialer != nil {
		_ = s.dialer.Close()
	}
}

// migrate applies the schema, one replica at a time.
//
// The advisory lock is not paranoia about the `IF NOT EXISTS` clauses: two
// backends running `CREATE TABLE IF NOT EXISTS` concurrently race in the system
// catalogue and one of them fails with a duplicate key on pg_type, which on a
// deployment that starts several replicas at once is a crash loop that clears up
// on its own and teaches an operator to ignore startup failures.
func (s *Store) migrate(ctx context.Context, target string) error {
	if target != "" {
		if _, err := s.pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{target}.Sanitize()); err != nil {
			return fmt.Errorf("creating schema %s: %w", target, err)
		}
	}

	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection to migrate: %w", err)
	}
	defer connection.Release()

	// A constant, so every replica of every version takes the same lock.
	const key = 0x6d6f716f6d // "moqom"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	}()

	if _, err := connection.Exec(ctx, schema); err != nil {
		return fmt.Errorf("applying the schema: %w", err)
	}
	return nil
}

func randomIDs() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Cannot happen on any supported platform, and a store that quietly
		// issued predictable IDs would be worse than one that stopped.
		panic("no randomness available for identifiers: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// Rooms.

func (s *Store) CreateRoom(ctx context.Context, room control.Room) (control.Room, error) {
	if room.CreatedAt.IsZero() {
		room.CreatedAt = s.now()
	}
	room.Policy = room.Policy.WithDefaults()

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO rooms (
			tenant, room, lifecycle, owner, acting_host_user, acting_host_device,
			starts_at, created_at, ended_at,
			age_minimum, age_verified, recording, residency_zone,
			max_participants, stage_slots, watchdog_limit, e2ee
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (tenant, room) DO NOTHING`,
		room.Tenant, room.Room, string(room.Lifecycle), room.Owner,
		room.ActingHost.Username, room.ActingHost.Device,
		moment(room.StartsAt), room.CreatedAt, moment(room.EndedAt),
		int64(room.Policy.AgeGate.MinimumAge), room.Policy.AgeGate.RequireVerification,
		string(room.Policy.Recording), room.Policy.ResidencyZone,
		int64(room.Policy.MaxParticipants), int64(room.Policy.StageSlots),
		int64(room.Policy.WatchdogLimit), string(room.Policy.E2EE),
	)
	if err != nil {
		return control.Room{}, fmt.Errorf("creating a room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", control.ErrAlreadyExists, room.Tenant, room.Room)
	}
	return room, nil
}

const roomColumns = `
	tenant, room, lifecycle, owner, acting_host_user, acting_host_device,
	starts_at, created_at, ended_at,
	age_minimum, age_verified, recording, residency_zone,
	max_participants, stage_slots, watchdog_limit, e2ee`

func scanRoom(row pgx.Row) (control.Room, error) {
	var room control.Room
	var lifecycle, recording, e2ee string
	var startsAt, endedAt *time.Time
	var age, maximum, slots, watchdogs int64
	err := row.Scan(
		&room.Tenant, &room.Room, &lifecycle, &room.Owner,
		&room.ActingHost.Username, &room.ActingHost.Device,
		&startsAt, &room.CreatedAt, &endedAt,
		&age, &room.Policy.AgeGate.RequireVerification, &recording, &room.Policy.ResidencyZone,
		&maximum, &slots, &watchdogs, &e2ee,
	)
	if err != nil {
		return control.Room{}, err
	}
	room.Lifecycle = control.Lifecycle(lifecycle)
	room.Policy.Recording = control.RecordingPolicy(recording)
	room.Policy.E2EE = control.E2EEPolicy(e2ee)
	room.Policy.AgeGate.MinimumAge = uint32(age)
	room.Policy.MaxParticipants = uint32(maximum)
	room.Policy.StageSlots = uint32(slots)
	room.Policy.WatchdogLimit = uint32(watchdogs)
	room.StartsAt = at(startsAt)
	room.EndedAt = at(endedAt)
	return room, nil
}

func (s *Store) GetRoom(ctx context.Context, tenant, room string) (control.Room, error) {
	found, err := scanRoom(s.pool.QueryRow(ctx,
		`SELECT `+roomColumns+` FROM rooms WHERE tenant = $1 AND room = $2`, tenant, room))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	if err != nil {
		return control.Room{}, fmt.Errorf("reading a room: %w", err)
	}
	return found, nil
}

func (s *Store) SaveRoom(ctx context.Context, room control.Room) (control.Room, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE rooms SET
			lifecycle = $3, owner = $4, acting_host_user = $5, acting_host_device = $6,
			starts_at = $7, ended_at = $8,
			age_minimum = $9, age_verified = $10, recording = $11, residency_zone = $12,
			max_participants = $13, stage_slots = $14, watchdog_limit = $15, e2ee = $16
		WHERE tenant = $1 AND room = $2`,
		room.Tenant, room.Room, string(room.Lifecycle), room.Owner,
		room.ActingHost.Username, room.ActingHost.Device,
		moment(room.StartsAt), moment(room.EndedAt),
		int64(room.Policy.AgeGate.MinimumAge), room.Policy.AgeGate.RequireVerification,
		string(room.Policy.Recording), room.Policy.ResidencyZone,
		int64(room.Policy.MaxParticipants), int64(room.Policy.StageSlots),
		int64(room.Policy.WatchdogLimit), string(room.Policy.WithDefaults().E2EE),
	)
	if err != nil {
		return control.Room{}, fmt.Errorf("saving a room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, room.Tenant, room.Room)
	}
	return room, nil
}

func (s *Store) ListRooms(ctx context.Context, query store.RoomQuery) (store.RoomPage, error) {
	after, err := decodeToken(query.PageToken)
	if err != nil {
		return store.RoomPage{}, err
	}
	lifecycles := make([]string, 0, len(query.Lifecycle))
	for _, lifecycle := range query.Lifecycle {
		lifecycles = append(lifecycles, string(lifecycle))
	}

	limit := int(query.Clamp())
	// One more than asked for, which is how the page knows whether there is
	// another without counting the whole table.
	rows, err := s.pool.Query(ctx, `
		SELECT `+roomColumns+` FROM rooms
		WHERE tenant = $1
		  AND (cardinality($2::text[]) = 0 OR lifecycle = ANY($2))
		  AND ($3 = '' OR room COLLATE "C" > $3)
		ORDER BY room COLLATE "C"
		LIMIT $4`,
		query.Tenant, lifecycles, after, limit+1)
	if err != nil {
		return store.RoomPage{}, fmt.Errorf("listing rooms: %w", err)
	}
	defer rows.Close()

	var page store.RoomPage
	for rows.Next() {
		room, err := scanRoom(rows)
		if err != nil {
			return store.RoomPage{}, fmt.Errorf("reading a room: %w", err)
		}
		page.Rooms = append(page.Rooms, room)
	}
	if err := rows.Err(); err != nil {
		return store.RoomPage{}, fmt.Errorf("listing rooms: %w", err)
	}
	if len(page.Rooms) > limit {
		page.Rooms = page.Rooms[:limit]
		page.NextPageToken = encodeToken(page.Rooms[limit-1].Room)
	}
	return page, nil
}

// Participants.

func (s *Store) SaveParticipant(ctx context.Context, tenant, room string, participant control.Participant) error {
	if participant.JoinedAt.IsZero() {
		participant.JoinedAt = s.now()
	}
	participant.LastSeenAt = s.now()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO participants (
			tenant, room, username, device,
			publish, moderation, is_owner,
			presence, self_muted, muted_everyone, muted_audience,
			spotlighted, backstage, arrived_via, locale,
			joined_at, last_seen_at, relay_id, session_id, remote_addr, published_now
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (tenant, room, username, device) DO UPDATE SET
			publish = EXCLUDED.publish, moderation = EXCLUDED.moderation,
			is_owner = EXCLUDED.is_owner, presence = EXCLUDED.presence,
			self_muted = EXCLUDED.self_muted, muted_everyone = EXCLUDED.muted_everyone,
			muted_audience = EXCLUDED.muted_audience, spotlighted = EXCLUDED.spotlighted,
			backstage = EXCLUDED.backstage, arrived_via = EXCLUDED.arrived_via,
			locale = EXCLUDED.locale,
			joined_at = EXCLUDED.joined_at, last_seen_at = EXCLUDED.last_seen_at,
			relay_id = EXCLUDED.relay_id, session_id = EXCLUDED.session_id,
			remote_addr = EXCLUDED.remote_addr, published_now = EXCLUDED.published_now`,
		tenant, room, participant.Identity.Username, participant.Identity.Device,
		int64(participant.Grants.Publish), int64(participant.Grants.Moderation),
		participant.Grants.IsOwner, string(participant.Presence),
		int64(participant.Mute.SelfMuted), int64(participant.Mute.ForcedForEveryone),
		int64(participant.Mute.ForcedForAudience), participant.Spotlighted,
		participant.Backstage, participant.ArrivedVia, participant.Locale,
		participant.JoinedAt, participant.LastSeenAt, participant.RelayID,
		participant.SessionID, participant.RemoteAddr, listed(participant.PublishedNow),
	)
	// A participant in a room that does not exist is the foreign key's job to
	// refuse, and it is not-found rather than invalid: the room is the thing
	// that is missing.
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	if err != nil {
		return fmt.Errorf("saving a participant: %w", err)
	}
	return nil
}

const participantColumns = `
	username, device, publish, moderation, is_owner,
	presence, self_muted, muted_everyone, muted_audience,
	spotlighted, backstage, arrived_via, locale,
	joined_at, last_seen_at, relay_id, session_id, remote_addr, published_now`

func scanParticipant(row pgx.Row) (control.Participant, error) {
	var participant control.Participant
	var presence string
	var publish, moderation, self, everyone, audience int64
	err := row.Scan(
		&participant.Identity.Username, &participant.Identity.Device,
		&publish, &moderation, &participant.Grants.IsOwner,
		&presence, &self, &everyone, &audience,
		&participant.Spotlighted, &participant.Backstage, &participant.ArrivedVia,
		&participant.Locale,
		&participant.JoinedAt, &participant.LastSeenAt, &participant.RelayID,
		&participant.SessionID, &participant.RemoteAddr, &participant.PublishedNow,
	)
	if err != nil {
		return control.Participant{}, err
	}
	participant.Grants.Publish = control.PublishScope(publish)
	participant.Grants.Moderation = control.Capability(moderation)
	participant.Presence = control.Presence(presence)
	participant.Mute.SelfMuted = control.MediaKind(self)
	participant.Mute.ForcedForEveryone = control.MediaKind(everyone)
	participant.Mute.ForcedForAudience = control.MediaKind(audience)
	return participant, nil
}

func (s *Store) GetParticipant(ctx context.Context, tenant, room string, id control.Identity) (control.Participant, error) {
	found, err := scanParticipant(s.pool.QueryRow(ctx,
		`SELECT `+participantColumns+` FROM participants
		 WHERE tenant = $1 AND room = $2 AND username = $3 AND device = $4`,
		tenant, room, id.Username, id.Device))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Participant{}, fmt.Errorf("%w: participant %s in %s/%s", store.ErrNotFound, id, tenant, room)
	}
	if err != nil {
		return control.Participant{}, fmt.Errorf("reading a participant: %w", err)
	}
	return found, nil
}

func (s *Store) ListParticipants(ctx context.Context, query store.ParticipantQuery) (store.ParticipantPage, error) {
	after, err := decodeToken(query.PageToken)
	if err != nil {
		return store.ParticipantPage{}, err
	}

	limit := int(query.Clamp())
	rows, err := s.pool.Query(ctx, `
		SELECT `+participantColumns+` FROM participants
		WHERE tenant = $1 AND room = $2
		  AND ($3 OR NOT backstage)
		  AND ($4 OR presence <> 'left')
		  AND ($5 = '' OR (username || '/' || device) COLLATE "C" > $5)
		ORDER BY (username || '/' || device) COLLATE "C"
		LIMIT $6`,
		query.Tenant, query.Room, query.IncludeBackstage, query.IncludeDeparted, after, limit+1)
	if err != nil {
		return store.ParticipantPage{}, fmt.Errorf("listing participants: %w", err)
	}
	defer rows.Close()

	var page store.ParticipantPage
	for rows.Next() {
		participant, err := scanParticipant(rows)
		if err != nil {
			return store.ParticipantPage{}, fmt.Errorf("reading a participant: %w", err)
		}
		page.Participants = append(page.Participants, participant)
	}
	if err := rows.Err(); err != nil {
		return store.ParticipantPage{}, fmt.Errorf("listing participants: %w", err)
	}
	if len(page.Participants) > limit {
		page.Participants = page.Participants[:limit]
		page.NextPageToken = encodeToken(page.Participants[limit-1].Identity.String())
	}
	return page, nil
}

func (s *Store) DeleteParticipant(ctx context.Context, tenant, room string, id control.Identity) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM participants WHERE tenant = $1 AND room = $2 AND username = $3 AND device = $4`,
		tenant, room, id.Username, id.Device)
	if err != nil {
		return fmt.Errorf("deleting a participant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: participant %s", store.ErrNotFound, id)
	}
	return nil
}

// Bans.

// subjectOf blanks the fields that do not belong to a ban's kind.
//
// The unique index is over all three key columns, so a username ban that
// arrived carrying a device would be a different row from the same ban issued
// without one -- two bans on one person, and an unban that lifts one of them.
// Normalising here makes the index mean what the domain means.
func subjectOf(ban control.Ban) control.Ban {
	switch ban.Kind {
	case control.BanByUsername:
		ban.Device, ban.IPPrefix = "", ""
	case control.BanByDevice:
		ban.IPPrefix = ""
	case control.BanByAddress:
		ban.Username, ban.Device = "", ""
	}
	return ban
}

func (s *Store) PutBan(ctx context.Context, ban control.Ban) (control.Ban, error) {
	ban = subjectOf(ban)
	if ban.ID == "" {
		ban.ID = s.ids()
	}
	if ban.CreatedAt.IsZero() {
		ban.CreatedAt = s.now()
	}

	// The ID and the creation time of an existing ban survive it being
	// reissued: the row is the same ban with new terms, and an audit trail that
	// renumbered it on every edit could not be followed.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO bans (id, tenant, room, kind, username, device, ip_prefix,
		                  code, reason, expires_at, created_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant, room, kind, username, device, ip_prefix) DO UPDATE SET
			code = EXCLUDED.code, reason = EXCLUDED.reason,
			expires_at = EXCLUDED.expires_at, created_by = EXCLUDED.created_by
		RETURNING id, created_at`,
		ban.ID, ban.Tenant, ban.Room, string(ban.Kind), ban.Username, ban.Device,
		ban.IPPrefix, ban.Code, ban.Reason, moment(ban.ExpiresAt), ban.CreatedAt, ban.CreatedBy,
	).Scan(&ban.ID, &ban.CreatedAt)
	if err != nil {
		return control.Ban{}, fmt.Errorf("writing a ban: %w", err)
	}
	return ban, nil
}

func (s *Store) DeleteBan(ctx context.Context, tenant, room string, kind control.BanKind, subject control.Ban) error {
	subject.Kind = kind
	subject = subjectOf(subject)

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM bans
		WHERE tenant = $1 AND room = $2 AND kind = $3
		  AND username = $4 AND device = $5 AND ip_prefix = $6`,
		tenant, room, string(kind), subject.Username, subject.Device, subject.IPPrefix)
	if err != nil {
		return fmt.Errorf("deleting a ban: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such ban", store.ErrNotFound)
	}
	return nil
}

func (s *Store) ListBans(ctx context.Context, tenant, room string) ([]control.Ban, error) {
	// The room's own bans and the tenant-wide ones that also apply. The union is
	// computed here so a relay is handed the answer rather than the rule, which
	// would otherwise be a second implementation of it in another language on
	// the enforcement path.
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant, room, kind, username, device, ip_prefix,
		       code, reason, expires_at, created_at, created_by
		FROM bans
		WHERE tenant = $1 AND (room = $2 OR room = '')
		  AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY id COLLATE "C"`,
		tenant, room, s.now())
	if err != nil {
		return nil, fmt.Errorf("listing bans: %w", err)
	}
	defer rows.Close()

	var bans []control.Ban
	for rows.Next() {
		var ban control.Ban
		var kind string
		var expires *time.Time
		if err := rows.Scan(&ban.ID, &ban.Tenant, &ban.Room, &kind, &ban.Username,
			&ban.Device, &ban.IPPrefix, &ban.Code, &ban.Reason, &expires,
			&ban.CreatedAt, &ban.CreatedBy); err != nil {
			return nil, fmt.Errorf("reading a ban: %w", err)
		}
		ban.Kind = control.BanKind(kind)
		ban.ExpiresAt = at(expires)
		bans = append(bans, ban)
	}
	return bans, rows.Err()
}

// Mutes.

// One statement per kind rather than one per request, because the key includes
// the kind: a mute of the camera has to leave a mute of the microphone where it
// is, with the expiry that mute was given.
func (s *Store) PutMute(ctx context.Context, mute store.Mute) (store.Mute, error) {
	if mute.CreatedAt.IsZero() {
		mute.CreatedAt = s.now()
	}

	batch := &pgx.Batch{}
	for _, kind := range mute.Media.Each() {
		batch.Queue(`
			INSERT INTO mutes (tenant, room, username, device, audience_only, media, expires_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (tenant, room, username, device, audience_only, media) DO UPDATE SET
				expires_at = EXCLUDED.expires_at, created_at = EXCLUDED.created_at`,
			mute.Tenant, mute.Room, mute.Identity.Username, mute.Identity.Device,
			mute.AudienceOnly, int64(kind), moment(mute.ExpiresAt), mute.CreatedAt)
	}
	if batch.Len() == 0 {
		return mute, nil
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return store.Mute{}, fmt.Errorf("writing a mute: %w", err)
	}
	return mute, nil
}

// Only the kinds named. ErrNotFound when the scope held none of them, which is
// the same "already lifted" answer an expired mute gives.
func (s *Store) DeleteMute(
	ctx context.Context, tenant, room string, id control.Identity,
	media control.MediaKind, audienceOnly bool,
) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM mutes
		WHERE tenant = $1 AND room = $2 AND username = $3 AND device = $4
		  AND audience_only = $5 AND media & $6 <> 0`,
		tenant, room, id.Username, id.Device, audienceOnly, int64(media))
	if err != nil {
		return fmt.Errorf("deleting a mute: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such mute", store.ErrNotFound)
	}
	return nil
}

func (s *Store) ListMutes(ctx context.Context, tenant, room string) ([]store.Mute, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT username, device, audience_only, media, expires_at, created_at
		FROM mutes
		WHERE tenant = $1 AND room = $2
		  AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY (username || '/' || device) COLLATE "C", audience_only, media`,
		tenant, room, s.now())
	if err != nil {
		return nil, fmt.Errorf("listing mutes: %w", err)
	}
	defer rows.Close()

	var mutes []store.Mute
	for rows.Next() {
		mute := store.Mute{Tenant: tenant, Room: room}
		var media int64
		var expires *time.Time
		if err := rows.Scan(&mute.Identity.Username, &mute.Identity.Device,
			&mute.AudienceOnly, &media, &expires, &mute.CreatedAt); err != nil {
			return nil, fmt.Errorf("reading a mute: %w", err)
		}
		mute.Media = control.MediaKind(media)
		mute.ExpiresAt = at(expires)
		mutes = append(mutes, mute)
	}
	return mutes, rows.Err()
}

// Grant overrides.

func (s *Store) PutGrantOverride(ctx context.Context, tenant, room string, id control.Identity, grants control.Grants) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO grant_overrides (tenant, room, username, device, publish, moderation, is_owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant, room, username, device) DO UPDATE SET
			publish = EXCLUDED.publish, moderation = EXCLUDED.moderation,
			is_owner = EXCLUDED.is_owner`,
		tenant, room, id.Username, id.Device,
		int64(grants.Publish), int64(grants.Moderation), grants.IsOwner)
	if err != nil {
		return fmt.Errorf("writing a grant override: %w", err)
	}
	return nil
}

func (s *Store) ListGrantOverrides(ctx context.Context, tenant, room string) (map[control.Identity]control.Grants, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT username, device, publish, moderation, is_owner
		FROM grant_overrides WHERE tenant = $1 AND room = $2`, tenant, room)
	if err != nil {
		return nil, fmt.Errorf("listing grant overrides: %w", err)
	}
	defer rows.Close()

	overrides := map[control.Identity]control.Grants{}
	for rows.Next() {
		var id control.Identity
		var grants control.Grants
		var publish, moderation int64
		if err := rows.Scan(&id.Username, &id.Device, &publish, &moderation, &grants.IsOwner); err != nil {
			return nil, fmt.Errorf("reading a grant override: %w", err)
		}
		grants.Publish = control.PublishScope(publish)
		grants.Moderation = control.Capability(moderation)
		overrides[id] = grants
	}
	return overrides, rows.Err()
}

// Watchdogs.

func (s *Store) AttachWatchdog(ctx context.Context, tenant, room string, watchdog control.Watchdog) (control.Watchdog, error) {
	if watchdog.ID == "" {
		watchdog.ID = s.ids()
	}
	if watchdog.AttachedAt.IsZero() {
		watchdog.AttachedAt = s.now()
	}

	// One transaction, with the room row locked, because the limit is checked
	// against a count and then acted on. Two recorders attaching at once would
	// otherwise both see room for one more.
	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return control.Watchdog{}, fmt.Errorf("attaching a watchdog: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	var limit int64
	err = transaction.QueryRow(ctx,
		`SELECT watchdog_limit FROM rooms WHERE tenant = $1 AND room = $2 FOR UPDATE`,
		tenant, room).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Watchdog{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	if err != nil {
		return control.Watchdog{}, fmt.Errorf("reading the watchdog limit: %w", err)
	}
	ceiling := int64(control.Policy{WatchdogLimit: uint32(limit)}.WithDefaults().WatchdogLimit)

	var attached int64
	if err := transaction.QueryRow(ctx,
		`SELECT count(*) FROM watchdogs WHERE tenant = $1 AND room = $2`,
		tenant, room).Scan(&attached); err != nil {
		return control.Watchdog{}, fmt.Errorf("counting watchdogs: %w", err)
	}
	if attached >= ceiling {
		return control.Watchdog{}, fmt.Errorf("%w: room already has %d watchdogs, limit %d",
			control.ErrInvalid, attached, ceiling)
	}

	if _, err := transaction.Exec(ctx, `
		INSERT INTO watchdogs (id, tenant, room, kind, custom_kind, operated_by,
		                       operator_name, descriptor, scope, health, attached_at,
		                       billable, pinned_layer)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		watchdog.ID, tenant, room, string(watchdog.Kind), watchdog.CustomKind,
		string(watchdog.OperatedBy), watchdog.OperatorName, watchdog.Descriptor,
		listed([]string(watchdog.Scope)), string(watchdog.Health), watchdog.AttachedAt,
		watchdog.Billable, watchdog.PinnedLayer); err != nil {
		return control.Watchdog{}, fmt.Errorf("attaching a watchdog: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return control.Watchdog{}, fmt.Errorf("attaching a watchdog: %w", err)
	}
	return watchdog, nil
}

func (s *Store) DetachWatchdog(ctx context.Context, tenant, room, id string) (control.Watchdog, error) {
	watchdog, err := scanWatchdog(s.pool.QueryRow(ctx, `
		DELETE FROM watchdogs WHERE tenant = $1 AND room = $2 AND id = $3
		RETURNING id, kind, custom_kind, operated_by, operator_name, descriptor,
		          scope, health, attached_at, billable, pinned_layer`,
		tenant, room, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Watchdog{}, fmt.Errorf("%w: watchdog %s", store.ErrNotFound, id)
	}
	if err != nil {
		return control.Watchdog{}, fmt.Errorf("detaching a watchdog: %w", err)
	}
	return watchdog, nil
}

func scanWatchdog(row pgx.Row) (control.Watchdog, error) {
	var watchdog control.Watchdog
	var kind, operator, health string
	var scope []string
	err := row.Scan(&watchdog.ID, &kind, &watchdog.CustomKind, &operator,
		&watchdog.OperatorName, &watchdog.Descriptor, &scope, &health,
		&watchdog.AttachedAt, &watchdog.Billable, &watchdog.PinnedLayer)
	if err != nil {
		return control.Watchdog{}, err
	}
	watchdog.Kind = control.WatchdogKind(kind)
	watchdog.OperatedBy = control.WatchdogOperator(operator)
	watchdog.Health = control.WatchdogHealth(health)
	if len(scope) > 0 {
		watchdog.Scope = control.Namespace(scope)
	}
	return watchdog, nil
}

func (s *Store) ListWatchdogs(ctx context.Context, tenant, room string) ([]control.Watchdog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, custom_kind, operated_by, operator_name, descriptor,
		       scope, health, attached_at, billable, pinned_layer
		FROM watchdogs WHERE tenant = $1 AND room = $2
		ORDER BY id COLLATE "C"`, tenant, room)
	if err != nil {
		return nil, fmt.Errorf("listing watchdogs: %w", err)
	}
	defer rows.Close()

	watchdogs := []control.Watchdog{}
	for rows.Next() {
		watchdog, err := scanWatchdog(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a watchdog: %w", err)
		}
		watchdogs = append(watchdogs, watchdog)
	}
	return watchdogs, rows.Err()
}

// The audit trail.

func (s *Store) AppendModerationEntry(ctx context.Context, entry control.ModerationEntry) (control.ModerationEntry, error) {
	if entry.ID == "" {
		entry.ID = s.ids()
	}
	if entry.At.IsZero() {
		entry.At = s.now()
	}

	var publish, moderation *int64
	var owner *bool
	if entry.Grants != nil {
		publish = pointer(int64(entry.Grants.Publish))
		moderation = pointer(int64(entry.Grants.Moderation))
		owner = pointer(entry.Grants.IsOwner)
	}

	// The retry check and the write are one statement. Reading first and
	// inserting after would let two deliveries of one queued action both find
	// nothing and both write, which for "ban for 24 hours" means 48 -- and no
	// amount of care in a service can fix that, because the two deliveries can
	// be on different replicas.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO moderation_log (
			id, tenant, room, at, verb, service_account, on_behalf_of,
			target_username, target_device, target_all_devices,
			ban_kind, ban_subject, ban_scope_tenant, code, reason, expires_at,
			media, audience_only, grants_publish, grants_moderation, grants_is_owner,
			enforced, decided_by_model, idempotency_key, policy_change
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (tenant, idempotency_key) WHERE idempotency_key <> '' DO NOTHING`,
		entry.ID, entry.Tenant, entry.Room, entry.At, string(entry.Verb),
		entry.ServiceAccount, entry.OnBehalfOf,
		entry.Target.Username, entry.Target.Device, entry.TargetAllDevices,
		string(entry.BanKind), entry.BanSubject, entry.BanScopeTenant,
		entry.Code, entry.Reason, moment(entry.ExpiresAt),
		int64(entry.Media), entry.AudienceOnly, publish, moderation, owner,
		entry.Enforced, entry.DecidedByModel, entry.IdempotencyKey, entry.PolicyChange,
	)
	if err != nil {
		return control.ModerationEntry{}, fmt.Errorf("appending a moderation entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Somebody else's copy of this action won. Theirs is the entry, and
		// returning it is what makes a retry look like the first attempt.
		return s.FindModerationEntry(ctx, entry.Tenant, entry.IdempotencyKey)
	}
	return entry, nil
}

const entryColumns = `
	id, tenant, room, at, verb, service_account, on_behalf_of,
	target_username, target_device, target_all_devices,
	ban_kind, ban_subject, ban_scope_tenant, code, reason, expires_at,
	media, audience_only, grants_publish, grants_moderation, grants_is_owner,
	enforced, decided_by_model, idempotency_key, policy_change`

func scanEntry(row pgx.Row) (control.ModerationEntry, error) {
	var entry control.ModerationEntry
	var verb, banKind string
	var expires *time.Time
	var media int64
	var publish, moderation *int64
	var owner *bool
	err := row.Scan(&entry.ID, &entry.Tenant, &entry.Room, &entry.At, &verb,
		&entry.ServiceAccount, &entry.OnBehalfOf,
		&entry.Target.Username, &entry.Target.Device, &entry.TargetAllDevices,
		&banKind, &entry.BanSubject, &entry.BanScopeTenant, &entry.Code,
		&entry.Reason, &expires, &media, &entry.AudienceOnly,
		&publish, &moderation, &owner, &entry.Enforced,
		&entry.DecidedByModel, &entry.IdempotencyKey, &entry.PolicyChange)
	if err != nil {
		return control.ModerationEntry{}, err
	}
	entry.Verb = control.ModerationVerb(verb)
	entry.BanKind = control.BanKind(banKind)
	entry.ExpiresAt = at(expires)
	entry.Media = control.MediaKind(media)
	if publish != nil && moderation != nil && owner != nil {
		entry.Grants = &control.Grants{
			Publish:    control.PublishScope(*publish),
			Moderation: control.Capability(*moderation),
			IsOwner:    *owner,
		}
	}
	return entry, nil
}

func (s *Store) FindModerationEntry(ctx context.Context, tenant, key string) (control.ModerationEntry, error) {
	if key == "" {
		return control.ModerationEntry{}, fmt.Errorf("%w: no entry for an empty key", store.ErrNotFound)
	}
	entry, err := scanEntry(s.pool.QueryRow(ctx,
		`SELECT `+entryColumns+` FROM moderation_log WHERE tenant = $1 AND idempotency_key = $2`,
		tenant, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.ModerationEntry{}, fmt.Errorf("%w: no entry for key %q", store.ErrNotFound, key)
	}
	if err != nil {
		return control.ModerationEntry{}, fmt.Errorf("finding a moderation entry: %w", err)
	}
	return entry, nil
}

func (s *Store) MarkEnforced(ctx context.Context, tenant, id string, enforced bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE moderation_log SET enforced = $3 WHERE tenant = $1 AND id = $2`,
		tenant, id, enforced)
	if err != nil {
		return fmt.Errorf("marking a moderation entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: moderation entry %s", store.ErrNotFound, id)
	}
	return nil
}

func (s *Store) ListModerationLog(ctx context.Context, query store.LogQuery) (store.LogPage, error) {
	cursor, err := decodeCursor(query.PageToken)
	if err != nil {
		return store.LogPage{}, err
	}
	verbs := make([]string, 0, len(query.Verbs))
	for _, verb := range query.Verbs {
		verbs = append(verbs, string(verb))
	}

	limit := int(query.Clamp())
	rows, err := s.pool.Query(ctx, `
		SELECT `+entryColumns+` FROM moderation_log
		WHERE tenant = $1
		  AND ($2 = '' OR room = $2)
		  AND ($3::timestamptz IS NULL OR at >= $3)
		  AND ($4::timestamptz IS NULL OR at < $4)
		  AND ($5 = '' OR (target_username = $5
		       AND ($6 = '' OR target_device = '' OR target_device = $6)))
		  AND (cardinality($7::text[]) = 0 OR verb = ANY($7))
		  AND ($8::timestamptz IS NULL
		       OR at < $8 OR (at = $8 AND id COLLATE "C" < $9))
		ORDER BY at DESC, id COLLATE "C" DESC
		LIMIT $10`,
		query.Tenant, query.Room, moment(query.Since), moment(query.Until),
		query.Target.Username, query.Target.Device, verbs,
		moment(cursor.at), cursor.id, limit+1)
	if err != nil {
		return store.LogPage{}, fmt.Errorf("listing the moderation log: %w", err)
	}
	defer rows.Close()

	var page store.LogPage
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return store.LogPage{}, fmt.Errorf("reading a moderation entry: %w", err)
		}
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return store.LogPage{}, fmt.Errorf("listing the moderation log: %w", err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		page.NextPageToken = encodeCursor(last.At, last.ID)
	}
	return page, nil
}

// Events.

// howHardToTryForASequence bounds the retry when two writers race for the next
// number in one room. Each attempt loses only to a writer that succeeded, so
// reaching the end of this means a room being written to faster than it can be
// numbered rather than a bug.
const howHardToTryForASequence = 8

func (s *Store) AppendEvent(ctx context.Context, event store.Event) (store.Event, error) {
	if event.At.IsZero() {
		event.At = s.now()
	}
	if event.Body == nil {
		event.Body = []byte{}
	}

	// The number is chosen and taken in one statement, and the primary key is
	// what settles a tie. Reading the maximum and inserting after would let two
	// writers pick the same number, and a consumer watching for gaps would see
	// none while an event went missing.
	for attempt := range howHardToTryForASequence {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO events (tenant, room, sequence, at, kind, body)
			SELECT $1, $2, COALESCE(MAX(sequence), 0) + 1, $3, $4, $5
			FROM events WHERE tenant = $1 AND room = $2
			RETURNING sequence`,
			event.Tenant, event.Room, event.At, string(event.Kind), event.Body,
		).Scan(&event.Sequence)
		if err == nil {
			return event, nil
		}
		if !isUniqueViolation(err) {
			return store.Event{}, fmt.Errorf("appending an event: %w", err)
		}
		_ = attempt
	}
	return store.Event{}, fmt.Errorf("%w: could not number an event in %s/%s",
		store.ErrConflict, event.Tenant, event.Room)
}

func (s *Store) ListEvents(ctx context.Context, query store.EventQuery) ([]store.Event, error) {
	kinds := make([]string, 0, len(query.Kinds))
	for _, kind := range query.Kinds {
		kinds = append(kinds, string(kind))
	}
	// Zero means everything, and there is still a ceiling: an unbounded read is
	// a denial of service against the database rather than a feature.
	limit := int64(query.Limit)
	if limit <= 0 {
		limit = store.MaxPageSize
	}

	rows, err := s.pool.Query(ctx, `
		SELECT tenant, room, sequence, at, kind, body FROM events
		WHERE tenant = $1
		  AND ($2 = '' OR room = $2)
		  AND sequence > $3
		  AND (cardinality($4::text[]) = 0 OR kind = ANY($4))
		ORDER BY room COLLATE "C", sequence
		LIMIT $5`,
		query.Tenant, query.Room, int64(query.AfterSequence), kinds, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	var events []store.Event
	for rows.Next() {
		var event store.Event
		var sequence int64
		var kind string
		if err := rows.Scan(&event.Tenant, &event.Room, &sequence, &event.At, &kind, &event.Body); err != nil {
			return nil, fmt.Errorf("reading an event: %w", err)
		}
		event.Sequence = uint64(sequence)
		event.Kind = store.EventKind(kind)
		events = append(events, event)
	}
	return events, rows.Err()
}

// Odds and ends.

// moment turns Go's zero time into SQL NULL.
//
// Both mean "not set", and keeping them as one value is what lets a query say
// `expires_at IS NULL` for a permanent ban rather than comparing against a date
// in the year one.
func moment(when time.Time) *time.Time {
	if when.IsZero() {
		return nil
	}
	return &when
}

func at(when *time.Time) time.Time {
	if when == nil {
		return time.Time{}
	}
	return *when
}

// listed makes a nil slice storable in a NOT NULL array column.
//
// Nothing and the empty list mean the same thing everywhere they are used here
// -- nobody publishing, no scope narrowing -- and a nullable array would add a
// third state that every reader would have to handle and none would test.
func listed[T ~string](values []T) []string {
	stored := make([]string, len(values))
	for i, value := range values {
		stored[i] = string(value)
	}
	return stored
}

func pointer[T any](value T) *T { return &value }

func isUniqueViolation(err error) bool { return hasSQLState(err, "23505") }

func isForeignKeyViolation(err error) bool { return hasSQLState(err, "23503") }

func hasSQLState(err error, code string) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == code
}

// Page tokens are opaque and encoded, so nobody builds one by hand and then
// depends on its shape when the ordering key changes.
func encodeToken(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: page token is not one we issued", control.ErrInvalid)
	}
	return string(decoded), nil
}

// cursor is where a moderation log page stopped.
//
// Two fields because the log is ordered by time and time is not unique: several
// actions can land in the same microsecond, and a cursor holding only the
// timestamp would either repeat them or skip them.
type cursor struct {
	at time.Time
	id string
}

func encodeCursor(when time.Time, id string) string {
	// Microseconds, which is what Postgres stores. Encoding nanoseconds would
	// produce a cursor that does not match the value it came from.
	return encodeToken(strconv.FormatInt(when.UnixMicro(), 10) + " " + id)
}

func decodeCursor(token string) (cursor, error) {
	decoded, err := decodeToken(token)
	if err != nil || decoded == "" {
		return cursor{}, err
	}
	stamp, id, found := strings.Cut(decoded, " ")
	if !found {
		return cursor{}, fmt.Errorf("%w: page token is malformed", control.ErrInvalid)
	}
	micros, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: page token is malformed", control.ErrInvalid)
	}
	return cursor{at: time.UnixMicro(micros).UTC(), id: id}, nil
}
