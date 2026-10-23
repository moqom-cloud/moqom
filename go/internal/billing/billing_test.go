package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const mebibyte = 1 << 20

func TestRateIsEightyFivePercentOfAgora(t *testing.T) {
	pricing := DefaultPricing()
	charge := pricing.Rate(Usage{Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte, Hosting: HostingCloud})
	// $3.99 x 85% = $3.3915, down to $3.39 per 1,000 minutes: 3390 micros a
	// minute. Cost is still recorded: egress 120000/1024 = 117, compute 60.
	if charge.Cost != 177 || charge.Price != 3390 || charge.Capped {
		t.Fatalf("got %+v, want cost 177 price 3390", charge)
	}
}

func TestRatePer1000AtEveryBand(t *testing.T) {
	pricing := DefaultPricing()
	for _, c := range []struct {
		role Role
		tier Tier
		band int
		want Micros
	}{
		{RoleHost, TierHD, 0, 339 * Cent},                // 3.99 x .85
		{RoleHost, TierHD, 1, 322 * Cent},                // 3.99 x .95 = 3.7905 x .85 = 3.2219
		{RoleHost, TierHD, 3, 305 * Cent},                // 3.99 x .90 = 3.591 x .85 = 3.0523
		{RoleHost, TierAudio, 0, 84 * Cent},              // 0.99 x .85 = 0.8415
		{RoleBroadcastAudience, TierAudio, 3, 45 * Cent}, // 0.59 x .90 = 0.531 x .85 = 0.4513
		{RoleHost, Tier2KPlus, 0, 3059 * Cent},           // 35.99 x .85 = 30.5915
	} {
		if got, _ := pricing.RatePer1000(c.role, c.tier, c.band); got != c.want {
			t.Errorf("%s %s band %d: %s, want %s", c.role, c.tier, c.band, got, c.want)
		}
	}
}

func TestRateIsAlwaysAtLeastACentUnderAgora(t *testing.T) {
	for _, share := range []int64{8500, 9990, 10_000} {
		pricing := DefaultPricing()
		pricing.ShareBasisPoints = share
		for role, tiers := range pricing.Reference {
			for tier, list := range tiers {
				for band, b := range pricing.Bands {
					agora := list - scale(list, b.DiscountBasisPoints, 10_000)
					if got, _ := pricing.RatePer1000(role, tier, band); got > agora-Cent {
						t.Errorf("share %d %s %s band %d: %s, Agora %s", share, role, tier, band, got, agora)
					}
				}
			}
		}
	}
}

func TestHostedPriceSplitsAcrossBands(t *testing.T) {
	pricing := DefaultPricing()
	edge := int64(100_000 * 60)
	// Ten minutes either side of the first band's edge: half at $3.39, half at $3.22.
	got, _ := pricing.HostedPrice(RoleHost, TierHD, edge-600, 1200)
	want := Per1000Minutes(339*Cent, 600) + Per1000Minutes(322*Cent, 600)
	if got != want {
		t.Fatalf("across the edge: %s, want %s", got, want)
	}
	// A month cut into records costs what it costs in one.
	var parts Micros
	for before := int64(0); before < 2*edge; before += edge / 4 {
		p, _ := pricing.HostedPrice(RoleHost, TierHD, before, edge/4)
		parts += p
	}
	whole, _ := pricing.HostedPrice(RoleHost, TierHD, 0, 2*edge)
	if parts != whole {
		t.Fatalf("in parts %s, whole %s", parts, whole)
	}
}

func TestRecordUsageMovesATenantIntoTheNextBand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 1_000_000)
	record := func(id string, seconds int64) Micros {
		t.Helper()
		result, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud},
			[]UsageRecord{{ID: id, Tenant: tenant, Role: RoleHost, Tier: TierHD, Seconds: seconds}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Total
	}
	// Records are at most a day long, so the first 100,000 minutes take several.
	var first Micros
	for used, i := int64(0), 0; used < 100_000*60; i++ {
		seconds := min(int64(24*60*60), 100_000*60-used)
		first += record(fmt.Sprintf("a%d", i), seconds)
		used += seconds
	}
	second := record("b", 60_000)
	if first != Per1000Minutes(339*Cent, 100_000*60) || second != Per1000Minutes(322*Cent, 60_000) {
		t.Fatalf("first %s, second %s", first, second)
	}
	if again := record("b", 60_000); again != 0 {
		t.Fatalf("a re-sent record charged %s", again)
	}
}

func TestRateSelfHostedIsTheFeeAlone(t *testing.T) {
	charge := DefaultPricing().Rate(Usage{Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte, Hosting: HostingSelf})
	// 50000/1024 = 48.8, rounded half up.
	if charge.Cost != 0 || charge.Price != 49 {
		t.Fatalf("got %+v, want cost 0 price 49", charge)
	}
}

func TestSelfHostedNeverCostsMoreThanHosted(t *testing.T) {
	pricing := DefaultPricing()
	charge := pricing.Rate(Usage{Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: 10 << 30, Hosting: HostingSelf})
	if !charge.Capped || charge.Price != 3390 {
		t.Fatalf("got %+v, want capped at 3390", charge)
	}
}

func TestRateDoesNotOverflowOnAPetabyte(t *testing.T) {
	pricing := Pricing{Costs: GCPListPrice()}
	charge := pricing.Rate(Usage{Role: RoleHost, Tier: TierHD, EgressBytes: 1 << 50})
	if want := Micros(120_000) << 20; charge.Cost != want {
		t.Fatalf("cost %d, want %d", charge.Cost, want)
	}
}

func TestTierBoundaries(t *testing.T) {
	cases := []struct {
		audio  bool
		pixels uint64
		want   Tier
	}{
		{true, 1920 * 1080, TierAudio},
		{false, 0, TierAudio},
		{false, 1280 * 720, TierHD},
		{false, 1280*720 + 1, TierFullHD},
		{false, 1920 * 1080, TierFullHD},
		{false, 2560 * 1440, Tier2K},
		{false, 3840 * 2160, Tier2KPlus},
	}
	for _, c := range cases {
		if got := TierFor(c.audio, c.pixels); got != c.want {
			t.Errorf("TierFor(%v, %d) = %s, want %s", c.audio, c.pixels, got, c.want)
		}
	}
}

func TestWebhookSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{}}}`)
	header := SignWebhook(payload, "whsec_test", now)

	if event, err := VerifyWebhook(payload, header, "whsec_test", now); err != nil || event.ID != "evt_1" {
		t.Fatalf("valid delivery rejected: %v", err)
	}
	if _, err := VerifyWebhook(payload, header, "whsec_other", now); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	tampered := bytes.Replace(payload, []byte("evt_1"), []byte("evt_2"), 1)
	if _, err := VerifyWebhook(tampered, header, "whsec_test", now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered payload accepted: %v", err)
	}
	if _, err := VerifyWebhook(payload, header, "whsec_test", now.Add(WebhookTolerance+time.Second)); !errors.Is(err, ErrSignature) {
		t.Fatalf("replay outside tolerance accepted: %v", err)
	}
	if _, err := VerifyWebhook(payload, "", "whsec_test", now); !errors.Is(err, ErrSignature) {
		t.Fatalf("unsigned delivery accepted: %v", err)
	}
}

func TestKeys(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	key, plaintext, err := NewKey(nil, "t_1", "ci", KeyBackend, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(key.Hash, plaintext) || !strings.HasPrefix(plaintext, "mqk_") {
		t.Fatalf("key %q stored as %q", plaintext, key.Hash)
	}
	if err := store.PutKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveKey(ctx, store, plaintext); err != nil || got.Tenant != "t_1" {
		t.Fatalf("resolve: %v %+v", err, got)
	}
	if _, err := ResolveKey(ctx, store, alterSecret(plaintext)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("altered secret accepted: %v", err)
	}
	account, err := Accounts{Store: store}.Lookup(ctx, plaintext)
	if err != nil || account.Tenant != "t_1" || account.IsRelay {
		t.Fatalf("account: %v %+v", err, account)
	}
	if err := store.RevokeKey(ctx, "t_1", key.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveKey(ctx, store, plaintext); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key accepted: %v", err)
	}
}

// alterSecret changes one character of a key's secret, always to a different
// one. Overwriting the last character with a fixed letter, as this test once
// did, is a no-op one time in sixteen: 32 bytes in unpadded base64 leave four
// bits in the final character, so it is one of only 16 letters, A among them.
func alterSecret(plaintext string) string {
	last := plaintext[len(plaintext)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'E'
	}
	return plaintext[:len(plaintext)-1] + string(replacement)
}

func TestAlterSecretAlwaysAlters(t *testing.T) {
	for range 200 {
		_, plaintext, err := NewKey(nil, "t_1", "ci", KeyBackend, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if alterSecret(plaintext) == plaintext {
			t.Fatalf("%q was not altered", plaintext)
		}
	}
}

func TestRelayKeysCannotDriveTheControlPlane(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	key, plaintext, _ := NewKey(nil, "t_1", "edge", KeyRelay, time.Now())
	_ = store.PutKey(ctx, key)
	if _, err := (Accounts{Store: store}).Lookup(ctx, plaintext); err == nil {
		t.Fatal("a relay key authenticated to gRPC")
	}
}

// fakeStripe answers the five endpoints billing calls.
type fakeStripe struct {
	mu        sync.Mutex
	charges   []url.Values
	keys      []string
	customers int
	declining bool
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/customers":
		f.customers++
		fmt.Fprintf(w, `{"id":"cus_%d"}`, f.customers)
	case "/v1/checkout/sessions":
		fmt.Fprintf(w, `{"id":"cs_1","url":"https://checkout.stripe.test/%s"}`, r.Form.Get("client_reference_id"))
	case "/v1/billing_portal/sessions":
		fmt.Fprint(w, `{"url":"https://billing.stripe.test/portal"}`)
	case "/v1/payment_intents":
		if f.declining {
			w.WriteHeader(http.StatusPaymentRequired)
			fmt.Fprint(w, `{"error":{"type":"card_error","code":"card_declined","message":"Your card was declined."}}`)
			return
		}
		f.charges = append(f.charges, r.Form)
		amount, _ := strconv.ParseInt(r.Form.Get("amount"), 10, 64)
		_ = json.NewEncoder(w).Encode(PaymentIntent{
			ID: fmt.Sprintf("pi_auto_%d", len(f.charges)), Status: "succeeded",
			Amount: amount, AmountReceived: amount, Currency: "usd",
			Customer: r.Form.Get("customer"), PaymentMethod: r.Form.Get("payment_method"),
			Metadata: map[string]string{"tenant": r.Form.Get("metadata[tenant]"), "purpose": r.Form.Get("metadata[purpose]")},
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeStripe) chargeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.charges)
}

type fixture struct {
	service *Service
	stripe  *fakeStripe
	store   *Memory
	now     time.Time
}

const webhookSecret = "whsec_test"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{stripe: &fakeStripe{}, store: NewMemory(), now: time.Unix(1_800_000_000, 0)}
	server := httptest.NewServer(f.stripe)
	t.Cleanup(server.Close)
	service, err := New(Config{
		Store:         f.store,
		Stripe:        &Stripe{SecretKey: "sk_test", BaseURL: server.URL, Client: server.Client()},
		WebhookSecret: webhookSecret,
		PublicURL:     "https://moqom.test",
		Now:           func() time.Time { return f.now },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func (f *fixture) paid(t *testing.T, tenant, intent string, cents int64) {
	t.Helper()
	object, _ := json.Marshal(PaymentIntent{
		ID: intent, Status: "succeeded", Amount: cents, AmountReceived: cents, Currency: "usd",
		Customer: "cus_1", PaymentMethod: "pm_card", SetupFutureUsage: "off_session",
		Metadata: map[string]string{"tenant": tenant, "purpose": "top_up"},
	})
	f.deliver(t, "payment_intent.succeeded", object)
}

func (f *fixture) deliver(t *testing.T, kind string, object []byte) {
	t.Helper()
	var event Event
	event.ID, event.Type, event.Data.Object = "evt_"+kind, kind, object
	if err := f.service.HandleStripeEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) balance(t *testing.T, tenant string) Micros {
	t.Helper()
	balance, err := f.store.Balance(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

func TestPaidLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	signup, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	tenant := signup.Tenant.ID
	if signup.Tenant.Status != StatusPending || signup.Tenant.StripeCustomer != "cus_1" {
		t.Fatalf("signup tenant %+v", signup.Tenant)
	}
	if signup.Checkout.URL == "" || signup.APIKey == "" || signup.Job.Status != JobCompleted {
		t.Fatalf("signup %+v", signup)
	}
	if err := f.service.Allow(ctx, tenant); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("unpaid tenant admitted: %v", err)
	}

	f.paid(t, tenant, "pi_first", 5000)
	f.paid(t, tenant, "pi_first", 5000) // Stripe redelivers.
	if got := f.balance(t, tenant); got != 50*Dollar {
		t.Fatalf("balance %s after one $50 payment delivered twice", got)
	}
	if err := f.service.Allow(ctx, tenant); err != nil {
		t.Fatalf("paid tenant refused: %v", err)
	}
	record, _ := f.store.GetTenant(ctx, tenant)
	if record.Status != StatusActive || record.PaymentMethod != "pm_card" {
		t.Fatalf("after payment %+v", record)
	}

	usage := []UsageRecord{{ID: "r1", Room: "lobby", Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte}}
	result, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, append(usage[:0:0], UsageRecord{
		ID: "r1", Tenant: tenant, Room: "lobby", Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte,
	}))
	if err != nil || result.Charged != 1 || result.Total != 3390 {
		t.Fatalf("usage %+v %v", result, err)
	}
	again, _ := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, []UsageRecord{{
		ID: "r1", Tenant: tenant, Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte,
	}})
	if again.Duplicates != 1 || f.balance(t, tenant) != 50*Dollar-3390 {
		t.Fatalf("a re-sent record was charged twice: %+v, balance %s", again, f.balance(t, tenant))
	}

	refund, _ := json.Marshal(Refund{ID: "re_1", Amount: 5000, Status: "succeeded", PaymentIntent: "pi_first"})
	f.deliver(t, "refund.created", refund)
	f.deliver(t, "refund.updated", refund)
	if got := f.balance(t, tenant); got != -3390 {
		t.Fatalf("balance %s after refunding $50 twice", got)
	}
	if err := f.service.Allow(ctx, tenant); !errors.Is(err, ErrInsufficientCredit) {
		t.Fatalf("tenant with no credit admitted: %v", err)
	}
}

func TestAutomaticTopUpChargesOnceAndCredits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 1000)
	if _, err := f.service.SetAutoTopUp(ctx, tenant, AutoTopUp{Enabled: true, Below: 20 * Dollar, Amount: 25 * Dollar}); err != nil {
		t.Fatal(err)
	}

	report := func(id string) {
		if _, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, []UsageRecord{{
			ID: id, Tenant: tenant, Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	report("r1")
	if f.stripe.chargeCount() != 1 {
		t.Fatalf("%d charges, want 1", f.stripe.chargeCount())
	}
	if got := f.balance(t, tenant); got != 35*Dollar-3390 {
		t.Fatalf("balance %s, want $35 less one minute", got)
	}
	report("r2")
	if f.stripe.chargeCount() != 1 {
		t.Fatal("charged again with the balance above the threshold")
	}

	// A declined card is recorded and not retried until the claim goes stale.
	f.stripe.declining = true
	_, _ = f.service.SetAutoTopUp(ctx, tenant, AutoTopUp{Enabled: true, Below: 100 * Dollar, Amount: 25 * Dollar})
	report("r3")
	record, _ := f.store.GetTenant(ctx, tenant)
	if !strings.Contains(record.LastTopUpError, "declined") {
		t.Fatalf("decline not recorded: %+v", record)
	}
}

func TestSelfHostedRelayPaysTheFeeForItsOwnTenantOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 1000)

	reporter := Reporter{Tenant: tenant, Hosting: HostingSelf}
	result, err := f.service.RecordUsage(ctx, reporter, []UsageRecord{{
		ID: "r1", Role: RoleBroadcastAudience, Tier: TierFullHD, Seconds: 60, EgressBytes: mebibyte,
	}})
	if err != nil || result.Total != 49 {
		t.Fatalf("self-hosted usage %+v %v, want the 49-micro fee", result, err)
	}
	if _, err := f.service.RecordUsage(ctx, reporter, []UsageRecord{{
		ID: "r2", Tenant: "t_someone_else", Role: RoleHost, Tier: TierHD, Seconds: 60,
	}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a tenant's relay billed another tenant: %v", err)
	}
}

type withhold map[string]bool

func (w withhold) Unbillable(_ context.Context, tenant, room, participant string) (bool, string) {
	return w[tenant+"/"+room+"/"+participant], "excluded"
}

func TestUsageFilterWithholdsExcludedParticipants(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	tenant := signup.Tenant.ID
	f.paid(t, tenant, "pi_first", 1000)
	f.service.usage = withhold{tenant + "/lobby/bot/d1": true}

	result, err := f.service.RecordUsage(ctx, Reporter{Hosting: HostingCloud}, []UsageRecord{
		{ID: "r1", Tenant: tenant, Room: "lobby", Participant: "bot/d1", Role: RoleAudience, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
		{ID: "r2", Tenant: tenant, Room: "lobby", Participant: "ana/d2", Role: RoleAudience, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
		{ID: "r3", Tenant: tenant, Room: "other", Participant: "bot/d1", Role: RoleAudience, Tier: TierHD, Seconds: 60, EgressBytes: mebibyte},
	})
	if err != nil || result.Unbilled != 1 || result.Charged != 2 {
		t.Fatalf("usage %+v %v, want the excluded viewer withheld in its room only", result, err)
	}
}

func TestHTTP(t *testing.T) {
	f := newFixture(t)
	server := httptest.NewServer(f.service.Handler(nil))
	t.Cleanup(server.Close)

	call := func(method, path, key string, body any) (*http.Response, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		}
		request, _ := http.NewRequest(method, server.URL+path, reader)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		decoded := map[string]any{}
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response, decoded
	}

	response, body := call("POST", "/v1/signup", "", map[string]string{"email": "dev@example.com", "name": "Example"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("signup %d %v", response.StatusCode, body)
	}
	key, tenant := body["api_key"].(string), body["tenant"].(string)

	if response, _ := call("GET", "/v1/billing", "", nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("billing without a key: %d", response.StatusCode)
	}
	response, body = call("GET", "/v1/billing", key, nil)
	if response.StatusCode != http.StatusOK || body["admitted"] != false {
		t.Fatalf("billing %d %v", response.StatusCode, body)
	}

	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","status":"succeeded","amount_received":2000,"currency":"usd","customer":"cus_1","metadata":{"tenant":"` + tenant + `"}}}}`)
	post := func(signature string) int {
		request, _ := http.NewRequest("POST", server.URL+"/v1/stripe/webhook", bytes.NewReader(payload))
		request.Header.Set("Stripe-Signature", signature)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if code := post(SignWebhook(payload, "whsec_forged", f.now)); code != http.StatusBadRequest {
		t.Fatalf("forged webhook: %d", code)
	}
	if code := post(SignWebhook(payload, webhookSecret, f.now)); code != http.StatusOK {
		t.Fatalf("signed webhook: %d", code)
	}
	if _, body = call("GET", "/v1/billing", key, nil); body["admitted"] != true {
		t.Fatalf("not admitted after paying: %v", body)
	}

	if response, _ := call("POST", "/v1/usage", key, usageBody{}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a backend key reported usage: %d", response.StatusCode)
	}
	response, body = call("POST", "/v1/keys", key, map[string]string{"name": "edge", "kind": "relay"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("issue relay key %d %v", response.StatusCode, body)
	}
	relayKey := body["api_key"].(string)
	response, body = call("POST", "/v1/usage", relayKey, map[string]any{"records": []map[string]any{{
		"id": "r1", "role": "host", "tier": "hd", "seconds": 60, "egress_bytes": mebibyte,
	}}})
	if response.StatusCode != http.StatusOK || body["charged"] != float64(1) || body["admitted"] != true {
		t.Fatalf("relay usage %d %v", response.StatusCode, body)
	}

	// Exactly as moqom-relay serialises a self-hosted batch: no tenant, ids
	// carrying the relay's boot and session, every role and the top tier. A
	// field or value the relay sends that this handler refuses would 400 every
	// report it makes.
	relayBatch := json.RawMessage(`{"records":[` +
		`{"id":"relay-eu-1:2026-10-06T09:15:00Z:bea/41d07a9e#1791270000123.42","room":"room-42","participant":"bea/41d07a9e","role":"host","tier":"2k_plus","seconds":60,"egress_bytes":1048576,"ingress_bytes":524288},` +
		`{"id":"relay-eu-1:2026-10-06T09:15:00Z:cy/0c1d2e3f#1791270000123.43","room":"room-42","participant":"cy/0c1d2e3f","role":"audience","tier":"full_hd","seconds":37,"egress_bytes":2048,"ingress_bytes":0},` +
		`{"id":"relay-eu-1:2026-10-06T09:15:00Z:anonymous#1791270000123.44","role":"broadcast_audience","tier":"audio","seconds":1,"egress_bytes":0,"ingress_bytes":0}` +
		`]}`)
	response, body = call("POST", "/v1/usage", relayKey, relayBatch)
	if response.StatusCode != http.StatusOK || body["charged"] != float64(3) {
		t.Fatalf("relay-shaped batch %d %v", response.StatusCode, body)
	}
	// The relay's retry after a lost answer.
	response, body = call("POST", "/v1/usage", relayKey, relayBatch)
	if response.StatusCode != http.StatusOK || body["charged"] != float64(0) || body["duplicates"] != float64(3) {
		t.Fatalf("relay retry %d %v", response.StatusCode, body)
	}
	// The relay's heartbeat, which is how it hears admission with nobody connected.
	response, body = call("POST", "/v1/usage", relayKey, json.RawMessage(`{"records":[]}`))
	if response.StatusCode != http.StatusOK || body["admitted"] != true {
		t.Fatalf("relay heartbeat %d %v", response.StatusCode, body)
	}

	if response, _ := call("POST", "/v1/billing/checkout", key, map[string]int{"amount_cents": 1}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("one-cent checkout: %d", response.StatusCode)
	}
	response, body = call("GET", "/v1/rates", "", nil)
	if response.StatusCode != http.StatusOK || body["currency"] != "usd" {
		t.Fatalf("rates %d %v", response.StatusCode, body)
	}
	if fee, _ := body["direct_fee_per_1000_minutes"].(map[string]any); body["direct_free_minutes_per_month"] != float64(1000) ||
		fee["micros"] != float64(50*Cent) {
		t.Fatalf("direct rates %v", body)
	}

	// 1,020 direct minutes from a self-hosted relay, where the standby session
	// carried nothing: 1,000 free, then 20 at 50 cents per 1,000.
	response, body = call("POST", "/v1/usage", relayKey, json.RawMessage(
		`{"records":[{"id":"direct-1","role":"host","tier":"audio","seconds":60,"egress_bytes":0,"ingress_bytes":0,"direct_seconds":61200}]}`))
	if total, _ := body["total"].(map[string]any); response.StatusCode != http.StatusOK || total["micros"] != float64(Cent) {
		t.Fatalf("direct usage %d %v", response.StatusCode, body)
	}
}

func TestStandbyTimeIsChargedAtCostNotTheRate(t *testing.T) {
	pricing := DefaultPricing()
	minute := Usage{Role: RoleHost, Tier: TierHD, Seconds: 60, EgressBytes: 1 << 20, Hosting: HostingCloud}
	relayed := pricing.Rate(minute)
	standby := minute
	standby.Standby = 60
	direct := pricing.Rate(standby)
	if direct.Price != direct.Cost || direct.Price >= relayed.Price {
		t.Fatalf("a standby minute cost %s against %s relayed", direct.Price, relayed.Price)
	}
	half := minute
	half.Standby = 30
	rate, _ := pricing.HostedPrice(RoleHost, TierHD, 0, 30)
	if got, want := pricing.Rate(half).Price, rate+scale(relayed.Cost, 30, 60); got != want {
		t.Fatalf("half standby cost %s, want %s", got, want)
	}
}
