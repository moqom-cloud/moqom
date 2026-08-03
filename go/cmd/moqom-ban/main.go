// Command moqom-ban bans an account from a room through the admin API.
//
// The counterpart to moqom-mint, and the other half of what an application's
// backend would do: mint says who may join, this says who may not. It exists
// for the same reason -- the sample application has no server to issue the call
// from, and the path it exercises is worth being able to run by hand.
//
//	MOQOM_BAN_TARGET=127.0.0.1:8443 \
//	MOQOM_BAN_CREDENTIAL=$(gcloud secrets versions access latest --secret=moqom-dev-admin-credential) \
//	moqom-ban -user publisher -room bench
//
// # What this is really testing
//
// A ban is the only operation that has to travel the whole way back down: the
// control plane writes it, finds the relays holding a session for that account,
// and pushes a directive over RelayLink, which closes the session mid-stream.
// Everything else in the admin API is a database write that a client discovers
// later. So a ban that lands within a second of the call is evidence about the
// link -- that it is attached, authenticated, and carrying directives in the
// direction that has no request to ride along with.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"

	"github.com/moqom-cloud/moqom/go/internal/admintls"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("moqom-ban: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	user := flag.String("user", "", "the account to ban")
	room := flag.String("room", "", "which room")
	reason := flag.String("reason", "verifying enforcement", "recorded on the audit entry")
	lift := flag.Bool("unban", false, "lift the ban instead of placing it")
	// Empty by default, and that is the difference between two kinds of ban.
	// Naming a moderator asks the control plane to check that they hold
	// moderation in this room, which is the right check and refuses an operator
	// who is not a participant -- "elliott holds nothing in bantest". Left
	// empty, the action is the service account's own, which is what a backend
	// deciding on its own behalf looks like.
	behalf := flag.String("on-behalf-of", "", "the moderator to attribute this to; they must hold moderation in the room")
	flag.Parse()

	if *user == "" || *room == "" {
		return errors.New("-user and -room are both required")
	}

	target := os.Getenv("MOQOM_BAN_TARGET")
	if target == "" {
		return errors.New("MOQOM_BAN_TARGET is required, e.g. 127.0.0.1:8443")
	}
	credential := os.Getenv("MOQOM_BAN_CREDENTIAL")
	if credential == "" {
		return errors.New("MOQOM_BAN_CREDENTIAL is required: a service account credential")
	}
	tenant := value("MOQOM_BAN_TENANT", "moqom")

	options := []moqom.Option{
		moqom.WithCredential(credential),
		moqom.WithTenant(tenant),
	}
	transport, err := admintls.Credentials()
	if err != nil {
		return err
	}
	if transport != nil {
		options = append(options, moqom.WithTLS(transport))
	} else {
		options = append(options, moqom.WithoutTransportSecurity())
	}

	client, err := moqom.Dial(target, options...)
	if err != nil {
		return fmt.Errorf("dialling %s: %w", target, err)
	}
	defer client.Close()

	ctx, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()

	// An actor, because an audit entry that cannot say why is not an audit
	// entry. Who is optional; why is not.
	actor := moqom.Actor{OnBehalfOf: *behalf, Reason: *reason}
	handle := client.Room(*room)

	if *lift {
		entry, err := handle.Unban(ctx, moqom.BanAccount(*user), actor)
		if err != nil {
			return fmt.Errorf("unbanning %s: %w", *user, err)
		}
		log.Printf("lifted: %s may rejoin %s (entry %s)", *user, *room, entry.ID)
		return nil
	}

	entry, err := handle.Ban(ctx, moqom.BanRequest{
		Subject: moqom.BanAccount(*user),
		Code:    moqom.CodeHarassment,
		Actor:   actor,
	})
	if err != nil {
		return fmt.Errorf("banning %s: %w", *user, err)
	}
	log.Printf("banned: %s is out of %s and cannot rejoin (entry %s, enforced=%t)",
		*user, *room, entry.ID, entry.Enforced)
	return nil
}

func value(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
