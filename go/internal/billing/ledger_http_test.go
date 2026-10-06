package billing

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestLedgerRouteShowsOnlyTheCallersEntriesWithoutCost(t *testing.T) {
	f := newOperatorFixture(t)
	acme, globex := f.signup(t, "Acme"), f.signup(t, "Globex")
	f.paid(t, acme.Tenant.ID, "pi_acme", 5000)
	f.paid(t, globex.Tenant.ID, "pi_globex", 7000)
	_, relayKey, err := f.service.IssueKey(context.Background(), acme.Tenant.ID, "edge", KeyRelay)
	if err != nil {
		t.Fatal(err)
	}

	recorder, body := serve(f.tenants, "GET", "/v1/billing/ledger", acme.APIKey, nil)
	if recorder.Code != http.StatusOK || len(body["entries"].([]any)) != 1 || body["complete"] != true {
		t.Fatalf("%d %v", recorder.Code, body)
	}
	if strings.Contains(recorder.Body.String(), "pi_globex") || strings.Contains(recorder.Body.String(), `"cost"`) {
		t.Errorf("leaked: %s", recorder.Body.String())
	}
	for name, credential := range map[string]string{"no key": "", "a relay key": relayKey, "an operator token": f.token} {
		if recorder, _ := serve(f.tenants, "GET", "/v1/billing/ledger", credential, nil); recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
	for _, limit := range []string{"0", "5001", "x"} {
		if recorder, _ := serve(f.tenants, "GET", "/v1/billing/ledger?limit="+limit, acme.APIKey, nil); recorder.Code != http.StatusBadRequest {
			t.Errorf("limit %s: %d", limit, recorder.Code)
		}
	}
}
