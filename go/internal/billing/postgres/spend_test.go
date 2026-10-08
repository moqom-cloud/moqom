package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/billing"
	"github.com/moqom-cloud/moqom/go/internal/billing/postgres"
)

func TestSpendMemory(t *testing.T) { checkSpend(t, billing.NewMemory()) }

func TestSpendPostgres(t *testing.T) {
	dsn := os.Getenv("MOQOM_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("MOQOM_TEST_POSTGRES is not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("billing_spend_%d", time.Now().UnixNano())
	settings, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	settings.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		// Not UTC, so a month that depended on the session's zone would show.
		_, err := connection.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()+"; SET TIME ZONE 'Pacific/Kiritimati'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	subject, err := postgres.New(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	checkSpend(t, subject)
}

func checkSpend(t *testing.T, store billing.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)
	month := billing.MonthOf(now)
	tenant := billing.Tenant{ID: "t_spend", Name: "Spend", Email: "spend@example.com", Status: billing.StatusActive,
		CreatedAt: now, UpdatedAt: now, Spending: billing.Spending{Limit: 7 * billing.Dollar}}
	if _, err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTenant(ctx, tenant.ID)
	if err != nil || got.Spending.Limit != 7*billing.Dollar {
		t.Fatalf("created spending = %+v, %v", got.Spending, err)
	}

	want := billing.Spending{Limit: 20 * billing.Dollar, Alerts: []billing.Micros{5 * billing.Dollar, 15 * billing.Dollar}}
	saved, err := store.SetSpending(ctx, tenant.ID, want, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.GetTenant(ctx, tenant.ID)
	if err != nil || got.Spending.Limit != want.Limit || !slices.Equal(got.Spending.Alerts, want.Alerts) ||
		got.Spending.AlertsOff || !slices.Equal(saved.Spending.Alerts, want.Alerts) {
		t.Fatalf("spending = %+v, %v; want %+v", got.Spending, err, want)
	}
	// SaveTenant leaves spending alone.
	got.Name = "Renamed"
	if _, err := store.SaveTenant(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = store.GetTenant(ctx, tenant.ID); got.Spending.Limit != want.Limit {
		t.Fatalf("spending after SaveTenant = %+v", got.Spending)
	}
	if _, err := store.SetSpending(ctx, "t_missing", want, now); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("SetSpending on a missing tenant = %v", err)
	}

	entries := []billing.Entry{
		{ID: "1", Kind: billing.KindUsage, Amount: -2 * billing.Dollar, IdempotencyKey: "u1", At: month.Add(-time.Second)},
		{ID: "2", Kind: billing.KindUsage, Amount: -3 * billing.Dollar, IdempotencyKey: "u2", At: month},
		{ID: "3", Kind: billing.KindUsage, Amount: -4 * billing.Dollar, IdempotencyKey: "u3", At: now},
		{ID: "4", Kind: billing.KindTopUp, Amount: 50 * billing.Dollar, IdempotencyKey: "p1", At: now},
	}
	for _, entry := range entries {
		entry.Tenant = tenant.ID
		if _, _, err := store.AppendEntry(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	if spent, err := store.UsageSince(ctx, tenant.ID, month); err != nil || spent != 7*billing.Dollar {
		t.Fatalf("UsageSince = %s, %v; want $7.00", spent, err)
	}

	for _, threshold := range []billing.Micros{15 * billing.Dollar, 5 * billing.Dollar} {
		if claimed, err := store.ClaimSpendAlert(ctx, tenant.ID, month, threshold, now); err != nil || !claimed {
			t.Fatalf("first claim of %s = %v, %v", threshold, claimed, err)
		}
	}
	if claimed, err := store.ClaimSpendAlert(ctx, tenant.ID, month, 5*billing.Dollar, now); err != nil || claimed {
		t.Fatalf("second claim = %v, %v", claimed, err)
	}
	next := billing.MonthOf(month.AddDate(0, 1, 0))
	if claimed, err := store.ClaimSpendAlert(ctx, tenant.ID, next, 5*billing.Dollar, now); err != nil || !claimed {
		t.Fatalf("claim in the next month = %v, %v", claimed, err)
	}
	if _, err := store.ClaimSpendAlert(ctx, "t_missing", month, billing.Dollar, now); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("claim for a missing tenant = %v", err)
	}

	fired, err := store.FiredSpendAlerts(ctx, tenant.ID, month)
	if err != nil || len(fired) != 2 || fired[0].Threshold != 5*billing.Dollar || fired[1].Threshold != 15*billing.Dollar ||
		!fired[0].At.Equal(now) {
		t.Fatalf("fired = %+v, %v", fired, err)
	}
	if err := store.ReleaseSpendAlert(ctx, tenant.ID, month, 5*billing.Dollar); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimSpendAlert(ctx, tenant.ID, month, 5*billing.Dollar, now); err != nil || !claimed {
		t.Fatalf("claim after release = %v, %v", claimed, err)
	}
}
