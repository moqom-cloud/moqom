package compliance

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDiff(t *testing.T) {
	before := []Finding{
		{Rule: "NET-001", Severity: SeverityHigh, Resource: "firewall/a"},
		{Rule: "SQL-001", Severity: SeverityMedium, Resource: "sql/pg"},
		{Rule: "IAM-004", Severity: SeverityHigh, Resource: "auditConfig/allServices"},
	}
	after := []Finding{
		{Rule: "NET-001", Severity: SeverityHigh, Resource: "firewall/a"},
		{Rule: "SQL-001", Severity: SeverityCritical, Resource: "sql/pg"},
		{Rule: "STO-001", Severity: SeverityCritical, Resource: "gs://x"},
	}
	drift := Diff(before, after)
	if len(drift.New) != 2 || drift.New[0].Key() != "SQL-001|sql/pg" || drift.New[1].Key() != "STO-001|gs://x" {
		t.Fatalf("new: %+v", drift.New)
	}
	if len(drift.Resolved) != 1 || drift.Resolved[0].Key() != "IAM-004|auditConfig/allServices" {
		t.Fatalf("resolved: %+v", drift.Resolved)
	}
}

func TestFirstReportCountsEverythingAsNew(t *testing.T) {
	findings := []Finding{{Rule: "MON-001", Severity: SeverityHigh, Resource: "monitoring/notificationChannels"}}
	report := NewReport(Evidence{Project: "p"}, findings, nil, "test", now)
	if len(report.Drift.New) != 1 || len(report.Drift.Resolved) != 0 {
		t.Fatalf("drift: %+v", report.Drift)
	}
	if report.Summary.Total != 1 || report.Summary.Severity[SeverityHigh] != 1 || report.Summary.Severity[SeverityCritical] != 0 {
		t.Fatalf("summary: %+v", report.Summary)
	}
	if Worst(findings) != SeverityHigh || Worst(nil) != "" {
		t.Fatal("worst")
	}
}

func TestEmptyReportSerialisesEmptyLists(t *testing.T) {
	raw, err := json.Marshal(NewReport(Evidence{Project: "p"}, nil, nil, "test", now))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	// null would read as "not evaluated" to whatever consumes this.
	if decoded["findings"] == nil {
		t.Fatalf("findings serialised as null: %s", raw)
	}
}

func TestObjectNamesSortByTime(t *testing.T) {
	a := ObjectName("moqom-503700", time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC))
	b := ObjectName("moqom-503700", time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC))
	if a != "compliance/reports/moqom-503700/2026/09/30/20260930T230000Z.json" || !(a < b) {
		t.Fatalf("%s !< %s", a, b)
	}
}
