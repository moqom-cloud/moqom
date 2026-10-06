# SOC 2 mapping

Controls designed to align with the AICPA Trust Services Criteria (2017, with
the 2022 revised points of focus). **No SOC 2 examination has been performed.**
The plan is a Type I report once the Planned controls marked "code ready" are
applied and have run for a month, then a Type II over a six- to twelve-month
observation window. Control IDs refer to the [catalog](README.md#control-catalog).

In scope for the first report: Security (Common Criteria), Availability and
Confidentiality. Processing Integrity is mapped for the billing ledger but not
proposed for the first report. Privacy is handled under [GDPR.md](GDPR.md)
and [CCPA.md](CCPA.md) rather than the SOC 2 Privacy criteria.

System boundary: GCP project `moqom-503700` (GKE control plane, Cloud SQL,
relay instance groups, load balancers, Secret Manager, logging), the
repositories under github.com/moqom-cloud, and the Stripe account. The
marketing sites are out of scope except where they collect signups.

## CC1 Control environment

| Criterion | How MOQOM addresses it | Controls | Status |
| --- | --- | --- | --- |
| CC1.1 Integrity and ethical values | Acceptable use and conduct in POLICIES.md; one person, so commitment is personal and recorded by signature | GOV-1 | Planned (sign-off) |
| CC1.2 Board oversight | No board. The owner is the governing body; an external auditor is the independent check | GOV-5 | Planned |
| CC1.3 Structure, authority, responsibility | Roles table in README.md; all roles held by Elliott | — | Implemented |
| CC1.4 Competence | Annual self-training recorded | GOV-6 | Planned |
| CC1.5 Accountability | Automated gates and alerts stand in for peer accountability | GOV-4 | Planned (code ready) |

## CC2 Communication and information

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC2.1 Quality information | Nightly compliance report is a complete, dated snapshot of configuration | LOG-5 | Planned (code ready) |
| CC2.2 Internal communication | Alerts to the owner's email | LOG-4, INC-2 | Planned (code ready) |
| CC2.3 External communication | security@moqom.cloud and the disclosure page; status of subprocessors; privacy notice | VUL-6, VEN-1, PRV-1 | Partial |

## CC3 Risk assessment

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC3.1 Objectives | Stated in POLICIES.md §1 | GOV-1 | Planned (sign-off) |
| CC3.2 Identify and analyse risk | RISK-REGISTER.md, fed by the nightly findings | GOV-3 | Implemented (initial) |
| CC3.3 Fraud risk | Billing: prepaid only, Stripe-verified webhooks, append-only ledger | PCI-3, DAT-8 | Implemented |
| CC3.4 Significant change | Risk register reviewed when architecture changes | GOV-3 | Planned |

## CC4 Monitoring activities

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC4.1 Ongoing evaluations | Nightly compliance check with drift detection; weekly dependency scans | LOG-5, VUL-1 | Planned (code ready) |
| CC4.2 Communicate deficiencies | `compliance_drift` alert; missed-run alert | LOG-5, LOG-6 | Planned (code ready) |

## CC5 Control activities

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC5.1 Select controls | This catalog | — | Implemented |
| CC5.2 Technology general controls | Terraform, CI, security gate | CHG-1, CHG-3 | Partial |
| CC5.3 Policies and procedures | POLICIES.md and the documents it links | GOV-1 | Planned (sign-off) |

## CC6 Logical and physical access

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC6.1 Access security architecture | Workload Identity, no SA keys, IAM database auth, hashed API keys, mTLS on RelayLink | IAM-4, IAM-7, IAM-9 | Implemented |
| CC6.2 Provisioning | Service accounts only through Terraform; one human | CHG-1 | Implemented |
| CC6.3 Least privilege, removal | No basic roles on SAs | IAM-3 | **Fail**: default compute SA has editor |
| CC6.4 Physical access | Inherited from Google Cloud (data centres) | VEN-2 | Inherited |
| CC6.5 Disposal | Retention sweep; Google media sanitisation (inherited) | DAT-7 | Planned (code ready) |
| CC6.6 External threats | Firewall allowlist check, private nodes, TLS | NET-1..4 | **Partial**: ssh/rdp open |
| CC6.7 Data in transit | TLS 1.2+/1.3, ENCRYPTED_ONLY SQL, SFrame | DAT-2, DAT-3 | Implemented |
| CC6.8 Malicious software | Distroless images, image scanning, dependency scanning | VUL-1, VUL-3 | Planned |

## CC7 System operations

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC7.1 Detect configuration changes and vulnerabilities | Nightly check, security gate, Dependabot | LOG-5, CHG-3, CHG-5 | Planned (code ready) |
| CC7.2 Monitor for anomalies | Log-based alerts (IAM, secrets, firewall, logging tamper, errors) | LOG-4 | Planned (code ready) |
| CC7.3 Evaluate security events | Severity matrix in INCIDENT-RESPONSE.md | INC-1 | Implemented (document) |
| CC7.4 Respond | Runbooks in INCIDENT-RESPONSE.md | INC-1 | Implemented (document) |
| CC7.5 Recover | BCP-DR.md; backups and PITR | AVL-1, AVL-2, AVL-4 | Partial |

## CC8 Change management

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC8.1 Authorise, test, approve changes | Commits to Terraform and code; CI; security gate; digest-pinned images | CHG-1..7 | Partial |

## CC9 Risk mitigation

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| CC9.1 Business disruption | BCP-DR.md, including key-person risk | AVL-*, GOV-7 | Partial |
| CC9.2 Vendors | VENDOR-MANAGEMENT.md, SUBPROCESSORS.md | VEN-1..3 | Planned |

## A1 Availability

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| A1.1 Capacity | Relay autoscaling; capacity alerts (off in dev) | AVL-8 | Planned |
| A1.2 Backups, recovery infrastructure | Daily backups, PITR, two relay regions | AVL-1, AVL-2, AVL-5 | Partial (3 backups, 1-day PITR) |
| A1.3 Recovery testing | Quarterly restore test | AVL-4 | Planned |

## C1 Confidentiality

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| C1.1 Identify and protect confidential information | Data inventory in DATA-RETENTION.md; encryption; media not recorded | DAT-1..4 | Implemented |
| C1.2 Dispose | Retention sweep | DAT-7 | Planned (code ready) |

## PI1 Processing integrity (billing ledger; not proposed for the first report)

| Criterion | How | Controls | Status |
| --- | --- | --- | --- |
| PI1.1 Definitions | Ledger entry kinds and money units in `internal/billing` | — | Implemented |
| PI1.2 Input | Webhooks signature-verified; usage reports authenticated by relay keys | PCI-3, IAM-7 | Implemented |
| PI1.3 Processing | Idempotent references; append-only ledger; balance computed from entries | DAT-8 | Implemented |
| PI1.4 Output | Ledger and export endpoints | DAT-9 | Planned (export deploys with next image) |
| PI1.5 Storage | Cloud SQL backups; ledger never deleted | AVL-1, DAT-8 | Implemented |

## Before engaging an auditor

1. Apply `deploy/modules/compliance` and let the nightly check run for 30 days.
2. Close the CRITICAL and HIGH baseline findings.
3. Sign POLICIES.md and record the first quarterly access review and restore test.
4. Choose a readiness assessment or go straight to Type I. Auditor selection is the owner's decision.
