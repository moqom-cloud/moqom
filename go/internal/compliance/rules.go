package compliance

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severity orders findings. The nightly job alerts on High and above.
type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityHigh     Severity = "HIGH"
	SeverityMedium   Severity = "MEDIUM"
	SeverityLow      Severity = "LOW"
	SeverityInfo     Severity = "INFO"
)

var severityRank = map[Severity]int{
	SeverityInfo: 0, SeverityLow: 1, SeverityMedium: 2, SeverityHigh: 3, SeverityCritical: 4,
}

// AtLeast reports whether s is as severe as other or more.
func (s Severity) AtLeast(other Severity) bool { return severityRank[s] >= severityRank[other] }

// ParseSeverity accepts a severity name in any case.
func ParseSeverity(name string) (Severity, error) {
	s := Severity(strings.ToUpper(strings.TrimSpace(name)))
	if _, ok := severityRank[s]; !ok {
		return "", fmt.Errorf("unknown severity %q", name)
	}
	return s, nil
}

// Finding is one way the evidence departs from the baseline.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Resource string   `json:"resource"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	// Controls are the framework references the rule provides evidence for,
	// e.g. "SOC2:CC6.1", "ISO27001:A.8.20". The mapping is MOQOM's own and has
	// not been reviewed by an auditor.
	Controls []string `json:"controls"`
}

// Key identifies a finding across runs, which is what drift is computed on.
func (f Finding) Key() string { return f.Rule + "|" + f.Resource }

// Policy is the baseline. DefaultPolicy is what the nightly job runs with;
// every field is overridable by flag so an environment that differs on
// purpose says so in its job definition rather than in a code change.
type Policy struct {
	// AllowedOwners are the only members that may hold roles/owner.
	AllowedOwners []string
	// PublicBuckets may serve objects to the internet: the static sites.
	PublicBuckets []string

	// RequiredAuditLogs maps a service to the audit log types it must have,
	// either directly or through "allServices". The default deliberately does
	// not require DATA_READ on allServices: on GKE that is every get, list and
	// watch against the Kubernetes API, which is a large bill and no extra
	// assurance. Reads are required where reads are the event that matters.
	RequiredAuditLogs map[string][]string
	// AuditLogBucket is the long-retention log bucket Data Access logs are
	// routed to. Empty skips the retention and sink checks.
	AuditLogBucket           string
	MinAuditLogRetentionDays int

	// EvidenceBucket is where these reports are written. It must have a
	// retention policy and versioning, or the evidence can be edited.
	EvidenceBucket              string
	MinEvidenceRetentionSeconds int64

	MinRetainedBackups    int
	MinTransactionLogDays int
	MaxBackupAge          time.Duration
	// RequireDeletionProtection raises a missing deletion flag on Cloud SQL
	// from Low to High. Off in dev, where instances are torn down on purpose.
	RequireDeletionProtection bool

	// PublicIngress lists protocol:port entries that may be open to the
	// internet: the relay's QUIC port, the join endpoint, HTTPS.
	PublicIngress []string
	// ManagedFirewallPrefixes name rules a Google controller owns (GKE's
	// load balancer rules). They are reported, at Low, because the rule is
	// still open, but fixing it means changing the Service, not the rule.
	ManagedFirewallPrefixes []string

	MaxServiceAccountKeyAge time.Duration
	MaxSecretVersionAge     time.Duration
}

// DefaultPolicy is MOQOM's baseline. Every number here is quoted in
// docs/compliance and changing one should change the other.
func DefaultPolicy() Policy {
	return Policy{
		AllowedOwners:               []string{"user:elliott@moqom.cloud"},
		RequiredAuditLogs:           DefaultAuditLogs(),
		MinAuditLogRetentionDays:    400,
		MinEvidenceRetentionSeconds: 400 * 24 * 60 * 60,
		MinRetainedBackups:          7,
		MinTransactionLogDays:       7,
		MaxBackupAge:                36 * time.Hour,
		PublicIngress:               []string{"udp:443", "tcp:443", "tcp:8444"},
		ManagedFirewallPrefixes:     []string{"k8s-", "gke-"},
		MaxServiceAccountKeyAge:     90 * 24 * time.Hour,
		MaxSecretVersionAge:         365 * 24 * time.Hour,
	}
}

// DefaultAuditLogs is the audit configuration deploy/modules/compliance
// applies. The two must agree, or the check reports the module's own output as
// a finding.
func DefaultAuditLogs() map[string][]string {
	return map[string][]string{
		"allServices":                  {"ADMIN_READ", "DATA_WRITE"},
		"secretmanager.googleapis.com": {"DATA_READ"},
		"cloudsql.googleapis.com":      {"DATA_READ"},
		"iam.googleapis.com":           {"DATA_READ"},
		"storage.googleapis.com":       {"DATA_READ"},
	}
}

// Evaluate runs every rule. Pure: the same evidence, policy and time always
// produce the same findings, in the same order.
func Evaluate(e Evidence, p Policy, now time.Time) []Finding {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }

	collectionRules(e, add)
	iamRules(e, p, now, add)
	storageRules(e, p, add)
	sqlRules(e, p, now, add)
	networkRules(e, p, add)
	clusterRules(e, add)
	secretRules(e, p, now, add)
	loggingRules(e, p, add)
	monitoringRules(e, add)
	registryRules(e, add)

	sort.SliceStable(out, func(i, j int) bool {
		if severityRank[out[i].Severity] != severityRank[out[j].Severity] {
			return severityRank[out[i].Severity] > severityRank[out[j].Severity]
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

func collectionRules(e Evidence, add func(Finding)) {
	sections := make([]string, 0, len(e.Errors))
	for section := range e.Errors {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	for _, section := range sections {
		add(Finding{
			Rule: "COL-001", Severity: SeverityHigh, Resource: section,
			Title:    "Evidence could not be collected",
			Detail:   e.Errors[section] + " -- the rules for this section did not run, so its absence of findings means nothing.",
			Controls: []string{"SOC2:CC4.1", "ISO27001:A.5.36"},
		})
	}
}

func iamRules(e Evidence, p Policy, now time.Time, add func(Finding)) {
	allowedOwner := set(p.AllowedOwners)
	for _, binding := range e.IAMPolicy.Bindings {
		for _, member := range binding.Members {
			switch {
			case member == "allUsers" || member == "allAuthenticatedUsers":
				add(Finding{
					Rule: "IAM-001", Severity: SeverityCritical, Resource: "project/" + binding.Role,
					Title:    "Project role granted to the public",
					Detail:   fmt.Sprintf("%s holds %s on the project.", member, binding.Role),
					Controls: []string{"SOC2:CC6.1", "SOC2:CC6.3", "ISO27001:A.5.15", "ISO27001:A.8.2"},
				})
			case binding.Role == "roles/owner" && !allowedOwner[member]:
				add(Finding{
					Rule: "IAM-002", Severity: SeverityHigh, Resource: member,
					Title:    "Unexpected project owner",
					Detail:   member + " holds roles/owner and is not in the allowed owner list.",
					Controls: []string{"SOC2:CC6.2", "SOC2:CC6.3", "ISO27001:A.8.2"},
				})
			case (binding.Role == "roles/editor" || binding.Role == "roles/owner") && strings.HasPrefix(member, "serviceAccount:"):
				add(Finding{
					Rule: "IAM-003", Severity: SeverityHigh, Resource: member,
					Title:    "Service account holds a basic role",
					Detail:   fmt.Sprintf("%s holds %s. Basic roles grant write on nearly every service; replace with the predefined roles the workload uses.", member, binding.Role),
					Controls: []string{"SOC2:CC6.3", "ISO27001:A.5.15", "ISO27001:A.8.2"},
				})
			}
		}
	}

	services := make([]string, 0, len(p.RequiredAuditLogs))
	for service := range p.RequiredAuditLogs {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		missing := missingAuditLogTypes(e.IAMPolicy.AuditConfigs, service, p.RequiredAuditLogs[service])
		if len(missing) > 0 {
			add(Finding{
				Rule: "IAM-004", Severity: SeverityHigh, Resource: "auditConfig/" + service,
				Title:    "Data Access audit logs not enabled",
				Detail:   fmt.Sprintf("%s is missing %s. Without them, reads of secrets and data leave no trail.", service, strings.Join(missing, ", ")),
				Controls: []string{"SOC2:CC7.2", "ISO27001:A.8.15", "ISO27001:A.8.16"},
			})
		}
	}

	for _, account := range e.ServiceAccounts {
		for _, key := range account.UserManagedKeys {
			if key.Disabled {
				continue
			}
			age := now.Sub(key.ValidAfterTime)
			severity := SeverityMedium
			detail := fmt.Sprintf("User-managed key %s on %s, %d days old. Workload Identity and federation make downloaded keys unnecessary here.", lastSegment(key.Name), account.Email, int(age.Hours()/24))
			if p.MaxServiceAccountKeyAge > 0 && age > p.MaxServiceAccountKeyAge {
				severity = SeverityHigh
				detail += fmt.Sprintf(" Older than the %d-day rotation limit.", int(p.MaxServiceAccountKeyAge.Hours()/24))
			}
			add(Finding{
				Rule: "IAM-005", Severity: severity, Resource: account.Email + "/" + lastSegment(key.Name),
				Title: "User-managed service account key", Detail: detail,
				Controls: []string{"SOC2:CC6.1", "ISO27001:A.5.17", "ISO27001:A.8.24"},
			})
		}
	}
}

// missingAuditLogTypes reports which of the wanted log types are not enabled
// for service. A type enabled on allServices counts for every service.
func missingAuditLogTypes(configs []AuditConfig, service string, wanted []string) []string {
	have := map[string]bool{}
	for _, config := range configs {
		if config.Service != service && config.Service != "allServices" {
			continue
		}
		for _, log := range config.AuditLogConfigs {
			have[log.LogType] = true
		}
	}
	var missing []string
	for _, want := range wanted {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

func storageRules(e Evidence, p Policy, add func(Finding)) {
	public := set(p.PublicBuckets)
	evidenceSeen := p.EvidenceBucket == ""
	for _, bucket := range e.Buckets {
		resource := "gs://" + bucket.Name
		if len(bucket.PublicMembers) > 0 && !public[bucket.Name] {
			add(Finding{
				Rule: "STO-001", Severity: SeverityCritical, Resource: resource,
				Title:    "Bucket readable by the public",
				Detail:   "Public bindings: " + strings.Join(bucket.PublicMembers, ", ") + ". The bucket is not in the public-bucket allow list.",
				Controls: []string{"SOC2:CC6.1", "SOC2:C1.1", "ISO27001:A.5.15", "ISO27001:A.8.3"},
			})
		}
		if bucket.PublicAccessPrevention != "enforced" && !public[bucket.Name] {
			add(Finding{
				Rule: "STO-002", Severity: SeverityMedium, Resource: resource,
				Title:    "Public access prevention not enforced",
				Detail:   fmt.Sprintf("publicAccessPrevention is %q. Nothing stops a future binding from making this bucket public.", bucket.PublicAccessPrevention),
				Controls: []string{"SOC2:CC6.1", "ISO27001:A.8.3"},
			})
		}
		if !bucket.UniformBucketLevelAccess {
			add(Finding{
				Rule: "STO-003", Severity: SeverityLow, Resource: resource,
				Title:    "Uniform bucket-level access disabled",
				Detail:   "Object ACLs can grant access that the bucket's IAM policy does not show.",
				Controls: []string{"SOC2:CC6.1", "ISO27001:A.8.3"},
			})
		}
		if bucket.Name == p.EvidenceBucket {
			evidenceSeen = true
			if !bucket.Versioning || bucket.RetentionPeriodSeconds < p.MinEvidenceRetentionSeconds {
				add(Finding{
					Rule: "STO-004", Severity: SeverityHigh, Resource: resource,
					Title: "Evidence bucket is not tamper-resistant",
					Detail: fmt.Sprintf("versioning=%v, retention=%ds (want versioning and at least %ds). Evidence that can be overwritten is not evidence.",
						bucket.Versioning, bucket.RetentionPeriodSeconds, p.MinEvidenceRetentionSeconds),
					Controls: []string{"SOC2:CC4.1", "ISO27001:A.5.28", "ISO27001:A.5.33"},
				})
			}
		}
	}
	if !evidenceSeen {
		add(Finding{
			Rule: "STO-004", Severity: SeverityHigh, Resource: "gs://" + p.EvidenceBucket,
			Title:    "Evidence bucket missing",
			Detail:   "The configured evidence bucket does not exist in this project.",
			Controls: []string{"SOC2:CC4.1", "ISO27001:A.5.28"},
		})
	}
}

func sqlRules(e Evidence, p Policy, now time.Time, add func(Finding)) {
	for _, instance := range e.SQLInstances {
		resource := "sql/" + instance.Name
		finding := func(rule string, severity Severity, title, detail string, controls ...string) {
			add(Finding{Rule: rule, Severity: severity, Resource: resource, Title: title, Detail: detail, Controls: controls})
		}
		if !instance.BackupEnabled {
			finding("SQL-001", SeverityCritical, "Automated backups disabled",
				"The ledger and moderation log have no restorable copy.", "SOC2:A1.2", "ISO27001:A.8.13")
		} else if instance.RetainedBackups < p.MinRetainedBackups {
			finding("SQL-001", SeverityMedium, "Too few retained backups",
				fmt.Sprintf("%d retained, baseline is %d. A problem noticed after a long weekend may already be unrecoverable.", instance.RetainedBackups, p.MinRetainedBackups),
				"SOC2:A1.2", "ISO27001:A.8.13")
		}
		if !instance.PointInTimeRecovery {
			finding("SQL-002", SeverityHigh, "Point-in-time recovery disabled",
				"Recovery is limited to the last daily backup.", "SOC2:A1.2", "ISO27001:A.8.13")
		} else if instance.TransactionLogRetentionDays < p.MinTransactionLogDays {
			finding("SQL-002", SeverityLow, "Short point-in-time recovery window",
				fmt.Sprintf("%d days of transaction logs, baseline is %d.", instance.TransactionLogRetentionDays, p.MinTransactionLogDays),
				"SOC2:A1.2", "ISO27001:A.8.13")
		}
		if instance.BackupEnabled {
			last, failed := latestBackup(instance.RecentBackups)
			switch {
			case failed != "":
				finding("SQL-003", SeverityHigh, "Most recent backup failed",
					"The latest backup run ended "+failed+".", "SOC2:A1.2", "SOC2:CC7.2", "ISO27001:A.8.13")
			case last.IsZero():
				finding("SQL-003", SeverityHigh, "No successful backup on record",
					"No successful backup run was returned.", "SOC2:A1.2", "ISO27001:A.8.13")
			case p.MaxBackupAge > 0 && now.Sub(last) > p.MaxBackupAge:
				finding("SQL-003", SeverityHigh, "Latest successful backup is stale",
					fmt.Sprintf("Last success %s, %.0f hours ago (limit %.0f).", last.Format(time.RFC3339), now.Sub(last).Hours(), p.MaxBackupAge.Hours()),
					"SOC2:A1.2", "ISO27001:A.8.13")
			}
		}
		if instance.PublicIPEnabled {
			finding("SQL-004", SeverityHigh, "Cloud SQL has a public IP",
				"The instance is reachable from outside the VPC.", "SOC2:CC6.6", "ISO27001:A.8.20", "ISO27001:A.8.22")
		}
		if instance.SSLMode != "ENCRYPTED_ONLY" && instance.SSLMode != "TRUSTED_CLIENT_CERTIFICATE_REQUIRED" {
			finding("SQL-005", SeverityHigh, "Cloud SQL accepts unencrypted connections",
				fmt.Sprintf("sslMode is %q.", instance.SSLMode), "SOC2:CC6.7", "ISO27001:A.8.24")
		}
		if !instance.DeletionProtection {
			severity := SeverityLow
			if p.RequireDeletionProtection {
				severity = SeverityHigh
			}
			finding("SQL-006", severity, "Deletion protection off",
				"One API call deletes the instance and its automated backups.", "SOC2:A1.2", "ISO27001:A.8.13")
		}
	}
}

// latestBackup returns the end time of the newest successful run, and the
// status of the newest run if that run failed.
func latestBackup(runs []BackupRun) (lastSuccess time.Time, failedStatus string) {
	var newest BackupRun
	for _, run := range runs {
		if run.EndTime.After(newest.EndTime) {
			newest = run
		}
		if run.Status == "SUCCESSFUL" && run.EndTime.After(lastSuccess) {
			lastSuccess = run.EndTime
		}
	}
	if newest.Status != "" && newest.Status != "SUCCESSFUL" && newest.Status != "RUNNING" && newest.Status != "ENQUEUED" {
		failedStatus = newest.Status
	}
	return lastSuccess, failedStatus
}

// sensitivePorts are never acceptable open to the internet.
var sensitivePorts = []struct {
	protocol string
	port     int
}{{"tcp", 22}, {"tcp", 3389}, {"tcp", 5432}, {"tcp", 3306}, {"tcp", 6443}, {"tcp", 10250}, {"tcp", 2379}}

func networkRules(e Evidence, p Policy, add func(Finding)) {
	permitted := set(p.PublicIngress)
	for _, rule := range e.Firewalls {
		if rule.Disabled || rule.Denied || rule.Direction != "INGRESS" || !worldOpen(rule.SourceRanges) {
			continue
		}
		var unexpected []string
		severity := SeverityLow
		for _, allowed := range rule.Allowed {
			if permitted[allowed] {
				continue
			}
			unexpected = append(unexpected, allowed)
			// ICMP from anywhere is noise rather than exposure; anything else
			// unexpected is High, and a port from sensitivePorts is Critical.
			if allowed != "icmp" && severity == SeverityLow {
				severity = SeverityHigh
			}
			for _, sensitive := range sensitivePorts {
				if exposes(allowed, sensitive.protocol, sensitive.port) {
					severity = SeverityCritical
				}
			}
		}
		resource := "firewall/" + rule.Name
		if len(unexpected) > 0 {
			detail := fmt.Sprintf("Open to the internet on %s (network %s, targets %s).",
				strings.Join(unexpected, ", "), lastSegment(rule.Network), orAll(rule.TargetTags))
			if hasPrefix(rule.Name, p.ManagedFirewallPrefixes) {
				severity = SeverityLow
				detail += " Managed by GKE for a Service; nodes are reached through the load balancer, so fix it on the Service (loadBalancerSourceRanges) rather than here."
			}
			add(Finding{
				Rule: "NET-001", Severity: severity, Resource: resource,
				Title: "Firewall rule open to the internet", Detail: detail,
				Controls: []string{"SOC2:CC6.6", "ISO27001:A.8.20", "ISO27001:A.8.22"},
			})
		}
		if !rule.LogsEnabled && !hasPrefix(rule.Name, p.ManagedFirewallPrefixes) {
			add(Finding{
				Rule: "NET-002", Severity: SeverityLow, Resource: resource,
				Title:    "Internet-facing firewall rule without logging",
				Detail:   "Connections admitted by this rule leave no record.",
				Controls: []string{"SOC2:CC7.2", "ISO27001:A.8.15"},
			})
		}
	}
	for _, network := range e.Networks {
		if network.Name == "default" {
			add(Finding{
				Rule: "NET-003", Severity: SeverityMedium, Resource: "network/default",
				Title:    "Default network exists",
				Detail:   "The auto-mode default network ships with permissive firewall rules and is not managed by Terraform. Delete it once nothing uses it.",
				Controls: []string{"SOC2:CC6.6", "ISO27001:A.8.9", "ISO27001:A.8.20"},
			})
		}
	}
	for _, policy := range e.SSLPolicies {
		if (policy.MinTLSVersion != "TLS_1_2" && policy.MinTLSVersion != "TLS_1_3") || policy.Profile == "COMPATIBLE" {
			add(Finding{
				Rule: "NET-004", Severity: SeverityHigh, Resource: "sslPolicy/" + policy.Name,
				Title:    "Weak TLS policy",
				Detail:   fmt.Sprintf("profile %s, minimum %s. Baseline is MODERN or RESTRICTED with TLS 1.2 or later.", policy.Profile, policy.MinTLSVersion),
				Controls: []string{"SOC2:CC6.7", "ISO27001:A.8.24"},
			})
		}
	}
}

func worldOpen(ranges []string) bool {
	for _, r := range ranges {
		if r == "0.0.0.0/0" || r == "::/0" {
			return true
		}
	}
	return false
}

// exposes reports whether an allowed entry ("tcp", "tcp:22", "tcp:0-65535",
// "all") admits protocol/port.
func exposes(allowed, protocol string, port int) bool {
	proto, ports, hasPorts := strings.Cut(allowed, ":")
	if proto != "all" && proto != protocol {
		return false
	}
	if !hasPorts || proto == "all" {
		return true
	}
	for _, span := range strings.Split(ports, ",") {
		low, high, isRange := strings.Cut(span, "-")
		lo, err := strconv.Atoi(low)
		if err != nil {
			continue
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(high); err != nil {
				continue
			}
		}
		if port >= lo && port <= hi {
			return true
		}
	}
	return false
}

func clusterRules(e Evidence, add func(Finding)) {
	for _, cluster := range e.Clusters {
		resource := "gke/" + cluster.Name
		if !cluster.PrivateNodes {
			add(Finding{Rule: "GKE-001", Severity: SeverityHigh, Resource: resource,
				Title: "Cluster nodes have public IPs", Detail: "enablePrivateNodes is false.",
				Controls: []string{"SOC2:CC6.6", "ISO27001:A.8.20"}})
		}
		if cluster.PublicEndpoint && !cluster.AuthorizedNetworks {
			add(Finding{Rule: "GKE-002", Severity: SeverityMedium, Resource: resource,
				Title:    "Control plane endpoint open to the internet",
				Detail:   "The public endpoint accepts connections from any address (authentication is still required). Restrict it with master authorized networks or use the DNS endpoint only.",
				Controls: []string{"SOC2:CC6.6", "ISO27001:A.8.20", "ISO27001:A.8.22"}})
		}
		if !cluster.WorkloadIdentity {
			add(Finding{Rule: "GKE-003", Severity: SeverityHigh, Resource: resource,
				Title: "Workload Identity disabled", Detail: "Pods would fall back to the node's identity.",
				Controls: []string{"SOC2:CC6.1", "ISO27001:A.5.17"}})
		}
		if cluster.ReleaseChannel == "" || cluster.ReleaseChannel == "UNSPECIFIED" {
			add(Finding{Rule: "GKE-004", Severity: SeverityMedium, Resource: resource,
				Title: "No release channel", Detail: "Security patches are not applied automatically.",
				Controls: []string{"SOC2:CC7.1", "ISO27001:A.8.8"}})
		}
		if !cluster.SecretsEncryptionCMEK {
			add(Finding{Rule: "GKE-005", Severity: SeverityInfo, Resource: resource,
				Title:    "Kubernetes Secrets not encrypted with a customer key",
				Detail:   "Secrets are encrypted at rest by Google's default keys only. Application-layer encryption with Cloud KMS is optional hardening.",
				Controls: []string{"ISO27001:A.8.24"}})
		}
	}
}

func secretRules(e Evidence, p Policy, now time.Time, add func(Finding)) {
	for _, secret := range e.Secrets {
		resource := "secret/" + secret.Name
		if len(secret.EnabledVersions) == 0 {
			add(Finding{Rule: "SEC-001", Severity: SeverityInfo, Resource: resource,
				Title: "Secret has no enabled version", Detail: "The container exists but holds nothing; whatever reads it will fail closed.",
				Controls: []string{"ISO27001:A.5.17"}})
			continue
		}
		newest := secret.EnabledVersions[0]
		for _, created := range secret.EnabledVersions {
			if created.After(newest) {
				newest = created
			}
		}
		if p.MaxSecretVersionAge > 0 && now.Sub(newest) > p.MaxSecretVersionAge {
			add(Finding{Rule: "SEC-002", Severity: SeverityMedium, Resource: resource,
				Title:    "Secret not rotated",
				Detail:   fmt.Sprintf("Newest enabled version is %d days old (limit %d).", int(now.Sub(newest).Hours()/24), int(p.MaxSecretVersionAge.Hours()/24)),
				Controls: []string{"SOC2:CC6.1", "ISO27001:A.5.17", "ISO27001:A.8.24"}})
		}
		if len(secret.EnabledVersions) > 2 {
			add(Finding{Rule: "SEC-003", Severity: SeverityLow, Resource: resource,
				Title:    "Old secret versions still enabled",
				Detail:   fmt.Sprintf("%d enabled versions. A rotated credential that stays enabled has not been rotated.", len(secret.EnabledVersions)),
				Controls: []string{"ISO27001:A.5.17"}})
		}
	}
}

func loggingRules(e Evidence, p Policy, add func(Finding)) {
	if p.AuditLogBucket == "" {
		return
	}
	var bucket *LogBucket
	for i := range e.LogBuckets {
		if e.LogBuckets[i].Name == p.AuditLogBucket {
			bucket = &e.LogBuckets[i]
		}
	}
	if bucket == nil {
		add(Finding{Rule: "LOG-001", Severity: SeverityHigh, Resource: "logBucket/" + p.AuditLogBucket,
			Title: "Audit log bucket missing", Detail: "Data Access logs fall back to _Default and its 30-day retention.",
			Controls: []string{"SOC2:CC7.2", "ISO27001:A.8.15", "ISO27001:A.5.28"}})
		return
	}
	if bucket.RetentionDays < p.MinAuditLogRetentionDays {
		add(Finding{Rule: "LOG-001", Severity: SeverityMedium, Resource: "logBucket/" + bucket.Name,
			Title:    "Audit log retention below baseline",
			Detail:   fmt.Sprintf("%d days, baseline %d.", bucket.RetentionDays, p.MinAuditLogRetentionDays),
			Controls: []string{"SOC2:CC7.2", "ISO27001:A.8.15"}})
	}
	routed := false
	for _, sink := range e.LogSinks {
		if !sink.Disabled && strings.HasSuffix(sink.Destination, "/buckets/"+bucket.Name) && strings.Contains(sink.Filter, "data_access") {
			routed = true
		}
	}
	if !routed {
		add(Finding{Rule: "LOG-002", Severity: SeverityHigh, Resource: "logBucket/" + bucket.Name,
			Title: "No sink routes Data Access logs to the audit bucket", Detail: "The bucket exists but nothing is written to it.",
			Controls: []string{"SOC2:CC7.2", "ISO27001:A.8.15"}})
	}
}

func monitoringRules(e Evidence, add func(Finding)) {
	enabledChannels := 0
	for _, channel := range e.NotificationChans {
		if channel.Enabled {
			enabledChannels++
		}
	}
	if enabledChannels == 0 {
		add(Finding{Rule: "MON-001", Severity: SeverityHigh, Resource: "monitoring/notificationChannels",
			Title: "No notification channel", Detail: "Alerts have nowhere to go.",
			Controls: []string{"SOC2:CC7.3", "ISO27001:A.5.25", "ISO27001:A.8.16"}})
	}
	routed := 0
	for _, policy := range e.AlertPolicies {
		if policy.Enabled && policy.Channels > 0 {
			routed++
		}
	}
	if routed == 0 {
		add(Finding{Rule: "MON-002", Severity: SeverityHigh, Resource: "monitoring/alertPolicies",
			Title: "No enabled alert policy notifies anyone", Detail: "Security events are logged but nothing pages.",
			Controls: []string{"SOC2:CC7.2", "SOC2:CC7.3", "ISO27001:A.8.16"}})
	}
}

func registryRules(e Evidence, add func(Finding)) {
	for _, repo := range e.Repositories {
		if repo.Format == "DOCKER" && repo.VulnerabilityScanning != "SCANNING_ACTIVE" {
			add(Finding{Rule: "REG-001", Severity: SeverityMedium, Resource: "artifactRegistry/" + repo.Name,
				Title:    "Container vulnerability scanning off",
				Detail:   fmt.Sprintf("enablementState is %q. Images are scanned only when the security pipeline runs, not continuously.", repo.VulnerabilityScanning),
				Controls: []string{"SOC2:CC7.1", "ISO27001:A.8.8"}})
		}
	}
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func hasPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func orAll(tags []string) string {
	if len(tags) == 0 {
		return "all instances"
	}
	return strings.Join(tags, ",")
}
