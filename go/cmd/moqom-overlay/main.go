// Command moqom-overlay forwards a local TCP port to a service on the OpenZiti
// overlay, as an enrolled identity.
//
// Ziti Desktop Edge is the ordinary way an operator reaches a dark service:
// it intercepts a name like postgres.moqom-dev.ziti and needs nothing else.
// This is for where Desktop Edge is not installed -- a CI job, a container, a
// second identity on the same laptop -- and for reaching one relay among many,
// which is a dial to a named terminator:
//
//	moqom-overlay -identity me.json -service moqom-dev-relay-admin \
//	    -terminator moqom-dev-relay-us-west1-0jml.relays.moqom-dev.ziti -listen 127.0.0.1:8082
//
// The local port is plain TCP on loopback; anyone on the machine who can
// reach it is, for the overlay, this identity.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/openziti/sdk-golang/ziti"
)

func main() {
	identity := flag.String("identity", os.Getenv("MOQOM_ZITI_IDENTITY"), "enrolled identity file")
	service := flag.String("service", "", "overlay service to dial")
	terminator := flag.String("terminator", "", "the hosting identity to reach, for a service many hosts bind")
	address := flag.String("listen", "127.0.0.1:0", "local address to accept on; loopback only")
	flag.Parse()

	if err := run(*identity, *service, *terminator, *address); err != nil {
		fmt.Fprintln(os.Stderr, "moqom-overlay:", err)
		os.Exit(1)
	}
}

func run(identity, service, terminator, address string) error {
	if identity == "" || service == "" {
		return errors.New("-identity and -service are required")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on %s: the forward is as good as the identity, so it stays on loopback", host)
	}
	config, err := ziti.NewConfigFromFile(identity)
	if err != nil {
		return err
	}
	overlay, err := ziti.NewContext(config)
	if err != nil {
		return err
	}
	defer overlay.Close()

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	// Printed on stdout, alone, so a script can read the port it was given.
	fmt.Println(listener.Addr().String())
	slog.Info("forwarding", "listen", listener.Addr().String(), "service", service, "terminator", terminator)

	options := &ziti.DialOptions{ConnectTimeout: 15 * time.Second, Identity: terminator}
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer local.Close()
			remote, err := overlay.DialWithOptions(service, options)
			if err != nil {
				slog.Warn("dial failed", "service", service, "error", err)
				return
			}
			defer remote.Close()
			pipe(local, remote)
		}()
	}
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeBoth() }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeBoth() }()
	wg.Wait()
}
