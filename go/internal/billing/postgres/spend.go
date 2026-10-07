package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moqom-cloud/moqom/go/internal/billing"
)

func toAmounts(amounts []billing.Micros) []int64 {
	out := make([]int64, len(amounts))
	for i, amount := range amounts {
		out[i] = int64(amount)
	}
	return out
}

func fromAmounts(amounts []int64) []billing.Micros {
	if len(amounts) == 0 {
		return nil
	}
	out := make([]billing.Micros, len(amounts))
	for i, amount := range amounts {
		out[i] = billing.Micros(amount)
	}
	return out
}

// monthDate is the month as text, so the cast to date does not depend on the
// session's time zone.
func monthDate(month time.Time) string { return month.UTC().Format(time.DateOnly) }

func (s *Store) SetSpending(ctx context.Context, tenant string, spending billing.Spending, at time.Time) (billing.Tenant, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE billing_tenants SET
		spend_limit = $2, spend_alerts = $3, spend_alerts_off = $4, updated_at = $5
		WHERE id = $1`,
		tenant, int64(spending.Limit), toAmounts(spending.Alerts), spending.AlertsOff, at.UTC())
	if err != nil {
		return billing.Tenant{}, err
	}
	if tag.RowsAffected() == 0 {
		return billing.Tenant{}, billing.ErrNotFound
	}
	return s.GetTenant(ctx, tenant)
}

// UsageSince is answered from billing_ledger_tenant_usage alone.
func (s *Store) UsageSince(ctx context.Context, tenant string, since time.Time) (billing.Micros, error) {
	var spent int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(-SUM(amount), 0)::bigint FROM billing_ledger
		WHERE tenant = $1 AND kind = $2 AND at >= $3`, tenant, billing.KindUsage, since.UTC()).Scan(&spent)
	return billing.Micros(spent), err
}

// UsageBreakdown reads the range from billing_ledger_tenant_usage and groups
// on the detail RecordUsage wrote. Every usage entry's detail is a JSON
// object; an empty one groups under empty role and tier rather than failing
// the query.
func (s *Store) UsageBreakdown(ctx context.Context, tenant string, from, to time.Time) ([]billing.UsageBucket, error) {
	rows, err := s.pool.Query(ctx, `SELECT
			COALESCE(d->>'role', ''), COALESCE(d->>'tier', ''), COALESCE(d->>'hosting', ''),
			count(*),
			COALESCE(SUM((d->>'seconds')::bigint), 0)::bigint,
			COALESCE(SUM((d->>'egress_bytes')::bigint), 0)::bigint,
			COALESCE(SUM((d->>'ingress_bytes')::bigint), 0)::bigint,
			COALESCE(-SUM(amount), 0)::bigint,
			count(*) FILTER (WHERE capped)
		FROM (SELECT amount, capped,
				CASE WHEN detail LIKE '{%' THEN detail::jsonb ELSE '{}'::jsonb END AS d
			FROM billing_ledger
			WHERE tenant = $1 AND kind = $2 AND at >= $3 AND at < $4) usage
		GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`,
		tenant, billing.KindUsage, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var buckets []billing.UsageBucket
	for rows.Next() {
		var b billing.UsageBucket
		var charged int64
		if err := rows.Scan(&b.Role, &b.Tier, &b.Hosting, &b.Records, &b.Seconds,
			&b.EgressBytes, &b.IngressBytes, &charged, &b.Capped); err != nil {
			return nil, err
		}
		b.Charged = billing.Micros(charged)
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// ClaimSpendAlert is claimed by the primary key: of two replicas inserting the
// same row, one inserts and the other does nothing.
func (s *Store) ClaimSpendAlert(ctx context.Context, tenant string, month time.Time, threshold billing.Micros, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO billing_spend_alerts (tenant, month, threshold, fired_at)
		SELECT id, $2::date, $3, $4 FROM billing_tenants WHERE id = $1
		ON CONFLICT (tenant, month, threshold) DO NOTHING`,
		tenant, monthDate(month), int64(threshold), at.UTC())
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

func (s *Store) ReleaseSpendAlert(ctx context.Context, tenant string, month time.Time, threshold billing.Micros) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM billing_spend_alerts
		WHERE tenant = $1 AND month = $2::date AND threshold = $3`, tenant, monthDate(month), int64(threshold))
	return err
}

func (s *Store) FiredSpendAlerts(ctx context.Context, tenant string, month time.Time) ([]billing.FiredAlert, error) {
	rows, err := s.pool.Query(ctx, `SELECT threshold, fired_at FROM billing_spend_alerts
		WHERE tenant = $1 AND month = $2::date ORDER BY threshold`, tenant, monthDate(month))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fired []billing.FiredAlert
	for rows.Next() {
		var threshold int64
		var alert billing.FiredAlert
		if err := rows.Scan(&threshold, &alert.At); err != nil {
			return nil, err
		}
		alert.Threshold = billing.Micros(threshold)
		fired = append(fired, alert)
	}
	return fired, rows.Err()
}

// ClaimDirectSeconds takes the month's row lock through the upsert, so two
// replicas claiming at once are serialised, and a claim whose key another
// replica recorded first is rolled back with its increment.
func (s *Store) ClaimDirectSeconds(ctx context.Context, tenant string, month time.Time, key string, seconds int64) (int64, error) {
	return s.claimSeconds(ctx, "billing_direct_months", "billing_direct_claims", tenant, month, key, seconds)
}

// ClaimCloudflareSeconds is ClaimDirectSeconds against its own allowance.
func (s *Store) ClaimCloudflareSeconds(ctx context.Context, tenant string, month time.Time, key string, seconds int64) (int64, error) {
	return s.claimSeconds(ctx, "billing_cloudflare_months", "billing_cloudflare_claims", tenant, month, key, seconds)
}

// claimSeconds takes table names from the two callers above only, never from
// input, which is what makes formatting them into the SQL safe.
func (s *Store) claimSeconds(ctx context.Context, months, claims, tenant string, month time.Time, key string, seconds int64) (int64, error) {
	claimed := func() (int64, error) {
		var prior int64
		err := s.pool.QueryRow(ctx,
			`SELECT prior FROM `+claims+` WHERE idempotency_key = $1`, key).Scan(&prior)
		return prior, err
	}
	if prior, err := claimed(); err == nil {
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var total int64
	err = tx.QueryRow(ctx, `INSERT INTO `+months+` (tenant, month, seconds)
		VALUES ($1, $2::date, $3)
		ON CONFLICT (tenant, month) DO UPDATE SET seconds = `+months+`.seconds + EXCLUDED.seconds
		RETURNING seconds`, tenant, monthDate(billing.MonthOf(month)), seconds).Scan(&total)
	if err != nil {
		return 0, err
	}
	prior := total - seconds
	tag, err := tx.Exec(ctx, `INSERT INTO `+claims+` (idempotency_key, tenant, month, seconds, prior)
		VALUES ($1, $2, $3::date, $4, $5) ON CONFLICT (idempotency_key) DO NOTHING`,
		key, tenant, monthDate(billing.MonthOf(month)), seconds, prior)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		return claimed()
	}
	return prior, tx.Commit(ctx)
}
