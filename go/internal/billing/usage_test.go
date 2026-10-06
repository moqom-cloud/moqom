package billing

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAgoraLessOneCentIsTheListLessOneCent(t *testing.T) {
	list, ceiling := AgoraListPrice(), AgoraLessOneCent()
	for role, tiers := range list {
		for tier, rate := range tiers {
			if ceiling[role][tier] != rate-Cent {
				t.Errorf("%s %s: ceiling %s, list %s", role, tier, ceiling[role][tier], rate)
			}
		}
	}
	if list[RoleHost][TierHD] != 399*Cent || ceiling[RoleBroadcastAudience][Tier2KPlus] != 1798*Cent {
		t.Fatalf("rates moved: %s, %s", list[RoleHost][TierHD], ceiling[RoleBroadcastAudience][Tier2KPlus])
	}
}

func microsOf(t *testing.T, value any) Micros {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("not money: %v", value)
	}
	return Micros(m["micros"].(float64))
}

func TestUsageSummaryIsTheLedgerBesideAgorasList(t *testing.T) {
	ctx := context.Background()
	f := newOperatorFixture(t)
	acme, globex := f.signup(t, "Acme"), f.signup(t, "Globex")
	f.paid(t, acme.Tenant.ID, "pi_acme", 5000)
	f.paid(t, globex.Tenant.ID, "pi_globex", 5000)

	cloud := Reporter{Hosting: HostingCloud}
	charged, err := f.service.RecordUsage(ctx, cloud, []UsageRecord{
		{ID: "r1", Tenant: acme.Tenant.ID, Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
		{ID: "r2", Tenant: acme.Tenant.ID, Role: RoleAudience, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
		{ID: "r3", Tenant: acme.Tenant.ID, Role: RoleBroadcastAudience, Tier: TierFullHD, Seconds: 120, EgressBytes: 10 << 30},
		{ID: "r4", Tenant: globex.Tenant.ID, Role: RoleHost, Tier: Tier2KPlus, Seconds: 600, EgressBytes: 10 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, relayKey, err := f.service.IssueKey(ctx, acme.Tenant.ID, "edge", KeyRelay)
	if err != nil {
		t.Fatal(err)
	}
	self, err := f.service.RecordUsage(ctx, Reporter{Tenant: acme.Tenant.ID, Hosting: HostingSelf}, []UsageRecord{
		{ID: "s1", Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
	})
	if err != nil {
		t.Fatal(err)
	}
	globexCharge := DefaultPricing().Rate(Usage{Role: RoleHost, Tier: Tier2KPlus, Seconds: 600, EgressBytes: 10 << 30}).Price
	acmeCharged := charged.Total - globexCharge + self.Total

	// f.now is 2027-01-15 08:00 UTC: two weeks and eight hours into January.
	path := "/v1/usage/summary?from=2027-01-01&to=2027-02-01"
	recorder, body := serve(f.tenants, "GET", path, acme.APIKey, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	total := body["total"].(map[string]any)
	if got := microsOf(t, total["charged"]); got != acmeCharged {
		t.Errorf("charged %s, want the ledger's %s", got, acmeCharged)
	}
	// Hosts and interactive audience at $3.99, broadcast audience at $4.59,
	// self-hosted usage compared like any other.
	wantAgora := Per1000Minutes(399*Cent, 60)*3 + Per1000Minutes(459*Cent, 120)
	if got := microsOf(t, total["agora_list"]); got != wantAgora {
		t.Errorf("agora %s, want %s", got, wantAgora)
	}
	if got := microsOf(t, total["savings"]); got != wantAgora-acmeCharged {
		t.Errorf("savings %s", got)
	}
	if total["seconds"].(float64) != 300 || total["records"].(float64) != 4 || total["capped_records"].(float64) != 1 {
		t.Errorf("total %v", total)
	}
	if lines := body["lines"].([]any); len(lines) != 4 {
		t.Errorf("lines %v", lines)
	}
	if tiers := body["by_tier"].([]any); len(tiers) != 2 || tiers[0].(map[string]any)["tier"] != "hd" {
		t.Errorf("by tier %v", tiers)
	}
	if roles := body["by_role"].([]any); len(roles) != 3 {
		t.Errorf("by role %v", roles)
	}
	elapsed := (14*24 + 8) * time.Hour
	if body["elapsed_seconds"].(float64) != elapsed.Seconds() {
		t.Errorf("elapsed %v", body["elapsed_seconds"])
	}
	month := 31 * 24 * time.Hour
	projected := body["projection_period"].(map[string]any)
	if got, want := microsOf(t, projected["charged"]), extrapolate(acmeCharged, elapsed, month); got != want {
		t.Errorf("projected %s, want %s", got, want)
	}
	if _, ok := body["projection_30_days"]; !ok {
		t.Error("no 30-day projection")
	}
	if strings.Contains(recorder.Body.String(), "2k_plus") || strings.Contains(recorder.Body.String(), `"cost"`) {
		t.Errorf("leaked another tenant's usage or MOQOM's cost: %s", recorder.Body)
	}

	// A finished period has no period projection.
	if _, body := serve(f.tenants, "GET", "/v1/usage/summary?from=2027-01-01T00:00:00Z&to=2027-01-15T00:00:00Z", acme.APIKey, nil); body["projection_period"] != nil || body["total"].(map[string]any)["records"].(float64) != 0 {
		t.Errorf("finished period: %v", body)
	}
	for name, credential := range map[string]string{"no key": "", "a relay key": relayKey, "an operator token": f.token} {
		if recorder, _ := serve(f.tenants, "GET", path, credential, nil); recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
	for _, query := range []string{"", "?from=2027-01-01", "?from=2027-02-01&to=2027-01-01", "?from=2025-01-01&to=2027-01-01", "?from=yesterday&to=2027-01-01"} {
		if recorder, _ := serve(f.tenants, "GET", "/v1/usage/summary"+query, acme.APIKey, nil); recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", query, recorder.Code)
		}
	}
}

func TestEstimatePricesAScenarioWithTheRateCard(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	scenario := func(hosting string) map[string]any {
		return map[string]any{"hosting": hosting, "minutes": 60, "groups": []map[string]any{
			{"role": "broadcast_audience", "count": 1000, "tiles": 1, "tile_width": 1280, "tile_height": 720, "kbps_per_tile": 1800},
		}}
	}
	// The self-hosted price card: 1,000 viewers at 720p for an hour.
	recorder, body := serve(f.tenants, "POST", "/v1/usage/estimate", acme.APIKey, scenario("self_hosted"))
	if recorder.Code != http.StatusOK || body["estimate"] != true {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	total := body["total"].(map[string]any)
	if got := microsOf(t, total["agora_list"]); got != 119_400_000 {
		t.Errorf("agora %s, want $119.40", got)
	}
	if got := microsOf(t, total["charge"]); got != 37_719_000 {
		t.Errorf("self-hosted %s, want $37.719", got)
	}
	group := body["groups"].([]any)[0].(map[string]any)
	if group["tier"] != "hd" || group["kbps"].(float64) != 1800 || total["participant_minutes"].(float64) != 60_000 {
		t.Errorf("group %v total %v", group, total)
	}

	_, body = serve(f.tenants, "POST", "/v1/usage/estimate", acme.APIKey, scenario("cloud"))
	want := DefaultPricing().Rate(Usage{Role: RoleBroadcastAudience, Tier: TierHD, Seconds: 3600,
		EgressBytes: 1800 * 125 * 3600, Hosting: HostingCloud}).Price * 1000
	if got := microsOf(t, body["total"].(map[string]any)["charge"]); got != want {
		t.Errorf("cloud %s, want %s", got, want)
	}

	// Four 720p tiles are 2K by aggregate pixels, and audio-only is audio.
	_, body = serve(f.tenants, "POST", "/v1/usage/estimate", acme.APIKey, map[string]any{
		"hosting": "cloud", "minutes": 10, "groups": []map[string]any{
			{"role": "host", "count": 4, "tiles": 3, "tile_width": 1280, "tile_height": 720},
			{"role": "audience", "count": 2},
		}})
	groups := body["groups"].([]any)
	if groups[0].(map[string]any)["tier"] != "2k" || groups[0].(map[string]any)["kbps"].(float64) != float64(AssumedKbps[Tier2K]) ||
		groups[1].(map[string]any)["tier"] != "audio" {
		t.Errorf("tiers %v", groups)
	}

	for name, bad := range map[string]map[string]any{
		"no groups":      {"hosting": "cloud", "minutes": 60, "groups": []any{}},
		"no minutes":     {"hosting": "cloud", "minutes": 0, "groups": []map[string]any{{"role": "host", "count": 1}}},
		"a long month":   {"hosting": "cloud", "minutes": 50_000, "groups": []map[string]any{{"role": "host", "count": 1}}},
		"unknown role":   {"hosting": "cloud", "minutes": 60, "groups": []map[string]any{{"role": "owner", "count": 1}}},
		"unknown host":   {"hosting": "edge", "minutes": 60, "groups": []map[string]any{{"role": "host", "count": 1}}},
		"no tile size":   {"hosting": "cloud", "minutes": 60, "groups": []map[string]any{{"role": "host", "count": 1, "tiles": 2}}},
		"huge audience":  {"hosting": "cloud", "minutes": 60, "groups": []map[string]any{{"role": "host", "count": 2_000_000}}},
		"unknown field":  {"hosting": "cloud", "minutes": 60, "groups": []map[string]any{{"role": "host", "count": 1}}, "x": 1},
		"too many tiles": {"hosting": "cloud", "minutes": 60, "groups": []map[string]any{{"role": "host", "count": 1, "tiles": 65, "tile_width": 1, "tile_height": 1}}},
	} {
		if recorder, _ := serve(f.tenants, "POST", "/v1/usage/estimate", acme.APIKey, bad); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
	if recorder, _ := serve(f.tenants, "POST", "/v1/usage/estimate", "", scenario("cloud")); recorder.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", recorder.Code)
	}
}
