package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// statsCall runs the handler on the test goroutine, so moving f.now between
// calls is not a data race with a server goroutine.
func statsCall(t *testing.T, handler http.Handler, method, address, origin string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, PublicStatsPath, nil)
	request.RemoteAddr = address
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeStats(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("stats body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func TestPublicStatsCountsWithoutIdentifyingAnyone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := f.service.Handler(nil)

	first, err := f.service.Signup(ctx, SignupRequest{Email: "first@example.com", Name: "First Co"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Signup(ctx, SignupRequest{Email: "second@example.com", Name: "Second Co"}); err != nil {
		t.Fatal(err)
	}
	active := first.Tenant
	active.Status = StatusActive
	if _, err := f.store.SaveTenant(ctx, active); err != nil {
		t.Fatal(err)
	}

	recorder := statsCall(t, handler, "GET", "192.0.2.1:5000", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("stats %d %s", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cache-control %q", got)
	}
	body := decodeStats(t, recorder)
	if len(body) != 3 || body["signups"] != float64(2) || body["active_tenants"] != float64(1) {
		t.Fatalf("stats body %v", body)
	}
	if body["updated_at"] != f.now.UTC().Format(time.RFC3339) {
		t.Fatalf("updated_at %v", body["updated_at"])
	}
	for _, secret := range []string{"example.com", "First Co", "Second Co", first.Tenant.ID} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("stats leaked %q: %s", secret, recorder.Body)
		}
	}

	if _, err := f.service.Signup(ctx, SignupRequest{Email: "third@example.com", Name: "Third"}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(59 * time.Second)
	if body := decodeStats(t, statsCall(t, handler, "GET", "192.0.2.1:5000", "")); body["signups"] != float64(2) {
		t.Fatalf("cache did not hold for a minute: %v", body)
	}
	f.now = f.now.Add(2 * time.Second)
	if body := decodeStats(t, statsCall(t, handler, "GET", "192.0.2.1:5000", "")); body["signups"] != float64(3) {
		t.Fatalf("cache did not expire: %v", body)
	}
}

func TestPublicStatsRateLimitsPerAddress(t *testing.T) {
	f := newFixture(t)
	handler := f.service.Handler(nil)
	for i := range 120 {
		if code := statsCall(t, handler, "GET", "192.0.2.1:5000", "").Code; code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	limited := statsCall(t, handler, "GET", "192.0.2.1:6000", "")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("121st request: %d %v", limited.Code, limited.Header())
	}
	if code := statsCall(t, handler, "GET", "192.0.2.2:5000", "").Code; code != http.StatusOK {
		t.Fatalf("another address was limited: %d", code)
	}
	f.now = f.now.Add(time.Minute)
	if code := statsCall(t, handler, "GET", "192.0.2.1:5000", "").Code; code != http.StatusOK {
		t.Fatalf("limit did not reset: %d", code)
	}
}

type failingCounts struct {
	Store
	fail bool
}

func (s *failingCounts) CountTenants(ctx context.Context) (int64, int64, error) {
	if s.fail {
		return 0, 0, errors.New("database down")
	}
	return s.Store.CountTenants(ctx)
}

func TestPublicStatsServesStaleWhenTheStoreFails(t *testing.T) {
	f := newFixture(t)
	store := &failingCounts{Store: f.store}
	f.service.store = store
	handler := f.service.Handler(nil)

	store.fail = true
	if code := statsCall(t, handler, "GET", "192.0.2.1:5000", "").Code; code != http.StatusInternalServerError {
		t.Fatalf("no cache and no store: %d", code)
	}
	store.fail = false
	if code := statsCall(t, handler, "GET", "192.0.2.1:5000", "").Code; code != http.StatusOK {
		t.Fatalf("recovered store: %d", code)
	}
	store.fail = true
	f.now = f.now.Add(2 * time.Minute)
	if code := statsCall(t, handler, "GET", "192.0.2.1:5000", "").Code; code != http.StatusOK {
		t.Fatalf("stale body not served: %d", code)
	}
}

func TestPublicStatsCORSIsGetOnly(t *testing.T) {
	f := newFixture(t)
	handler := CORS(f.service.Handler(nil), []string{"https://moqom.cloud"})

	preflight := statsCall(t, handler, "OPTIONS", "192.0.2.1:5000", "https://moqom.cloud")
	if preflight.Code != http.StatusNoContent ||
		preflight.Header().Get("Access-Control-Allow-Origin") != "https://moqom.cloud" ||
		preflight.Header().Get("Access-Control-Allow-Methods") != "GET" ||
		preflight.Header().Get("Access-Control-Allow-Headers") != "" ||
		preflight.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight %d %v", preflight.Code, preflight.Header())
	}
	allowed := statsCall(t, handler, "GET", "192.0.2.1:5000", "https://moqom.cloud")
	if allowed.Code != http.StatusOK || allowed.Header().Get("Access-Control-Allow-Origin") != "https://moqom.cloud" {
		t.Fatalf("allowed origin %d %v", allowed.Code, allowed.Header())
	}
	foreign := statsCall(t, handler, "GET", "192.0.2.1:5000", "https://evil.example")
	if foreign.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin allowed: %v", foreign.Header())
	}
	if code := statsCall(t, handler, "POST", "192.0.2.1:5000", "https://moqom.cloud").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("POST stats: %d", code)
	}
}

func TestLimiterForgetsQuietAddresses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	limiter := &windowLimiter{per: 1, window: time.Minute, now: func() time.Time { return now }}
	for i := range sweepAbove {
		limiter.permit(strconv.Itoa(i))
	}
	now = now.Add(time.Minute)
	limiter.permit("fresh")
	if len(limiter.seen) != 1 {
		t.Fatalf("%d addresses held after they went quiet", len(limiter.seen))
	}
}
