package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountExport(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var logs bytes.Buffer
	f.service.log = slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(f.service.Handler(nil))
	t.Cleanup(server.Close)

	mine, err := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	f.paid(t, mine.Tenant.ID, "pi_mine", 2000)
	if _, _, err := f.service.IssueKey(ctx, mine.Tenant.ID, "edge", KeyRelay); err != nil {
		t.Fatal(err)
	}
	theirs, _ := f.service.Signup(ctx, SignupRequest{Email: "other@example.com", Name: "Other"})
	f.paid(t, theirs.Tenant.ID, "pi_theirs", 9900)

	get := func(key string) (*http.Response, []byte) {
		t.Helper()
		request, _ := http.NewRequest("GET", server.URL+"/v1/account/export", nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response, body
	}

	if response, _ := get(""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("export without a key: %d", response.StatusCode)
	}

	response, body := get(mine.APIKey)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export %d %s", response.StatusCode, body)
	}
	var export AccountExport
	if err := json.Unmarshal(body, &export); err != nil {
		t.Fatal(err)
	}
	if export.Tenant.ID != mine.Tenant.ID || export.Tenant.Email != "dev@example.com" {
		t.Fatalf("tenant: %+v", export.Tenant)
	}
	if len(export.Keys) != 2 || len(export.Ledger) == 0 || len(export.Elsewhere) == 0 {
		t.Fatalf("export: %d keys, %d entries, %d elsewhere", len(export.Keys), len(export.Ledger), len(export.Elsewhere))
	}
	for _, entry := range export.Ledger {
		if entry.Reference == "pi_theirs" {
			t.Fatal("another tenant's payment is in this export")
		}
	}
	// A credential verifier and MOQOM's own costs are not the tenant's data.
	for _, absent := range []string{`"hash"`, `"cost"`, mine.Key.Hash} {
		if strings.Contains(string(body), absent) {
			t.Errorf("export contains %s", absent)
		}
	}
	if !strings.Contains(logs.String(), `"event":"dsar_export"`) {
		t.Fatalf("export not logged: %s", logs.String())
	}

	// A relay key is for usage reports and can read nothing.
	relay, plaintext, _ := NewKey(nil, mine.Tenant.ID, "edge-2", KeyRelay, f.now)
	_ = f.store.PutKey(ctx, relay)
	if response, _ := get(plaintext); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("relay key exported the account: %d", response.StatusCode)
	}
}

func TestRejectedWebhookSignatureIsLogged(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.service.log = slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(f.service.Handler(nil))
	t.Cleanup(server.Close)

	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{}}}`)
	request, _ := http.NewRequest("POST", server.URL+"/v1/stripe/webhook", bytes.NewReader(payload))
	request.Header.Set("Stripe-Signature", SignWebhook(payload, "whsec_forged", f.now))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(logs.String(), `"event":"stripe_webhook_signature_rejected"`) {
		t.Fatalf("status %d, logs %s", response.StatusCode, logs.String())
	}
}
