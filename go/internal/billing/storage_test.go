package billing

import (
	"context"
	"net/http"
	"testing"
)

func TestStorageIsPricedAtCostPlusTheMarkup(t *testing.T) {
	pricing := DefaultPricing()
	month := int64(hoursPerMonth * 3600)
	// A GiB for a month on Standard in us-west1 is $0.020, plus 15%.
	charge := pricing.RateStorage(StorageStandard, gibibyte, month)
	if charge.Cost != 20_000 || charge.Price != 23_000 || charge.Capped {
		t.Fatalf("a GiB-month on Standard: %+v", charge)
	}
	// Prorated by the second, and colder classes cost less.
	day := pricing.RateStorage(StorageNearline, 10*gibibyte, 24*3600)
	if want := scale(10_000, 10*gibibyte*24*3600, gibibyte*hoursPerMonth*3600); day.Cost != want || want != 3288 {
		t.Fatalf("ten GiB for a day on Nearline cost %s, want %s", day.Cost, want)
	}
	if got := pricing.RateStorage(StorageArchive, 0, month); got != (Charge{}) {
		t.Fatalf("nothing stored is free: %+v", got)
	}
	if _, ok := GCSStorageListPrice("europe-west4"); ok {
		t.Fatal("a location without a listed price must not be guessed")
	}
	if us, ok := GCSStorageListPrice("US"); !ok || us[StorageStandard] != 26_000 {
		t.Fatalf("US multi-region: %v %v", us, ok)
	}
}

func TestRecordingStorageIsChargedOnceAndShownInTheUsageSummary(t *testing.T) {
	ctx := context.Background()
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	f.paid(t, acme.Tenant.ID, "pi_acme", 5000)
	before, err := f.service.store.UsageSince(ctx, acme.Tenant.ID, MonthOf(f.service.now()))
	if err != nil {
		t.Fatal(err)
	}

	records := []StorageRecord{
		{ID: "rec_1:a1:2027-01-14:standard", Tenant: acme.Tenant.ID, Room: "standup", Recording: "rec_1",
			Class: StorageStandard, Bytes: 3 * gibibyte, Seconds: 24 * 3600},
		{ID: "rec_1:a1:2027-01-14:nearline", Tenant: acme.Tenant.ID, Room: "standup", Recording: "rec_1",
			Class: StorageNearline, Bytes: gibibyte, Seconds: 12 * 3600},
		{ID: "exempt", Tenant: "t_unknown", Class: StorageStandard, Bytes: gibibyte, Seconds: 3600},
	}
	result, err := f.service.RecordStorage(ctx, records)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultPricing().RateStorage(StorageStandard, 3*gibibyte, 24*3600).Price +
		DefaultPricing().RateStorage(StorageNearline, gibibyte, 12*3600).Price
	if result.Charged != 2 || result.Unbilled != 1 || result.Total != want {
		t.Fatalf("result %+v, want two charged for %s and the unregistered tenant unbilled", result, want)
	}
	again, err := f.service.RecordStorage(ctx, records[:2])
	if err != nil {
		t.Fatal(err)
	}
	if again.Charged != 0 || again.Duplicates != 2 {
		t.Fatalf("a retried report charged again: %+v", again)
	}
	spent, err := f.service.store.UsageSince(ctx, acme.Tenant.ID, MonthOf(f.service.now()))
	if err != nil {
		t.Fatal(err)
	}
	if spent-before != want {
		t.Fatalf("the month's spend moved by %s, want %s", spent-before, want)
	}

	recorder, body := serve(f.tenants, "GET", "/v1/usage/summary?from=2027-01-01&to=2027-02-01", acme.APIKey, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	storage := body["storage"].(map[string]any)
	if got := microsOf(t, storage["charged"]); got != want {
		t.Errorf("storage charged %s, want %s", got, want)
	}
	if got := storage["product"]; got != ProductRecordingStorage {
		t.Errorf("storage product %v", got)
	}
	wantGiBMonths := GiBMonths(3*gibibyte*24 + gibibyte*12)
	if got := storage["storage_gib_months"].(float64); got != wantGiBMonths {
		t.Errorf("storage GiB-months %v, want %v", got, wantGiBMonths)
	}
	if got := microsOf(t, body["total"].(map[string]any)["charged"]); got != want {
		t.Errorf("total charged %s, want the storage %s", got, want)
	}
	if roles := body["by_role"].([]any); len(roles) != 0 {
		t.Errorf("storage is not a participant role: %v", roles)
	}
	if lines := body["lines"].([]any); len(lines) != 1 || lines[0].(map[string]any)["unrated"] != nil {
		t.Errorf("one storage line, compared at nothing rather than unrated: %v", lines)
	}

	if _, err := f.service.RecordStorage(ctx, []StorageRecord{
		{ID: "x", Tenant: acme.Tenant.ID, Class: "glacier", Bytes: 1, Seconds: 1},
	}); err == nil {
		t.Error("an unpriced class was accepted")
	}
	if _, err := f.service.RecordStorage(ctx, []StorageRecord{
		{ID: "y", Tenant: acme.Tenant.ID, Class: StorageStandard, Bytes: 1, Seconds: 2 * 24 * 3600},
	}); err == nil {
		t.Error("a record longer than a day was accepted")
	}
}

func TestRatesListRecordingStorage(t *testing.T) {
	f := newOperatorFixture(t)
	recorder, body := serve(f.tenants, "GET", "/v1/rates", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	storage := body["recording_storage_cost_per_gib_month"].(map[string]any)
	if got := microsOf(t, storage["standard"]); got != 20_000 {
		t.Fatalf("standard storage listed at %s", got)
	}
}
