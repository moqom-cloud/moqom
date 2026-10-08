package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/billing"
	"github.com/moqom-cloud/moqom/go/internal/billing/billingtest"
	"github.com/moqom-cloud/moqom/go/internal/billing/postgres"
)

// Skipped without MOQOM_TEST_POSTGRES, like the control plane's store.
func TestConformance(t *testing.T) {
	dsn := os.Getenv("MOQOM_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("MOQOM_TEST_POSTGRES is not set")
	}
	var counter atomic.Uint64
	billingtest.Run(t, func(t *testing.T) billing.Store {
		ctx := context.Background()
		schema := fmt.Sprintf("billing_%d_%d", time.Now().UnixNano(), counter.Add(1))
		settings, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		settings.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize())
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
		return subject
	})
}
