// Command moqom-compliance collects configuration evidence from a Google Cloud
// project, evaluates it against MOQOM's control baseline, and writes a dated
// JSON report to the evidence bucket.
//
// It runs nightly as a Cloud Run Job (deploy/modules/compliance) and can be run
// by hand with the caller's own credentials:
//
//	go run ./cmd/moqom-compliance -project=moqom-503700 -out=report.json
//
// Every finding is also logged as a structured line. Cloud Logging turns those
// into log-based metrics, and the alert policies in the compliance module page
// on `compliance_drift` -- a High or Critical finding that was not in the
// previous report -- rather than on every finding every night, which is how an
// alert channel gets muted.
//
// Exit status is 0 when the check ran, whatever it found; 1 when it could not
// write its report. -fail-on makes findings themselves fail the run, for use as
// a CI gate.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/moqom-cloud/moqom/go/internal/compliance"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: cloudLogging}))
	slog.SetDefault(logger)
	if err := run(); err != nil {
		slog.Error("compliance check failed", "event", "compliance_check_failed", "error", err.Error())
		os.Exit(1)
	}
}

// cloudLogging renames slog's keys to the ones Cloud Logging reads, so a line's
// level becomes the entry's severity rather than a field nobody filters on.
func cloudLogging(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		level := a.Value.String()
		if level == "WARN" {
			level = "WARNING"
		}
		return slog.String("severity", level)
	case slog.MessageKey:
		return slog.Attr{Key: "message", Value: a.Value}
	}
	return a
}

type options struct {
	project, evidenceBucket, out string
	policy                       compliance.Policy
	registryLocations            []string
	domains                      []string
	certSpotterToken             string
	alertOn                      compliance.Severity
	failOn                       compliance.Severity
}

func parse() (options, error) {
	var o options
	policy := compliance.DefaultPolicy()
	var publicBuckets, owners, registries, ingress, alertOn, failOn, auditBucket, domains, caa, knownKeys string
	var requireDeletion bool

	flag.StringVar(&o.project, "project", env("MOQOM_COMPLIANCE_PROJECT", os.Getenv("GOOGLE_CLOUD_PROJECT")), "project to audit")
	flag.StringVar(&o.evidenceBucket, "evidence-bucket", os.Getenv("MOQOM_EVIDENCE_BUCKET"), "bucket reports are written to; empty skips upload and drift")
	flag.StringVar(&o.out, "out", "", "also write the report to this file ('-' for stdout)")
	flag.StringVar(&auditBucket, "audit-log-bucket", os.Getenv("MOQOM_AUDIT_LOG_BUCKET"), "long-retention log bucket Data Access logs must reach")
	flag.StringVar(&publicBuckets, "public-buckets", os.Getenv("MOQOM_PUBLIC_BUCKETS"), "comma-separated buckets allowed to be public")
	flag.StringVar(&owners, "allowed-owners", env("MOQOM_ALLOWED_OWNERS", strings.Join(policy.AllowedOwners, ",")), "comma-separated members allowed roles/owner")
	flag.StringVar(&registries, "registry-locations", env("MOQOM_REGISTRY_LOCATIONS", "us"), "comma-separated Artifact Registry locations")
	flag.StringVar(&ingress, "public-ingress", env("MOQOM_PUBLIC_INGRESS", strings.Join(policy.PublicIngress, ",")), "protocol:port entries allowed from 0.0.0.0/0")
	flag.StringVar(&domains, "domains", os.Getenv("MOQOM_DOMAINS"), "comma-separated public domains whose CAA, DNSSEC and CT log are checked; empty skips")
	flag.StringVar(&caa, "caa-issuers", env("MOQOM_CAA_ISSUERS", strings.Join(policy.CAAIssuers, ",")), "certificate authorities CAA may name")
	flag.StringVar(&knownKeys, "known-public-keys", os.Getenv("MOQOM_KNOWN_PUBLIC_KEYS"), "comma-separated hex SHA-256 SPKI hashes of certificates served outside this project")
	o.certSpotterToken = os.Getenv("MOQOM_CERTSPOTTER_TOKEN")
	flag.BoolVar(&requireDeletion, "require-deletion-protection", os.Getenv("MOQOM_REQUIRE_DELETION_PROTECTION") == "true", "treat Cloud SQL without deletion protection as High")
	flag.StringVar(&alertOn, "alert-on", env("MOQOM_ALERT_ON", "HIGH"), "new findings at or above this severity are logged as compliance_drift")
	flag.StringVar(&failOn, "fail-on", os.Getenv("MOQOM_FAIL_ON"), "exit 2 if any finding is at or above this severity (CI gate); empty never fails")
	flag.Parse()

	if o.project == "" {
		return o, errors.New("-project is required")
	}
	policy.PublicBuckets = split(publicBuckets)
	policy.AllowedOwners = split(owners)
	policy.PublicIngress = split(ingress)
	policy.AuditLogBucket = auditBucket
	policy.EvidenceBucket = o.evidenceBucket
	policy.RequireDeletionProtection = requireDeletion
	policy.CAAIssuers = split(caa)
	policy.KnownPublicKeys = split(knownKeys)
	o.policy = policy
	o.registryLocations = split(registries)
	o.domains = split(domains)

	var err error
	if o.alertOn, err = compliance.ParseSeverity(alertOn); err != nil {
		return o, err
	}
	if failOn != "" {
		if o.failOn, err = compliance.ParseSeverity(failOn); err != nil {
			return o, err
		}
	}
	return o, nil
}

func run() error {
	o, err := parse()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	client.Timeout = time.Minute
	collector := &compliance.Collector{
		Client: client, Project: o.project, RegistryLocations: o.registryLocations,
		Domains: o.domains, CertSpotterToken: o.certSpotterToken,
	}
	return check(ctx, collector, o, time.Now().UTC())
}

func check(ctx context.Context, collector *compliance.Collector, o options, now time.Time) error {
	evidence := collector.Collect(ctx)
	findings := compliance.Evaluate(evidence, o.policy, now)
	name := compliance.ObjectName(o.project, now)

	var previous *compliance.Report
	var previousName string
	if o.evidenceBucket != "" {
		var err error
		previous, previousName, err = collector.Previous(ctx, o.evidenceBucket, compliance.ReportPrefix+o.project+"/", name, now.AddDate(0, 0, -40))
		if err != nil {
			// Drift degrades to "everything is new" rather than stopping the
			// run: a report without a diff is still evidence.
			slog.Warn("previous report unreadable", "event", "compliance_previous_unreadable", "error", err.Error())
			previous = nil
		}
	}

	report := compliance.NewReport(evidence, findings, previous, version, now)
	report.Previous = previousName
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	logFindings(report, o.alertOn)

	if o.out != "" {
		if o.out == "-" {
			_, err = os.Stdout.Write(append(body, '\n'))
		} else {
			err = os.WriteFile(o.out, body, 0o600)
		}
		if err != nil {
			return fmt.Errorf("writing %s: %w", o.out, err)
		}
	}
	if o.evidenceBucket != "" {
		if err := collector.Upload(ctx, o.evidenceBucket, name, body); err != nil {
			return fmt.Errorf("uploading gs://%s/%s: %w", o.evidenceBucket, name, err)
		}
	}

	slog.Info("compliance check complete",
		"event", "compliance_summary",
		"project", o.project,
		"report", reportURI(o.evidenceBucket, name),
		"total", report.Summary.Total,
		"critical", report.Summary.Severity[compliance.SeverityCritical],
		"high", report.Summary.Severity[compliance.SeverityHigh],
		"medium", report.Summary.Severity[compliance.SeverityMedium],
		"new", len(report.Drift.New),
		"resolved", len(report.Drift.Resolved),
		"collection_errors", len(evidence.Errors),
	)

	if o.failOn != "" && compliance.Worst(findings) != "" && compliance.Worst(findings).AtLeast(o.failOn) {
		slog.Error("findings at or above the failure threshold", "event", "compliance_gate_failed", "threshold", string(o.failOn))
		os.Exit(2)
	}
	return nil
}

func logFindings(report compliance.Report, alertOn compliance.Severity) {
	isNew := map[string]bool{}
	for _, f := range report.Drift.New {
		isNew[f.Key()] = true
	}
	for _, f := range report.Findings {
		level := slog.LevelInfo
		if f.Severity.AtLeast(alertOn) {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, f.Title, attrs("compliance_finding", f, isNew[f.Key()])...)
	}
	for _, f := range report.Drift.New {
		if f.Severity.AtLeast(alertOn) {
			slog.Error("new finding: "+f.Title, attrs("compliance_drift", f, true)...)
		}
	}
	for _, f := range report.Drift.Resolved {
		slog.Info("resolved: "+f.Title, attrs("compliance_resolved", f, false)...)
	}
}

func attrs(event string, f compliance.Finding, isNew bool) []any {
	return []any{
		"event", event, "rule", f.Rule, "finding_severity", string(f.Severity),
		"resource", f.Resource, "detail", f.Detail, "controls", f.Controls, "new", isNew,
	}
}

func reportURI(bucket, name string) string {
	if bucket == "" {
		return ""
	}
	return "gs://" + bucket + "/" + name
}

func split(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
