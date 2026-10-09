package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pool relay is sent one tenant's keys and rooms, so an account that names
// a pool and could serve every tenant, or is not a relay at all, is a file
// that must not load.
func TestAPoolRelayAccountMustBeScopedToATenant(t *testing.T) {
	for _, c := range []struct {
		name, account, refusal string
	}{
		{"a tenant's pool relay loads", `{"name":"r","credential":"c","tenant":"acme","pool":"p1","is_relay":true}`, ""},
		{"a pool relay serving every tenant is refused", `{"name":"r","credential":"c","pool":"p1","is_relay":true}`, "not a relay scoped to a tenant"},
		{"a pool on a backend is refused", `{"name":"b","credential":"c","tenant":"acme","pool":"p1"}`, "not a relay scoped to a tenant"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			if err := os.WriteFile(path, []byte(`{"accounts":[`+c.account+`]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MOQOM_CONTROL_ACCOUNTS", path)
			accounts, tenants, err := serviceAccounts()
			if c.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), c.refusal) {
					t.Fatalf("loaded with %v, want a refusal saying %q", err, c.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			account, err := accounts.Lookup(context.Background(), "c")
			if err != nil || account.Pool != "p1" || account.Tenant != "acme" || !account.IsRelay {
				t.Errorf("loaded %+v (%v)", account, err)
			}
			if len(tenants) != 1 || tenants[0] != "acme" {
				t.Errorf("the tenants sealed for are %v, want [acme]", tenants)
			}
		})
	}
}
