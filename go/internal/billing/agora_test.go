package billing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	agoraID     = "c0ffee0123456789customerid"
	agoraSecret = "s3cr3t-Agora-Customer-Secret-xyz"
	appCert     = "4855aaaabbbbccccddddeeeeffff0e2a"
	appID       = "eb00cd2b222a4eeaa24fc6046d90b227"
)

// fakeAgora is Agora's Console RESTful API as its documentation describes it.
type fakeAgora struct {
	mu       sync.Mutex
	queries  []string
	projects int
	// status, if set, answers every request on that path with it.
	status   map[string]int
	oversize bool
}

func (a *fakeAgora) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	user, pass, ok := r.BasicAuth()
	if !ok || user != agoraID || pass != agoraSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid credential ` + pass + `"}`))
		return
	}
	if code := a.status[r.URL.Path]; code != 0 {
		if code == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", code)
			return
		}
		w.WriteHeader(code)
		return
	}
	switch r.URL.Path {
	case "/dev/v1/projects":
		var projects []string
		for i := range max(a.projects, 2) {
			projects = append(projects, fmt.Sprintf(
				`{"id":"p%d","name":"project %d","vendor_key":%q,"sign_key":%q,"recording_server":null,"status":%d,"created":1637153755}`,
				i, i, appID, appCert, 1-i%2))
		}
		_, _ = fmt.Fprintf(w, `{"projects":[%s]}`, strings.Join(projects, ","))
	case "/dev/v3/usage":
		a.queries = append(a.queries, r.URL.RawQuery)
		if a.oversize {
			_, _ = w.Write(bytes.Repeat([]byte(" "), largestAgoraBody+10))
			return
		}
		if r.URL.Query().Get("project_id") == "p1" {
			_, _ = w.Write([]byte(`{"usages":[{"date":"2026-09-01T00:00:00.000Z","usage":{"durationAudioAll":600}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"durationAudioAll":{"en":"Total Audio Duration","unit":"second"}},"usages":[
			{"date":"2026-09-01T00:00:00.000Z","usage":{"durationAudioAll":779,"durationVideoHd":60000,"durationVideoHdp":600,
				"durationVideo1080P":1200,"durationVideo2K":0,"durationVideo4K":120,"durationSomethingNew":5}},
			{"date":1725235200,"usage":{"durationAudioAll":1,"durationVideoHd":-5,"durationVideo1080P":1.6}}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type agoraFixture struct {
	*operatorFixture
	agora *fakeAgora
	logs  *bytes.Buffer
}

func newAgoraFixture(t *testing.T) *agoraFixture {
	t.Helper()
	f := newOperatorFixture(t)
	agora := &fakeAgora{status: map[string]int{}}
	server := httptest.NewTLSServer(agora)
	t.Cleanup(server.Close)
	logs := &bytes.Buffer{}
	f.service.log = slog.New(slog.NewTextHandler(logs, nil))
	// The production client's redirect policy and timeout, with a transport
	// that trusts the test server's certificate and may dial loopback.
	client := newAgoraClient()
	client.base, client.pace = server.URL, 0
	client.http.Transport = server.Client().Transport
	f.service.agora = client
	f.tenants = f.service.Handler(nil)
	return &agoraFixture{operatorFixture: f, agora: agora, logs: logs}
}

func credentials(id, secret string) map[string]string {
	return map[string]string{"customer_id": id, "customer_secret": secret}
}

func (f *agoraFixture) assertNothingLeaked(t *testing.T, texts ...string) {
	t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte(agoraID + ":" + agoraSecret))
	for _, text := range append(texts, f.logs.String()) {
		for _, secret := range []string{agoraSecret, agoraID, basic, appCert, appID} {
			if strings.Contains(text, secret) {
				t.Errorf("%q leaked into %s", secret, text)
			}
		}
	}
}

func TestAgoraPreviewReadsProjectsAndUsageAndPricesThem(t *testing.T) {
	f := newAgoraFixture(t)
	acme := f.signup(t, "Acme")

	recorder, body := serve(f.tenants, "POST", "/v1/migrate/agora/preview", acme.APIKey, credentials(" "+agoraID+" ", agoraSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	f.assertNothingLeaked(t, recorder.Body.String())
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Error("the preview is cacheable")
	}

	// f.now is 2027-01-15: the 90 full days before it.
	if body["from_date"] != "2026-10-17" || body["to_date"] != "2027-01-14" || body["estimate"] != true {
		t.Errorf("range %v to %v", body["from_date"], body["to_date"])
	}
	if len(f.agora.queries) != 2 || f.agora.queries[0] != "business=default&from_date=2026-10-17&project_id=p0&to_date=2027-01-14" {
		t.Errorf("queries %v", f.agora.queries)
	}
	projects := body["projects"].([]any)
	if len(projects) != 2 || projects[0].(map[string]any)["name"] != "project 0" || projects[0].(map[string]any)["enabled"] != true ||
		projects[1].(map[string]any)["enabled"] != false {
		t.Errorf("projects %v", projects)
	}

	want := map[Tier]int64{TierAudio: 779 + 1 + 600, TierHD: 60000, TierFullHD: 600 + 1200 + 2, Tier2KPlus: 120}
	tiers := body["tiers"].([]any)
	if len(tiers) != len(want) {
		t.Fatalf("tiers %v", tiers)
	}
	var agora, estimate Micros
	for _, raw := range tiers {
		tier := raw.(map[string]any)
		name := Tier(tier["tier"].(string))
		seconds := int64(tier["seconds"].(float64))
		if seconds != want[name] {
			t.Errorf("%s: %d seconds, want %d", name, seconds, want[name])
		}
		if got := microsOf(t, tier["agora_list"]); got != Per1000Minutes(AgoraListPrice()[RoleHost][name], seconds) {
			t.Errorf("%s agora %s", name, got)
		}
		bytes := seconds * AssumedKbps[name] * 125
		if int64(tier["estimated_egress_bytes"].(float64)) != bytes {
			t.Errorf("%s bytes %v", name, tier["estimated_egress_bytes"])
		}
		moqom := DefaultPricing().Rate(Usage{Role: RoleHost, Tier: name, Seconds: seconds, EgressBytes: bytes, Hosting: HostingCloud}).Price
		if got := microsOf(t, tier["moqom_estimate"]); got != moqom {
			t.Errorf("%s estimate %s, want %s", name, got, moqom)
		}
		agora, estimate = agora+microsOf(t, tier["agora_list"]), estimate+microsOf(t, tier["moqom_estimate"])
	}
	total := body["total"].(map[string]any)
	if microsOf(t, total["agora_list"]) != agora || microsOf(t, total["savings"]) != agora-estimate || estimate >= agora {
		t.Errorf("total %v", total)
	}
	if microsOf(t, total["agora_list_if_broadcast_audience"]) >= agora {
		t.Errorf("broadcast list not lower: %v", total)
	}
	notes, _ := json.Marshal(body["notes"])
	if !strings.Contains(string(notes), "HDP") || !strings.Contains(string(notes), "list price") {
		t.Errorf("notes %s", notes)
	}
	if body["guide"] != migrationGuide || len(body["checklist"].([]any)) == 0 {
		t.Errorf("guide %v", body["guide"])
	}
}

func TestAgoraPreviewFailuresAreSafeToShow(t *testing.T) {
	f := newAgoraFixture(t)
	acme := f.signup(t, "Acme")
	preview := func(creds map[string]string) (*httptest.ResponseRecorder, map[string]any) {
		recorder, body := serve(f.tenants, "POST", "/v1/migrate/agora/preview", acme.APIKey, creds)
		f.assertNothingLeaked(t, recorder.Body.String())
		return recorder, body
	}

	// Rejected credentials are the tenant's mistake, and not a 401: the
	// console signs out on a 401.
	recorder, body := preview(credentials(agoraID, agoraSecret+"x"))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "rejected") {
		t.Errorf("rejected: %d %v", recorder.Code, body)
	}
	if strings.Contains(recorder.Body.String(), agoraSecret+"x") {
		t.Error("Agora's error body was echoed")
	}

	f.agora.status["/dev/v1/projects"] = http.StatusTooManyRequests
	if recorder, _ := preview(credentials(agoraID, agoraSecret)); recorder.Code != http.StatusTooManyRequests {
		t.Errorf("rate limited: %d", recorder.Code)
	}
	f.agora.status["/dev/v1/projects"] = http.StatusFound
	if recorder, body := preview(credentials(agoraID, agoraSecret)); recorder.Code != http.StatusBadGateway || !strings.Contains(body["error"].(string), "302") {
		t.Errorf("redirect: %d %v", recorder.Code, body)
	}
	delete(f.agora.status, "/dev/v1/projects")

	// One project's failure is reported on it; the rest still read.
	f.agora.oversize = true
	recorder, body = preview(credentials(agoraID, agoraSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("oversize: %d %v", recorder.Code, body)
	}
	for _, p := range body["projects"].([]any) {
		if !strings.Contains(p.(map[string]any)["error"].(string), "larger") {
			t.Errorf("oversize project %v", p)
		}
	}

	// Another tenant, because every attempt counts against the limit.
	globex := f.signup(t, "Globex")
	for name, bad := range map[string]any{
		"empty secret":  credentials(agoraID, ""),
		"colon in id":   credentials("a:b", agoraSecret),
		"control chars": credentials(agoraID, "abc\ndef"),
		"unknown field": map[string]string{"customer_id": agoraID, "customer_secret": agoraSecret, "url": "http://169.254.169.254/"},
	} {
		recorder, _ := serve(f.tenants, "POST", "/v1/migrate/agora/preview", globex.APIKey, bad)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, recorder.Code)
		}
		f.assertNothingLeaked(t, recorder.Body.String())
	}
}

func TestAgoraPreviewNeedsABackendKeyAndIsRateLimitedPerTenant(t *testing.T) {
	f := newAgoraFixture(t)
	acme, globex := f.signup(t, "Acme"), f.signup(t, "Globex")
	_, relayKey, _ := f.service.IssueKey(t.Context(), acme.Tenant.ID, "edge", KeyRelay)
	for name, credential := range map[string]string{"no key": "", "a relay key": relayKey, "an operator token": f.token} {
		if recorder, _ := serve(f.tenants, "POST", "/v1/migrate/agora/preview", credential, credentials(agoraID, agoraSecret)); recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
	for i := range 6 {
		if recorder, _ := serve(f.tenants, "POST", "/v1/migrate/agora/preview", acme.APIKey, credentials(agoraID, agoraSecret)); recorder.Code != http.StatusOK {
			t.Fatalf("preview %d: %d", i, recorder.Code)
		}
	}
	if recorder, _ := serve(f.tenants, "POST", "/v1/migrate/agora/preview", acme.APIKey, credentials(agoraID, agoraSecret)); recorder.Code != http.StatusTooManyRequests {
		t.Errorf("seventh preview: %d", recorder.Code)
	}
	if recorder, _ := serve(f.tenants, "POST", "/v1/migrate/agora/preview", globex.APIKey, credentials(agoraID, agoraSecret)); recorder.Code != http.StatusOK {
		t.Errorf("another tenant was limited: %d", recorder.Code)
	}
}

func TestAgoraPreviewReadsAtMostTwentyProjects(t *testing.T) {
	f := newAgoraFixture(t)
	acme := f.signup(t, "Acme")
	f.agora.projects = 25
	_, body := serve(f.tenants, "POST", "/v1/migrate/agora/preview", acme.APIKey, credentials(agoraID, agoraSecret))
	if len(body["projects"].([]any)) != mostAgoraProjects || body["projects_truncated"] != true || len(f.agora.queries) != mostAgoraProjects {
		t.Errorf("%d projects, %d queries, truncated %v", len(body["projects"].([]any)), len(f.agora.queries), body["projects_truncated"])
	}
}

func TestAgoraClientOnlyReachesAgora(t *testing.T) {
	client := newAgoraClient()
	if client.base != "https://api.agora.io" || client.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatalf("base %q", client.base)
	}
	for _, address := range []string{"127.0.0.1:443", "10.0.0.8:443", "192.168.1.1:443", "169.254.169.254:80",
		"100.64.0.1:443", "[::1]:443", "[fe80::1]:443", "[fd00::1]:443", "0.0.0.0:443"} {
		if publicAddressOnly("tcp", address, nil) == nil {
			t.Errorf("%s allowed", address)
		}
	}
	if err := publicAddressOnly("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("a public address refused: %v", err)
	}
	creds := AgoraCredentials{CustomerID: agoraID, CustomerSecret: agoraSecret}
	for _, text := range []string{fmt.Sprint(creds), fmt.Sprintf("%+v %#v", creds, creds)} {
		if strings.Contains(text, agoraSecret) || strings.Contains(text, agoraID) {
			t.Errorf("formatted as %s", text)
		}
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "credentials", creds)
	if strings.Contains(logs.String(), agoraSecret) {
		t.Errorf("logged as %s", logs.String())
	}
}
