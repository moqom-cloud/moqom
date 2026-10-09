package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHooksHostServesOnlyWebhooks(t *testing.T) {
	stripe, err := parseNetworks("3.18.12.63, 54.187.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	from := func(r *http.Request) string { return r.Header.Get("Test-Client") }
	handler := hooksOnly("hooks.moqom.cloud", stripe, from, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	for _, c := range []struct {
		method, host, target, client string
		want                         int
	}{
		{"POST", "hooks.moqom.cloud", "/v1/stripe/webhook", "3.18.12.63", http.StatusTeapot},
		{"POST", "hooks.moqom.cloud", "/v1/stripe/webhook", "54.187.205.235", http.StatusTeapot},
		{"POST", "hooks.moqom.cloud", "/v1/stripe/webhook", "203.0.113.9", http.StatusNotFound},
		{"POST", "hooks.moqom.cloud", "/v1/stripe/webhook", "", http.StatusNotFound},
		{"POST", "HOOKS.moqom.cloud:443", "/v1/github/secret-scanning", "203.0.113.9", http.StatusTeapot},
		{"POST", "hooks.moqom.cloud.", "/v1/stripe/webhook", "3.18.12.63", http.StatusTeapot},
		{"GET", "hooks.moqom.cloud", "/v1/stripe/webhook", "3.18.12.63", http.StatusNotFound},
		{"POST", "hooks.moqom.cloud", "/v1/stripe/webhook?x=1", "3.18.12.63", http.StatusNotFound},
		{"POST", "hooks.moqom.cloud", "/v1/signup", "3.18.12.63", http.StatusNotFound},
		{"POST", "hooks.moqom.cloud", "/v1/apple/sign-in", "203.0.113.9", http.StatusNotFound},
		{"GET", "hooks.moqom.cloud", "/mls/v1/groups", "203.0.113.9", http.StatusNotFound},
		{"GET", "hooks.moqom.cloud", "/", "203.0.113.9", http.StatusNotFound},
		{"POST", "api.moqom.cloud", "/v1/signup", "203.0.113.9", http.StatusTeapot},
	} {
		request := httptest.NewRequest(c.method, "https://"+c.host+c.target, nil)
		request.Host = c.host
		request.Header.Set("Test-Client", c.client)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != c.want {
			t.Errorf("%s %s%s from %q: %d, want %d", c.method, c.host, c.target, c.client, recorder.Code, c.want)
		}
	}
}

func TestParseNetworksRefusesNonsense(t *testing.T) {
	if _, err := parseNetworks("3.18.12.63,not-an-address"); err == nil {
		t.Fatal("parsed")
	}
}
