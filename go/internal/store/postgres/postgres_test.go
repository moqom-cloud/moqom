package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moqom-cloud/moqom/go/internal/control"
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

// A database written by the previous build has to come out the other side with
// the same mutes in force.
//
// The conformance suite above only ever sees a schema created from scratch, so
// it says nothing about the one migration in schema.sql: the mutes table used to
// hold a mask per participant and now holds a row per kind. Getting that wrong
// in the quiet direction -- dropping the table, or splitting it into nothing --
// would lift every force-mute in the middle of the deployment that fixed
// force-mutes, and nobody would see an error.
func TestTheMuteMigrationKeepsEveryKind(t *testing.T) {
	dsn := os.Getenv("MOQOM_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("MOQOM_TEST_POSTGRES is not set; run deploy/tools/conformance.sh")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("m%d", time.Now().UnixNano())
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer connection.Close(ctx)
	defer drop(t, dsn, schema)

	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatalf("creating the schema: %v", err)
	}

	// The old shape, and a room in the state a busy moderator leaves behind: one
	// participant silenced on audio and video for everybody, another hidden from
	// the audience on screenshare with an expiry.
	lapses := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	// One statement at a time: a parameter puts pgx on the extended protocol,
	// which takes exactly one command per exec.
	if _, err := connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatalf("setting the search path: %v", err)
	}
	if _, err := connection.Exec(ctx, `
		CREATE TABLE mutes (
		    tenant        text        NOT NULL,
		    room          text        NOT NULL,
		    username      text        NOT NULL,
		    device        text        NOT NULL,
		    audience_only boolean     NOT NULL,
		    media         bigint      NOT NULL,
		    expires_at    timestamptz,
		    created_at    timestamptz NOT NULL,
		    PRIMARY KEY (tenant, room, username, device, audience_only)
		)`); err != nil {
		t.Fatalf("building the old table: %v", err)
	}
	if _, err := connection.Exec(ctx, `
		INSERT INTO mutes VALUES
		    ('acme', 'room-1', 'alice', 'd1', false, 3, NULL, now()),
		    ('acme', 'room-1', 'bob', 'd1', true, 4, $1, now())`, lapses); err != nil {
		t.Fatalf("seeding the old table: %v", err)
	}

	subject, err := postgres.Open(ctx, postgres.Config{DSN: dsn, Schema: schema})
	if err != nil {
		t.Fatalf("opening the store, which applies the schema: %v", err)
	}
	defer subject.Close()

	mutes, err := subject.ListMutes(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing after the migration: %v", err)
	}

	type row struct {
		who          string
		media        control.MediaKind
		audienceOnly bool
	}
	got := map[row]time.Time{}
	for _, mute := range mutes {
		got[row{mute.Identity.Username, mute.Media, mute.AudienceOnly}] = mute.ExpiresAt
	}
	want := []row{
		{"alice", control.MediaAudio, false},
		{"alice", control.MediaVideo, false},
		{"bob", control.MediaScreenShare, true},
	}
	if len(got) != len(want) {
		t.Fatalf("the migration produced %d rows, want %d: %+v", len(got), len(want), mutes)
	}
	for _, wanted := range want {
		if _, ok := got[wanted]; !ok {
			t.Errorf("%+v did not survive the migration", wanted)
		}
	}
	if expires := got[row{"bob", control.MediaScreenShare, true}]; !expires.Equal(lapses) {
		t.Errorf("the timed mute came back with expiry %v, want %v", expires, lapses)
	}

	// And the new key is in place, so a second kind no longer replaces the first.
	if _, err := subject.PutMute(ctx, store.Mute{
		Tenant: "acme", Room: "room-1",
		Identity: control.Identity{Username: "bob", Device: "d1"},
		Media:    control.MediaAudio, AudienceOnly: true,
	}); err != nil {
		t.Fatalf("muting bob's microphone after the migration: %v", err)
	}
	mutes, err = subject.ListMutes(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing after a new mute: %v", err)
	}
	if len(mutes) != 4 {
		t.Errorf("a mute of another kind left %d rows, want 4: %+v", len(mutes), mutes)
	}
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
