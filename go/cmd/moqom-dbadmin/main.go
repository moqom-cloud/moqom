// Command moqom-dbadmin grants a role the privileges the control plane needs.
//
// It exists because of a gap in how Cloud SQL and PostgreSQL 15 meet. The
// control plane logs in as its own Google identity, which Cloud SQL turns into
// a PostgreSQL role with no privileges beyond connecting; PostgreSQL 15 took
// CREATE on the public schema away from PUBLIC. So the first thing the control
// plane does -- create its tables -- fails, and the fix is a grant that only a
// member of cloudsqlsuperuser can issue. That is the built-in `postgres` user,
// and nothing else in this system has a password.
//
// So this runs once, as that user, from tools/grant-database.sh -- which
// generates a password, uses it here, and scrambles it afterwards, so no
// standing credential is created and none is written to Terraform state.
//
//	MOQOM_DBADMIN_URL=postgres://postgres:...@127.0.0.1:5432/moqom \
//	MOQOM_DBADMIN_ROLE=moqom-dev-control@project.iam \
//	moqom-dbadmin
//
// Idempotent. Every statement is a grant that is already true after the first
// run.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("moqom-dbadmin: %v", err)
	}
}

func run() error {
	url := os.Getenv("MOQOM_DBADMIN_URL")
	if url == "" {
		return errors.New("MOQOM_DBADMIN_URL is required")
	}
	role := os.Getenv("MOQOM_DBADMIN_ROLE")
	if role == "" {
		return errors.New("MOQOM_DBADMIN_ROLE is required: the database role to grant to")
	}
	// A role name is interpolated into DDL below, because PostgreSQL takes no
	// parameter in that position. It is quoted, and this rejects the one thing
	// quoting cannot survive.
	if strings.Contains(role, `"`) {
		return fmt.Errorf("role %q contains a quote", role)
	}

	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()

	connection, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer connection.Close(context.Background())

	quoted := `"` + role + `"`
	statements := []string{
		// Connect, and create tables in the schema it will use. Not ownership of
		// the database and not superuser: the control plane creates its own
		// tables and reads its own rows, and nothing it does needs more.
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`, quoted),

		// Tables that already exist, for the case where this runs after a
		// migration rather than before one.
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, quoted),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, quoted),
	}

	for _, statement := range statements {
		if _, err := connection.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
		log.Printf("ok: %s", statement)
	}

	log.Printf("%s can now create and use its tables", role)
	return nil
}
