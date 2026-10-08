// Package compliance collects configuration evidence from a Google Cloud
// project and evaluates it against MOQOM's control baseline.
//
// The split is deliberate. Collection talks to Google's REST APIs and is the
// part that can only be tested against a fake server; evaluation is a pure
// function from an Evidence snapshot to findings, which is where every rule
// lives and where the tests are. A rule that needs a network call to be tested
// is a rule nobody will test, so none of them make one.
//
// What this produces is evidence, not certification. A clean report says the
// configuration matched the baseline at the time it ran, and nothing about
// whether the baseline is sufficient. See docs/compliance/README.md.
package compliance

import "time"

// Evidence is one snapshot of a project's security-relevant configuration.
//
// Trimmed to the fields the rules read, plus enough identification to find the
// resource again. It is written into the report alongside the findings, so an
// auditor can check a finding against what was observed rather than taking the
// rule's word for it.
type Evidence struct {
	Project     string    `json:"project"`
	CollectedAt time.Time `json:"collected_at"`

	IAMPolicy         IAMPolicy          `json:"iam_policy"`
	ServiceAccounts   []ServiceAccount   `json:"service_accounts"`
	Buckets           []Bucket           `json:"buckets"`
	SQLInstances      []SQLInstance      `json:"sql_instances"`
	Firewalls         []Firewall         `json:"firewalls"`
	Networks          []Network          `json:"networks"`
	SSLPolicies       []SSLPolicy        `json:"ssl_policies"`
	Secrets           []Secret           `json:"secrets"`
	LogBuckets        []LogBucket        `json:"log_buckets"`
	LogSinks          []LogSink          `json:"log_sinks"`
	AlertPolicies     []AlertPolicy      `json:"alert_policies"`
	NotificationChans []NotificationChan `json:"notification_channels"`
	Clusters          []Cluster          `json:"clusters"`
	Repositories      []Repository       `json:"repositories"`
	DomainDNS         []DomainDNS        `json:"domain_dns,omitempty"`
	Issuances         []Issuance         `json:"ct_issuances,omitempty"`
	OwnCertificates   []OwnCertificate   `json:"ssl_certificates,omitempty"`

	// Errors records collectors that failed. A section that could not be read
	// is reported as a finding of its own rather than silently treated as
	// empty, because "no firewall rules" and "could not list firewall rules"
	// must not produce the same clean report.
	Errors map[string]string `json:"collection_errors,omitempty"`
}

type IAMPolicy struct {
	Bindings     []Binding     `json:"bindings"`
	AuditConfigs []AuditConfig `json:"auditConfigs"`
	Etag         string        `json:"etag,omitempty"`
}

type Binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

type AuditConfig struct {
	Service         string           `json:"service"`
	AuditLogConfigs []AuditLogConfig `json:"auditLogConfigs"`
}

type AuditLogConfig struct {
	LogType         string   `json:"logType"`
	ExemptedMembers []string `json:"exemptedMembers,omitempty"`
}

type ServiceAccount struct {
	Email    string `json:"email"`
	Disabled bool   `json:"disabled"`
	// UserManagedKeys are keys somebody downloaded. Google-managed keys are
	// rotated by Google and never leave it, so they are not collected.
	UserManagedKeys []ServiceAccountKey `json:"user_managed_keys"`
}

type ServiceAccountKey struct {
	Name           string    `json:"name"`
	ValidAfterTime time.Time `json:"validAfterTime"`
	Disabled       bool      `json:"disabled"`
}

type Bucket struct {
	Name                     string `json:"name"`
	Location                 string `json:"location"`
	PublicAccessPrevention   string `json:"public_access_prevention"`
	UniformBucketLevelAccess bool   `json:"uniform_bucket_level_access"`
	Versioning               bool   `json:"versioning"`
	RetentionPeriodSeconds   int64  `json:"retention_period_seconds"`
	RetentionLocked          bool   `json:"retention_locked"`
	// PublicMembers are allUsers or allAuthenticatedUsers bindings, with
	// their role, from the bucket's own IAM policy.
	PublicMembers []string `json:"public_members,omitempty"`
}

type SQLInstance struct {
	Name                        string      `json:"name"`
	Region                      string      `json:"region"`
	DatabaseVersion             string      `json:"database_version"`
	AvailabilityType            string      `json:"availability_type"`
	BackupEnabled               bool        `json:"backup_enabled"`
	PointInTimeRecovery         bool        `json:"point_in_time_recovery"`
	RetainedBackups             int         `json:"retained_backups"`
	TransactionLogRetentionDays int         `json:"transaction_log_retention_days"`
	PublicIPEnabled             bool        `json:"public_ip_enabled"`
	SSLMode                     string      `json:"ssl_mode"`
	DeletionProtection          bool        `json:"deletion_protection"`
	RecentBackups               []BackupRun `json:"recent_backups"`
}

type BackupRun struct {
	Status  string    `json:"status"`
	Type    string    `json:"type"`
	EndTime time.Time `json:"end_time"`
}

type Firewall struct {
	Name         string   `json:"name"`
	Network      string   `json:"network"`
	Direction    string   `json:"direction"`
	Disabled     bool     `json:"disabled"`
	SourceRanges []string `json:"source_ranges,omitempty"`
	// Allowed is protocol[:ports], one entry per protocol, e.g. "tcp:22",
	// "udp:443", "icmp", "all".
	Allowed     []string `json:"allowed,omitempty"`
	Denied      bool     `json:"denied"`
	TargetTags  []string `json:"target_tags,omitempty"`
	LogsEnabled bool     `json:"logs_enabled"`
}

type Network struct {
	Name string `json:"name"`
}

type SSLPolicy struct {
	Name          string `json:"name"`
	Profile       string `json:"profile"`
	MinTLSVersion string `json:"min_tls_version"`
}

type Secret struct {
	Name string `json:"name"`
	// EnabledVersions is the creation time of each enabled version, newest
	// first. Payloads are never read: the collector has no access to them.
	EnabledVersions []time.Time `json:"enabled_versions"`
	HasRotation     bool        `json:"has_rotation"`
}

type LogBucket struct {
	Name          string `json:"name"`
	RetentionDays int    `json:"retention_days"`
	Locked        bool   `json:"locked"`
}

type LogSink struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Filter      string `json:"filter"`
	Disabled    bool   `json:"disabled"`
}

type AlertPolicy struct {
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	Channels    int    `json:"channels"`
}

type NotificationChan struct {
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Enabled     bool   `json:"enabled"`
}

type Cluster struct {
	Name                      string `json:"name"`
	Location                  string `json:"location"`
	PrivateNodes              bool   `json:"private_nodes"`
	PublicEndpoint            bool   `json:"public_endpoint"`
	AuthorizedNetworks        bool   `json:"authorized_networks"`
	WorkloadIdentity          bool   `json:"workload_identity"`
	ReleaseChannel            string `json:"release_channel"`
	SecretsEncryptionCMEK     bool   `json:"secrets_encryption_cmek"`
	SecurityPostureMode       string `json:"security_posture_mode"`
	VulnerabilityScanningMode string `json:"vulnerability_scanning_mode"`
}

type Repository struct {
	Name                  string `json:"name"`
	Format                string `json:"format"`
	VulnerabilityScanning string `json:"vulnerability_scanning"`
}
