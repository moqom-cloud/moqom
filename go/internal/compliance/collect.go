package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBases are the REST roots of the APIs the collector reads. Every call
// is a get or a list; the only write anywhere in this package is Upload, to
// the evidence bucket.
var DefaultBases = map[string]string{
	"crm":              "https://cloudresourcemanager.googleapis.com/v1",
	"storage":          "https://storage.googleapis.com/storage/v1",
	"upload":           "https://storage.googleapis.com/upload/storage/v1",
	"sql":              "https://sqladmin.googleapis.com/v1",
	"compute":          "https://compute.googleapis.com/compute/v1",
	"iam":              "https://iam.googleapis.com/v1",
	"secretmanager":    "https://secretmanager.googleapis.com/v1",
	"logging":          "https://logging.googleapis.com/v2",
	"monitoring":       "https://monitoring.googleapis.com/v3",
	"container":        "https://container.googleapis.com/v1",
	"artifactregistry": "https://artifactregistry.googleapis.com/v1",
}

// Collector reads a project's configuration over Google's REST APIs.
//
// Client must attach credentials; the nightly job uses Application Default
// Credentials, which on Cloud Run is the job's own service account. That
// account holds read-only roles (roles/viewer, roles/iam.securityReviewer)
// plus object create on the evidence bucket, so a bug here cannot change the
// project it is auditing.
type Collector struct {
	Client  *http.Client
	Project string
	// Bases overrides DefaultBases per API, for tests.
	Bases map[string]string
	// RegistryLocations are the Artifact Registry locations to list.
	RegistryLocations []string
	Now               func() time.Time

	// Domains are checked from outside: CAA, DNSSEC and Certificate
	// Transparency. Empty skips those sections.
	Domains []string
	// Public is the client for the services Domains are checked against. It
	// must not carry credentials; nil uses a plain client.
	Public *http.Client
	// CertSpotterToken raises Cert Spotter's rate limit; empty is anonymous.
	CertSpotterToken string
}

func (c *Collector) base(api string) string {
	if b, ok := c.Bases[api]; ok {
		return b
	}
	return DefaultBases[api]
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	URL    string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.Status, e.Body)
}

func (c *Collector) do(ctx context.Context, method, rawURL string, body io.Reader, contentType string, into any) error {
	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return send(c.Client, request, into)
}

func send(client *http.Client, request *http.Request, into any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		text := strings.TrimSpace(string(payload))
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		return &APIError{Status: response.StatusCode, URL: request.URL.Redacted(), Body: text}
	}
	if into == nil {
		return nil
	}
	if raw, ok := into.(*[]byte); ok {
		*raw = payload
		return nil
	}
	return json.Unmarshal(payload, into)
}

func (c *Collector) get(ctx context.Context, api, path string, query url.Values, into any) error {
	target := c.base(api) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, target, nil, "", into)
}

// list follows nextPageToken until it runs out. decode receives each page and
// returns that page's token.
func (c *Collector) list(ctx context.Context, api, path string, query url.Values, decode func([]byte) (string, error)) error {
	if query == nil {
		query = url.Values{}
	}
	for page := 0; page < 1000; page++ {
		var raw []byte
		if err := c.get(ctx, api, path, query, &raw); err != nil {
			return err
		}
		next, err := decode(raw)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		query.Set("pageToken", next)
	}
	return errors.New("more than 1000 pages; refusing to continue")
}

// Collect reads every section. A section that fails is recorded in
// Evidence.Errors and the rest still run.
func (c *Collector) Collect(ctx context.Context) Evidence {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	e := Evidence{Project: c.Project, CollectedAt: now().UTC(), Errors: map[string]string{}}
	sections := []struct {
		name string
		run  func(context.Context, *Evidence) error
	}{
		{"iam_policy", c.iamPolicy},
		{"service_accounts", c.serviceAccounts},
		{"buckets", c.buckets},
		{"sql_instances", c.sqlInstances},
		{"firewalls", c.firewalls},
		{"networks", c.networks},
		{"ssl_policies", c.sslPolicies},
		{"secrets", c.secrets},
		{"log_buckets", c.logBuckets},
		{"log_sinks", c.logSinks},
		{"alert_policies", c.alertPolicies},
		{"notification_channels", c.notificationChannels},
		{"clusters", c.clusters},
		{"repositories", c.repositories},
	}
	if len(c.Domains) > 0 {
		sections = append(sections, []struct {
			name string
			run  func(context.Context, *Evidence) error
		}{
			{"domain_dns", c.domainDNS},
			{"ct_issuances", c.issuances},
			{"ssl_certificates", c.ownCertificates},
		}...)
	}
	for _, section := range sections {
		if err := section.run(ctx, &e); err != nil {
			e.Errors[section.name] = err.Error()
		}
	}
	if len(e.Errors) == 0 {
		e.Errors = nil
	}
	return e
}

func (c *Collector) projectPath() string { return "/projects/" + url.PathEscape(c.Project) }

func (c *Collector) iamPolicy(ctx context.Context, e *Evidence) error {
	body := strings.NewReader(`{"options":{"requestedPolicyVersion":3}}`)
	return c.do(ctx, http.MethodPost, c.base("crm")+c.projectPath()+":getIamPolicy", body, "application/json", &e.IAMPolicy)
}

func (c *Collector) serviceAccounts(ctx context.Context, e *Evidence) error {
	e.ServiceAccounts = []ServiceAccount{}
	err := c.list(ctx, "iam", c.projectPath()+"/serviceAccounts", nil, func(raw []byte) (string, error) {
		var page struct {
			Accounts []struct {
				Email    string `json:"email"`
				Disabled bool   `json:"disabled"`
			} `json:"accounts"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, a := range page.Accounts {
			e.ServiceAccounts = append(e.ServiceAccounts, ServiceAccount{Email: a.Email, Disabled: a.Disabled, UserManagedKeys: []ServiceAccountKey{}})
		}
		return page.NextPageToken, nil
	})
	if err != nil {
		return err
	}
	for i := range e.ServiceAccounts {
		var keys struct {
			Keys []ServiceAccountKey `json:"keys"`
		}
		path := c.projectPath() + "/serviceAccounts/" + url.PathEscape(e.ServiceAccounts[i].Email) + "/keys"
		if err := c.get(ctx, "iam", path, url.Values{"keyTypes": {"USER_MANAGED"}}, &keys); err != nil {
			return fmt.Errorf("keys for %s: %w", e.ServiceAccounts[i].Email, err)
		}
		if keys.Keys != nil {
			e.ServiceAccounts[i].UserManagedKeys = keys.Keys
		}
	}
	return nil
}

func (c *Collector) buckets(ctx context.Context, e *Evidence) error {
	e.Buckets = []Bucket{}
	err := c.list(ctx, "storage", "/b", url.Values{"project": {c.Project}}, func(raw []byte) (string, error) {
		var page struct {
			Items []struct {
				Name             string `json:"name"`
				Location         string `json:"location"`
				IAMConfiguration struct {
					PublicAccessPrevention   string `json:"publicAccessPrevention"`
					UniformBucketLevelAccess struct {
						Enabled bool `json:"enabled"`
					} `json:"uniformBucketLevelAccess"`
				} `json:"iamConfiguration"`
				Versioning struct {
					Enabled bool `json:"enabled"`
				} `json:"versioning"`
				RetentionPolicy struct {
					RetentionPeriod string `json:"retentionPeriod"`
					IsLocked        bool   `json:"isLocked"`
				} `json:"retentionPolicy"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, b := range page.Items {
			retention, _ := strconv.ParseInt(b.RetentionPolicy.RetentionPeriod, 10, 64)
			e.Buckets = append(e.Buckets, Bucket{
				Name: b.Name, Location: b.Location,
				PublicAccessPrevention:   b.IAMConfiguration.PublicAccessPrevention,
				UniformBucketLevelAccess: b.IAMConfiguration.UniformBucketLevelAccess.Enabled,
				Versioning:               b.Versioning.Enabled,
				RetentionPeriodSeconds:   retention,
				RetentionLocked:          b.RetentionPolicy.IsLocked,
			})
		}
		return page.NextPageToken, nil
	})
	if err != nil {
		return err
	}
	for i := range e.Buckets {
		var policy struct {
			Bindings []Binding `json:"bindings"`
		}
		if err := c.get(ctx, "storage", "/b/"+url.PathEscape(e.Buckets[i].Name)+"/iam", nil, &policy); err != nil {
			return fmt.Errorf("iam for %s: %w", e.Buckets[i].Name, err)
		}
		for _, binding := range policy.Bindings {
			for _, member := range binding.Members {
				if member == "allUsers" || member == "allAuthenticatedUsers" {
					e.Buckets[i].PublicMembers = append(e.Buckets[i].PublicMembers, member+":"+binding.Role)
				}
			}
		}
	}
	return nil
}

func (c *Collector) sqlInstances(ctx context.Context, e *Evidence) error {
	e.SQLInstances = []SQLInstance{}
	err := c.list(ctx, "sql", c.projectPath()+"/instances", nil, func(raw []byte) (string, error) {
		var page struct {
			Items []struct {
				Name            string `json:"name"`
				Region          string `json:"region"`
				DatabaseVersion string `json:"databaseVersion"`
				Settings        struct {
					AvailabilityType          string `json:"availabilityType"`
					DeletionProtectionEnabled bool   `json:"deletionProtectionEnabled"`
					BackupConfiguration       struct {
						Enabled                     bool `json:"enabled"`
						PointInTimeRecoveryEnabled  bool `json:"pointInTimeRecoveryEnabled"`
						TransactionLogRetentionDays int  `json:"transactionLogRetentionDays"`
						BackupRetentionSettings     struct {
							RetainedBackups int `json:"retainedBackups"`
						} `json:"backupRetentionSettings"`
					} `json:"backupConfiguration"`
					IPConfiguration struct {
						IPv4Enabled bool   `json:"ipv4Enabled"`
						SSLMode     string `json:"sslMode"`
					} `json:"ipConfiguration"`
				} `json:"settings"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, i := range page.Items {
			s := i.Settings
			e.SQLInstances = append(e.SQLInstances, SQLInstance{
				Name: i.Name, Region: i.Region, DatabaseVersion: i.DatabaseVersion,
				AvailabilityType:            s.AvailabilityType,
				BackupEnabled:               s.BackupConfiguration.Enabled,
				PointInTimeRecovery:         s.BackupConfiguration.PointInTimeRecoveryEnabled,
				RetainedBackups:             s.BackupConfiguration.BackupRetentionSettings.RetainedBackups,
				TransactionLogRetentionDays: s.BackupConfiguration.TransactionLogRetentionDays,
				PublicIPEnabled:             s.IPConfiguration.IPv4Enabled,
				SSLMode:                     s.IPConfiguration.SSLMode,
				DeletionProtection:          s.DeletionProtectionEnabled,
				RecentBackups:               []BackupRun{},
			})
		}
		return page.NextPageToken, nil
	})
	if err != nil {
		return err
	}
	for i := range e.SQLInstances {
		var runs struct {
			Items []struct {
				Status  string    `json:"status"`
				Type    string    `json:"type"`
				EndTime time.Time `json:"endTime"`
			} `json:"items"`
		}
		path := c.projectPath() + "/instances/" + url.PathEscape(e.SQLInstances[i].Name) + "/backupRuns"
		if err := c.get(ctx, "sql", path, url.Values{"maxResults": {"10"}}, &runs); err != nil {
			return fmt.Errorf("backup runs for %s: %w", e.SQLInstances[i].Name, err)
		}
		for _, run := range runs.Items {
			e.SQLInstances[i].RecentBackups = append(e.SQLInstances[i].RecentBackups, BackupRun{Status: run.Status, Type: run.Type, EndTime: run.EndTime})
		}
	}
	return nil
}

func (c *Collector) firewalls(ctx context.Context, e *Evidence) error {
	e.Firewalls = []Firewall{}
	return c.list(ctx, "compute", c.projectPath()+"/global/firewalls", nil, func(raw []byte) (string, error) {
		type permission struct {
			Protocol string   `json:"IPProtocol"`
			Ports    []string `json:"ports"`
		}
		var page struct {
			Items []struct {
				Name         string       `json:"name"`
				Network      string       `json:"network"`
				Direction    string       `json:"direction"`
				Disabled     bool         `json:"disabled"`
				SourceRanges []string     `json:"sourceRanges"`
				Allowed      []permission `json:"allowed"`
				Denied       []permission `json:"denied"`
				TargetTags   []string     `json:"targetTags"`
				LogConfig    struct {
					Enable bool `json:"enable"`
				} `json:"logConfig"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, f := range page.Items {
			rule := Firewall{
				Name: f.Name, Network: f.Network, Direction: f.Direction, Disabled: f.Disabled,
				SourceRanges: f.SourceRanges, TargetTags: f.TargetTags, LogsEnabled: f.LogConfig.Enable,
				Denied: len(f.Denied) > 0,
			}
			for _, a := range f.Allowed {
				if len(a.Ports) == 0 {
					rule.Allowed = append(rule.Allowed, a.Protocol)
					continue
				}
				for _, port := range a.Ports {
					rule.Allowed = append(rule.Allowed, a.Protocol+":"+port)
				}
			}
			e.Firewalls = append(e.Firewalls, rule)
		}
		return page.NextPageToken, nil
	})
}

func (c *Collector) networks(ctx context.Context, e *Evidence) error {
	e.Networks = []Network{}
	return c.list(ctx, "compute", c.projectPath()+"/global/networks", nil, func(raw []byte) (string, error) {
		var page struct {
			Items         []Network `json:"items"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		e.Networks = append(e.Networks, page.Items...)
		return page.NextPageToken, nil
	})
}

func (c *Collector) sslPolicies(ctx context.Context, e *Evidence) error {
	e.SSLPolicies = []SSLPolicy{}
	return c.list(ctx, "compute", c.projectPath()+"/global/sslPolicies", nil, func(raw []byte) (string, error) {
		var page struct {
			Items []struct {
				Name          string `json:"name"`
				Profile       string `json:"profile"`
				MinTLSVersion string `json:"minTlsVersion"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, p := range page.Items {
			e.SSLPolicies = append(e.SSLPolicies, SSLPolicy{Name: p.Name, Profile: p.Profile, MinTLSVersion: p.MinTLSVersion})
		}
		return page.NextPageToken, nil
	})
}

func (c *Collector) secrets(ctx context.Context, e *Evidence) error {
	e.Secrets = []Secret{}
	var names []string
	err := c.list(ctx, "secretmanager", c.projectPath()+"/secrets", nil, func(raw []byte) (string, error) {
		var page struct {
			Secrets []struct {
				Name     string           `json:"name"`
				Rotation *json.RawMessage `json:"rotation"`
			} `json:"secrets"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, s := range page.Secrets {
			names = append(names, s.Name)
			e.Secrets = append(e.Secrets, Secret{Name: lastSegment(s.Name), HasRotation: s.Rotation != nil, EnabledVersions: []time.Time{}})
		}
		return page.NextPageToken, nil
	})
	if err != nil {
		return err
	}
	for i, name := range names {
		err := c.list(ctx, "secretmanager", "/"+name+"/versions", url.Values{"filter": {"state:ENABLED"}}, func(raw []byte) (string, error) {
			var page struct {
				Versions []struct {
					CreateTime time.Time `json:"createTime"`
				} `json:"versions"`
				NextPageToken string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return "", err
			}
			for _, v := range page.Versions {
				e.Secrets[i].EnabledVersions = append(e.Secrets[i].EnabledVersions, v.CreateTime)
			}
			return page.NextPageToken, nil
		})
		if err != nil {
			return fmt.Errorf("versions of %s: %w", lastSegment(name), err)
		}
		sort.Slice(e.Secrets[i].EnabledVersions, func(a, b int) bool {
			return e.Secrets[i].EnabledVersions[a].After(e.Secrets[i].EnabledVersions[b])
		})
	}
	return nil
}

func (c *Collector) logBuckets(ctx context.Context, e *Evidence) error {
	e.LogBuckets = []LogBucket{}
	return c.list(ctx, "logging", c.projectPath()+"/locations/-/buckets", nil, func(raw []byte) (string, error) {
		var page struct {
			Buckets []struct {
				Name          string `json:"name"`
				RetentionDays int    `json:"retentionDays"`
				Locked        bool   `json:"locked"`
			} `json:"buckets"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, b := range page.Buckets {
			e.LogBuckets = append(e.LogBuckets, LogBucket{Name: lastSegment(b.Name), RetentionDays: b.RetentionDays, Locked: b.Locked})
		}
		return page.NextPageToken, nil
	})
}

func (c *Collector) logSinks(ctx context.Context, e *Evidence) error {
	e.LogSinks = []LogSink{}
	return c.list(ctx, "logging", c.projectPath()+"/sinks", nil, func(raw []byte) (string, error) {
		var page struct {
			Sinks         []LogSink `json:"sinks"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		e.LogSinks = append(e.LogSinks, page.Sinks...)
		return page.NextPageToken, nil
	})
}

func (c *Collector) alertPolicies(ctx context.Context, e *Evidence) error {
	e.AlertPolicies = []AlertPolicy{}
	return c.list(ctx, "monitoring", c.projectPath()+"/alertPolicies", nil, func(raw []byte) (string, error) {
		var page struct {
			AlertPolicies []struct {
				DisplayName          string   `json:"displayName"`
				Enabled              *bool    `json:"enabled"`
				NotificationChannels []string `json:"notificationChannels"`
			} `json:"alertPolicies"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, p := range page.AlertPolicies {
			e.AlertPolicies = append(e.AlertPolicies, AlertPolicy{
				DisplayName: p.DisplayName, Enabled: p.Enabled == nil || *p.Enabled, Channels: len(p.NotificationChannels),
			})
		}
		return page.NextPageToken, nil
	})
}

func (c *Collector) notificationChannels(ctx context.Context, e *Evidence) error {
	e.NotificationChans = []NotificationChan{}
	return c.list(ctx, "monitoring", c.projectPath()+"/notificationChannels", nil, func(raw []byte) (string, error) {
		var page struct {
			NotificationChannels []struct {
				DisplayName string `json:"displayName"`
				Type        string `json:"type"`
				Enabled     *bool  `json:"enabled"`
			} `json:"notificationChannels"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, ch := range page.NotificationChannels {
			e.NotificationChans = append(e.NotificationChans, NotificationChan{
				DisplayName: ch.DisplayName, Type: ch.Type, Enabled: ch.Enabled == nil || *ch.Enabled,
			})
		}
		return page.NextPageToken, nil
	})
}

func (c *Collector) clusters(ctx context.Context, e *Evidence) error {
	e.Clusters = []Cluster{}
	var page struct {
		Clusters []struct {
			Name                 string `json:"name"`
			Location             string `json:"location"`
			PrivateClusterConfig struct {
				EnablePrivateNodes    bool `json:"enablePrivateNodes"`
				EnablePrivateEndpoint bool `json:"enablePrivateEndpoint"`
			} `json:"privateClusterConfig"`
			NetworkConfig struct {
				DefaultEnablePrivateNodes bool `json:"defaultEnablePrivateNodes"`
			} `json:"networkConfig"`
			ControlPlaneEndpointsConfig *struct {
				IPEndpointsConfig struct {
					EnablePublicEndpoint     *bool `json:"enablePublicEndpoint"`
					AuthorizedNetworksConfig struct {
						Enabled bool `json:"enabled"`
					} `json:"authorizedNetworksConfig"`
				} `json:"ipEndpointsConfig"`
			} `json:"controlPlaneEndpointsConfig"`
			MasterAuthorizedNetworksConfig struct {
				Enabled bool `json:"enabled"`
			} `json:"masterAuthorizedNetworksConfig"`
			WorkloadIdentityConfig struct {
				WorkloadPool string `json:"workloadPool"`
			} `json:"workloadIdentityConfig"`
			ReleaseChannel struct {
				Channel string `json:"channel"`
			} `json:"releaseChannel"`
			DatabaseEncryption struct {
				State string `json:"state"`
			} `json:"databaseEncryption"`
			SecurityPostureConfig struct {
				Mode              string `json:"mode"`
				VulnerabilityMode string `json:"vulnerabilityMode"`
			} `json:"securityPostureConfig"`
		} `json:"clusters"`
	}
	if err := c.get(ctx, "container", c.projectPath()+"/locations/-/clusters", nil, &page); err != nil {
		return err
	}
	for _, k := range page.Clusters {
		public := !k.PrivateClusterConfig.EnablePrivateEndpoint
		authorized := k.MasterAuthorizedNetworksConfig.Enabled
		if k.ControlPlaneEndpointsConfig != nil {
			ip := k.ControlPlaneEndpointsConfig.IPEndpointsConfig
			if ip.EnablePublicEndpoint != nil {
				public = *ip.EnablePublicEndpoint
			}
			authorized = authorized || ip.AuthorizedNetworksConfig.Enabled
		}
		e.Clusters = append(e.Clusters, Cluster{
			Name: k.Name, Location: k.Location,
			PrivateNodes:              k.PrivateClusterConfig.EnablePrivateNodes || k.NetworkConfig.DefaultEnablePrivateNodes,
			PublicEndpoint:            public,
			AuthorizedNetworks:        authorized,
			WorkloadIdentity:          k.WorkloadIdentityConfig.WorkloadPool != "",
			ReleaseChannel:            k.ReleaseChannel.Channel,
			SecretsEncryptionCMEK:     k.DatabaseEncryption.State == "ENCRYPTED",
			SecurityPostureMode:       k.SecurityPostureConfig.Mode,
			VulnerabilityScanningMode: k.SecurityPostureConfig.VulnerabilityMode,
		})
	}
	return nil
}

func (c *Collector) repositories(ctx context.Context, e *Evidence) error {
	e.Repositories = []Repository{}
	for _, location := range c.RegistryLocations {
		path := c.projectPath() + "/locations/" + url.PathEscape(location) + "/repositories"
		err := c.list(ctx, "artifactregistry", path, nil, func(raw []byte) (string, error) {
			var page struct {
				Repositories []struct {
					Name                        string `json:"name"`
					Format                      string `json:"format"`
					VulnerabilityScanningConfig struct {
						EnablementState string `json:"enablementState"`
					} `json:"vulnerabilityScanningConfig"`
				} `json:"repositories"`
				NextPageToken string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return "", err
			}
			for _, r := range page.Repositories {
				e.Repositories = append(e.Repositories, Repository{
					Name: lastSegment(r.Name), Format: r.Format, VulnerabilityScanning: r.VulnerabilityScanningConfig.EnablementState,
				})
			}
			return page.NextPageToken, nil
		})
		if err != nil {
			return fmt.Errorf("%s: %w", location, err)
		}
	}
	return nil
}

// Upload writes an object that must not already exist. ifGenerationMatch=0
// makes a second write of the same name fail rather than replace the first,
// which the bucket's retention policy would refuse anyway.
func (c *Collector) Upload(ctx context.Context, bucket, name string, body []byte) error {
	target := c.base("upload") + "/b/" + url.PathEscape(bucket) + "/o?" + url.Values{
		"uploadType": {"media"}, "name": {name}, "ifGenerationMatch": {"0"},
	}.Encode()
	return c.do(ctx, http.MethodPost, target, bytes.NewReader(body), "application/json", nil)
}

// Previous returns the newest report under prefix whose name sorts before
// current, or nil when there is none. Listing starts a little over a month
// back so the scan stays short as reports accumulate.
func (c *Collector) Previous(ctx context.Context, bucket, prefix, current string, since time.Time) (*Report, string, error) {
	query := url.Values{"prefix": {prefix}, "fields": {"items(name),nextPageToken"}}
	if !since.IsZero() {
		query.Set("startOffset", prefix+since.UTC().Format("2006/01/02"))
	}
	var newest string
	err := c.list(ctx, "storage", "/b/"+url.PathEscape(bucket)+"/o", query, func(raw []byte) (string, error) {
		var page struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, item := range page.Items {
			if item.Name < current && item.Name > newest && strings.HasSuffix(item.Name, ".json") {
				newest = item.Name
			}
		}
		return page.NextPageToken, nil
	})
	if err != nil || newest == "" {
		return nil, "", err
	}
	var report Report
	path := "/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(newest)
	if err := c.get(ctx, "storage", path, url.Values{"alt": {"media"}}, &report); err != nil {
		return nil, newest, err
	}
	return &report, newest, nil
}
