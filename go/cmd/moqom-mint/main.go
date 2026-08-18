// Command moqom-mint issues a client token for a device that has no backend.
//
// Minting is an admin call, and deliberately so: an application's own server
// authenticates its user, decides what that user may do, and asks the control
// plane for a token saying it. A phone never talks to this API, because a phone
// that could mint its own token could mint any token.
//
// The sample application has no server, which leaves it with nothing to ask.
// This stands in for one -- a person at a terminal, deciding by hand what a
// device may do and handing it the result:
//
//	MOQOM_MINT_TARGET=127.0.0.1:8443 \
//	MOQOM_MINT_CREDENTIAL=$(gcloud secrets versions access latest --secret=moqom-dev-admin-credential) \
//	moqom-mint -user ada -room bench
//
// It prints the token and nothing else on stdout, so it can be substituted into
// a launch argument; everything explanatory goes to stderr.
//
// # What has to line up
//
// A relay compares the namespace a session announces against four things the
// token names: tenant, user, device, and room. Three of them are arguments
// here and the fourth is the sample's built-in device identifier, so the
// defaults below are not neutral -- they are what MoqomRoom announces. Change
// one on this side only and the session connects, announces, and is refused
// every track, which reads from the phone as a relay that drops everything.
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
	log.SetPrefix("moqom-mint: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	user := flag.String("user", "", "the account this token speaks for")
	room := flag.String("room", "", "which room; empty mints a token with no room")
	// The thumbprint of a key on the phone, which the join screen prints. This
	// defaulted to `sample-device-key` for as long as the sample had no key --
	// an opaque identifier a device ban could be placed on and nothing could
	// sign for. It has a key now, so the default is gone: a token minted
	// against the old string is bound to a thumbprint nobody holds, and a relay
	// requiring proof of possession refuses the session with the reason inside
	// a credential nobody reads. Being asked for it is cheaper than that.
	device := flag.String("device", "", "the device's thumbprint, as its join screen prints it")
	lifetime := flag.Duration("lifetime", 15*time.Minute, "how long it is good for; fifteen minutes is the server's ceiling")
	owner := flag.Bool("owner", false, "make the holder the room's owner")
	create := flag.Bool("create-room", true, "create the room if it does not exist yet")
	flag.Parse()

	if *user == "" {
		return errors.New("-user is required")
	}
	if *device == "" {
		return errors.New(
			"-device is required: the thumbprint the phone's join screen prints under its username")
	}

	target := os.Getenv("MOQOM_MINT_TARGET")
	if target == "" {
		return errors.New("MOQOM_MINT_TARGET is required, e.g. 127.0.0.1:8443")
	}
	credential := os.Getenv("MOQOM_MINT_CREDENTIAL")
	if credential == "" {
		return errors.New("MOQOM_MINT_CREDENTIAL is required: a service account credential")
	}
	// The tenant the admin account is scoped to, which is also the tenant the
	// client has to name. `moqom` is what deploy/tools/sync-secrets.sh writes.
	tenant := value("MOQOM_MINT_TENANT", "moqom")

	options := []moqom.Option{
		moqom.WithCredential(credential),
		moqom.WithTenant(tenant),
	}
	// A deployed control plane asks every caller on this port for a certificate,
	// including this one. Without one there is nothing to present and the dial
	// fails at the handshake, so plaintext is the local case only.
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

	// Minting checks that the room exists and is open, which is the right check
	// -- a token for a room nobody made would fail at the relay instead, where
	// nothing can explain it. Here that check is a chore rather than a
	// safeguard, so this makes the room the first time somebody joins it.
	if *room != "" && *create {
		switch _, err := client.CreateRoom(ctx, moqom.NewRoom{Name: *room}); {
		case err == nil:
			log.Printf("created room %s", *room)
		case errors.Is(err, moqom.ErrExists):
		default:
			return fmt.Errorf("creating room %s: %w", *room, err)
		}
	}

	token, err := client.MintToken(ctx, moqom.TokenRequest{
		Username:         *user,
		DeviceThumbprint: *device,
		Room:             *room,
		// Everything a participant in a two-way call sends. A viewer would take
		// PublishNone, and the difference between the two is exactly what the
		// relay now enforces.
		Publish:  moqom.PublishFull,
		Owner:    *owner,
		Lifetime: *lifetime,
	})
	if err != nil {
		return fmt.Errorf("minting: %w", err)
	}

	log.Printf("%s/%s in tenant %s, room %q, expires %s (in %s)",
		token.Identity.Username,
		token.Identity.Device,
		tenant,
		*room,
		token.ExpiresAt.Format(time.RFC3339),
		time.Until(token.ExpiresAt).Round(time.Second),
	)
	fmt.Println(token.Token)
	return nil
}

func value(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
