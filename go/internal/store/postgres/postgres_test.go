package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/postgres"
	"github.com/moqom-cloud/moqom/go/internal/store/storetest"
)

// The whole contract, against a real database.
//
// Skipped without MOQOM_TEST_POSTGRES, because `go test ./...` has to work on a
// machine with no database and a laptop is not going to have one. That makes a
// green run of the package list weak evidence about this store specifically.
// The run that counts is deploy/tools/conformance.sh, which builds this file
// into a binary and runs it as a Job beside the deployed Cloud SQL instance --
// the same version and collation as the database that will serve, and one with
// no address a laptop could reach.
func TestConformance(t *testing.T) {
	dsn := os.Getenv("MOQOM_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("MOQOM_TEST_POSTGRES is not set; run deploy/tools/conformance.sh")
	}

	var counter atomic.Uint64
	storetest.Run(t, func(t *testing.T, now func() time.Time) (store.Store, func()) {
		t.Helper()

		// A schema per case, dropped after. Sharing one would make a failure in
		// an early test show up as a mystery in a later one, and the suite runs
		// cases that assume an empty store.
		schema := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), counter.Add(1))
		ctx := context.Background()

		subject, err := postgres.Open(ctx, postgres.Config{
			DSN:    dsn,
			Schema: schema,
			Now:    now,
			// Ordered IDs, because two assertions in the suite are about
			// ordering by ID and random ones would make them flaky rather than
			// wrong.
			IDs: sequentialIDs(),
		})
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}

		return subject, func() {
			subject.Close()
			drop(t, dsn, schema)
		}
	})
}

// drop removes a case's schema, on a connection of its own.
//
// Deliberately not a method on the store: a type that can delete every table it
// owns is one keystroke from being called somewhere that is not a test.
func drop(t *testing.T, dsn, schema string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Logf("could not connect to drop schema %s: %v", schema, err)
		return
	}
	defer connection.Close(ctx)

	if _, err := connection.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
		t.Logf("could not drop schema %s: %v", schema, err)
	}
}

func sequentialIDs() func() string {
	var counter atomic.Uint64
	return func() string { return fmt.Sprintf("pg-%09d", counter.Add(1)) }
}
