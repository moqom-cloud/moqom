// Package byoctest is the behaviour every byoc.Store must have.
package byoctest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/byoc"
)

var epoch = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// Run runs the suite against a fresh store from each call to open.
func Run(t *testing.T, open func(t *testing.T) byoc.Store) {
	t.Run("RoundTrips", func(t *testing.T) { roundTrips(t, open(t)) })
	t.Run("IdentityNamesAreUnique", func(t *testing.T) { identityNamesAreUnique(t, open(t)) })
	t.Run("ListsOneTenantNewestFirst", func(t *testing.T) { listsOneTenantNewestFirst(t, open(t)) })
	t.Run("RevokingIsSticky", func(t *testing.T) { revokingIsSticky(t, open(t)) })
	t.Run("MissingIsNotFound", func(t *testing.T) { missingIsNotFound(t, open(t)) })
	t.Run("ActiveTenantsLeavesOutRevoked", func(t *testing.T) { activeTenants(t, open(t)) })
}

func activeTenants(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	create(t, store, enrollment("a1", "globex", "i1", epoch))
	create(t, store, enrollment("a2", "acme", "i2", epoch))
	create(t, store, enrollment("a3", "acme", "i3", epoch))
	create(t, store, enrollment("a4", "initech", "i4", epoch))
	if _, err := store.Revoke(ctx, "a4", epoch); err != nil {
		t.Fatal(err)
	}
	got, err := store.ActiveTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "acme,globex" {
		t.Errorf("active tenants %v, want [acme globex]", got)
	}
}

func enrollment(id, tenant, identity string, at time.Time) byoc.Enrollment {
	return byoc.Enrollment{
		ID: id, Tenant: tenant, Name: "relay " + id,
		IdentityID: "zid-" + id, IdentityName: identity,
		CreatedBy: "owner", CreatedAt: at,
	}
}

func create(t *testing.T, store byoc.Store, e byoc.Enrollment) {
	t.Helper()
	if err := store.Create(context.Background(), e); err != nil {
		t.Fatalf("creating %s: %v", e.ID, err)
	}
}

func roundTrips(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	want := enrollment("b1", "acme", "byoc-acme-b1", epoch)
	want.Pool = "pool-1"
	create(t, store, want)

	got, err := store.Get(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	if !same(got, want) {
		t.Errorf("Get returned %+v, want %+v", got, want)
	}
	byName, err := store.ByIdentity(ctx, "byoc-acme-b1")
	if err != nil || !same(byName, want) {
		t.Errorf("ByIdentity returned %+v (%v), want %+v", byName, err, want)
	}
	if !got.Active() {
		t.Error("a new enrolment is not active")
	}
}

func identityNamesAreUnique(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	create(t, store, enrollment("b1", "acme", "byoc-x", epoch))
	if err := store.Create(ctx, enrollment("b2", "globex", "byoc-x", epoch)); !errors.Is(err, byoc.ErrExists) {
		t.Errorf("a second enrolment for one identity gave %v, want ErrExists", err)
	}
	if err := store.Create(ctx, enrollment("b1", "acme", "byoc-y", epoch)); !errors.Is(err, byoc.ErrExists) {
		t.Errorf("a reused ID gave %v, want ErrExists", err)
	}
	// Revoked still holds the name: a name handed to one tenant is never
	// handed to another, so a stale identity cannot change hands.
	if _, err := store.Revoke(ctx, "b1", epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, enrollment("b3", "globex", "byoc-x", epoch)); !errors.Is(err, byoc.ErrExists) {
		t.Errorf("reusing a revoked identity's name gave %v, want ErrExists", err)
	}
}

func listsOneTenantNewestFirst(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	create(t, store, enrollment("b1", "acme", "i1", epoch))
	create(t, store, enrollment("b2", "acme", "i2", epoch.Add(2*time.Minute)))
	create(t, store, enrollment("b3", "globex", "i3", epoch.Add(time.Minute)))
	create(t, store, enrollment("b4", "acme", "i4", epoch.Add(time.Minute)))

	listed, err := store.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range listed {
		ids = append(ids, e.ID)
	}
	if len(ids) != 3 || ids[0] != "b2" || ids[1] != "b4" || ids[2] != "b1" {
		t.Errorf("acme lists %v, want [b2 b4 b1]", ids)
	}
	none, err := store.List(ctx, "initech")
	if err != nil || len(none) != 0 {
		t.Errorf("an empty tenant lists %v (%v)", none, err)
	}
}

func revokingIsSticky(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	create(t, store, enrollment("b1", "acme", "i1", epoch))
	first := epoch.Add(time.Hour)
	revoked, err := store.Revoke(ctx, "b1", first)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Active() || !revoked.RevokedAt.Equal(first) {
		t.Errorf("revoked as %+v", revoked)
	}
	again, err := store.Revoke(ctx, "b1", first.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.RevokedAt.Equal(first) {
		t.Errorf("a second revoke moved the time to %v", again.RevokedAt)
	}
	got, err := store.ByIdentity(ctx, "i1")
	if err != nil || got.Active() {
		t.Errorf("after revoking, ByIdentity gave %+v (%v)", got, err)
	}
}

func missingIsNotFound(t *testing.T, store byoc.Store) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "nope"); !errors.Is(err, byoc.ErrNotFound) {
		t.Errorf("Get gave %v", err)
	}
	if _, err := store.ByIdentity(ctx, "nope"); !errors.Is(err, byoc.ErrNotFound) {
		t.Errorf("ByIdentity gave %v", err)
	}
	if _, err := store.Revoke(ctx, "nope", epoch); !errors.Is(err, byoc.ErrNotFound) {
		t.Errorf("Revoke gave %v", err)
	}
}

func same(a, b byoc.Enrollment) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Name == b.Name && a.Pool == b.Pool &&
		a.IdentityID == b.IdentityID && a.IdentityName == b.IdentityName &&
		a.CreatedBy == b.CreatedBy && a.CreatedAt.Equal(b.CreatedAt) && a.RevokedAt.Equal(b.RevokedAt)
}
