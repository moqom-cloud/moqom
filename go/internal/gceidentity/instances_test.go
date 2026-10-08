package gceidentity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInstancesListsEveryZoneAndPage(t *testing.T) {
	pages := map[string]string{
		"": `{"items":{
			"zones/us-west1-b":{"instances":[{"name":"moqom-dev-relay-us-west1-a","id":"1"}]},
			"zones/us-east1-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}
		},"nextPageToken":"p2"}`,
		"p2": `{"items":{"zones/us-west2-a":{"instances":[{"name":"moqom-dev-relay-us-west2-b","id":"2"},{"name":"other","id":"3"}]}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/p/aggregated/instances" || r.URL.Query().Get("returnPartialSuccess") != "false" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("pageToken")]))
	}))
	defer server.Close()

	live, err := Instances(context.Background(), server.Client(), server.URL, "p", "moqom-dev-relay-")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || live["moqom-dev-relay-us-west1-a"] != "1" || live["moqom-dev-relay-us-west2-b"] != "2" {
		t.Errorf("listed %v", live)
	}
}

func TestAnUnreadableZoneIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":{"zones/us-west1-b":{"warning":{"code":"UNREACHABLE"}}}}`))
	}))
	defer server.Close()
	if _, err := Instances(context.Background(), server.Client(), server.URL, "p", "x"); err == nil {
		t.Error("a partial listing was taken as complete")
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer failing.Close()
	if _, err := Instances(context.Background(), failing.Client(), failing.URL, "p", "x"); err == nil {
		t.Error("a refused listing was taken as empty")
	}
}
