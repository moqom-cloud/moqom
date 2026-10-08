package compliance

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

// clean is a project that meets the baseline. Each test breaks one thing and
// asserts on exactly what that produces, so a rule that starts firing on the
// clean project fails every test rather than hiding in one.
func clean() Evidence {
	return Evidence{
		Project:     "moqom-test",
		CollectedAt: now,
		IAMPolicy: IAMPolicy{
			Bindings: []Binding{
				{Role: "roles/owner", Members: []string{"user:elliott@moqom.cloud"}},
				{Role: "roles/cloudsql.client", Members: []string{"serviceAccount:control@moqom-test.iam.gserviceaccount.com"}},
			},
			AuditConfigs: []AuditConfig{{Service: "allServices", AuditLogConfigs: []AuditLogConfig{
				{LogType: "ADMIN_READ"}, {LogType: "DATA_READ"}, {LogType: "DATA_WRITE"},
			}}},
		},
		ServiceAccounts: []ServiceAccount{{Email: "control@moqom-test.iam.gserviceaccount.com"}},
		Buckets: []Bucket{
			{Name: "evidence", PublicAccessPrevention: "enforced", UniformBucketLevelAccess: true, Versioning: true, RetentionPeriodSeconds: 400 * 86400},
			{Name: "sites", PublicAccessPrevention: "inherited", UniformBucketLevelAccess: true, PublicMembers: []string{"allUsers:roles/storage.objectViewer"}},
		},
		SQLInstances: []SQLInstance{{
			Name: "pg", BackupEnabled: true, PointInTimeRecovery: true, RetainedBackups: 14,
			TransactionLogRetentionDays: 7, SSLMode: "ENCRYPTED_ONLY", DeletionProtection: true,
			RecentBackups: []BackupRun{{Status: "SUCCESSFUL", EndTime: now.Add(-20 * time.Hour)}},
		}},
		Firewalls: []Firewall{
			{Name: "relay-quic", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"udp:443"}, LogsEnabled: true},
			{Name: "internal", Direction: "INGRESS", SourceRanges: []string{"10.0.0.0/8"}, Allowed: []string{"tcp", "udp"}},
			{Name: "deny-all", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Denied: true},
		},
		Networks:          []Network{{Name: "moqom"}},
		SSLPolicies:       []SSLPolicy{{Name: "sites", Profile: "MODERN", MinTLSVersion: "TLS_1_2"}},
		Secrets:           []Secret{{Name: "signing-key", EnabledVersions: []time.Time{now.Add(-30 * 24 * time.Hour)}}},
		LogBuckets:        []LogBucket{{Name: "audit", RetentionDays: 400}},
		LogSinks:          []LogSink{{Name: "audit", Destination: "logging.googleapis.com/projects/moqom-test/locations/global/buckets/audit", Filter: `log_id("cloudaudit.googleapis.com/data_access")`}},
		AlertPolicies:     []AlertPolicy{{DisplayName: "IAM changed", Enabled: true, Channels: 1}},
		NotificationChans: []NotificationChan{{DisplayName: "elliott", Type: "email", Enabled: true}},
		Clusters: []Cluster{{
			Name: "gke", PrivateNodes: true, PublicEndpoint: false, WorkloadIdentity: true,
			ReleaseChannel: "REGULAR", SecretsEncryptionCMEK: true,
		}},
		Repositories: []Repository{{Name: "images", Format: "DOCKER", VulnerabilityScanning: "SCANNING_ACTIVE"}},
	}
}

func policy() Policy {
	p := DefaultPolicy()
	p.PublicBuckets = []string{"sites"}
	p.EvidenceBucket = "evidence"
	p.AuditLogBucket = "audit"
	return p
}

func rules(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Rule+":"+string(f.Severity)+":"+f.Resource)
	}
	return out
}

func expect(t *testing.T, e Evidence, want ...string) {
	t.Helper()
	got := rules(Evaluate(e, policy(), now))
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestCleanProjectHasNoFindings(t *testing.T) {
	expect(t, clean())
}

func TestPublicProjectBindingIsCritical(t *testing.T) {
	e := clean()
	e.IAMPolicy.Bindings = append(e.IAMPolicy.Bindings, Binding{Role: "roles/viewer", Members: []string{"allUsers"}})
	expect(t, e, "IAM-001:CRITICAL:project/roles/viewer")
}

func TestUnexpectedOwnerAndBasicRoleOnServiceAccount(t *testing.T) {
	e := clean()
	e.IAMPolicy.Bindings = append(e.IAMPolicy.Bindings,
		Binding{Role: "roles/owner", Members: []string{"user:someone@example.com"}},
		Binding{Role: "roles/editor", Members: []string{"serviceAccount:123-compute@developer.gserviceaccount.com"}},
	)
	expect(t, e,
		"IAM-002:HIGH:user:someone@example.com",
		"IAM-003:HIGH:serviceAccount:123-compute@developer.gserviceaccount.com",
	)
}

func TestMissingDataAccessLogs(t *testing.T) {
	e := clean()
	e.IAMPolicy.AuditConfigs = nil
	expect(t, e,
		"IAM-004:HIGH:auditConfig/allServices",
		"IAM-004:HIGH:auditConfig/cloudsql.googleapis.com",
		"IAM-004:HIGH:auditConfig/iam.googleapis.com",
		"IAM-004:HIGH:auditConfig/secretmanager.googleapis.com",
		"IAM-004:HIGH:auditConfig/storage.googleapis.com",
	)

	e.IAMPolicy.AuditConfigs = []AuditConfig{{Service: "allServices", AuditLogConfigs: []AuditLogConfig{{LogType: "ADMIN_READ"}}}}
	p := policy()
	p.RequiredAuditLogs = map[string][]string{"allServices": {"ADMIN_READ", "DATA_READ", "DATA_WRITE"}}
	findings := Evaluate(e, p, now)
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, "DATA_READ, DATA_WRITE") {
		t.Fatalf("partial audit config: %+v", findings)
	}
}

// The module's configuration: writes everywhere, reads where reads matter.
func TestModuleAuditConfigPasses(t *testing.T) {
	e := clean()
	e.IAMPolicy.AuditConfigs = []AuditConfig{
		{Service: "allServices", AuditLogConfigs: []AuditLogConfig{{LogType: "ADMIN_READ"}, {LogType: "DATA_WRITE"}}},
		{Service: "iam.googleapis.com", AuditLogConfigs: []AuditLogConfig{{LogType: "DATA_READ"}}},
		{Service: "secretmanager.googleapis.com", AuditLogConfigs: []AuditLogConfig{{LogType: "DATA_READ"}}},
		{Service: "cloudsql.googleapis.com", AuditLogConfigs: []AuditLogConfig{{LogType: "DATA_READ"}}},
		{Service: "storage.googleapis.com", AuditLogConfigs: []AuditLogConfig{{LogType: "DATA_READ"}}},
	}
	expect(t, e)

	e.IAMPolicy.AuditConfigs = e.IAMPolicy.AuditConfigs[:4]
	expect(t, e, "IAM-004:HIGH:auditConfig/storage.googleapis.com")
}

func TestServiceAccountKeyAge(t *testing.T) {
	e := clean()
	e.ServiceAccounts[0].UserManagedKeys = []ServiceAccountKey{
		{Name: "projects/p/serviceAccounts/x/keys/young", ValidAfterTime: now.Add(-10 * 24 * time.Hour)},
		{Name: "projects/p/serviceAccounts/x/keys/old", ValidAfterTime: now.Add(-120 * 24 * time.Hour)},
		{Name: "projects/p/serviceAccounts/x/keys/off", ValidAfterTime: now.Add(-900 * 24 * time.Hour), Disabled: true},
	}
	expect(t, e,
		"IAM-005:HIGH:control@moqom-test.iam.gserviceaccount.com/old",
		"IAM-005:MEDIUM:control@moqom-test.iam.gserviceaccount.com/young",
	)
}

func TestPublicBucketOutsideAllowList(t *testing.T) {
	e := clean()
	e.Buckets = append(e.Buckets, Bucket{Name: "recordings", PublicAccessPrevention: "inherited", UniformBucketLevelAccess: false,
		PublicMembers: []string{"allUsers:roles/storage.objectViewer"}})
	expect(t, e,
		"STO-001:CRITICAL:gs://recordings",
		"STO-002:MEDIUM:gs://recordings",
		"STO-003:LOW:gs://recordings",
	)
}

func TestEvidenceBucketMustBeImmutable(t *testing.T) {
	e := clean()
	e.Buckets[0].RetentionPeriodSeconds = 0
	expect(t, e, "STO-004:HIGH:gs://evidence")

	e = clean()
	e.Buckets = e.Buckets[1:]
	expect(t, e, "STO-004:HIGH:gs://evidence")
}

// The dev instance as observed on 2026-10-06: three backups, a day of PITR,
// no deletion protection.
func TestSQLBackupPosture(t *testing.T) {
	e := clean()
	e.SQLInstances[0].RetainedBackups = 3
	e.SQLInstances[0].TransactionLogRetentionDays = 1
	e.SQLInstances[0].DeletionProtection = false
	expect(t, e,
		"SQL-001:MEDIUM:sql/pg",
		"SQL-002:LOW:sql/pg",
		"SQL-006:LOW:sql/pg",
	)

	p := policy()
	p.RequireDeletionProtection = true
	if got := rules(Evaluate(e, p, now)); got[0] != "SQL-006:HIGH:sql/pg" {
		t.Fatalf("prod policy: %v", got)
	}
}

func TestSQLBackupFailuresAndStaleness(t *testing.T) {
	e := clean()
	e.SQLInstances[0].RecentBackups = []BackupRun{
		{Status: "SUCCESSFUL", EndTime: now.Add(-48 * time.Hour)},
		{Status: "FAILED", EndTime: now.Add(-24 * time.Hour)},
	}
	expect(t, e, "SQL-003:HIGH:sql/pg")
	if f := Evaluate(e, policy(), now)[0]; !strings.Contains(f.Title, "failed") {
		t.Fatalf("want the failure reported, got %q", f.Title)
	}

	e.SQLInstances[0].RecentBackups = []BackupRun{{Status: "SUCCESSFUL", EndTime: now.Add(-40 * time.Hour)}}
	if f := Evaluate(e, policy(), now)[0]; !strings.Contains(f.Title, "stale") {
		t.Fatalf("want staleness, got %q", f.Title)
	}

	// A backup in progress is not a failure.
	e.SQLInstances[0].RecentBackups = []BackupRun{
		{Status: "SUCCESSFUL", EndTime: now.Add(-20 * time.Hour)},
		{Status: "RUNNING", EndTime: now.Add(-time.Minute)},
	}
	expect(t, e)

	e.SQLInstances[0].BackupEnabled = false
	expect(t, e, "SQL-001:CRITICAL:sql/pg")
}

func TestSQLNetworkExposure(t *testing.T) {
	e := clean()
	e.SQLInstances[0].PublicIPEnabled = true
	e.SQLInstances[0].SSLMode = "ALLOW_UNENCRYPTED_AND_ENCRYPTED"
	expect(t, e, "SQL-004:HIGH:sql/pg", "SQL-005:HIGH:sql/pg")
}

// The default network's rules as observed: SSH and RDP from anywhere.
func TestWorldOpenFirewallRules(t *testing.T) {
	e := clean()
	e.Networks = append(e.Networks, Network{Name: "default"})
	e.Firewalls = append(e.Firewalls,
		Firewall{Name: "default-allow-ssh", Network: "global/networks/default", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"tcp:22"}},
		Firewall{Name: "default-allow-icmp", Network: "global/networks/default", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"icmp"}, LogsEnabled: true},
		Firewall{Name: "k8s-fw-abc", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"tcp:8080", "tcp:8443"}},
		Firewall{Name: "join", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"tcp:8444"}, LogsEnabled: true},
		Firewall{Name: "old", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []string{"tcp:22"}, Disabled: true},
	)
	expect(t, e,
		"NET-001:CRITICAL:firewall/default-allow-ssh",
		"NET-003:MEDIUM:network/default",
		"NET-001:LOW:firewall/default-allow-icmp",
		"NET-001:LOW:firewall/k8s-fw-abc",
		"NET-002:LOW:firewall/default-allow-ssh",
	)
}

func TestExposes(t *testing.T) {
	cases := []struct {
		allowed string
		proto   string
		port    int
		want    bool
	}{
		{"tcp:22", "tcp", 22, true},
		{"tcp:0-65535", "tcp", 3389, true},
		{"tcp", "tcp", 5432, true},
		{"all", "tcp", 22, true},
		{"udp:0-65535", "tcp", 22, false},
		{"tcp:443", "tcp", 22, false},
		{"tcp:20-21", "tcp", 22, false},
		{"tcp:bogus", "tcp", 22, false},
	}
	for _, c := range cases {
		if got := exposes(c.allowed, c.proto, c.port); got != c.want {
			t.Errorf("exposes(%q, %s, %d) = %v", c.allowed, c.proto, c.port, got)
		}
	}
}

func TestWeakTLSPolicy(t *testing.T) {
	e := clean()
	e.SSLPolicies = []SSLPolicy{{Name: "legacy", Profile: "COMPATIBLE", MinTLSVersion: "TLS_1_0"}}
	expect(t, e, "NET-004:HIGH:sslPolicy/legacy")
}

// The dev cluster as observed: public endpoint, no authorized networks,
// Google-managed secret encryption.
func TestClusterPosture(t *testing.T) {
	e := clean()
	e.Clusters[0].PublicEndpoint = true
	e.Clusters[0].SecretsEncryptionCMEK = false
	expect(t, e, "GKE-002:MEDIUM:gke/gke", "GKE-005:INFO:gke/gke")

	e.Clusters[0] = Cluster{Name: "bare"}
	expect(t, e,
		"GKE-001:HIGH:gke/bare",
		"GKE-003:HIGH:gke/bare",
		"GKE-004:MEDIUM:gke/bare",
		"GKE-005:INFO:gke/bare",
	)
}

func TestSecretHygiene(t *testing.T) {
	e := clean()
	e.Secrets = []Secret{
		{Name: "empty"},
		{Name: "stale", EnabledVersions: []time.Time{now.Add(-400 * 24 * time.Hour)}},
		{Name: "unrevoked", EnabledVersions: []time.Time{now, now.Add(-time.Hour), now.Add(-2 * time.Hour)}},
	}
	expect(t, e,
		"SEC-002:MEDIUM:secret/stale",
		"SEC-003:LOW:secret/unrevoked",
		"SEC-001:INFO:secret/empty",
	)
}

// As observed: _Default at 30 days, nothing routed to a long-retention bucket.
func TestAuditLogRetention(t *testing.T) {
	e := clean()
	e.LogBuckets = []LogBucket{{Name: "_Default", RetentionDays: 30}}
	e.LogSinks = nil
	expect(t, e, "LOG-001:HIGH:logBucket/audit")

	e = clean()
	e.LogBuckets[0].RetentionDays = 30
	e.LogSinks[0].Disabled = true
	expect(t, e, "LOG-002:HIGH:logBucket/audit", "LOG-001:MEDIUM:logBucket/audit")
}

// As observed: no channels, no policies.
func TestNothingAlerts(t *testing.T) {
	e := clean()
	e.AlertPolicies = nil
	e.NotificationChans = nil
	expect(t, e, "MON-001:HIGH:monitoring/notificationChannels", "MON-002:HIGH:monitoring/alertPolicies")

	e = clean()
	e.AlertPolicies = []AlertPolicy{{DisplayName: "muted", Enabled: false, Channels: 1}, {DisplayName: "nowhere", Enabled: true}}
	expect(t, e, "MON-002:HIGH:monitoring/alertPolicies")
}

func TestRegistryScanning(t *testing.T) {
	e := clean()
	e.Repositories[0].VulnerabilityScanning = "SCANNING_DISABLED"
	expect(t, e, "REG-001:MEDIUM:artifactRegistry/images")
}

func TestCollectionErrorsAreFindings(t *testing.T) {
	e := clean()
	e.Firewalls = nil
	e.Errors = map[string]string{"firewalls": "HTTP 403"}
	expect(t, e, "COL-001:HIGH:firewalls")
}

func TestParseSeverity(t *testing.T) {
	if s, err := ParseSeverity(" high "); err != nil || s != SeverityHigh {
		t.Fatalf("got %v %v", s, err)
	}
	if _, err := ParseSeverity("urgent"); err == nil {
		t.Fatal("accepted an unknown severity")
	}
	if !SeverityCritical.AtLeast(SeverityHigh) || SeverityMedium.AtLeast(SeverityHigh) {
		t.Fatal("ordering")
	}
}

func TestEveryFindingNamesAControl(t *testing.T) {
	e := clean()
	e.IAMPolicy.AuditConfigs = nil
	e.Networks = append(e.Networks, Network{Name: "default"})
	e.Clusters[0] = Cluster{Name: "bare"}
	e.AlertPolicies, e.NotificationChans = nil, nil
	for _, f := range Evaluate(e, policy(), now) {
		if len(f.Controls) == 0 || f.Title == "" || f.Detail == "" {
			t.Errorf("%s is missing controls, title or detail", f.Key())
		}
	}
}
