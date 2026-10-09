package main

import (
	"context"
	"log/slog"

	"github.com/moqom-cloud/moqom/go/internal/applesignin"
	applepg "github.com/moqom-cloud/moqom/go/internal/applesignin/postgres"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/postgres"
)

// appleSignIn turns on Apple sign-in hosted on the public listener, which
// seals the enrollments it hands out and so needs seal keys.
func appleSignIn(backing store.Store, sealing bool) (*server.AppleSignInConfig, error) {
	if !sealing {
		slog.Info("Apple sign-in off", "reason", "no seal keys")
		return nil, nil
	}
	if subject, ok := backing.(*postgres.Store); ok {
		durable, err := applepg.New(context.Background(), subject.Pool())
		if err != nil {
			return nil, err
		}
		return &server.AppleSignInConfig{Settings: durable}, nil
	}
	slog.Warn("Apple sign-in settings in memory", "consequence", "they and App Attest counters are lost on restart")
	return &server.AppleSignInConfig{Settings: applesignin.NewMemory()}, nil
}
