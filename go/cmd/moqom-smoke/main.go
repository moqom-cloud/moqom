// Command moqom-smoke checks that a deployed control plane actually works.
//
// Not a test of the control plane -- that is what the suites in internal/ are
// for -- but of everything around it that only exists once something is
// deployed: that the address resolves, that the credential in the secret is the
// credential the process loaded, that the database it opened accepts writes,
// and that a room created by one call is visible to the next.
//
// Every one of those has failed at least once while standing this environment
// up, and each time the symptom was a gRPC error several layers away from the
// cause.
//
//	MOQOM_SMOKE_TARGET=127.0.0.1:8443 \
//	MOQOM_SMOKE_CREDENTIAL=$(gcloud secrets versions access latest --secret=moqom-dev-admin-credential) \
//	moqom-smoke
//
// It creates a room named after the clock, and deletes nothing: a room is cheap,
// and one left behind is evidence when somebody asks whether this ever ran.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"

	"github.com/moqom-cloud/moqom/go/internal/admintls"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("moqom-smoke: %v", err)
	}
}

func run() error {
	target := os.Getenv("MOQOM_SMOKE_TARGET")
	if target == "" {
		return errors.New("MOQOM_SMOKE_TARGET is required, e.g. 127.0.0.1:8443")
	}
	credential := os.Getenv("MOQOM_SMOKE_CREDENTIAL")
	if credential == "" {
		return errors.New("MOQOM_SMOKE_CREDENTIAL is required: a service account credential")
	}
	tenant := value("MOQOM_SMOKE_TENANT", "moqom")

	options := []moqom.Option{
		moqom.WithCredential(credential),
		moqom.WithTenant(tenant),
	}
	// A certificate where one is configured, and plaintext where none is. The
	// deployed control plane asks every caller on this port for one -- being
	// reachable only through a port-forward is not the same as knowing who is
	// on the other end of it -- so this is part of what the smoke test proves.
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

	name := fmt.Sprintf("smoke-%d", time.Now().Unix())
	log.Printf("creating room %s in tenant %s", name, tenant)

	room, err := client.CreateRoom(ctx, moqom.NewRoom{Name: name})
	if err != nil {
		return fmt.Errorf("creating a room: %w", err)
	}
	log.Printf("created: %s", room.Name)

	// Read it back through a second call, which is the part that proves the
	// write reached a store rather than a process's memory.
	rooms, err := client.AllRooms(ctx, moqom.RoomQuery{})
	if err != nil {
		return fmt.Errorf("listing rooms: %w", err)
	}
	for _, candidate := range rooms {
		if candidate.Name == name {
			log.Printf("ok: %s is in the store, alongside %d other room(s)", name, len(rooms)-1)
			return nil
		}
	}
	return fmt.Errorf("created %s but it is not in the %d rooms listed back", name, len(rooms))
}

func value(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
