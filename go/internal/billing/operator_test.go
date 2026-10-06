package billing

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	operatorClient = "1234-admin.apps.googleusercontent.com"
	operatorEmail  = "elliott@moqom.cloud"
	adminOrigin    = "https://admin.moqom.cloud"
)

type staticKeys map[string]*rsa.PublicKey

func (s staticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if key, ok := s[kid]; ok {
		return key, nil
	}
	return nil, ErrNotOperator
}

var (
	googleKey = mustRSA()
	otherKey  = mustRSA()
)

func mustRSA() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

func encodeSegment(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func signRS256(key *rsa.PrivateKey, header, claims map[string]any) string {
	input := encodeSegment(header) + "." + encodeSegment(claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func goodClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": operatorClient, "sub": "1180000000001",
		"email": operatorEmail, "email_verified": true, "hd": "moqom.cloud",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(59 * time.Minute).Unix(),
	}
}

func goodHeader() map[string]any { return map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"} }

func with(claims map[string]any, name string, value any) map[string]any {
	changed := map[string]any{}
	for k, v := range claims {
		changed[k] = v
	}
	if value == nil {
		delete(changed, name)
	} else {
		changed[name] = value
	}
	return changed
}

type operatorFixture struct {
	*fixture
	tenants http.Handler
	ops     http.Handler
	token   string
}

func newOperatorFixture(t *testing.T) *operatorFixture {
	t.Helper()
	f := newFixture(t)
	identity := &GoogleIdentity{
		ClientID: operatorClient, Allowed: ParseAllowList(" Elliott@moqom.cloud "), HostedDomain: "moqom.cloud",
		Keys: staticKeys{"k1": &googleKey.PublicKey}, Now: func() time.Time { return f.now },
	}
	ops, err := f.service.OperatorHandler(OperatorOptions{Verifier: identity, Origins: []string{adminOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	return &operatorFixture{
		fixture: f, tenants: f.service.Handler(nil), ops: ops,
		token: signRS256(googleKey, goodHeader(), goodClaims(f.now)),
	}
}

func serve(handler http.Handler, method, path, credential string, body any) (*httptest.ResponseRecorder, map[string]any) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = "203.0.113.9:1234"
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func (f *operatorFixture) signup(t *testing.T, name string) SignupResult {
	t.Helper()
	result, err := f.service.Signup(context.Background(), SignupRequest{Email: strings.ToLower(name) + "@example.com", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOperatorAPIRefusesEverythingButAnAllowListedGoogleToken(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	relay, relayKey, err := f.service.IssueKey(context.Background(), acme.Tenant.ID, "edge", KeyRelay)
	if err != nil || relay.ID == "" {
		t.Fatal(err)
	}
	claims := goodClaims(f.now)
	hmacToken := func() string {
		input := encodeSegment(map[string]any{"alg": "HS256", "kid": "k1"}) + "." + encodeSegment(claims)
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write([]byte(input))
		return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}

	for _, tc := range []struct{ name, credential string }{
		{"no credential", ""},
		{"a tenant backend key", acme.APIKey},
		{"a tenant relay key", relayKey},
		{"garbage", "not-a-token"},
		{"alg none", encodeSegment(map[string]any{"alg": "none", "kid": "k1"}) + "." + encodeSegment(claims) + "."},
		{"HS256", hmacToken()},
		{"signed by another key", signRS256(otherKey, goodHeader(), claims)},
		{"an unknown kid", signRS256(googleKey, map[string]any{"alg": "RS256", "kid": "k2"}, claims)},
		{"another audience", signRS256(googleKey, goodHeader(), with(claims, "aud", "someone-else.apps.googleusercontent.com"))},
		{"several audiences", signRS256(googleKey, goodHeader(), with(claims, "aud", []string{operatorClient, "other"}))},
		{"another issuer", signRS256(googleKey, goodHeader(), with(claims, "iss", "https://evil.example"))},
		{"expired", signRS256(googleKey, goodHeader(), with(claims, "exp", f.now.Add(-2*time.Minute).Unix()))},
		{"issued in the future", signRS256(googleKey, goodHeader(), with(claims, "iat", f.now.Add(10*time.Minute).Unix()))},
		{"unverified email", signRS256(googleKey, goodHeader(), with(claims, "email_verified", false))},
		{"an address not on the list", signRS256(googleKey, goodHeader(), with(claims, "email", "intern@moqom.cloud"))},
		{"a consumer account with the address", signRS256(googleKey, goodHeader(), with(claims, "hd", nil))},
		{"no subject", signRS256(googleKey, goodHeader(), with(claims, "sub", nil))},
	} {
		recorder, _ := serve(f.ops, "GET", "/v1/operator/tenants", tc.credential, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401: %s", tc.name, recorder.Code, recorder.Body.String())
		}
	}

	// A token in the query string is not a credential.
	recorder, _ := serve(f.ops, "GET", "/v1/operator/tenants?access_token="+f.token, "", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("a query-string token was accepted: %d", recorder.Code)
	}

	recorder, body := serve(f.ops, "GET", "/v1/operator/whoami", f.token, nil)
	if recorder.Code != http.StatusOK || body["email"] != operatorEmail {
		t.Fatalf("the operator was refused: %d %v", recorder.Code, body)
	}

	// Refusals are not audited rows (an attacker could fill the table), only
	// authenticated requests are.
	entries, _ := f.store.ListAudit(context.Background(), "", 0)
	if len(entries) != 1 || entries[0].Action != "whoami" || entries[0].Operator != operatorEmail {
		t.Fatalf("audit after refusals: %+v", entries)
	}
}

func TestOperatorRoutesAreNotOnTheTenantAPIAndViceVersa(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")

	// The tenant handler has no operator routes at all, so a tenant key there
	// reaches nothing.
	for _, path := range []string{"/v1/operator/tenants", "/v1/operator/tenants/" + acme.Tenant.ID} {
		if recorder, _ := serve(f.tenants, "GET", path, acme.APIKey, nil); recorder.Code != http.StatusNotFound {
			t.Errorf("tenant handler answered %s with %d", path, recorder.Code)
		}
	}
	// And an operator token is not a tenant key.
	if recorder, _ := serve(f.tenants, "GET", "/v1/billing", f.token, nil); recorder.Code != http.StatusUnauthorized {
		t.Errorf("an operator token read a tenant's billing: %d", recorder.Code)
	}
}

func TestOperatorListsEveryTenantWithBalanceAndSpend(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	f.now = f.now.Add(30 * time.Minute)
	globex := f.signup(t, "Globex")
	f.paid(t, acme.Tenant.ID, "pi_acme", 5000)

	recorder, body := serve(f.ops, "GET", "/v1/operator/tenants", f.token, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %v", recorder.Code, body)
	}
	rows := body["tenants"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["id"] != globex.Tenant.ID || second["id"] != acme.Tenant.ID {
		t.Fatalf("not newest first: %v", rows)
	}
	if second["status"] != "active" || second["balance"].(map[string]any)["micros"] != float64(50*Dollar) || second["last_activity"] == nil {
		t.Errorf("acme row: %v", second)
	}
	if first["status"] != "pending" || first["last_activity"] != nil {
		t.Errorf("globex row: %v", first)
	}

	recorder, body = serve(f.ops, "GET", "/v1/operator/tenants/"+acme.Tenant.ID, f.token, nil)
	if recorder.Code != http.StatusOK || len(body["ledger"].([]any)) != 1 || len(body["keys"].([]any)) != 1 {
		t.Fatalf("detail: %d %v", recorder.Code, body)
	}
	if strings.Contains(recorder.Body.String(), acme.APIKey) || strings.Contains(recorder.Body.String(), `"hash"`) {
		t.Fatal("the detail view leaks a key")
	}
	if recorder, _ := serve(f.ops, "GET", "/v1/operator/tenants/t_nobody", f.token, nil); recorder.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", recorder.Code)
	}

	recorder, body = serve(f.ops, "GET", "/v1/operator/signups?days=2", f.token, nil)
	days := body["days"].([]any)
	if recorder.Code != http.StatusOK || len(days) != 2 || body["total"] != float64(2) {
		t.Fatalf("signups: %d %v", recorder.Code, body)
	}
	if sum := days[0].(map[string]any)["signups"].(float64) + days[1].(map[string]any)["signups"].(float64); sum != 2 {
		t.Errorf("signups per day: %v", days)
	}
	if recorder, _ := serve(f.ops, "GET", "/v1/operator/signups?days=0", f.token, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("days=0: %d", recorder.Code)
	}
}

func TestCreditAdjustmentIsIdempotentAuditedAndShownToTheTenant(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	path := "/v1/operator/tenants/" + acme.Tenant.ID + "/adjustments"
	request := map[string]any{"amount_cents": 2500, "reason": "Credit for the 5 Oct relay outage", "idempotency_key": "adj-outage-acme-1"}

	recorder, body := serve(f.ops, "POST", path, f.token, request)
	if recorder.Code != http.StatusCreated || body["duplicate"] != false {
		t.Fatalf("first: %d %v", recorder.Code, body)
	}
	if balance, _ := f.store.Balance(context.Background(), acme.Tenant.ID); balance != 25*Dollar {
		t.Fatalf("balance %s", balance)
	}

	recorder, body = serve(f.ops, "POST", path, f.token, request)
	if recorder.Code != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("replay: %d %v", recorder.Code, body)
	}
	if balance, _ := f.store.Balance(context.Background(), acme.Tenant.ID); balance != 25*Dollar {
		t.Fatalf("a replay moved the balance to %s", balance)
	}

	for name, changed := range map[string]map[string]any{
		"a reused key for another amount": with(request, "amount_cents", 2600),
		"a reused key for another reason": with(request, "reason", "Something else entirely"),
	} {
		if recorder, _ := serve(f.ops, "POST", path, f.token, changed); recorder.Code != http.StatusConflict {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
	// The same key on another tenant is another adjustment.
	globex := f.signup(t, "Globex")
	if recorder, _ := serve(f.ops, "POST", "/v1/operator/tenants/"+globex.Tenant.ID+"/adjustments", f.token, request); recorder.Code != http.StatusCreated {
		t.Errorf("same key on another tenant: %d", recorder.Code)
	}

	for name, bad := range map[string]map[string]any{
		"no reason":          with(request, "reason", nil),
		"a blank reason":     with(request, "reason", "   "),
		"zero":               with(with(request, "amount_cents", 0), "idempotency_key", "adj-zero-0001"),
		"too much":           with(with(request, "amount_cents", 1_000_001), "idempotency_key", "adj-huge-0001"),
		"no idempotency key": with(request, "idempotency_key", nil),
		"a short key":        with(request, "idempotency_key", "abc"),
		"an unknown field":   with(request, "operator", "someone-else"),
	} {
		if recorder, _ := serve(f.ops, "POST", path, f.token, bad); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if recorder, _ := serve(f.ops, "POST", "/v1/operator/tenants/t_nobody/adjustments", f.token,
		with(request, "idempotency_key", "adj-nobody-01")); recorder.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", recorder.Code)
	}

	// A debit is allowed and recorded the same way.
	debit := map[string]any{"amount_cents": -500, "reason": "Reverse duplicate goodwill", "idempotency_key": "adj-debit-0001"}
	if recorder, _ := serve(f.ops, "POST", path, f.token, debit); recorder.Code != http.StatusCreated {
		t.Fatalf("debit: %d", recorder.Code)
	}

	// The tenant sees the adjustment and its reason, and not who made it.
	recorder, body = serve(f.tenants, "GET", "/v1/billing", acme.APIKey, nil)
	if recorder.Code != http.StatusOK || body["balance"].(map[string]any)["micros"] != float64(20*Dollar) {
		t.Fatalf("tenant summary: %v", body)
	}
	if !strings.Contains(recorder.Body.String(), "relay outage") || strings.Contains(recorder.Body.String(), operatorEmail) {
		t.Errorf("tenant ledger: %s", recorder.Body.String())
	}

	// Every request has a "requested" row and every change an outcome.
	entries, _ := f.store.ListAudit(context.Background(), acme.Tenant.ID, 0)
	outcomes := map[string]int{}
	for _, entry := range entries {
		if entry.Operator != operatorEmail || entry.Action != "adjust_credit" || entry.Address != "203.0.113.9" {
			t.Errorf("audit entry: %+v", entry)
		}
		outcomes[strings.SplitN(entry.Outcome, ":", 2)[0]]++
	}
	attempts := 2 + 2 + 7 + 1
	if outcomes["requested"] != attempts || outcomes["ok"] != 3 || outcomes["failed"] != attempts-3 {
		t.Errorf("audit outcomes %v over %d entries", outcomes, len(entries))
	}
	recorder, body = serve(f.ops, "GET", "/v1/operator/audit?tenant="+acme.Tenant.ID+"&limit=5", f.token, nil)
	if recorder.Code != http.StatusOK || len(body["entries"].([]any)) != 5 {
		t.Errorf("audit read: %d %v", recorder.Code, body)
	}
}

func TestOperatorSuspendsAndReactivatesWithAReason(t *testing.T) {
	f := newOperatorFixture(t)
	ctx := context.Background()
	acme := f.signup(t, "Acme")
	path := "/v1/operator/tenants/" + acme.Tenant.ID + "/status"

	// Pending tenants are not suspended or activated from the console.
	if recorder, _ := serve(f.ops, "POST", path, f.token, map[string]string{"status": "active", "reason": "looks fine"}); recorder.Code != http.StatusConflict {
		t.Errorf("activating an unpaid tenant: %d", recorder.Code)
	}
	f.paid(t, acme.Tenant.ID, "pi_acme", 5000)
	if err := f.service.Admitted(ctx, acme.Tenant.ID); err != nil {
		t.Fatal(err)
	}
	if recorder, _ := serve(f.ops, "POST", path, f.token, map[string]string{"status": "suspended"}); recorder.Code != http.StatusBadRequest {
		t.Errorf("suspending without a reason: %d", recorder.Code)
	}
	if recorder, _ := serve(f.ops, "POST", path, f.token, map[string]string{"status": "deleted", "reason": "bye now"}); recorder.Code != http.StatusConflict {
		t.Errorf("deleting from the console: %d", recorder.Code)
	}
	recorder, body := serve(f.ops, "POST", path, f.token, map[string]string{"status": "suspended", "reason": "Chargeback under review"})
	if recorder.Code != http.StatusOK || body["status"] != "suspended" {
		t.Fatalf("suspend: %d %v", recorder.Code, body)
	}
	if err := f.service.Admitted(ctx, acme.Tenant.ID); !errors.Is(err, ErrSuspended) {
		t.Fatalf("a suspended tenant is admitted: %v", err)
	}
	recorder, body = serve(f.ops, "POST", path, f.token, map[string]string{"status": "active", "reason": "Chargeback resolved"})
	if recorder.Code != http.StatusOK || body["status"] != "active" {
		t.Fatalf("reactivate: %d %v", recorder.Code, body)
	}
	if err := f.service.Admitted(ctx, acme.Tenant.ID); err != nil {
		t.Fatalf("not admitted after reactivation: %v", err)
	}
	entries, _ := f.store.ListAudit(ctx, acme.Tenant.ID, 0)
	found := false
	for _, entry := range entries {
		found = found || (entry.Outcome == "ok" && strings.Contains(entry.Detail, "Chargeback under review"))
	}
	if !found {
		t.Errorf("the suspension and its reason are not in the audit log: %+v", entries)
	}
}

type failingAudit struct{ *Memory }

func (failingAudit) AppendAudit(context.Context, AuditEntry) error { return errors.New("disk full") }

func TestNothingHappensIfTheAuditLogCannotBeWritten(t *testing.T) {
	f := newFixture(t)
	store := failingAudit{f.store}
	service, err := New(Config{Store: store, Now: func() time.Time { return f.now }, Logger: f.service.log})
	if err != nil {
		t.Fatal(err)
	}
	signup, err := service.Signup(context.Background(), SignupRequest{Email: "a@example.com", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	ops, err := service.OperatorHandler(OperatorOptions{Verifier: &GoogleIdentity{
		ClientID: operatorClient, Allowed: ParseAllowList(operatorEmail),
		Keys: staticKeys{"k1": &googleKey.PublicKey}, Now: func() time.Time { return f.now },
	}})
	if err != nil {
		t.Fatal(err)
	}
	token := signRS256(googleKey, goodHeader(), goodClaims(f.now))
	recorder, _ := serve(ops, "POST", "/v1/operator/tenants/"+signup.Tenant.ID+"/adjustments", token,
		map[string]any{"amount_cents": 100, "reason": "should not land", "idempotency_key": "adj-unaudited-1"})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", recorder.Code)
	}
	if balance, _ := store.Balance(context.Background(), signup.Tenant.ID); balance != 0 {
		t.Fatalf("an unaudited adjustment landed: %s", balance)
	}
}

func TestOperatorHandlerNeedsAVerifierAndAnAuditingStore(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.OperatorHandler(OperatorOptions{}); err == nil {
		t.Error("built without a verifier")
	}
	type plainStore struct{ Store }
	service, _ := New(Config{Store: plainStore{NewMemory()}})
	if _, err := service.OperatorHandler(OperatorOptions{Verifier: &GoogleIdentity{}}); err == nil {
		t.Error("built on a store with no audit log")
	}
	// A verifier missing its configuration accepts nothing.
	if _, err := (&GoogleIdentity{Keys: staticKeys{}}).Verify(context.Background(), "a.b.c"); !errors.Is(err, ErrNotOperator) {
		t.Error("an unconfigured verifier accepted a token")
	}
}

func TestOperatorCORSAnswersOnlyTheAdminOrigin(t *testing.T) {
	f := newOperatorFixture(t)
	for origin, want := range map[string]string{adminOrigin: adminOrigin, "https://moqom.cloud": "", "https://evil.example": ""} {
		request := httptest.NewRequest(http.MethodOptions, "/v1/operator/tenants", nil)
		request.Header.Set("Origin", origin)
		request.Header.Set("Access-Control-Request-Method", "GET")
		recorder := httptest.NewRecorder()
		f.ops.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("%s: allowed %q", origin, got)
		}
	}
}

func TestOperatorChangesAreRateLimited(t *testing.T) {
	f := newOperatorFixture(t)
	acme := f.signup(t, "Acme")
	path := "/v1/operator/tenants/" + acme.Tenant.ID + "/adjustments"
	var last int
	for i := 0; i < 31; i++ {
		recorder, _ := serve(f.ops, "POST", path, f.token,
			map[string]any{"amount_cents": 100, "reason": "rate limit test", "idempotency_key": fmt.Sprintf("adj-rate-%04d", i)})
		last = recorder.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("the 31st change in an hour: %d", last)
	}
	if recorder, _ := serve(f.ops, "GET", "/v1/operator/tenants", f.token, nil); recorder.Code != http.StatusOK {
		t.Errorf("reads were limited with changes: %d", recorder.Code)
	}
}

func TestRemoteKeysFetchesAndCachesAKeySet(t *testing.T) {
	fetches := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		w.Header().Set("Cache-Control", "public, max-age=3600")
		e := big.NewInt(int64(googleKey.PublicKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(googleKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	}))
	defer jwks.Close()
	now := time.Unix(1_800_000_000, 0)
	keys := &RemoteKeys{URL: jwks.URL, Client: jwks.Client(), Now: func() time.Time { return now }}
	identity := &GoogleIdentity{ClientID: operatorClient, Allowed: ParseAllowList(operatorEmail), Keys: keys, Now: func() time.Time { return now }}
	token := signRS256(googleKey, goodHeader(), goodClaims(now))
	for i := 0; i < 3; i++ {
		if operator, err := identity.Verify(context.Background(), token); err != nil || operator.Email != operatorEmail {
			t.Fatalf("verify: %v %+v", err, operator)
		}
	}
	// An unknown kid refetches once, then not again within the minute.
	for i := 0; i < 3; i++ {
		_, _ = identity.Verify(context.Background(), signRS256(googleKey, map[string]any{"alg": "RS256", "kid": "nope"}, goodClaims(now)))
	}
	if fetches != 1 {
		t.Errorf("fetched %d times", fetches)
	}
}
