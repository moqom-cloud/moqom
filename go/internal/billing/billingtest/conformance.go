// Package billingtest is the contract every billing.Store keeps, run against
// each implementation so the in-memory store cannot drift from Postgres.
package billingtest

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/billing"
)

// Run exercises a store. open returns an empty store per case.
func Run(t *testing.T, open func(t *testing.T) billing.Store) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tenant := func(id string) billing.Tenant {
		return billing.Tenant{ID: id, Name: "Example", Email: "dev@example.com",
			Status: billing.StatusPending, CreatedAt: at, UpdatedAt: at}
	}
	ctx := context.Background()

	t.Run("tenants", func(t *testing.T) {
		s := open(t)
		if _, err := s.CreateTenant(ctx, tenant("t_1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTenant(ctx, tenant("t_1")); !errors.Is(err, billing.ErrAlreadyExists) {
			t.Fatalf("duplicate tenant: %v", err)
		}
		if _, err := s.GetTenant(ctx, "t_missing"); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("missing tenant: %v", err)
		}
		saved := tenant("t_1")
		saved.Status, saved.StripeCustomer, saved.PaymentMethod = billing.StatusActive, "cus_1", "pm_1"
		saved.AutoTopUp = billing.AutoTopUp{Enabled: true, Below: 5 * billing.Dollar, Amount: 20 * billing.Dollar}
		if _, err := s.SaveTenant(ctx, saved); err != nil {
			t.Fatal(err)
		}
		got, err := s.FindTenantByStripeCustomer(ctx, "cus_1")
		if err != nil || got.ID != "t_1" || got.AutoTopUp != saved.AutoTopUp || got.Status != billing.StatusActive {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		if _, err := s.FindTenantByStripeCustomer(ctx, ""); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("empty customer matched: %v", err)
		}
		if _, err := s.SaveTenant(ctx, tenant("t_missing")); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("saving a missing tenant: %v", err)
		}
	})

	t.Run("counts tenants", func(t *testing.T) {
		s := open(t)
		if total, active, err := s.CountTenants(ctx); err != nil || total != 0 || active != 0 {
			t.Fatalf("empty store counted %d, %d, %v", total, active, err)
		}
		for _, id := range []string{"t_1", "t_2", "t_3", "t_4"} {
			if _, err := s.CreateTenant(ctx, tenant(id)); err != nil {
				t.Fatal(err)
			}
		}
		for id, status := range map[string]billing.Status{
			"t_1": billing.StatusActive, "t_2": billing.StatusActive, "t_3": billing.StatusDeleted,
		} {
			changed := tenant(id)
			changed.Status = status
			if _, err := s.SaveTenant(ctx, changed); err != nil {
				t.Fatal(err)
			}
		}
		total, active, err := s.CountTenants(ctx)
		if err != nil || total != 4 || active != 2 {
			t.Fatalf("counted %d total, %d active, %v; want 4, 2", total, active, err)
		}
	})

	t.Run("one top-up claim at a time", func(t *testing.T) {
		s := open(t)
		_, _ = s.CreateTenant(ctx, tenant("t_1"))
		var wins int
		var mu sync.Mutex
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if won, err := s.ClaimTopUp(ctx, "t_1", at, time.Hour); err == nil && won {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d concurrent claims won, want 1", wins)
		}
		if won, _ := s.ClaimTopUp(ctx, "t_1", at.Add(2*time.Hour), time.Hour); !won {
			t.Fatal("a stale claim was not reclaimable")
		}
		if _, err := s.ClaimTopUp(ctx, "t_missing", at, time.Hour); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("claim on a missing tenant: %v", err)
		}
	})

	t.Run("ledger is idempotent and sums", func(t *testing.T) {
		s := open(t)
		_, _ = s.CreateTenant(ctx, tenant("t_1"))
		_, _ = s.CreateTenant(ctx, tenant("t_2"))
		entry := func(id, key string, amount billing.Micros) billing.Entry {
			return billing.Entry{ID: id, Tenant: "t_1", Kind: billing.KindTopUp, Amount: amount,
				IdempotencyKey: key, Reference: "pi_1", At: at}
		}
		var wg sync.WaitGroup
		created := make(chan bool, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, made, err := s.AppendEntry(ctx, entry("e1", "stripe-pi:pi_1", 50*billing.Dollar))
				if err != nil {
					t.Error(err)
				}
				created <- made
			}()
		}
		wg.Wait()
		close(created)
		var made int
		for c := range created {
			if c {
				made++
			}
		}
		if made != 1 {
			t.Fatalf("%d concurrent appends created an entry, want 1", made)
		}
		_, _, _ = s.AppendEntry(ctx, billing.Entry{ID: "e2", Tenant: "t_1", Kind: billing.KindUsage,
			Amount: -214, Cost: 177, Capped: true, IdempotencyKey: "usage:1", At: at.Add(time.Second)})
		_, _, _ = s.AppendEntry(ctx, billing.Entry{ID: "e3", Tenant: "t_2", Kind: billing.KindTopUp,
			Amount: billing.Dollar, IdempotencyKey: "other", At: at})

		if balance, err := s.Balance(ctx, "t_1"); err != nil || balance != 50*billing.Dollar-214 {
			t.Fatalf("balance %v %v", balance, err)
		}
		if balance, _ := s.Balance(ctx, "t_none"); balance != 0 {
			t.Fatalf("empty balance %v", balance)
		}
		found, err := s.FindEntry(ctx, "usage:1")
		if err != nil || found.Cost != 177 || !found.Capped || found.Amount != -214 {
			t.Fatalf("find %+v %v", found, err)
		}
		recent, _ := s.ListEntries(ctx, "t_1", 1)
		if len(recent) != 1 || recent[0].IdempotencyKey != "usage:1" {
			t.Fatalf("newest first: %+v", recent)
		}
		if all, _ := s.ListEntries(ctx, "t_1", 0); len(all) != 2 {
			t.Fatalf("all entries: %d", len(all))
		}
		if _, err := s.FindEntry(ctx, "nope"); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("missing entry: %v", err)
		}
	})

	t.Run("usage breakdown groups a range by role, tier and hosting", func(t *testing.T) {
		s := open(t)
		_, _ = s.CreateTenant(ctx, tenant("t_1"))
		_, _ = s.CreateTenant(ctx, tenant("t_2"))
		usage := func(tenant, key string, amount billing.Micros, capped bool, offset time.Duration, detail string) {
			if _, _, err := s.AppendEntry(ctx, billing.Entry{ID: key, Tenant: tenant, Kind: billing.KindUsage,
				Amount: -amount, Capped: capped, IdempotencyKey: key, Detail: detail, At: at.Add(offset)}); err != nil {
				t.Fatal(err)
			}
		}
		hd := `{"egress_bytes":1000,"hosting":"cloud","ingress_bytes":10,"role":"host","seconds":60,"tier":"hd"}`
		usage("t_1", "u1", 100, false, 0, hd)
		usage("t_1", "u2", 300, true, time.Minute, hd)
		usage("t_1", "u3", 50, false, 2*time.Minute,
			`{"egress_bytes":7,"hosting":"self_hosted","ingress_bytes":0,"role":"broadcast_audience","seconds":30,"tier":"audio"}`)
		usage("t_1", "u4", 999, false, -time.Second, hd) // before the range
		usage("t_1", "u5", 999, false, time.Hour, hd)    // at its end, excluded
		usage("t_2", "u6", 999, false, time.Minute, hd)  // another tenant
		_, _, _ = s.AppendEntry(ctx, billing.Entry{ID: "a1", Tenant: "t_1", Kind: billing.KindAdjustment,
			Amount: -5000, IdempotencyKey: "a1", Detail: `{"reason":"x"}`, At: at})

		got, err := s.UsageBreakdown(ctx, "t_1", at, at.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := []billing.UsageBucket{
			{Role: billing.RoleBroadcastAudience, Tier: billing.TierAudio, Hosting: billing.HostingSelf,
				Records: 1, Seconds: 30, EgressBytes: 7, Charged: 50},
			{Role: billing.RoleHost, Tier: billing.TierHD, Hosting: billing.HostingCloud,
				Records: 2, Seconds: 120, EgressBytes: 2000, IngressBytes: 20, Charged: 400, Capped: 1},
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("bucket %d: got %+v, want %+v", i, got[i], want[i])
			}
		}
		if empty, err := s.UsageBreakdown(ctx, "t_none", at, at.Add(time.Hour)); err != nil || len(empty) != 0 {
			t.Fatalf("unknown tenant: %+v %v", empty, err)
		}
	})

	t.Run("keys", func(t *testing.T) {
		s := open(t)
		_, _ = s.CreateTenant(ctx, tenant("t_1"))
		key := billing.Key{ID: "k1", Tenant: "t_1", Name: "ci", Kind: billing.KeyRelay, Hash: "h", CreatedAt: at}
		if err := s.PutKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := s.PutKey(ctx, key); !errors.Is(err, billing.ErrAlreadyExists) {
			t.Fatalf("duplicate key: %v", err)
		}
		if err := s.RevokeKey(ctx, "t_other", "k1", at); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("revoked another tenant's key: %v", err)
		}
		if err := s.RevokeKey(ctx, "t_1", "k1", at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		_ = s.RevokeKey(ctx, "t_1", "k1", at.Add(time.Hour))
		got, _ := s.GetKey(ctx, "k1")
		if !got.RevokedAt.Equal(at.Add(time.Minute)) || got.Kind != key.Kind {
			t.Fatalf("revocation moved or kind lost: %+v", got)
		}
		if err := s.RevokeKey(ctx, "t_1", "k1", at); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetKey(ctx, "k1"); !got.RevokedAt.Equal(at) {
			t.Fatalf("a scheduled revocation was not brought forward: %+v", got)
		}
		if keys, _ := s.ListKeys(ctx, "t_1"); len(keys) != 1 {
			t.Fatalf("list %d", len(keys))
		}
	})

	t.Run("key hardening fields round trip", func(t *testing.T) {
		s := open(t)
		_, _ = s.CreateTenant(ctx, tenant("t_1"))
		key := billing.Key{ID: "k2", Tenant: "t_1", Name: "agent", Kind: billing.KeyMint, Hash: "h1",
			PepperVersion: "", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour), RateLimit: 120,
			AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}}
		if err := s.PutKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetKey(ctx, "k2")
		if err != nil || got.Kind != billing.KeyMint || !got.ExpiresAt.Equal(key.ExpiresAt) || got.RateLimit != 120 ||
			len(got.AllowedNetworks) != 2 || got.AllowedNetworks[0] != key.AllowedNetworks[0] ||
			got.AllowedNetworks[1] != key.AllowedNetworks[1] || got.PepperVersion != "" {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		if err := s.RehashKey(ctx, "k2", "h1", "h2", "v2"); err != nil {
			t.Fatal(err)
		}
		// A second replica rehashing from the old value loses, quietly.
		if err := s.RehashKey(ctx, "k2", "h1", "h3", "v3"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetKey(ctx, "k2"); got.Hash != "h2" || got.PepperVersion != "v2" {
			t.Fatalf("rehash: %+v", got)
		}
		if err := s.RehashKey(ctx, "k_missing", "a", "b", "c"); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("rehash of a missing key: %v", err)
		}
		plain := billing.Key{ID: "k3", Tenant: "t_1", Name: "plain", Kind: billing.KeyBackend, Hash: "h", CreatedAt: at}
		_ = s.PutKey(ctx, plain)
		if got, _ := s.GetKey(ctx, "k3"); !got.ExpiresAt.IsZero() || len(got.AllowedNetworks) != 0 || got.RateLimit != 0 {
			t.Fatalf("defaults: %+v", got)
		}
	})

	t.Run("key use is merged per minute and listed newest first", func(t *testing.T) {
		s := open(t)
		minute := at.Truncate(time.Minute)
		use := func(key, tenant, outcome string, count int64, first, last time.Duration) billing.KeyUse {
			return billing.KeyUse{Key: key, Tenant: tenant, Minute: minute, Operation: "GET /v1/keys",
				Address: "203.0.113.9", Outcome: outcome, Count: count, FirstAt: at.Add(first), LastAt: at.Add(last)}
		}
		if err := s.RecordKeyUse(ctx, []billing.KeyUse{
			use("k1", "t_1", "ok", 3, 0, time.Second),
			use("k1", "t_1", "rate_limited", 1, 2*time.Second, 2*time.Second),
			use("k9", "t_2", "ok", 1, 0, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordKeyUse(ctx, []billing.KeyUse{use("k1", "t_1", "ok", 2, -time.Second, 3*time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordKeyUse(ctx, nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListKeyUse(ctx, "t_1", "", 0)
		if err != nil || len(got) != 2 {
			t.Fatalf("list: %+v %v", got, err)
		}
		if got[0].Outcome != "ok" || got[0].Count != 5 || !got[0].FirstAt.Equal(at.Add(-time.Second)) ||
			!got[0].LastAt.Equal(at.Add(3*time.Second)) || !got[0].Minute.Equal(minute) {
			t.Fatalf("merged entry: %+v", got[0])
		}
		if got[1].Outcome != "rate_limited" || got[1].Count != 1 {
			t.Fatalf("second entry: %+v", got[1])
		}
		if one, _ := s.ListKeyUse(ctx, "t_1", "k1", 1); len(one) != 1 || one[0].Outcome != "ok" {
			t.Fatalf("limit: %+v", one)
		}
		if other, _ := s.ListKeyUse(ctx, "t_1", "k9", 0); len(other) != 0 {
			t.Fatalf("another tenant's key use: %+v", other)
		}
	})

	t.Run("jobs", func(t *testing.T) {
		s := open(t)
		job := billing.Job{ID: "j1", Tenant: "t_1", Status: billing.JobRunning, Current: billing.StepCreateBucket,
			Completed: []billing.Step{billing.StepRegisterTenant}, CreatedAt: at}
		if err := s.SaveJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		job.Status, job.CompletedAt = billing.JobCompleted, at.Add(time.Second)
		job.Completed = append(job.Completed, billing.StepCreateBucket)
		if err := s.SaveJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, "j1")
		if err != nil || got.Status != billing.JobCompleted || len(got.Completed) != 2 || !got.CompletedAt.Equal(job.CompletedAt) {
			t.Fatalf("job %+v %v", got, err)
		}
	})

	t.Run("direct seconds are claimed once per key, per tenant and month", func(t *testing.T) {
		s := open(t)
		october := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		november := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		claim := func(tenant string, month time.Time, key string, seconds int64) int64 {
			t.Helper()
			before, err := s.ClaimDirectSeconds(ctx, tenant, month, key, seconds)
			if err != nil {
				t.Fatal(err)
			}
			return before
		}
		if got := claim("t_1", october, "k1", 600); got != 0 {
			t.Fatalf("first claim began at %d", got)
		}
		// A time inside the month names the month.
		if got := claim("t_1", october.Add(20*24*time.Hour), "k2", 60); got != 600 {
			t.Fatalf("second claim began at %d, want 600", got)
		}
		if got := claim("t_1", november, "k1", 600); got != 0 {
			t.Fatalf("a re-claimed key began at %d, want where it first did", got)
		}
		if got := claim("t_1", october, "k3", 1); got != 660 {
			t.Fatalf("a re-claim was counted: next began at %d, want 660", got)
		}
		if got := claim("t_1", november, "k4", 30); got != 0 {
			t.Fatalf("November began at %d", got)
		}
		if got := claim("t_2", october, "k5", 30); got != 0 {
			t.Fatalf("another tenant began at %d", got)
		}

		// Replicas claiming at once see distinct totals that add up.
		var wg sync.WaitGroup
		befores := make([]int64, 20)
		for i := range befores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				befores[i] = claim("t_3", october, fmt.Sprintf("c%d", i%10), 10)
			}()
		}
		wg.Wait()
		seen := map[int64]bool{}
		for _, before := range befores {
			seen[before] = true
		}
		if len(seen) != 10 || claim("t_3", october, "after", 1) != 100 {
			t.Fatalf("concurrent claims began at %v", befores)
		}
	})
}
