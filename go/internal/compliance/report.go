package compliance

import (
	"fmt"
	"sort"
	"time"
)

// ReportPrefix is where reports live in the evidence bucket. Names sort by
// time, which is how the previous report is found without a "latest" object:
// the bucket's retention policy forbids overwriting one.
const ReportPrefix = "compliance/reports/"

// Report is one run's output: what was observed, what it was judged against,
// and what changed since the run before.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Project       string    `json:"project"`
	GeneratedAt   time.Time `json:"generated_at"`
	Version       string    `json:"tool_version"`

	Summary  Summary   `json:"summary"`
	Findings []Finding `json:"findings"`
	Drift    Drift     `json:"drift"`

	// Previous names the report drift was computed against, empty on a
	// first run or when it could not be read.
	Previous string   `json:"previous_report,omitempty"`
	Evidence Evidence `json:"evidence"`
}

// Summary counts findings by severity.
type Summary struct {
	Total    int              `json:"total"`
	Severity map[Severity]int `json:"by_severity"`
}

// Drift is the difference between this run's findings and the previous run's.
type Drift struct {
	New      []Finding `json:"new"`
	Resolved []Finding `json:"resolved"`
}

// NewReport assembles a report and computes drift against previous, which may
// be nil.
func NewReport(evidence Evidence, findings []Finding, previous *Report, version string, now time.Time) Report {
	report := Report{
		SchemaVersion: 1,
		Project:       evidence.Project,
		GeneratedAt:   now.UTC(),
		Version:       version,
		Summary:       Summarize(findings),
		Findings:      findings,
		Evidence:      evidence,
	}
	if report.Findings == nil {
		report.Findings = []Finding{}
	}
	var before []Finding
	if previous != nil {
		before = previous.Findings
	}
	report.Drift = Diff(before, findings)
	return report
}

// Summarize counts findings by severity.
func Summarize(findings []Finding) Summary {
	summary := Summary{Total: len(findings), Severity: map[Severity]int{}}
	for _, s := range []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo} {
		summary.Severity[s] = 0
	}
	for _, f := range findings {
		summary.Severity[f.Severity]++
	}
	return summary
}

// Diff reports findings present now and not before, and the reverse. A
// finding whose severity changed counts as new, because an escalation is
// exactly what drift alerting exists for.
func Diff(before, after []Finding) Drift {
	previous := map[string]Finding{}
	for _, f := range before {
		previous[f.Key()] = f
	}
	current := map[string]bool{}
	drift := Drift{New: []Finding{}, Resolved: []Finding{}}
	for _, f := range after {
		current[f.Key()] = true
		if old, ok := previous[f.Key()]; !ok || old.Severity != f.Severity {
			drift.New = append(drift.New, f)
		}
	}
	for _, f := range before {
		if !current[f.Key()] {
			drift.Resolved = append(drift.Resolved, f)
		}
	}
	sort.SliceStable(drift.Resolved, func(i, j int) bool { return drift.Resolved[i].Key() < drift.Resolved[j].Key() })
	return drift
}

// ObjectName is where a report generated at now is stored.
func ObjectName(project string, now time.Time) string {
	now = now.UTC()
	return fmt.Sprintf("%s%s/%s/%s.json", ReportPrefix, project, now.Format("2006/01/02"), now.Format("20060102T150405Z"))
}

// Worst returns the highest severity among findings, or "" for none.
func Worst(findings []Finding) Severity {
	var worst Severity
	for _, f := range findings {
		if worst == "" || severityRank[f.Severity] > severityRank[worst] {
			worst = f.Severity
		}
	}
	return worst
}
