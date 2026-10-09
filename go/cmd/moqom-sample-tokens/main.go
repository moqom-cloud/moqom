// Command moqom-sample-tokens is the token server MOQOM's own sample app signs
// in to, and the reference for the one every app runs on its own backend.
//
// A phone never holds anything that mints tokens. It signs in with Apple, with
// the request's nonce set to its device key's thumbprint, and posts the identity
// token here with that key. This checks Apple's signature and the nonce, then
// mints a token bound to the key through the admin API, with a credential that
// only this server holds. What the phone keeps is an enrollment: a sealed note
// that the person Apple named may have tokens for that one key, which it trades
// for a fresh token before each one expires.
//
//	POST /v1/sign-in  {"identityToken", "key", "room"}            -> token + enrollment
//	POST /v1/token    Bearer <enrollment>, {"key", "room"}         -> token
//
// Rooms are a fixed list made ahead of time. A phone cannot create one, and the
// tokens carry publish rights and nothing else: no moderation, no ownership.
//
// Environment:
//
//	PORT                           where to listen (Cloud Run sets it)
//	MOQOM_TOKENS_TARGET            the control plane's admin address, host:port
//	MOQOM_TOKENS_CREDENTIAL        the service account credential minted with
//	MOQOM_TOKENS_TENANT            its tenant (default moqom)
//	MOQOM_TOKENS_AUDIENCES         the app's bundle IDs, comma separated
//	MOQOM_TOKENS_ROOMS             rooms a phone may ask for, comma separated
//	MOQOM_TOKENS_ENROLLMENT_KEY    32 bytes, base64url, that seal enrollments
//	MOQOM_ADMIN_CA, _CERT, _KEY    files for the admin port's mutual TLS
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"
	"github.com/moqom-cloud/moqom/go/internal/admintls"
)

func main() {
	if err := run(); err != nil {
		slog.Error("moqom-sample-tokens", "error", err)
		os.Exit(1)
	}
}

func run() error {
	target := os.Getenv("MOQOM_TOKENS_TARGET")
	credential := os.Getenv("MOQOM_TOKENS_CREDENTIAL")
	if target == "" || credential == "" {
		return errors.New("MOQOM_TOKENS_TARGET and MOQOM_TOKENS_CREDENTIAL are required")
	}
	audiences := list(os.Getenv("MOQOM_TOKENS_AUDIENCES"))
	if len(audiences) == 0 {
		return errors.New("MOQOM_TOKENS_AUDIENCES is required: the app's bundle IDs")
	}
	rooms := list(os.Getenv("MOQOM_TOKENS_ROOMS"))
	if len(rooms) == 0 {
		return errors.New("MOQOM_TOKENS_ROOMS is required: the rooms a phone may join")
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(os.Getenv("MOQOM_TOKENS_ENROLLMENT_KEY"), "=\n"))
	if err != nil {
		return fmt.Errorf("MOQOM_TOKENS_ENROLLMENT_KEY: %w", err)
	}
	sealer, err := newEnrollmentSealer(secret)
	if err != nil {
		return fmt.Errorf("MOQOM_TOKENS_ENROLLMENT_KEY: %w", err)
	}

	tenant := value("MOQOM_TOKENS_TENANT", "moqom")
	options := []moqom.Option{
		moqom.WithCredential(credential),
		moqom.WithTenant(tenant),
	}
	transport, err := admintls.Credentials()
	if err != nil {
		return err
	}
	if transport == nil {
		return errors.New("MOQOM_ADMIN_CA, MOQOM_ADMIN_CERT and MOQOM_ADMIN_KEY are required")
	}
	options = append(options, moqom.WithTLS(transport))
	client, err := moqom.Dial(target, options...)
	if err != nil {
		return fmt.Errorf("dialling %s: %w", target, err)
	}
	defer client.Close()

	s := newServer(client, newAppleVerifier(audiences), sealer, tenant, rooms)
	httpServer := &http.Server{
		Addr:              ":" + value("PORT", "8080"),
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = httpServer.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", httpServer.Addr, "rooms", rooms, "audiences", audiences)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func list(raw string) []string {
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func value(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
