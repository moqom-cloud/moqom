package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCloudflarePriceIsFreeUpToTheAllowanceThenTheSelfHostedFee(t *testing.T) {
	pricing := DefaultPricing()
	allowance := pricing.CloudflareFreeMinutesPerMonth * 60
	for _, c := range []struct {
		name            string
		before, seconds int64
		want            Micros
	}{
		{"inside the allowance", 0, 60, 0},
		{"ending on it", allowance - 60, 60, 0},
		{"past it", allowance, 60, 1000},
		{"straddling it", allowance - 30, 60, 500},
		{"bytes with no time", allowance, 0, 1000},
	} {
		if got := pricing.CloudflarePrice(1000, c.before, c.seconds); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	// The self-hosted fee, with nothing for cost or markup: MOQOM paid for
	// none of it.
	charge := pricing.Rate(Usage{Role: RoleBroadcastAudience, Tier: TierFullHD, Seconds: 60, EgressBytes: mebibyte, Hosting: HostingCloudflare})
	self := pricing.Rate(Usage{Role: RoleBroadcastAudience, Tier: TierFullHD, Seconds: 60, EgressBytes: mebibyte, Hosting: HostingSelf})
	if charge != self || charge.Cost != 0 {
		t.Fatalf("cloudflare %+v, self-hosted %+v", charge, self)
	}
}

func TestCloudflareUsageIsFreeInTheTrialThenMeteredAfterTheAllowance(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.now = time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 10_000)
	reporter := Reporter{Tenant: tenant, Hosting: HostingCloudflare}
	gib := int64(1 << 30)
	record := func(id string, seconds, bytes int64) UsageRecord {
		return UsageRecord{ID: id, Room: "stage", Participant: "ada/0123456789abcdef",
			Role: RoleBroadcastAudience, Tier: TierFullHD, Seconds: seconds, EgressBytes: bytes}
	}
	charge := func(r UsageRecord) UsageResult {
		t.Helper()
		result, err := f.service.RecordUsage(ctx, reporter, []UsageRecord{r})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	balance := f.balance(t, tenant)
	if got := charge(record("oct", 86_400, 100*gib)); got.Charged != 1 || got.Total != 0 {
		t.Fatalf("a day in the October trial: %+v", got)
	}
	if f.balance(t, tenant) != balance {
		t.Fatal("the trial moved the balance")
	}

	// November: the trial's day didn't use any of the allowance.
	f.now = time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	if got := charge(record("n1", 60_000, gib)); got.Total != 0 {
		t.Fatalf("the first 1,000 minutes cost %s", got.Total)
	}
	if got := charge(record("n2", 60, mebibyte)); got.Total != 49 {
		t.Fatalf("a MiB past the allowance cost %s, want the 49-micro self-hosted fee", got.Total)
	}
	if got := charge(record("n2", 60, mebibyte)); got.Total != 0 || got.Duplicates != 1 {
		t.Fatalf("a re-sent record: %+v", got)
	}
	if got := charge(record("n3", 60, mebibyte)); got.Total != 49 {
		t.Fatalf("the re-send counted against the allowance: %s", got.Total)
	}

	direct := record("d", 60, 0)
	direct.DirectSeconds = 60
	if _, err := f.service.RecordUsage(ctx, reporter, []UsageRecord{direct}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("direct time from Cloudflare: %v", err)
	}
	// The allowance is Cloudflare's own: direct time is counted apart.
	if before, _ := f.service.store.ClaimDirectSeconds(ctx, tenant, f.now, "probe", 1); before != 0 {
		t.Fatalf("Cloudflare minutes used %d direct seconds", before)
	}
}

func TestCloudflareHostingNeedsATenantsRelayKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	f.paid(t, signup.Tenant.ID, "pi_first", 1000)
	_, relayKey, err := f.service.IssueKey(ctx, signup.Tenant.ID, "cloudflare-reporter", KeyRelay)
	if err != nil {
		t.Fatal(err)
	}
	handler := f.service.Handler(nil)
	post := func(hosting string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"hosting": hosting, "records": []map[string]any{{
			"id": "r-" + hosting, "role": "audience", "tier": "hd", "seconds": 60, "egress_bytes": 1 << 20,
		}}})
		request := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+relayKey)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var response map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		return recorder.Code, response
	}
	if code, body := post("cloudflare"); code != http.StatusOK || body["charged"] != float64(1) {
		t.Fatalf("cloudflare report: %d %v", code, body)
	}
	if code, _ := post("cloud"); code != http.StatusBadRequest {
		t.Fatalf("a relay key claiming MOQOM's own relays: %d", code)
	}
}
