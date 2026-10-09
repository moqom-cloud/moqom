package billing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/server"
)

func newPepper(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mustPeppers(t *testing.T, lines ...string) Peppers {
	t.Helper()
	peppers, err := ParsePeppers(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return peppers
}

// newFixtureWith is newFixture with the hardening configuration applied.
func newFixtureWith(t *testing.T, configure func(*Config)) *fixture {
	t.Helper()
	f := &fixture{stripe: &fakeStripe{}, store: NewMemory(), now: time.Unix(1_800_000_000, 0)}
	stripe := httptest.NewServer(f.stripe)
	t.Cleanup(stripe.Close)
	config := Config{
		Store:         f.store,
		Stripe:        &Stripe{SecretKey: "sk_test", BaseURL: stripe.URL, Client: stripe.Client()},
		WebhookSecret: webhookSecret,
		PublicURL:     "https://moqom.test",
		Now:           func() time.Time { return f.now },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if configure != nil {
		configure(&config)
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func serveFrom(handler http.Handler, method, path, credential, remote string, forwarded []string, body any) (*httptest.ResponseRecorder, map[string]any) {
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = remote
	for _, value := range forwarded {
		request.Header.Add("X-Forwarded-For", value)
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func TestPeppersParse(t *testing.T) {
	one, two := newPepper(t), newPepper(t)
	peppers := mustPeppers(t, "# newest first", one, "", two)
	if len(peppers.Versions()) != 2 || peppers.Current() == "" || peppers.Current() == peppers.Versions()[1] {
		t.Fatalf("versions %v current %q", peppers.Versions(), peppers.Current())
	}
	if again := mustPeppers(t, one); again.Current() != peppers.Current() {
		t.Fatal("a pepper's version is not derived from it")
	}
	if none := mustPeppers(t, ""); none.Current() != "" {
		t.Fatal("an empty file produced a pepper")
	}
	for name, contents := range map[string]string{
		"short":    base64.StdEncoding.EncodeToString([]byte("too short")),
		"garbage":  "not base64 at all!",
		"repeated": one + "\n" + one,
	} {
		if _, err := ParsePeppers(contents); err == nil {
			t.Errorf("%s pepper accepted", name)
		}
	}
	if strings.Contains(peppers.Current(), one) {
		t.Fatal("the version id contains the pepper")
	}
}

func TestPepperedHashesAndLegacyMigration(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	old, oldPlain, _ := NewKey(nil, "t_1", "legacy", KeyBackend, time.Now())
	if err := store.PutKey(ctx, old); err != nil {
		t.Fatal(err)
	}

	first := newPepper(t)
	auth := &KeyAuth{Store: store, Peppers: mustPeppers(t, first)}
	use := Use{Operation: "GET /v1/keys"}

	// A key from before peppering still works, and is rehashed by its use.
	if _, err := auth.Authorize(ctx, oldPlain, use); err != nil {
		t.Fatalf("legacy key refused: %v", err)
	}
	migrated, _ := store.GetKey(ctx, old.ID)
	if migrated.PepperVersion != auth.Peppers.Current() || migrated.Hash == old.Hash {
		t.Fatalf("not rehashed: %+v", migrated)
	}
	if _, err := auth.Authorize(ctx, oldPlain, use); err != nil {
		t.Fatalf("rehashed key refused: %v", err)
	}
	if _, err := auth.Authorize(ctx, alterSecret(oldPlain), use); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong secret after rehash: %v", err)
	}
	// The bare hash no longer verifies the stored one: a dump without the
	// pepper is worth nothing.
	if _, err := ResolveKey(ctx, store, oldPlain); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("a peppered hash verified without the pepper: %v", err)
	}

	// Rotation: a new pepper on top, the old one still configured.
	second := newPepper(t)
	rotated := &KeyAuth{Store: store, Peppers: mustPeppers(t, second, first)}
	if _, err := rotated.Authorize(ctx, oldPlain, use); err != nil {
		t.Fatalf("refused across a pepper rotation: %v", err)
	}
	if again, _ := store.GetKey(ctx, old.ID); again.PepperVersion != rotated.Peppers.Current() {
		t.Fatalf("not moved to the new pepper: %+v", again)
	}

	// A hash under a pepper nobody holds any more fails closed.
	stranded := &KeyAuth{Store: store, Peppers: mustPeppers(t, first)}
	if _, err := stranded.Authorize(ctx, oldPlain, use); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("a key hashed under a removed pepper verified: %v", err)
	}
}

func TestIssuedKeysArePepperedAndNotStoredInTheClear(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, func(c *Config) { c.Peppers = mustPeppers(t, newPepper(t)) })
	signup, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := f.store.GetKey(ctx, signup.Key.ID)
	_, secret, _ := ParseKey(signup.APIKey)
	if stored.PepperVersion == "" || stored.Hash == hashSecret(secret) || strings.Contains(stored.Hash, secret) {
		t.Fatalf("stored %+v", stored)
	}
	if _, err := f.service.Keys().Authorize(ctx, signup.APIKey, Use{Operation: "GET /v1/billing"}); err != nil {
		t.Fatal(err)
	}
}

// grpcCall is the context the interceptor hands to Lookup: the method, the
// peer, and the credential.
type fakeTransport struct{ method string }

func (f fakeTransport) Method() string                  { return f.method }
func (f fakeTransport) SetHeader(metadata.MD) error     { return nil }
func (f fakeTransport) SendHeader(metadata.MD) error    { return nil }
func (f fakeTransport) SetTrailer(metadata.MD) error    { return nil }
func (f fakeTransport) SendHeaderMD(metadata.MD) error  { return nil }
func (f fakeTransport) SetSendCompress(s string) error  { return nil }
func (f fakeTransport) SendCompressSet(s string) string { return s }

func grpcCall(method, address, credential string) context.Context {
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), fakeTransport{method: method})
	if address != "" {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(address), Port: 4444}})
	}
	return metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+credential))
}

const (
	rpcMint         = "/moqom.admin.v1.Tokens/MintClientToken"
	rpcGetRoom      = "/moqom.admin.v1.Rooms/GetRoom"
	rpcCreateRoom   = "/moqom.admin.v1.Rooms/CreateRoom"
	rpcBan          = "/moqom.admin.v1.Moderation/Ban"
	rpcModLog       = "/moqom.admin.v1.Moderation/ListModerationLog"
	rpcUpdatePolicy = "/moqom.admin.v1.Rooms/UpdateRoomPolicy"
	rpcCreatePool   = "/moqom.admin.v1.RelayPools/CreateRelayPool"
	rpcListPools    = "/moqom.admin.v1.RelayPools/ListRelayPools"
	rpcSetNetwork   = "/moqom.admin.v1.RelayPools/SetTenantNetwork"
)

func intercept(t *testing.T, accounts server.Accounts, ctx context.Context) (server.ServiceAccount, codes.Code) {
	t.Helper()
	var got server.ServiceAccount
	_, err := server.Authenticator{Accounts: accounts}.Unary(ctx, nil, &grpc.UnaryServerInfo{},
		func(ctx context.Context, _ any) (any, error) {
			got, _ = server.PrincipalFrom(ctx)
			return nil, nil
		})
	return got, status.Code(err)
}

func TestScopedKeysOverGRPC(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	signup, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	tenant := signup.Tenant.ID
	_, mint, err := f.service.IssueKeyWith(ctx, tenant, KeySpec{Name: "agent", Kind: KeyMint})
	if err != nil {
		t.Fatal(err)
	}
	_, readonly, _ := f.service.IssueKeyWith(ctx, tenant, KeySpec{Name: "dashboards", Kind: KeyReadOnly})
	_, relay, _ := f.service.IssueKey(ctx, tenant, "edge", KeyRelay)
	accounts := server.AnyAccounts{server.NewStaticAccounts(map[string]server.ServiceAccount{"op": {Name: "op"}}), f.service.Accounts()}

	cases := []struct {
		name, method, credential string
		want                     codes.Code
	}{
		{"owner key creates rooms", rpcCreateRoom, signup.APIKey, codes.OK},
		{"owner key bans", rpcBan, signup.APIKey, codes.OK},
		{"mint key mints", rpcMint, mint, codes.OK},
		{"mint key reads rooms", rpcGetRoom, mint, codes.OK},
		{"mint key cannot create rooms", rpcCreateRoom, mint, codes.PermissionDenied},
		{"mint key cannot moderate", rpcBan, mint, codes.PermissionDenied},
		{"mint key cannot change policy", rpcUpdatePolicy, mint, codes.PermissionDenied},
		{"readonly key reads rooms", rpcGetRoom, readonly, codes.OK},
		{"readonly key reads the moderation log", rpcModLog, readonly, codes.OK},
		{"readonly key cannot mint", rpcMint, readonly, codes.PermissionDenied},
		{"readonly key cannot create rooms", rpcCreateRoom, readonly, codes.PermissionDenied},
		{"relay key is not an admin credential", rpcGetRoom, relay, codes.Unauthenticated},
		{"a wrong secret learns nothing", rpcGetRoom, alterSecret(mint), codes.Unauthenticated},
		{"an RPC added later is closed to scoped keys", "/moqom.admin.v1.Rooms/SomethingNew", mint, codes.PermissionDenied},
		{"owner key manages relay pools", rpcCreatePool, signup.APIKey, codes.OK},
		{"mint key cannot create a relay pool", rpcCreatePool, mint, codes.PermissionDenied},
		{"mint key cannot change the tenant's network", rpcSetNetwork, mint, codes.PermissionDenied},
		{"readonly key cannot read relay pools", rpcListPools, readonly, codes.PermissionDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, code := intercept(t, accounts, grpcCall(c.method, "10.0.0.5", c.credential)); code != c.want {
				t.Fatalf("got %s, want %s", code, c.want)
			}
		})
	}

	// The ceilings: a mint key's account can grant publishers and viewers,
	// never moderators or owners.
	account, _ := intercept(t, accounts, grpcCall(rpcMint, "10.0.0.5", mint))
	if account.Grants.IsOwner || account.Grants.CanGrant(control.CapKick) || !account.Grants.Publish.Has(control.PublishFull) ||
		!account.Grants.CanGrant(0) || account.Tenant != tenant {
		t.Fatalf("mint ceiling %+v", account)
	}
	owner, _ := intercept(t, accounts, grpcCall(rpcMint, "10.0.0.5", signup.APIKey))
	if !owner.Grants.IsOwner {
		t.Fatalf("the owner key lost owner standing: %+v", owner)
	}
}

func TestScopedKeysOverHTTP(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})

	recorder, body := serve(handler, "POST", "/v1/keys", signup.APIKey, map[string]any{"name": "agent", "kind": "mint"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("issue mint: %d %v", recorder.Code, body)
	}
	mint := body["api_key"].(string)
	view := body["key"].(map[string]any)
	if view["kind"] != "mint" || view["rate_limit_per_minute"].(float64) != DefaultScopedRateLimit {
		t.Fatalf("mint view %v", view)
	}
	_, body = serve(handler, "POST", "/v1/keys", signup.APIKey, map[string]any{"name": "dash", "kind": "readonly"})
	readonly := body["api_key"].(string)

	for _, c := range []struct {
		method, path, credential string
		body                     any
		want                     int
	}{
		{"GET", "/v1/keys", mint, nil, http.StatusForbidden},
		{"POST", "/v1/keys", mint, map[string]any{"name": "escalate", "kind": "backend"}, http.StatusForbidden},
		{"GET", "/v1/billing", mint, nil, http.StatusForbidden},
		{"POST", "/v1/billing/checkout", mint, map[string]any{"amount_cents": 1000}, http.StatusForbidden},
		{"GET", "/v1/billing", readonly, nil, http.StatusOK},
		{"GET", "/v1/billing/ledger", readonly, nil, http.StatusOK},
		{"PUT", "/v1/billing/spend-limit", readonly, map[string]any{"enabled": false}, http.StatusForbidden},
		{"GET", "/v1/keys", readonly, nil, http.StatusForbidden},
		{"GET", "/v1/account/export", readonly, nil, http.StatusForbidden},
		{"GET", "/v1/keys", signup.APIKey, nil, http.StatusOK},
	} {
		if recorder, body := serve(handler, c.method, c.path, c.credential, c.body); recorder.Code != c.want {
			t.Errorf("%s %s: %d %v, want %d", c.method, c.path, recorder.Code, body, c.want)
		}
	}

	// Revoking a scoped key is not a "last key" decision.
	_, keys := serve(handler, "GET", "/v1/keys", signup.APIKey, nil)
	for _, k := range keys["keys"].([]any) {
		entry := k.(map[string]any)
		if entry["kind"] == "mint" {
			if recorder, body := serve(handler, "DELETE", "/v1/keys/"+entry["id"].(string), signup.APIKey, nil); recorder.Code != http.StatusNoContent {
				t.Fatalf("revoke mint: %d %v", recorder.Code, body)
			}
		}
	}
}

func TestKeyCreationValidatesExpiryNetworksAndRate(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	for name, body := range map[string]map[string]any{
		"expiry in the past": {"name": "a", "expires_at": f.now.Add(-time.Hour)},
		"expiry too soon":    {"name": "a", "expires_at": f.now.Add(time.Minute)},
		"expiry too far":     {"name": "a", "expires_at": f.now.Add(4 * 365 * 24 * time.Hour)},
		"a bad network":      {"name": "a", "allowed_networks": []string{"203.0.113.0/33"}},
		"not an address":     {"name": "a", "allowed_networks": []string{"office"}},
		"a negative rate":    {"name": "a", "rate_limit_per_minute": -1},
		"too high a rate":    {"name": "a", "rate_limit_per_minute": MaxRateLimit + 1},
		"an unknown kind":    {"name": "a", "kind": "superuser"},
		"too many networks":  {"name": "a", "allowed_networks": strings.Split(strings.Repeat("10.0.0.1,", 21), ",")[:21]},
	} {
		if recorder, response := serve(handler, "POST", "/v1/keys", signup.APIKey, body); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, recorder.Code, response)
		}
	}
	expires := f.now.Add(30 * 24 * time.Hour).UTC()
	recorder, body := serve(handler, "POST", "/v1/keys", signup.APIKey, map[string]any{
		"name": "ci", "kind": "backend", "expires_at": expires,
		"allowed_networks":      []string{"203.0.113.77/24", "198.51.100.4", "::ffff:192.0.2.1"},
		"rate_limit_per_minute": 30,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("%d %v", recorder.Code, body)
	}
	view := body["key"].(map[string]any)
	networks, _ := json.Marshal(view["allowed_networks"])
	if view["expires_at"] != expires.Format(time.RFC3339) || string(networks) != `["203.0.113.0/24","198.51.100.4/32","192.0.2.1/32"]` ||
		view["rate_limit_per_minute"].(float64) != 30 || view["expired"] != nil {
		t.Fatalf("view %v", view)
	}
}

func TestExpiredKeysStopWorking(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	week := KeySpec{Name: "week", Kind: KeyBackend, ExpiresAt: f.now.Add(7 * 24 * time.Hour)}
	issued, plaintext, err := f.service.IssueKeyWith(ctx, signup.Tenant.ID, week)
	if err != nil {
		t.Fatal(err)
	}
	renewing, _, _ := f.service.IssueKeyWith(ctx, signup.Tenant.ID, week)
	if recorder, _ := serve(handler, "GET", "/v1/keys", plaintext, nil); recorder.Code != http.StatusOK {
		t.Fatalf("before expiry: %d", recorder.Code)
	}

	// Rotation renews the lifetime rather than keeping the old date.
	f.now = f.now.Add(6 * 24 * time.Hour)
	renewed, _, _, err := f.service.RotateKey(ctx, signup.Tenant.ID, renewing.ID, time.Hour)
	if err != nil || !renewed.ExpiresAt.Equal(f.now.Add(7*24*time.Hour)) {
		t.Fatalf("renewed %+v %v", renewed, err)
	}

	f.now = f.now.Add(2 * 24 * time.Hour)
	recorder, body := serve(handler, "GET", "/v1/keys", plaintext, nil)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(body["error"].(string), "expired") {
		t.Fatalf("after expiry: %d %v", recorder.Code, body)
	}
	_, list := serve(handler, "GET", "/v1/keys", signup.APIKey, nil)
	found := false
	for _, k := range list["keys"].([]any) {
		if entry := k.(map[string]any); entry["id"] == issued.ID {
			found = entry["expired"] == true && entry["expires_at"] != nil
		}
	}
	if !found {
		t.Fatalf("the list does not show the expiry: %v", list)
	}
	// An expired key is not a working one for the last-key rule.
	keys, _ := f.store.ListKeys(ctx, signup.Tenant.ID)
	working := 0
	for _, k := range keys {
		if k.Kind == KeyBackend && k.WorksAt(f.now) {
			working++
		}
	}
	if working != 2 {
		t.Fatalf("%d working backend keys, want the signup key and the renewal", working)
	}
	if _, _, _, err := f.service.RotateKey(ctx, signup.Tenant.ID, issued.ID, time.Hour); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rotated an expired key: %v", err)
	}
}

func TestAllowedNetworks(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	networks, _ := ParseNetworks([]string{"198.51.100.0/24", "2001:db8::/32"})
	_, plaintext, err := f.service.IssueKeyWith(ctx, signup.Tenant.ID, KeySpec{Name: "office", Kind: KeyBackend, AllowedNetworks: networks})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, remote string
		forwarded    []string
		want         int
	}{
		{"direct from the network", "198.51.100.7:5000", nil, http.StatusOK},
		{"IPv6 in the network", "[2001:db8::1]:5000", nil, http.StatusOK},
		{"direct from elsewhere", "203.0.113.9:5000", nil, http.StatusForbidden},
		{"behind Google's balancer", "35.191.4.2:40000", []string{"198.51.100.7, 34.107.228.246"}, http.StatusOK},
		{"forged header behind the balancer", "35.191.4.2:40000", []string{"198.51.100.7, 203.0.113.9, 34.107.228.246"}, http.StatusForbidden},
		{"forged header from outside Google", "203.0.113.9:5000", []string{"198.51.100.7, 34.107.228.246"}, http.StatusForbidden},
	} {
		if recorder, body := serveFrom(handler, "GET", "/v1/keys", plaintext, c.remote, c.forwarded, nil); recorder.Code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, recorder.Code, body, c.want)
		}
	}

	accounts := f.service.Accounts()
	if _, code := intercept(t, accounts, grpcCall(rpcGetRoom, "198.51.100.200", plaintext)); code != codes.OK {
		t.Errorf("gRPC from the network: %s", code)
	}
	if _, code := intercept(t, accounts, grpcCall(rpcGetRoom, "203.0.113.9", plaintext)); code != codes.PermissionDenied {
		t.Errorf("gRPC from elsewhere: %s", code)
	}
	if _, code := intercept(t, accounts, grpcCall(rpcGetRoom, "", plaintext)); code != codes.PermissionDenied {
		t.Errorf("gRPC with no peer: %s", code)
	}

	// The control plane's own HTTP routes are annotated with the same address.
	annotated := Annotate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := accounts.Lookup(r.Context(), bearer(r)); err != nil {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	if recorder, _ := serveFrom(annotated, "GET", "/v1/rooms", plaintext, "35.191.4.2:40000", []string{"198.51.100.7, 34.107.228.246"}, nil); recorder.Code != http.StatusOK {
		t.Errorf("annotated route from the network: %d", recorder.Code)
	}
	if recorder, _ := serveFrom(annotated, "GET", "/v1/rooms", plaintext, "203.0.113.9:5000", nil, nil); recorder.Code != http.StatusForbidden {
		t.Errorf("annotated route from elsewhere: %d", recorder.Code)
	}
}

func TestKeyRateLimits(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	_, plaintext, _ := f.service.IssueKeyWith(ctx, signup.Tenant.ID, KeySpec{Name: "slow", Kind: KeyBackend, RateLimit: 60})

	// 60 a minute allows a burst of ten seconds' worth.
	for i := range burstSeconds {
		if recorder, _ := serve(handler, "GET", "/v1/keys", plaintext, nil); recorder.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, recorder.Code)
		}
	}
	recorder, _ := serve(handler, "GET", "/v1/keys", plaintext, nil)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d", recorder.Code)
	}
	// The owner key has no limit of its own and is not slowed by another's.
	for range 50 {
		if recorder, _ := serve(handler, "GET", "/v1/keys", signup.APIKey, nil); recorder.Code != http.StatusOK {
			t.Fatalf("the unlimited key was limited: %d", recorder.Code)
		}
	}
	f.now = f.now.Add(time.Second)
	if recorder, _ := serve(handler, "GET", "/v1/keys", plaintext, nil); recorder.Code != http.StatusOK {
		t.Fatalf("not refilled after a second: %d", recorder.Code)
	}
	if recorder, _ := serve(handler, "GET", "/v1/keys", plaintext, nil); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("refilled more than a second's worth: %d", recorder.Code)
	}
	_, mintKey, _ := f.service.IssueKeyWith(ctx, signup.Tenant.ID, KeySpec{Name: "agent", Kind: KeyMint, RateLimit: 6})
	accounts := f.service.Accounts()
	if _, code := intercept(t, accounts, grpcCall(rpcMint, "10.0.0.1", mintKey)); code != codes.OK {
		t.Fatalf("first mint: %s", code)
	}
	if _, code := intercept(t, accounts, grpcCall(rpcMint, "10.0.0.1", mintKey)); code != codes.ResourceExhausted {
		t.Fatalf("over the limit over gRPC: %s", code)
	}
}

func TestKeyAudit(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	handler := f.service.Handler(nil)
	acme, _ := f.service.Signup(ctx, SignupRequest{Email: "acme@example.com", Name: "Acme"})
	globex, _ := f.service.Signup(ctx, SignupRequest{Email: "globex@example.com", Name: "Globex"})
	_, readonly, _ := f.service.IssueKeyWith(ctx, acme.Tenant.ID, KeySpec{Name: "dash", Kind: KeyReadOnly})

	for range 3 {
		serve(handler, "GET", "/v1/billing", readonly, nil)
	}
	serve(handler, "GET", "/v1/keys", readonly, nil)
	serve(handler, "GET", "/v1/keys", alterSecret(readonly), nil)
	_, _ = f.service.Accounts().Lookup(grpcCall(rpcMint, "10.1.2.3", readonly), readonly)

	readonlyID, _, _ := ParseKey(readonly)
	recorder, body := serve(handler, "GET", "/v1/keys/"+readonlyID+"/audit", acme.APIKey, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %v", recorder.Code, body)
	}
	_, secret, _ := ParseKey(readonly)
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatal("the audit contains a secret")
	}
	counts := map[string]float64{}
	for _, e := range body["entries"].([]any) {
		entry := e.(map[string]any)
		if entry["key_id"] != readonlyID {
			t.Fatalf("another key's entry: %v", entry)
		}
		counts[entry["operation"].(string)+" "+entry["outcome"].(string)] += entry["count"].(float64)
		if entry["address"] == "" || entry["first_at"] == nil {
			t.Fatalf("entry %v", entry)
		}
	}
	want := map[string]float64{
		"GET /v1/billing ok":        3,
		"GET /v1/keys out_of_scope": 1,
		"GET /v1/keys bad_secret":   1,
		rpcMint + " out_of_scope":   1,
	}
	for name, count := range want {
		if counts[name] != count {
			t.Errorf("%s: %v, want %v (all %v)", name, counts[name], count, counts)
		}
	}

	if recorder, _ := serve(handler, "GET", "/v1/keys/"+readonlyID+"/audit", globex.APIKey, nil); recorder.Code != http.StatusNotFound {
		t.Errorf("another tenant read the audit: %d", recorder.Code)
	}
	if recorder, _ := serve(handler, "GET", "/v1/keys/"+readonlyID+"/audit", readonly, nil); recorder.Code != http.StatusForbidden {
		t.Errorf("a readonly key read the audit: %d", recorder.Code)
	}
	_, all := serve(handler, "GET", "/v1/audit?limit=1000", acme.APIKey, nil)
	keys := map[any]bool{}
	for _, e := range all["entries"].([]any) {
		keys[e.(map[string]any)["key_id"]] = true
	}
	if !keys[readonlyID] || !keys[acme.Key.ID] || len(keys) != 2 {
		t.Errorf("tenant audit keys %v", keys)
	}
	_, globexAudit := serve(handler, "GET", "/v1/audit", globex.APIKey, nil)
	for _, e := range globexAudit["entries"].([]any) {
		if e.(map[string]any)["key_id"] != globex.Key.ID {
			t.Errorf("globex sees %v", e)
		}
	}
	if recorder, _ := serve(handler, "GET", "/v1/audit?limit=0", acme.APIKey, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("limit 0: %d", recorder.Code)
	}
}

func TestKeyAuditFlushRetriesAFailedWrite(t *testing.T) {
	ctx := context.Background()
	store := &flakyKeyUse{Memory: NewMemory(), fail: true}
	auth := &KeyAuth{Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	key := Key{ID: "k1", Tenant: "t_1"}
	at := time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)
	auth.Record(key, Use{Operation: "GET /v1/keys", Address: "203.0.113.1"}, OutcomeOK, "", at)
	if err := auth.Flush(ctx); err == nil {
		t.Fatal("a failed write was not reported")
	}
	auth.Record(key, Use{Operation: "GET /v1/keys", Address: "203.0.113.1"}, OutcomeOK, "", at.Add(time.Second))
	store.fail = false
	if err := auth.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	uses, _ := store.ListKeyUse(ctx, "t_1", "", 0)
	if len(uses) != 1 || uses[0].Count != 2 || !uses[0].FirstAt.Equal(at) {
		t.Fatalf("uses %+v", uses)
	}
}

type flakyKeyUse struct {
	*Memory
	fail bool
}

func (f *flakyKeyUse) RecordKeyUse(ctx context.Context, uses []KeyUse) error {
	if f.fail {
		return errors.New("disk full")
	}
	return f.Memory.RecordKeyUse(ctx, uses)
}

func TestRotationKeepsTheKeysTerms(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	networks, _ := ParseNetworks([]string{"203.0.113.0/24"})
	old, _, _ := f.service.IssueKeyWith(ctx, signup.Tenant.ID, KeySpec{Name: "agent", Kind: KeyMint, AllowedNetworks: networks, RateLimit: 90})
	issued, _, _, err := f.service.RotateKey(ctx, signup.Tenant.ID, old.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Kind != KeyMint || issued.RateLimit != 90 || len(issued.AllowedNetworks) != 1 ||
		issued.AllowedNetworks[0] != netip.MustParsePrefix("203.0.113.0/24") || !issued.ExpiresAt.IsZero() {
		t.Fatalf("replacement %+v", issued)
	}
}

func TestLimiterRefillsAtTheStatedRate(t *testing.T) {
	var l keyLimiter
	now := time.Unix(0, 0)
	allowed := 0
	for range 1000 {
		if l.allow("k", 600, now) {
			allowed++
		}
	}
	if allowed != 100 {
		t.Fatalf("burst %d, want 100", allowed)
	}
	now = now.Add(time.Minute)
	allowed = 0
	for range 1000 {
		if l.allow("k", 600, now) {
			allowed++
		}
	}
	if allowed != 100 {
		t.Fatalf("after a minute idle %d, want the burst capped at 100", allowed)
	}
	if !l.allow("other", 1, now) || l.allow("other", 1, now) {
		t.Fatal("a 1-a-minute key allowed more than one")
	}
}
