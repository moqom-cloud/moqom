package billing

import (
	"context"
	"testing"
	"time"
)

func TestDirectFeeIsFreeUpToTheAllowanceThenPerMinute(t *testing.T) {
	pricing := DefaultPricing()
	allowance := pricing.DirectFreeMinutesPerMonth * 60
	for _, c := range []struct {
		name            string
		before, seconds int64
		want            Micros
	}{
		{"inside the allowance", 0, allowance, 0},
		{"a minute past it", allowance, 60, 500},
		{"straddling it", allowance - 30, 60, 250},
		{"a thousand minutes past it", allowance, 60_000, 50 * Cent},
		{"nothing", allowance, 0, 0},
	} {
		if got := pricing.DirectFee(c.before, c.seconds); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	// However a month is cut into records, it costs what its total does.
	var split Micros
	used := int64(0)
	for _, seconds := range []int64{allowance - 7, 13, 1, 59, 3600, 7} {
		split += pricing.DirectFee(used, seconds)
		used += seconds
	}
	if whole := pricing.DirectFee(0, used); split != whole {
		t.Fatalf("records cost %s, their total %s", split, whole)
	}
}

func TestDirectMinutesAreFreeEachMonthThenChargedOnceEach(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.now = time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 10_000)

	// What a standby relay session costs on its own, so what each record
	// adds over it is the direct fee alone.
	standby := func(id string, direct int64) UsageRecord {
		return UsageRecord{ID: id, Tenant: tenant, Room: "call", Participant: "ada/0123456789abcdef",
			Role: RoleHost, Tier: TierAudio, Seconds: 60, DirectSeconds: direct}
	}
	base, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, []UsageRecord{standby("base", 0)})
	if err != nil || base.Charged != 1 {
		t.Fatalf("standby %+v %v", base, err)
	}
	fee := func(record UsageRecord) (Micros, int) {
		t.Helper()
		result, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, []UsageRecord{record})
		if err != nil {
			t.Fatal(err)
		}
		if result.Charged == 0 {
			return 0, result.Duplicates
		}
		return result.Total - base.Total, result.Duplicates
	}

	if got, _ := fee(standby("d1", 60_000)); got != 0 {
		t.Fatalf("the first 1,000 minutes cost %s", got)
	}
	if got, _ := fee(standby("d2", 600)); got != 5000 {
		t.Fatalf("ten minutes past the allowance cost %s, want half a cent", got)
	}
	balance := f.balance(t, tenant)
	if got, duplicates := fee(standby("d2", 600)); got != 0 || duplicates != 1 || f.balance(t, tenant) != balance {
		t.Fatalf("a re-sent record charged %s again", got)
	}
	if got, _ := fee(standby("d3", 600)); got != 5000 {
		t.Fatalf("the re-send counted against the allowance: the next ten minutes cost %s", got)
	}

	// A day past the allowance in one record is far over what the Agora cap
	// allows a minute of relaying, and is charged in full: the cap is a
	// ceiling on relaying, not on direct time.
	if got, _ := fee(standby("d4", 86_400)); got != 720_000 {
		t.Fatalf("a day of direct time cost %s, want $0.72", got)
	}

	// November starts a fresh allowance, and a record re-sent across the
	// boundary is still the one already charged.
	f.now = time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	if got, duplicates := fee(standby("d3", 600)); got != 0 || duplicates != 1 {
		t.Fatalf("a re-send across the month boundary charged %s", got)
	}
	if got, _ := fee(standby("n1", 600)); got != 0 {
		t.Fatalf("November's first ten minutes cost %s", got)
	}
}
