package main

import (
	"context"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/apple"
)

// appleVerifier checks Sign in with Apple identity tokens issued to one of
// this deployment's bundle IDs.
type appleVerifier struct {
	audiences []string
	inner     *apple.IdentityVerifier
}

func newAppleVerifier(audiences []string) *appleVerifier {
	return &appleVerifier{audiences: audiences, inner: apple.NewIdentityVerifier()}
}

func (v *appleVerifier) verify(ctx context.Context, token string, now time.Time) (apple.Identity, error) {
	return v.inner.Verify(ctx, token, v.audiences, now)
}
