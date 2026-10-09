package server_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sectoken "github.com/moqom-cloud/moqom-security/go/token"
	"github.com/moqom-cloud/moqom/go/internal/mls"
	"github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

const tenantRelayKey = "tenant-relay-key"

func newSealingHarness(t *testing.T, ownRelays ...string) (*harness, sectoken.SealKey) {
	t.Helper()
	key, err := sectoken.GenerateSealKey()
	if err != nil {
		t.Fatal(err)
	}
	return newConfiguredHarness(t, func(config *server.Config) {
		config.SealKeys = []sectoken.SealKey{key}
		config.SealTenants = ownRelays
		config.Accounts = server.AnyAccounts{config.Accounts, server.NewStaticAccounts(map[string]server.ServiceAccount{
			tenantRelayKey: {Name: "acme-relay", Tenant: tenant, IsRelay: true},
		})}
	}), key
}

func tenantKey(t *testing.T, master sectoken.SealKey, name string) sectoken.SealKey {
	t.Helper()
	key, err := token.TenantSealKey(master, name)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Every token leaves the control plane sealed, and opens with the
// deployment's key into the signed token it was.
func TestMintedTokensAreSealed(t *testing.T) {
	h, key := newSealingHarness(t)
	h.createRoom(t, "alice")

	minted := h.mintFor(t, "bob", roomName).GetToken()
	if !sectoken.IsSealed(minted) {
		t.Fatalf("minted %q, which is not sealed", minted[:min(len(minted), 40)])
	}
	opened, err := sectoken.Open(minted, key)
	if err != nil {
		t.Fatalf("opening with the deployment key: %v", err)
	}
	if got := scopeOf(t, opened).Subject; got != "bob" {
		t.Errorf("the opened token names %q, want bob", got)
	}

	// And the control plane's own routes take it as it is.
	request := httptest.NewRequest(http.MethodGet, mls.PathPrefix+"sync?wait=0", nil)
	request.Header.Set("Authorization", "Bearer "+minted)
	recorder := httptest.NewRecorder()
	h.server.KeyDeliveryHandler(mls.NewService(mls.NewMemory(), h.now)).ServeHTTP(recorder, request)
	if recorder.Code == http.StatusUnauthorized {
		t.Errorf("key delivery refused a sealed token: %s", recorder.Body)
	}
}

// A tenant with relays of its own has its tokens sealed with its own key, and
// that key opens nobody else's.
func TestATenantWithItsOwnRelaysHasItsOwnSealKey(t *testing.T) {
	h, key := newSealingHarness(t, tenant)
	h.createRoom(t, "alice")

	minted := h.mintFor(t, "bob", roomName).GetToken()
	if _, err := sectoken.Open(minted, key); err == nil {
		t.Error("the deployment key itself opened a token sealed for the tenant's relays")
	}
	if _, err := sectoken.Open(minted, tenantKey(t, key, "globex")); err == nil {
		t.Error("another tenant's key opened the token")
	}
	if _, err := sectoken.Open(minted, tenantKey(t, key, tenant)); err != nil {
		t.Errorf("the tenant's key did not open its token: %v", err)
	}
}

// The shared fleet holds the deployment key and every tenant key; a tenant's
// relay holds its own tenant's key and nothing else.
func TestRelaysHoldOnlyTheSealKeysForWhomTheyServe(t *testing.T) {
	h, key := newSealingHarness(t, tenant)
	own := tenantKey(t, key, tenant)

	shared := firstKeys(t, h.attachRelay(t))
	if !holds(shared, key) || !holds(shared, own) {
		t.Errorf("the shared fleet holds %d seal keys, want the deployment's and the tenant's", len(shared.GetSealKeys()))
	}
	if shared.GetRequireSealed() {
		t.Error("unsealed tokens are refused before the ones minted earlier can have expired")
	}

	scoped := firstKeys(t, h.attachRelayAs(t, tenantRelayKey, "relay-acme"))
	if len(scoped.GetSealKeys()) != 1 || !holds(scoped, own) {
		t.Errorf("the tenant's relay holds %d seal keys, want its tenant's alone", len(scoped.GetSealKeys()))
	}
	if holds(scoped, key) {
		t.Error("the tenant's relay was given the deployment key")
	}
}

func firstKeys(t *testing.T, relay *fakeRelay) *relaypb.VerificationKeys {
	t.Helper()
	defer relay.cancel()
	select {
	case keys := <-relay.keys:
		return keys
	case <-time.After(5 * time.Second):
		t.Fatal("no verification keys")
		return nil
	}
}

func holds(keys *relaypb.VerificationKeys, key sectoken.SealKey) bool {
	for _, held := range keys.GetSealKeys() {
		if bytes.Equal(held.GetKeyId(), key.ID()) && bytes.Equal(held.GetSecret(), key.Secret()) {
			return true
		}
	}
	return false
}
