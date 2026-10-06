package retention_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	billingpg "github.com/moqom-cloud/moqom/go/internal/billing/postgres"
	"github.com/moqom-cloud/moqom/go/internal/retention"
	"github.com/moqom-cloud/moqom/go/internal/store/postgres"
)

// The sweep against the real schema, so a column rename in schema.sql fails
// here rather than in the nightly job. Skipped without MOQOM_TEST_POSTGRES,
// like the store's own conformance suite.
func TestSweepAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("MOQOM_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("MOQOM_TEST_POSTGRES is not set; run deploy/tools/conformance.sh")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("retention_%d", time.Now().UnixNano())
	subject, err := postgres.Open(ctx, postgres.Config{DSN: dsn, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		subject.Close()
		if connection, err := pgx.Connect(ctx, dsn); err == nil {
			_, _ = connection.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = connection.Close(ctx)
		}
	}()
	pool := subject.Pool()
	if _, err := billingpg.New(ctx, pool); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 6, 3, 30, 0, 0, time.UTC)
	old, recent := now.AddDate(-2, 0, 0), now.Add(-time.Hour)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO rooms (tenant, room, lifecycle, created_at) VALUES ('t', 'r', 'open', $1)`, old)
	exec(`INSERT INTO participants (tenant, room, username, device, presence, joined_at, last_seen_at, remote_addr)
		VALUES ('t','r','gone','d','left',$1,$1,'203.0.113.1'),
		       ('t','r','stale','d','connected',$2,$2,'203.0.113.2'),
		       ('t','r','here','d','connected',$3,$3,'203.0.113.3')`, old, now.AddDate(0, 0, -45), recent)
	exec(`INSERT INTO bans (id, tenant, kind, ip_prefix, expires_at, created_at)
		VALUES ('b1','t','ip','203.0.113.0/24',$1,$1), ('b2','t','ip','198.51.100.0/24',NULL,$1)`, old)
	exec(`INSERT INTO billing_tenants (id, name, email, status, created_at, updated_at) VALUES ('t','T','t@example.com','active',$1,$1)`, old)
	exec(`INSERT INTO billing_ledger (id, tenant, kind, amount, idempotency_key, at) VALUES ('e1','t','top_up',100,'k1',$1)`, old)
	exec(`INSERT INTO billing_keys (id, tenant, name, kind, hash, created_at, revoked_at)
		VALUES ('k_old','t','a','backend','h',$1,$1), ('k_live','t','b','backend','h',$1,NULL)`, old)
	exec(`INSERT INTO billing_keys (id, tenant, name, kind, hash, created_at, expires_at)
		VALUES ('k_expired','t','c','mint','h',$1,$1), ('k_expiring','t','d','mint','h',$1,$2)`, old, now.Add(time.Hour))
	exec(`INSERT INTO billing_key_use (key_id, tenant, minute, operation, address, outcome, count, first_at, last_at)
		VALUES ('k_live','t',$1,'GET /v1/keys','203.0.113.1','ok',1,$1,$1),
		       ('k_live','t',$2,'GET /v1/keys','203.0.113.1','ok',1,$2,$2)`, old, recent)

	report := (&retention.Sweeper{DB: pool, Policy: retention.DefaultPolicy(), Now: func() time.Time { return now }}).Run(ctx)
	if report.Failed() {
		t.Fatalf("%+v", report)
	}

	count := func(sql string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM participants`); n != 2 {
		t.Errorf("participants: %d, want the departed one gone", n)
	}
	if n := count(`SELECT count(*) FROM participants WHERE remote_addr <> ''`); n != 1 {
		t.Errorf("addresses: %d, want only the recent one kept", n)
	}
	if n := count(`SELECT count(*) FROM bans`); n != 1 {
		t.Errorf("bans: %d, want the permanent one kept", n)
	}
	if n := count(`SELECT count(*) FROM billing_keys`); n != 2 {
		t.Errorf("keys: %d, want the live and the unexpired one kept", n)
	}
	if n := count(`SELECT count(*) FROM billing_key_use`); n != 1 {
		t.Errorf("key use: %d, want only the recent entry kept", n)
	}
	if n := count(`SELECT count(*) FROM billing_ledger`); n != 1 {
		t.Errorf("ledger: %d, it must never be pruned", n)
	}
}
