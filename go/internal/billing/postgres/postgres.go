// Package postgres keeps the billing ledger in Postgres, alongside the control
// plane's tables in the same database.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/billing"
)

//go:embed schema.sql
var schema string

// Store is billing.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ billing.Store = (*Store)(nil)

// New applies the billing schema and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	// Not the control plane's lock: the two migrations are independent and
	// sharing one would serialise them for nothing.
	const lock = 0x6d71626c6c // "mqbll"
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return nil, fmt.Errorf("taking the billing migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	if _, err := connection.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying the billing schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func nullable(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func value(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}
	return *at
}

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.ErrNotFound
	}
	return err
}

const tenantColumns = `id, name, email, status, stripe_customer, payment_method,
	auto_top_up_enabled, auto_top_up_below, auto_top_up_amount,
	top_up_pending_since, last_top_up_error, created_at, updated_at,
	spend_limit, spend_alerts, spend_alerts_off`

func scanTenant(row pgx.Row) (billing.Tenant, error) {
	var t billing.Tenant
	var below, amount, limit int64
	var alerts []int64
	var pending *time.Time
	err := row.Scan(&t.ID, &t.Name, &t.Email, &t.Status, &t.StripeCustomer, &t.PaymentMethod,
		&t.AutoTopUp.Enabled, &below, &amount, &pending, &t.LastTopUpError, &t.CreatedAt, &t.UpdatedAt,
		&limit, &alerts, &t.Spending.AlertsOff)
	if err != nil {
		return billing.Tenant{}, notFound(err)
	}
	t.AutoTopUp.Below, t.AutoTopUp.Amount = billing.Micros(below), billing.Micros(amount)
	t.TopUpPendingSince = value(pending)
	t.Spending.Limit = billing.Micros(limit)
	t.Spending.Alerts = fromAmounts(alerts)
	return t, nil
}

func (s *Store) CreateTenant(ctx context.Context, t billing.Tenant) (billing.Tenant, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO billing_tenants (`+tenantColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		t.ID, t.Name, t.Email, t.Status, t.StripeCustomer, t.PaymentMethod,
		t.AutoTopUp.Enabled, int64(t.AutoTopUp.Below), int64(t.AutoTopUp.Amount),
		nullable(t.TopUpPendingSince), t.LastTopUpError, t.CreatedAt.UTC(), t.UpdatedAt.UTC(),
		int64(t.Spending.Limit), toAmounts(t.Spending.Alerts), t.Spending.AlertsOff)
	if uniqueViolation(err) {
		return billing.Tenant{}, billing.ErrAlreadyExists
	}
	if err != nil {
		return billing.Tenant{}, err
	}
	return s.GetTenant(ctx, t.ID)
}

func (s *Store) GetTenant(ctx context.Context, id string) (billing.Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM billing_tenants WHERE id = $1`, id))
}

func (s *Store) SaveTenant(ctx context.Context, t billing.Tenant) (billing.Tenant, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE billing_tenants SET
		name=$2, email=$3, status=$4, stripe_customer=$5, payment_method=$6,
		auto_top_up_enabled=$7, auto_top_up_below=$8, auto_top_up_amount=$9,
		top_up_pending_since=$10, last_top_up_error=$11, updated_at=$12
		WHERE id = $1`,
		t.ID, t.Name, t.Email, t.Status, t.StripeCustomer, t.PaymentMethod,
		t.AutoTopUp.Enabled, int64(t.AutoTopUp.Below), int64(t.AutoTopUp.Amount),
		nullable(t.TopUpPendingSince), t.LastTopUpError, t.UpdatedAt.UTC())
	if err != nil {
		return billing.Tenant{}, err
	}
	if tag.RowsAffected() == 0 {
		return billing.Tenant{}, billing.ErrNotFound
	}
	return s.GetTenant(ctx, t.ID)
}

func (s *Store) FindTenantByStripeCustomer(ctx context.Context, customer string) (billing.Tenant, error) {
	if customer == "" {
		return billing.Tenant{}, billing.ErrNotFound
	}
	return scanTenant(s.pool.QueryRow(ctx,
		`SELECT `+tenantColumns+` FROM billing_tenants WHERE stripe_customer = $1`, customer))
}

func (s *Store) CountTenants(ctx context.Context) (total, active int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = $1) FROM billing_tenants`,
		billing.StatusActive).Scan(&total, &active)
	return total, active, err
}

func (s *Store) ClaimTopUp(ctx context.Context, tenant string, now time.Time, staleAfter time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE billing_tenants SET top_up_pending_since = $2
		WHERE id = $1 AND (top_up_pending_since IS NULL OR top_up_pending_since <= $3)`,
		tenant, now.UTC(), now.Add(-staleAfter).UTC())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	if _, err := s.GetTenant(ctx, tenant); err != nil {
		return false, err
	}
	return false, nil
}

const entryColumns = `id, tenant, kind, amount, cost, capped, idempotency_key, reference, detail, at`

func scanEntry(row pgx.Row) (billing.Entry, error) {
	var e billing.Entry
	var amount, cost int64
	if err := row.Scan(&e.ID, &e.Tenant, &e.Kind, &amount, &cost, &e.Capped,
		&e.IdempotencyKey, &e.Reference, &e.Detail, &e.At); err != nil {
		return billing.Entry{}, notFound(err)
	}
	e.Amount, e.Cost = billing.Micros(amount), billing.Micros(cost)
	return e, nil
}

func (s *Store) AppendEntry(ctx context.Context, e billing.Entry) (billing.Entry, bool, error) {
	if e.IdempotencyKey == "" {
		return billing.Entry{}, false, errors.New("an entry needs an idempotency key")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO billing_ledger (`+entryColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (idempotency_key) DO NOTHING`,
		e.ID, e.Tenant, e.Kind, int64(e.Amount), int64(e.Cost), e.Capped,
		e.IdempotencyKey, e.Reference, e.Detail, e.At.UTC())
	if err != nil {
		return billing.Entry{}, false, err
	}
	stored, err := s.FindEntry(ctx, e.IdempotencyKey)
	return stored, tag.RowsAffected() == 1, err
}

func (s *Store) FindEntry(ctx context.Context, idempotencyKey string) (billing.Entry, error) {
	return scanEntry(s.pool.QueryRow(ctx,
		`SELECT `+entryColumns+` FROM billing_ledger WHERE idempotency_key = $1`, idempotencyKey))
}

func (s *Store) Balance(ctx context.Context, tenant string) (billing.Micros, error) {
	var balance int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM billing_ledger WHERE tenant = $1`, tenant).Scan(&balance)
	return billing.Micros(balance), err
}

func (s *Store) ListEntries(ctx context.Context, tenant string, limit int) ([]billing.Entry, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.pool.Query(ctx, `SELECT `+entryColumns+` FROM billing_ledger
		WHERE tenant = $1 ORDER BY seq DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []billing.Entry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, entry)
	}
	return found, rows.Err()
}

const keyColumns = `id, tenant, name, kind, hash, created_at, revoked_at,
	pepper_version, expires_at, allowed_networks, rate_limit`

func scanKey(row pgx.Row) (billing.Key, error) {
	var k billing.Key
	var revoked, expires *time.Time
	var networks []string
	if err := row.Scan(&k.ID, &k.Tenant, &k.Name, &k.Kind, &k.Hash, &k.CreatedAt, &revoked,
		&k.PepperVersion, &expires, &networks, &k.RateLimit); err != nil {
		return billing.Key{}, notFound(err)
	}
	k.RevokedAt, k.ExpiresAt = value(revoked), value(expires)
	for _, raw := range networks {
		network, err := netip.ParsePrefix(raw)
		if err != nil {
			// Refusing the key is the safe reading of an allowlist that cannot
			// be read: dropping the entry would widen it.
			return billing.Key{}, fmt.Errorf("key %s has an unreadable allowed network %q: %w", k.ID, raw, err)
		}
		k.AllowedNetworks = append(k.AllowedNetworks, network)
	}
	return k, nil
}

func (s *Store) PutKey(ctx context.Context, k billing.Key) error {
	networks := make([]string, 0, len(k.AllowedNetworks))
	for _, network := range k.AllowedNetworks {
		networks = append(networks, network.String())
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO billing_keys (`+keyColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		k.ID, k.Tenant, k.Name, k.Kind, k.Hash, k.CreatedAt.UTC(), nullable(k.RevokedAt),
		k.PepperVersion, nullable(k.ExpiresAt), networks, k.RateLimit)
	if uniqueViolation(err) {
		return billing.ErrAlreadyExists
	}
	return err
}

func (s *Store) GetKey(ctx context.Context, id string) (billing.Key, error) {
	return scanKey(s.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM billing_keys WHERE id = $1`, id))
}

func (s *Store) ListKeys(ctx context.Context, tenant string) ([]billing.Key, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+keyColumns+` FROM billing_keys WHERE tenant = $1 ORDER BY created_at`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []billing.Key
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, key)
	}
	return found, rows.Err()
}

func (s *Store) RevokeKey(ctx context.Context, tenant, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE billing_keys SET revoked_at = LEAST(revoked_at, $3)
		WHERE id = $1 AND tenant = $2`, id, tenant, at.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return billing.ErrNotFound
	}
	return nil
}

func (s *Store) RehashKey(ctx context.Context, id, from, to, version string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE billing_keys SET hash = $3, pepper_version = $4
		WHERE id = $1 AND hash = $2`, id, from, to, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetKey(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordKeyUse(ctx context.Context, uses []billing.KeyUse) error {
	if len(uses) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, u := range uses {
		batch.Queue(`INSERT INTO billing_key_use
			(key_id, tenant, minute, operation, address, outcome, detail, count, first_at, last_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (key_id, minute, operation, address, outcome, detail) DO UPDATE SET
				count    = billing_key_use.count + EXCLUDED.count,
				first_at = LEAST(billing_key_use.first_at, EXCLUDED.first_at),
				last_at  = GREATEST(billing_key_use.last_at, EXCLUDED.last_at)`,
			u.Key, u.Tenant, u.Minute.UTC(), u.Operation, u.Address, u.Outcome, u.Detail,
			u.Count, u.FirstAt.UTC(), u.LastAt.UTC())
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *Store) ListKeyUse(ctx context.Context, tenant, key string, limit int) ([]billing.KeyUse, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT key_id, tenant, minute, operation, address, outcome, detail,
			count, first_at, last_at
		FROM billing_key_use WHERE tenant = $1 AND ($2 = '' OR key_id = $2)
		ORDER BY last_at DESC, key_id, operation LIMIT $3`, tenant, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []billing.KeyUse
	for rows.Next() {
		var u billing.KeyUse
		if err := rows.Scan(&u.Key, &u.Tenant, &u.Minute, &u.Operation, &u.Address, &u.Outcome, &u.Detail,
			&u.Count, &u.FirstAt, &u.LastAt); err != nil {
			return nil, err
		}
		found = append(found, u)
	}
	return found, rows.Err()
}

func (s *Store) SaveJob(ctx context.Context, j billing.Job) error {
	completed := make([]string, len(j.Completed))
	for i, step := range j.Completed {
		completed[i] = string(step)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO billing_jobs
		(id, tenant, status, current_step, completed, error, created_at, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE SET status=$3, current_step=$4, completed=$5, error=$6, completed_at=$8`,
		j.ID, j.Tenant, j.Status, j.Current, completed, j.Error, j.CreatedAt.UTC(), nullable(j.CompletedAt))
	return err
}

func (s *Store) GetJob(ctx context.Context, id string) (billing.Job, error) {
	var j billing.Job
	var completed []string
	var finished *time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, tenant, status, current_step, completed, error, created_at, completed_at
		FROM billing_jobs WHERE id = $1`, id).
		Scan(&j.ID, &j.Tenant, &j.Status, &j.Current, &completed, &j.Error, &j.CreatedAt, &finished)
	if err != nil {
		return billing.Job{}, notFound(err)
	}
	for _, step := range completed {
		j.Completed = append(j.Completed, billing.Step(step))
	}
	j.CompletedAt = value(finished)
	return j, nil
}
