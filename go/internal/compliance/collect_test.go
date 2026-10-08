package compliance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle answers the paths the collector calls with canned JSON shaped
// like the real APIs' responses, including the fields the collector ignores.
type fakeGoogle struct {
	mu      sync.Mutex
	uploads map[string]string
	objects map[string]string
	calls   []string
	deny    map[string]bool
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	for prefix := range f.deny {
		if strings.HasPrefix(path, prefix) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":403,"message":"permission denied"}}`)
			return
		}
	}
	reply := func(body string) { io.WriteString(w, body) }
	switch {
	case path == "/crm/projects/p:getIamPolicy" && r.Method == http.MethodPost:
		reply(`{"version":1,"etag":"BwY=","bindings":[
			{"role":"roles/owner","members":["user:elliott@moqom.cloud"]},
			{"role":"roles/editor","members":["serviceAccount:1-compute@developer.gserviceaccount.com"]}]}`)
	case path == "/iam/projects/p/serviceAccounts":
		if r.URL.Query().Get("pageToken") == "" {
			reply(`{"accounts":[{"email":"a@p.iam.gserviceaccount.com","uniqueId":"1"}],"nextPageToken":"two"}`)
		} else {
			reply(`{"accounts":[{"email":"b@p.iam.gserviceaccount.com","disabled":true}]}`)
		}
	case strings.HasPrefix(path, "/iam/projects/p/serviceAccounts/") && strings.HasSuffix(path, "/keys"):
		if r.URL.Query().Get("keyTypes") != "USER_MANAGED" {
			http.Error(w, "want keyTypes", http.StatusBadRequest)
			return
		}
		if strings.Contains(path, "/a@") {
			reply(`{"keys":[{"name":"projects/p/serviceAccounts/a/keys/k1","validAfterTime":"2026-01-01T00:00:00Z","keyType":"USER_MANAGED"}]}`)
		} else {
			reply(`{}`)
		}
	case path == "/storage/b":
		reply(`{"kind":"storage#buckets","items":[{"name":"evidence","location":"US-WEST1",
			"iamConfiguration":{"publicAccessPrevention":"enforced","uniformBucketLevelAccess":{"enabled":true}},
			"versioning":{"enabled":true},"retentionPolicy":{"retentionPeriod":"34560000","isLocked":false}},
			{"name":"sites","location":"US","iamConfiguration":{"publicAccessPrevention":"inherited","uniformBucketLevelAccess":{"enabled":true}}}]}`)
	case path == "/storage/b/sites/iam":
		reply(`{"bindings":[{"role":"roles/storage.objectViewer","members":["allUsers"]}]}`)
	case strings.HasPrefix(path, "/storage/b/") && strings.HasSuffix(path, "/iam"):
		reply(`{"bindings":[]}`)
	case path == "/sql/projects/p/instances":
		reply(`{"items":[{"name":"pg","region":"us-west1","databaseVersion":"POSTGRES_16","settings":{
			"availabilityType":"ZONAL","deletionProtectionEnabled":false,
			"backupConfiguration":{"enabled":true,"pointInTimeRecoveryEnabled":true,"transactionLogRetentionDays":1,
				"backupRetentionSettings":{"retainedBackups":3,"retentionUnit":"COUNT"}},
			"ipConfiguration":{"ipv4Enabled":false,"sslMode":"ENCRYPTED_ONLY"}}}]}`)
	case path == "/sql/projects/p/instances/pg/backupRuns":
		reply(`{"items":[{"status":"SUCCESSFUL","type":"AUTOMATED","endTime":"2026-10-06T03:12:00.000Z"}]}`)
	case path == "/compute/projects/p/global/firewalls":
		reply(`{"items":[{"name":"default-allow-ssh","network":"https://x/global/networks/default","direction":"INGRESS",
			"sourceRanges":["0.0.0.0/0"],"allowed":[{"IPProtocol":"tcp","ports":["22"]}]},
			{"name":"internal","direction":"INGRESS","sourceRanges":["10.0.0.0/8"],"allowed":[{"IPProtocol":"tcp"},{"IPProtocol":"udp","ports":["1-65535"]}],
			"logConfig":{"enable":true}},
			{"name":"deny","direction":"EGRESS","denied":[{"IPProtocol":"all"}]}]}`)
	case path == "/compute/projects/p/global/networks":
		reply(`{"items":[{"name":"default","autoCreateSubnetworks":true},{"name":"moqom-dev"}]}`)
	case path == "/compute/projects/p/global/sslPolicies":
		reply(`{"items":[{"name":"sites","profile":"MODERN","minTlsVersion":"TLS_1_2"}]}`)
	case path == "/secretmanager/projects/p/secrets":
		reply(`{"secrets":[{"name":"projects/1/secrets/signing","replication":{"automatic":{}}},
			{"name":"projects/1/secrets/stripe","rotation":{"rotationPeriod":"7776000s"}}]}`)
	case path == "/secretmanager/projects/1/secrets/signing/versions":
		if r.URL.Query().Get("filter") != "state:ENABLED" {
			http.Error(w, "want filter", http.StatusBadRequest)
			return
		}
		reply(`{"versions":[{"name":"v/1","createTime":"2026-08-02T21:35:51Z","state":"ENABLED"},
			{"name":"v/2","createTime":"2026-09-02T21:35:51Z","state":"ENABLED"}]}`)
	case path == "/secretmanager/projects/1/secrets/stripe/versions":
		reply(`{}`)
	case path == "/logging/projects/p/locations/-/buckets":
		reply(`{"buckets":[{"name":"projects/p/locations/global/buckets/_Default","retentionDays":30},
			{"name":"projects/p/locations/global/buckets/_Required","retentionDays":400,"locked":true}]}`)
	case path == "/logging/projects/p/sinks":
		reply(`{"sinks":[{"name":"_Default","destination":"logging.googleapis.com/projects/p/locations/global/buckets/_Default","filter":"NOT LOG_ID(x)"}]}`)
	case path == "/monitoring/projects/p/alertPolicies":
		reply(`{"alertPolicies":[{"displayName":"cpu","enabled":false,"notificationChannels":["c/1"]}]}`)
	case path == "/monitoring/projects/p/notificationChannels":
		reply(`{}`)
	case path == "/container/projects/p/locations/-/clusters":
		reply(`{"clusters":[{"name":"moqom-dev-cluster","location":"us-west1","autopilot":{"enabled":true},
			"privateClusterConfig":{"enablePrivateNodes":true},
			"controlPlaneEndpointsConfig":{"ipEndpointsConfig":{"enabled":true,"enablePublicEndpoint":true,
				"authorizedNetworksConfig":{"privateEndpointEnforcementEnabled":true}}},
			"masterAuthorizedNetworksConfig":{"privateEndpointEnforcementEnabled":true},
			"workloadIdentityConfig":{"workloadPool":"p.svc.id.goog"},"releaseChannel":{"channel":"REGULAR"},
			"databaseEncryption":{"state":"DECRYPTED"},"securityPostureConfig":{"mode":"BASIC"}}]}`)
	case path == "/artifactregistry/projects/p/locations/us/repositories":
		reply(`{"repositories":[{"name":"projects/p/locations/us/repositories/moqom-dev","format":"DOCKER",
			"vulnerabilityScanningConfig":{"enablementState":"SCANNING_DISABLED"}}]}`)
	case path == "/upload/b/evidence/o" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		name := r.URL.Query().Get("name")
		if r.URL.Query().Get("ifGenerationMatch") != "0" {
			http.Error(w, "want ifGenerationMatch=0", http.StatusBadRequest)
			return
		}
		if _, exists := f.objects[name]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.uploads[name] = string(body)
		f.objects[name] = string(body)
		reply(`{"name":"` + name + `"}`)
	case path == "/storage/b/evidence/o":
		prefix, offset := r.URL.Query().Get("prefix"), r.URL.Query().Get("startOffset")
		var items []string
		for name := range f.objects {
			if strings.HasPrefix(name, prefix) && name >= offset {
				items = append(items, `{"name":"`+name+`"}`)
			}
		}
		reply(`{"items":[` + strings.Join(items, ",") + `]}`)
	case strings.HasPrefix(path, "/storage/b/evidence/o/"):
		name := strings.TrimPrefix(path, "/storage/b/evidence/o/")
		body, ok := f.objects[name]
		if !ok || r.URL.Query().Get("alt") != "media" {
			http.NotFound(w, r)
			return
		}
		reply(body)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotImplemented)
	}
}

func newCollector(t *testing.T, fake *fakeGoogle) *Collector {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	bases := map[string]string{}
	for api := range DefaultBases {
		bases[api] = server.URL + "/" + api
	}
	return &Collector{
		Client: server.Client(), Project: "p", Bases: bases,
		RegistryLocations: []string{"us"},
		Now:               func() time.Time { return now },
	}
}

func TestCollectParsesEverySection(t *testing.T) {
	fake := &fakeGoogle{uploads: map[string]string{}, objects: map[string]string{}}
	e := newCollector(t, fake).Collect(context.Background())

	if e.Errors != nil {
		t.Fatalf("collection errors: %v", e.Errors)
	}
	if len(e.IAMPolicy.Bindings) != 2 || len(e.ServiceAccounts) != 2 || len(e.ServiceAccounts[0].UserManagedKeys) != 1 {
		t.Fatalf("iam: %+v %+v", e.IAMPolicy, e.ServiceAccounts)
	}
	if !e.ServiceAccounts[1].Disabled || e.ServiceAccounts[1].UserManagedKeys == nil {
		t.Fatalf("second page of service accounts: %+v", e.ServiceAccounts[1])
	}
	if e.Buckets[0].RetentionPeriodSeconds != 34560000 || !e.Buckets[0].Versioning || len(e.Buckets[1].PublicMembers) != 1 {
		t.Fatalf("buckets: %+v", e.Buckets)
	}
	sql := e.SQLInstances[0]
	if sql.RetainedBackups != 3 || sql.TransactionLogRetentionDays != 1 || !sql.PointInTimeRecovery || len(sql.RecentBackups) != 1 {
		t.Fatalf("sql: %+v", sql)
	}
	if got := e.Firewalls[0].Allowed; len(got) != 1 || got[0] != "tcp:22" {
		t.Fatalf("firewall ports: %v", got)
	}
	if got := e.Firewalls[1].Allowed; len(got) != 2 || got[0] != "tcp" || got[1] != "udp:1-65535" {
		t.Fatalf("firewall protocols: %v", got)
	}
	if !e.Firewalls[2].Denied {
		t.Fatal("deny rule not marked")
	}
	if len(e.Secrets) != 2 || len(e.Secrets[0].EnabledVersions) != 2 || !e.Secrets[0].EnabledVersions[0].After(e.Secrets[0].EnabledVersions[1]) {
		t.Fatalf("secrets not newest-first: %+v", e.Secrets)
	}
	if e.Secrets[0].Name != "signing" || e.Secrets[0].HasRotation || !e.Secrets[1].HasRotation {
		t.Fatalf("secret metadata: %+v", e.Secrets)
	}
	if e.LogBuckets[0].Name != "_Default" || e.LogBuckets[1].RetentionDays != 400 {
		t.Fatalf("log buckets: %+v", e.LogBuckets)
	}
	if e.AlertPolicies[0].Enabled || len(e.NotificationChans) != 0 {
		t.Fatalf("monitoring: %+v %+v", e.AlertPolicies, e.NotificationChans)
	}
	k := e.Clusters[0]
	if !k.PrivateNodes || !k.PublicEndpoint || k.AuthorizedNetworks || !k.WorkloadIdentity || k.SecretsEncryptionCMEK {
		t.Fatalf("cluster: %+v", k)
	}
	if e.Repositories[0].Name != "moqom-dev" || e.Repositories[0].VulnerabilityScanning != "SCANNING_DISABLED" {
		t.Fatalf("registry: %+v", e.Repositories)
	}

	// The same fake evaluated end to end produces the findings the real
	// project did on the day this was written.
	p := DefaultPolicy()
	p.PublicBuckets = []string{"sites"}
	p.EvidenceBucket = "evidence"
	got := map[string]bool{}
	for _, f := range Evaluate(e, p, now) {
		got[f.Rule+":"+f.Resource] = true
	}
	for _, want := range []string{
		"IAM-003:serviceAccount:1-compute@developer.gserviceaccount.com",
		"IAM-004:auditConfig/allServices",
		"NET-001:firewall/default-allow-ssh",
		"NET-003:network/default",
		"SQL-001:sql/pg",
		"GKE-002:gke/moqom-dev-cluster",
		"MON-001:monitoring/notificationChannels",
		"MON-002:monitoring/alertPolicies",
		"REG-001:artifactRegistry/moqom-dev",
		"SEC-001:secret/stripe",
	} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

func TestCollectRecordsSectionsThatFail(t *testing.T) {
	fake := &fakeGoogle{uploads: map[string]string{}, objects: map[string]string{}, deny: map[string]bool{"/compute/": true}}
	e := newCollector(t, fake).Collect(context.Background())
	for _, section := range []string{"firewalls", "networks", "ssl_policies"} {
		if !strings.Contains(e.Errors[section], "HTTP 403") {
			t.Errorf("%s: %q", section, e.Errors[section])
		}
	}
	if _, failed := e.Errors["buckets"]; failed || len(e.Buckets) != 2 {
		t.Fatal("one failing API stopped the others")
	}
}

func TestUploadAndPrevious(t *testing.T) {
	fake := &fakeGoogle{uploads: map[string]string{}, objects: map[string]string{}}
	c := newCollector(t, fake)
	ctx := context.Background()
	prefix := ReportPrefix + "p/"

	if previous, _, err := c.Previous(ctx, "evidence", prefix, ObjectName("p", now), now.AddDate(0, 0, -40)); err != nil || previous != nil {
		t.Fatalf("empty bucket: %v %v", previous, err)
	}

	first := NewReport(Evidence{Project: "p"}, []Finding{{Rule: "MON-001", Severity: SeverityHigh, Resource: "x"}}, nil, "test", now.Add(-24*time.Hour))
	body, _ := json.Marshal(first)
	older := ObjectName("p", now.Add(-48*time.Hour))
	yesterday := ObjectName("p", now.Add(-24*time.Hour))
	if err := c.Upload(ctx, "evidence", older, []byte(`{"findings":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, "evidence", yesterday, body); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, "evidence", yesterday, body); err == nil {
		t.Fatal("overwrote an existing report")
	}

	previous, name, err := c.Previous(ctx, "evidence", prefix, ObjectName("p", now), now.AddDate(0, 0, -40))
	if err != nil || name != yesterday || previous == nil || len(previous.Findings) != 1 {
		t.Fatalf("previous: %q %+v %v", name, previous, err)
	}
}
