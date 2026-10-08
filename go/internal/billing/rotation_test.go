package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRotationKeepsBothKeysWorkingThroughTheOverlap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, old := signup.Tenant.ID, signup.Key

	issued, plaintext, retires, err := f.service.RotateKey(ctx, tenant, old.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Name != old.Name || issued.Kind != old.Kind || !retires.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("replacement %+v retiring %v", issued, retires)
	}
	if _, err := ResolveKey(ctx, f.store, plaintext); err != nil {
		t.Fatalf("the replacement does not work: %v", err)
	}
	stored, _ := f.store.GetKey(ctx, old.ID)
	if stored.RevokedAsOf(f.now) || !stored.Retiring(f.now) {
		t.Fatalf("the old key stopped before the overlap ended: %+v", stored)
	}
	if !stored.RevokedAsOf(f.now.Add(time.Hour)) {
		t.Fatalf("the old key outlived the overlap: %+v", stored)
	}

	if _, _, _, err := f.service.RotateKey(ctx, tenant, old.ID, MaxRotationOverlap+time.Second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an overlap past the maximum was accepted: %v", err)
	}
	if _, _, _, err := f.service.RotateKey(ctx, "t_other", old.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant rotated this key: %v", err)
	}
}

func TestRevokingTheLastWorkingKeyNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	tenant, only := signup.Tenant.ID, signup.Key

	if err := f.service.RevokeKey(ctx, tenant, only.ID, false); !errors.Is(err, ErrWouldStopService) {
		t.Fatalf("revoked the last backend key without confirmation: %v", err)
	}

	second, _, err := f.service.IssueKey(ctx, tenant, "second", KeyBackend)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.RevokeKey(ctx, tenant, only.ID, false); err != nil {
		t.Fatalf("refused although another key still works: %v", err)
	}
	if err := f.service.RevokeKey(ctx, tenant, second.ID, false); !errors.Is(err, ErrWouldStopService) {
		t.Fatalf("revoked the new last key without confirmation: %v", err)
	}
	if err := f.service.RevokeKey(ctx, tenant, second.ID, true); err != nil {
		t.Fatalf("confirmed revocation refused: %v", err)
	}
}
