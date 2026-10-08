package postgres

import (
	"context"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/billing"
)

var _ billing.OperatorStore = (*Store)(nil)

// ListTenantOverviews is two reads rather than a join, so the tenant row is
// scanned by scanTenant and stays in step with whatever columns it grows.
func (s *Store) ListTenantOverviews(ctx context.Context, month time.Time) ([]billing.TenantOverview, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tenantColumns+` FROM billing_tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	var overviews []billing.TenantOverview
	index := map[string]int{}
	for rows.Next() {
		tenant, err := scanTenant(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		index[tenant.ID] = len(overviews)
		overviews = append(overviews, billing.TenantOverview{Tenant: tenant})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sums, err := s.pool.Query(ctx, `SELECT tenant,
			COALESCE(SUM(amount), 0)::bigint,
			COALESCE(-SUM(amount) FILTER (WHERE kind = 'usage' AND at >= $1), 0)::bigint,
			MAX(at)
		FROM billing_ledger GROUP BY tenant`, month.UTC())
	if err != nil {
		return nil, err
	}
	defer sums.Close()
	for sums.Next() {
		var tenant string
		var balance, spend int64
		var last *time.Time
		if err := sums.Scan(&tenant, &balance, &spend, &last); err != nil {
			return nil, err
		}
		i, ok := index[tenant]
		if !ok {
			continue
		}
		overviews[i].Balance, overviews[i].MonthSpend = billing.Micros(balance), billing.Micros(spend)
		overviews[i].LastActivity = value(last)
	}
	return overviews, sums.Err()
}

func (s *Store) AppendAudit(ctx context.Context, e billing.AuditEntry) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO billing_operator_audit
		(id, at, operator, subject, address, action, tenant, detail, outcome)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID, e.At.UTC(), e.Operator, e.Subject, e.Address, e.Action, e.Tenant, e.Detail, e.Outcome)
	return err
}

func (s *Store) ListAudit(ctx context.Context, tenant string, limit int) ([]billing.AuditEntry, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.pool.Query(ctx, `SELECT id, at, operator, subject, address, action, tenant, detail, outcome
		FROM billing_operator_audit WHERE ($1 = '' OR tenant = $1) ORDER BY seq DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []billing.AuditEntry
	for rows.Next() {
		var e billing.AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Operator, &e.Subject, &e.Address, &e.Action, &e.Tenant, &e.Detail, &e.Outcome); err != nil {
			return nil, err
		}
		found = append(found, e)
	}
	return found, rows.Err()
}
