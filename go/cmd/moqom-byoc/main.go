// Command moqom-byoc enrols, lists and revokes customer-run relays.
//
// It speaks the control plane's overlay API service, which binds no port: the
// call goes over OpenZiti as the identity in MOQOM_ZITI_IDENTITY, and the
// bearer credential in MOQOM_BYOC_CREDENTIAL says whose tenant it is. That
// account needs the owner ceiling on the tenant.
//
//	MOQOM_ZITI_IDENTITY=~/ziti/elliott.json \
//	MOQOM_BYOC_CREDENTIAL=$(gcloud secrets versions access latest --secret=moqom-dev-admin-credential) \
//	moqom-byoc -tenant acme create -name "on-prem rack 3"
//
// create prints the one-time enrolment token, once. The control plane does not
// keep it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/moqom-cloud/moqom/go/internal/byocpb"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("moqom-byoc: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("moqom-byoc", flag.ContinueOnError)
	tenant := global.String("tenant", os.Getenv("MOQOM_BYOC_TENANT"), "the tenant")
	service := global.String("service", envOr("MOQOM_BYOC_SERVICE", "moqom-dev-api"), "the overlay API service")
	target := global.String("target", os.Getenv("MOQOM_BYOC_TARGET"), "a loopback forward to the API service, instead of dialling the overlay")
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 || *tenant == "" {
		return errors.New("usage: moqom-byoc -tenant T create -name N | list | revoke -id ID")
	}
	credential := os.Getenv("MOQOM_BYOC_CREDENTIAL")
	if credential == "" {
		return errors.New("MOQOM_BYOC_CREDENTIAL is required")
	}

	connection, closeAll, err := connect(*service, *target)
	if err != nil {
		return err
	}
	defer closeAll()
	client := byocpb.NewRelayEnrollmentsClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)

	command := flag.NewFlagSet(rest[0], flag.ContinueOnError)
	switch rest[0] {
	case "create":
		name := command.String("name", "", "a label for the relay")
		if err := command.Parse(rest[1:]); err != nil {
			return err
		}
		created, err := client.Create(ctx, &byocpb.CreateRequest{Tenant: *tenant, Name: *name})
		if err != nil {
			return err
		}
		enrollment := created.GetEnrollment()
		fmt.Fprintf(os.Stderr, "enrolment %s, identity %s, token valid until %s\n",
			enrollment.GetId(), enrollment.GetIdentityName(), created.GetTokenExpiresAt().AsTime().Format(time.RFC3339))
		// The token alone on stdout, so it can be piped straight to a file.
		fmt.Println(created.GetEnrollmentToken())
	case "list":
		listed, err := client.List(ctx, &byocpb.ListRequest{Tenant: *tenant})
		if err != nil {
			return err
		}
		for _, e := range listed.GetEnrollments() {
			state := "active"
			if e.GetRevokedAt() != nil {
				state = "revoked " + e.GetRevokedAt().AsTime().Format(time.RFC3339)
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", e.GetId(), e.GetIdentityName(), e.GetName(), state)
		}
	case "revoke":
		id := command.String("id", "", "the enrolment to revoke")
		if err := command.Parse(rest[1:]); err != nil {
			return err
		}
		revoked, err := client.Revoke(ctx, &byocpb.RevokeRequest{Tenant: *tenant, Id: *id})
		if err != nil {
			return err
		}
		fmt.Printf("revoked %s at %s\n", revoked.GetEnrollment().GetId(),
			revoked.GetEnrollment().GetRevokedAt().AsTime().Format(time.RFC3339))
	default:
		return fmt.Errorf("unknown command %q", rest[0])
	}
	return nil
}

// connect dials the API service over the overlay, or a loopback forward to it.
// Plaintext either way: the overlay's circuit is the encryption, and a forward
// is required to be on loopback.
func connect(service, target string) (*grpc.ClientConn, func(), error) {
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	closeOverlay := func() {}
	if target != "" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, nil, err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, nil, fmt.Errorf("-target must be a loopback forward, not %s", host)
		}
	} else {
		identity := os.Getenv("MOQOM_ZITI_IDENTITY")
		if identity == "" {
			return nil, nil, errors.New("MOQOM_ZITI_IDENTITY (an enrolled identity file) or -target is required")
		}
		config, err := ziti.NewConfigFromFile(strings.TrimSpace(identity))
		if err != nil {
			return nil, nil, err
		}
		overlay, err := ziti.NewContext(config)
		if err != nil {
			return nil, nil, err
		}
		closeOverlay = overlay.Close
		target = "passthrough:///" + service
		options = append(options, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return overlay.Dial(service)
		}))
	}
	connection, err := grpc.NewClient(target, options...)
	if err != nil {
		closeOverlay()
		return nil, nil, err
	}
	return connection, func() { _ = connection.Close(); closeOverlay() }, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
