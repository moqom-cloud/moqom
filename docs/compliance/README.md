# MOQOM compliance program

Elliott Wave LLC ("elliottwave.studio lab") operates MOQOM: real-time video and
audio rooms over [QUIC](https://www.rfc-editor.org/rfc/rfc9000) (MOQT, [draft-ietf-moq-transport-19](https://datatracker.ietf.org/doc/html/draft-ietf-moq-transport-19)), with [SFrame](https://www.rfc-editor.org/rfc/rfc9605)
end-to-end encryption available in the Swift SDK and moderation in the control
plane. The company has one owner and one employee, Elliott, who holds every
role below.

**Nothing here is certified.** These controls are designed to align with SOC 2
(Trust Services Criteria, 2017 with 2022 points of focus) and ISO/IEC
27001:2022. An independent audit is planned and has not happened. Do not
describe MOQOM as "SOC 2 compliant", "ISO 27001 certified" or "GDPR certified"
anywhere, including sales conversations and questionnaires.

The program is built so that machines do the enforcing and the collecting,
and these documents describe what the machines do. A control is only as true
as the evidence it leaves, so every control below says where its evidence is.

## How to read the status columns

| Column | Values |
| --- | --- |
| **Status** | **Implemented**: operating in project `moqom-503700` or in shipped code today. **Planned**: not operating. "Code ready" means written and tested in this repository but not applied or deployed. |
| **Automation** | **Automated**: a machine performs and evidences it. **Partially automated**: a machine collects evidence or detects failure; a person acts. **Manual**: a person does it and records it. |
| **Verified** | What the read-only check of `moqom-503700` on 2026-10-06 found (see "Baseline" below). |

A Planned control is a gap. The gaps are listed at the end, not hidden.

## Roles

| Role | Holder |
| --- | --- |
| Control owner, security lead, privacy lead, incident commander, on-call | Elliott |
| Approver of policies and exceptions | Elliott |
| Second reviewer | None; see GOV-4 for what stands in for one |

## Control catalog

IDs are referenced from every other document. "Check" names the rule in
`moqom/go/internal/compliance` that tests the control nightly.

### Governance

| ID | Control | Automation | Status | Evidence |
| --- | --- | --- | --- | --- |
| GOV-1 | Security policies ([POLICIES.md](POLICIES.md)) approved by the owner | Manual | Planned (drafted 2026-10-06, not signed) | Signed commit of POLICIES.md |
| GOV-2 | Policies and this catalog reviewed every 12 months | Manual | Planned | Review entry in the risk register |
| GOV-3 | Risk register ([RISK-REGISTER.md](RISK-REGISTER.md)), reviewed quarterly | Manual | Implemented (initial register); quarterly review Planned | Register history in git |
| GOV-4 | Compensating controls for a one-person company: CI gates, nightly drift check and alerts act as the second reviewer | Partially automated | Planned (code ready) | Security build logs, compliance reports |
| GOV-5 | Independent audit (SOC 2 Type I, then Type II; ISO 27001 later) | Manual | Planned | Auditor engagement letter |
| GOV-6 | Annual security and privacy training, self-administered, recorded | Manual | Planned | Training log in the risk register |
| GOV-7 | Cyber liability insurance | Manual | Planned | Policy certificate |

### Identity and access ([ACCESS-CONTROL.md](ACCESS-CONTROL.md))

| ID | Control | Automation | Status | Check | Verified |
| --- | --- | --- | --- | --- | --- |
| IAM-1 | Only allowlisted principals hold `roles/owner` | Automated | Implemented; nightly check Planned (code ready) | IAM-002 | Pass: sole owner is elliott@moqom.cloud |
| IAM-2 | No `allUsers`/`allAuthenticatedUsers` on the project | Automated | Implemented; check Planned (code ready) | IAM-001 | Pass |
| IAM-3 | No basic roles (editor/owner) on service accounts | Automated | **Planned**: default compute SA holds `roles/editor` | IAM-003 | **Fail** |
| IAM-4 | No user-managed service account keys; Workload Identity and federation instead | Automated | Implemented | IAM-005 | Pass: none exist |
| IAM-5 | Phishing-resistant MFA on Google, GitHub, Stripe, the registrar and DNS | Manual | Planned (not evidenced) | — | Not checked |
| IAM-6 | Quarterly access review, using the bindings listed in the nightly report | Partially automated | Planned | Report `evidence.iam_policy` | — |
| IAM-7 | API keys stored as SHA-256 hashes, shown once, revocable | Automated | Implemented | Unit tests (`internal/billing`) | — |
| IAM-8 | A tenant's key reads only that tenant's data | Automated | Implemented | `TestAccountExport`, billing HTTP tests | — |
| IAM-9 | Relay↔control-plane link: mutual TLS plus a per-relay bearer credential | Automated | Implemented | Control-plane tests | — |

### Logging and monitoring

| ID | Control | Automation | Status | Check | Verified |
| --- | --- | --- | --- | --- | --- |
| LOG-1 | Admin Activity and System Event audit logs, 400 days, locked (`_Required`) | Automated (Google) | Implemented | — | Pass |
| LOG-2 | Data Access audit logs: writes and admin reads on all services; reads on Secret Manager, Cloud SQL Admin, IAM, Storage | Automated | **Planned (code ready)** | IAM-004 | **Fail**: no audit config |
| LOG-3 | Audit log bucket, 400 days, holding Data Access logs and control-plane logs | Automated | **Planned (code ready)** | LOG-001, LOG-002 | **Fail** |
| LOG-4 | Security alerts routed to email: IAM changes, unexpected secret reads, firewall changes, logging tampering, webhook signature failures, control-plane errors, load balancer 5xx | Automated | **Planned (code ready)** | MON-001, MON-002 | **Fail**: no channels or policies |
| LOG-5 | Nightly compliance check: dated JSON report to the evidence bucket; alert on drift | Automated | **Planned (code ready)**; run by hand once | — | Ran 2026-10-06 |
| LOG-6 | Alert when the compliance check fails or does not report for 23 hours | Automated | Planned (code ready) | — | — |
| LOG-7 | Control-plane HTTP access log | — | **Planned (not built)** | — | — |
| LOG-8 | Time synchronisation | Automated (Google) | Implemented | — | — |

### Change and vulnerability management ([CHANGE-MANAGEMENT.md](CHANGE-MANAGEMENT.md))

| ID | Control | Automation | Status | Verified |
| --- | --- | --- | --- | --- |
| CHG-1 | All infrastructure in Terraform, one root per environment | Partially automated | Implemented | — |
| CHG-2 | CI builds and tests every repository | Automated | Partially implemented: moqom and moqom-engine on self-hosted runners; public SDK repos' CI is blocked by the GitHub billing failure | — |
| CHG-3 | Security gate (`deploy/tools/cloudbuild-security.yaml`) fails on HIGH/CRITICAL | Automated | **Planned (code ready; no trigger)** | Would fail today (VUL-1) |
| CHG-4 | CycloneDX SBOM per image, kept in the evidence bucket | Automated | Planned (code ready) | — |
| CHG-5 | Dependabot on the public repositories | Automated | Planned (files written, not pushed) | — |
| CHG-6 | Branch protection on `main` | Manual (config) | Planned (not evidenced) | — |
| CHG-7 | Deployed images pinned by digest | Automated | Implemented (`terraform.tfvars`) | Pass |
| VUL-1 | Dependency vulnerabilities: govulncheck, cargo audit, cargo deny | Automated | Planned (code ready) | **9 reachable Go vulnerabilities** in moqom/go, incl. gRPC authorization bypass GO-2026-4762 |
| VUL-2 | Static analysis: gosec, staticcheck, CodeQL | Automated | Planned (code ready) | — |
| VUL-3 | Container image scanning (Trivy in CI; Artifact Registry scanning) | Automated | **Planned**: registry scanning disabled | REG-001 **Fail** |
| VUL-4 | Infrastructure-as-code scanning (trivy config, checkov) | Automated | Planned (code ready) | — |
| VUL-5 | Secret scanning (gitleaks) | Automated | Planned (code ready) | — |
| VUL-6 | Vulnerability disclosure: security@moqom.cloud published; HackerOne VDP | Manual | security@ Implemented; HackerOne Planned | — |
| VUL-7 | Patch targets: Critical 7 days, High 30, Medium 90 | Manual | Planned | — |
| VUL-8 | Annual third-party penetration test | Manual | Planned | — |

### Network ([NETWORK-SECURITY.md](NETWORK-SECURITY.md))

| ID | Control | Automation | Status | Check | Verified |
| --- | --- | --- | --- | --- | --- |
| NET-1 | Private GKE nodes, Workload Identity, Cloud SQL on private IP only | Automated | Implemented | GKE-001/003, SQL-004 | Pass |
| NET-2 | Only intended ports reachable from the internet (UDP/443, TCP/443, TCP/8444) | Automated | **Planned**: `default-allow-ssh`, `-rdp`, `-icmp` open to 0.0.0.0/0 | NET-001, NET-003 | **Fail (CRITICAL)** |
| NET-3 | TLS 1.2+ with the MODERN profile on HTTPS load balancers; [QUIC](https://www.rfc-editor.org/rfc/rfc9000) is [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) | Automated | Implemented | NET-004 | Pass |
| NET-4 | GKE control-plane endpoint restricted to authorized networks | Automated | **Planned** | GKE-002 | **Fail** |
| NET-5 | Firewall change alert | Automated | Planned (code ready) | — | — |
| NET-6 | Edge DDoS and WAF (Cloudflare or Cloud Armor) | — | Planned | — | — |
| NET-7 | CAA on every public domain names only Google Trust Services (`pki.goog`) and Let's Encrypt, with `issuewild ";"` | Automated | **Planned**: records not yet published at the registrar | DNS-001, DNS-002 | **Fail (HIGH)**, 8 Oct 2026 |
| NET-8 | DNSSEC on every public domain | Automated | **Planned**: zones unsigned | DNS-003 | **Fail**, 8 Oct 2026 |
| NET-9 | Certificate Transparency watched nightly: a certificate for one of our names whose key no load balancer here serves is Critical. Relays are self-signed and pinned, so they never appear in CT | Automated | Implemented | CT-001 | Pass, 8 Oct 2026 |

### Data protection ([DATA-RETENTION.md](DATA-RETENTION.md), [POLICIES.md](POLICIES.md))

| ID | Control | Automation | Status | Verified |
| --- | --- | --- | --- | --- |
| DAT-1 | Encryption at rest (Google-managed AES-256) | Automated (Google) | Implemented | — |
| DAT-2 | Encryption in transit: [QUIC](https://www.rfc-editor.org/rfc/rfc9000)/TLS 1.3, mTLS on RelayLink, Cloud SQL `ENCRYPTED_ONLY` | Automated | Implemented | SQL-005 Pass |
| DAT-3 | End-to-end media encryption ([SFrame](https://www.rfc-editor.org/rfc/rfc9605)) | Automated (client) | Implemented in the Swift SDK; optional per room; relays forward ciphertext | — |
| DAT-4 | Media is relayed, not recorded | — | Implemented (no relay code writes media to storage; the recording bucket is provisioned but unused) | Code review |
| DAT-5 | Secrets in Secret Manager or Kubernetes Secrets, never in repositories | Partially automated | Implemented; gitleaks gate Planned | — |
| DAT-6 | Secrets rotated at least every 365 days | Automated (detect) | Planned (code ready) | SEC-002 |
| DAT-7 | Retention sweep (`moqom-retention`) deletes personal data past its period | Automated | **Planned (code ready, tested)** | — |
| DAT-8 | Billing ledger kept 7 years, never deleted or edited | Automated | Implemented (append-only); the sweep refuses ledger tables | Unit tests |
| DAT-9 | Account data export for DSARs (`GET /v1/account/export`) | Automated | Planned (code ready; deploys with the next control image) | `TestAccountExport` |
| DAT-10 | Account erasure on request | Manual (runbook) | Planned | — |
| DAT-11 | Evidence bucket: versioned, retention policy, Bucket Lock | Automated | Planned (code ready; lock off) | STO-004 |

### Availability ([BCP-DR.md](BCP-DR.md))

| ID | Control | Automation | Status | Check | Verified |
| --- | --- | --- | --- | --- | --- |
| AVL-1 | Daily Cloud SQL backups, at least 7 retained | Automated | Partially implemented: daily, **3 retained** | SQL-001 | **Fail (MEDIUM)** |
| AVL-2 | Point-in-time recovery, at least 7 days | Automated | Partially implemented: on, **1 day** | SQL-002 | **Fail (LOW)** |
| AVL-3 | Backup failures detected | Automated | Planned (code ready) | SQL-003 | Latest backup succeeded |
| AVL-4 | Quarterly restore test | Manual | Planned | — | — |
| AVL-5 | Relays in two regions (us-west1, us-west2) | Automated | Implemented | — | — |
| AVL-6 | Control plane and database survive a zone loss | — | **Planned**: one replica, ZONAL database | — | — |
| AVL-7 | Environment rebuildable from Terraform | Partially automated | Implemented | — | — |
| AVL-8 | Capacity alerts (CPU, disk) | Automated | **Planned**: disabled in dev | — | — |

### Incident response ([INCIDENT-RESPONSE.md](INCIDENT-RESPONSE.md))

| ID | Control | Automation | Status |
| --- | --- | --- | --- |
| INC-1 | Written incident response plan with severities | Manual | Implemented (document) |
| INC-2 | Automated detection routed to the on-call | Automated | Planned (code ready; email only) |
| INC-3 | Personal-data breach notification: supervisory authority within 72 hours, customers without undue delay | Manual | Implemented (procedure); untested |
| INC-4 | Annual tabletop exercise | Manual | Planned |
| INC-5 | Phone paging (not just email) | — | Planned |

### Vendors and privacy

| ID | Control | Automation | Status |
| --- | --- | --- | --- |
| VEN-1 | Subprocessor list ([SUBPROCESSORS.md](SUBPROCESSORS.md)) published to customers | Manual | Planned (document ready; not on the website) |
| VEN-2 | Vendor assurance (SOC 2 / ISO reports) collected annually | Manual | Planned |
| VEN-3 | Data processing terms in place with each subprocessor | Manual | Planned (to confirm acceptance of Google's CDPA and Stripe's DPA) |
| PRV-1 | Privacy notice | Manual | Planned (draft on moqom.cloud, marked DRAFT) |
| PRV-2 | Records of processing (GDPR Art. 30) | Manual | Implemented ([GDPR.md](GDPR.md)) |
| PRV-3 | Data subject requests answered within 30 days (GDPR) / 45 days (CCPA) | Partially automated | Planned ([DSAR.md](DSAR.md)) |
| PRV-4 | Customer DPA with SCCs | Manual | Planned ([DPA-TEMPLATE.md](DPA-TEMPLATE.md), counsel review pending) |
| PCI-1 | Card data never touches MOQOM (Stripe Checkout) | — | Implemented (architecture) |
| PCI-2 | Annual SAQ A and Attestation of Compliance | Manual | Planned ([PCI-DSS.md](PCI-DSS.md)) |
| PCI-3 | Stripe webhook signatures verified; failures alerted | Automated | Verification Implemented; alert Planned (code ready) |

## Automation map

What runs, when, and what it leaves behind.

| Machine | Where | Schedule | Enforces / detects | Evidence |
| --- | --- | --- | --- | --- |
| `moqom-compliance` | Cloud Run job `moqom-dev-compliance` (`deploy/modules/compliance`) | 02:17 and 14:17 UTC | 33 rules over IAM, storage, SQL, network, GKE, secrets, logging, monitoring, registry | `gs://moqom-dev-evidence-moqom-503700/compliance/reports/<project>/YYYY/MM/DD/<ts>.json`; `compliance_*` log events |
| `moqom-retention` | GKE CronJob `moqom-dev-retention` (`deploy/modules/retention`) | 03:41 UTC | Deletes or anonymises personal data past retention | `retention_rule` / `retention_sweep` log events in the audit log bucket |
| Security build | Cloud Build, `deploy/tools/cloudbuild-security.yaml` | On demand; trigger Planned | govulncheck, gosec, staticcheck, cargo audit/deny, gitleaks, Trivy (images, IaC), checkov, SBOM | `gs://…/ci/security/YYYY/MM/DD/<build>/` |
| GitHub security workflows | moqom-go, moqom-rust, moqom-mcp `.github/workflows/security.yml` | Push, PR, weekly | govulncheck / cargo audit, gitleaks (history), CodeQL, Scorecard | GitHub code scanning |
| Dependabot | Same three repositories | Weekly | Outdated dependencies and actions | Pull requests |
| Log-based alerts | Cloud Monitoring (`deploy/modules/compliance`) | Continuous | See LOG-4 | Incidents in Cloud Monitoring |
| Audit logging | `google_project_iam_audit_config` + log bucket | Continuous | Who read and changed what | `moqom-dev-audit` log bucket (400 days) |
| Unit tests | `go test ./...` in moqom/go | Every CI run | Rules, retention, tenant isolation, export | CI logs |

## Baseline: 2026-10-06

`moqom-compliance` was run by hand against `moqom-503700` with read-only
credentials. 23 findings, 0 collection errors:

| Severity | Count | Findings |
| --- | --- | --- |
| CRITICAL | 2 | `default-allow-ssh`, `default-allow-rdp` open to 0.0.0.0/0 |
| HIGH | 5 | Default compute SA has `roles/editor`; no Data Access audit logs; no audit log bucket; no notification channel; no alert policy |
| MEDIUM | 5 | GKE endpoint public without authorized networks; default network exists; registry scanning off; 3 retained backups (<7); Cloud Build bucket without public access prevention |
| LOW / INFO | 11 | ICMP and GKE-managed rules open; no firewall logging; 1-day PITR; no deletion protection; no uniform access on the Cloud Build bucket; GKE Secrets without CMEK; Stripe secrets have no versions yet |

Applying `deploy/modules/compliance` closes the five HIGH logging and monitoring
findings. The rest need the remediations in
[RISK-REGISTER.md](RISK-REGISTER.md).

## Turning it on

Nothing in this program has been applied. In order:

```sh
cd /path/to/moqom-cloud

# 1. Logging, evidence bucket, alerts (no job yet: compliance_image is null).
#    Review the plan: -target also plans the resources these depend on.
terraform -chdir=moqom/deploy/envs/dev plan -out=compliance.plan \
  -target=module.compliance \
  -var='compliance_evidence_writers=["serviceAccount:449257903768-compute@developer.gserviceaccount.com"]'
terraform -chdir=moqom/deploy/envs/dev apply compliance.plan
# Confirm the email subscription Google sends to elliott@moqom.cloud.

# 2. Build the compliance image and pin its digest.
gcloud builds submit . --project=moqom-503700 \
  --config=moqom/deploy/tools/cloudbuild-compliance.yaml \
  --substitutions=_VERSION=$(git -C moqom rev-parse --short HEAD)
#    Put compliance_image = "us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-compliance@sha256:…"
#    in envs/dev/terraform.tfvars.

# 3. Nightly check and retention sweep. Try the sweep in count-only mode first.
terraform -chdir=moqom/deploy/envs/dev plan -out=jobs.plan \
  -target=module.compliance -target=module.retention -var=retention_dry_run=true
terraform -chdir=moqom/deploy/envs/dev apply jobs.plan
gcloud run jobs execute moqom-dev-compliance --region=us-west1 --project=moqom-503700 --wait

# 4. Security gate (manual until a trigger exists).
gcloud builds submit . --project=moqom-503700 \
  --config=moqom/deploy/tools/cloudbuild-security.yaml \
  --ignore-file=moqom/deploy/tools/security.gcloudignore \
  --substitutions=_CONTROL_IMAGE=<control digest>,_RELAY_IMAGE=<relay digest>
```

## Documents

| Document | Scope |
| --- | --- |
| [SOC2.md](SOC2.md) | Trust Services Criteria CC1–CC9, A1, C1, PI1 → controls |
| [ISO27001.md](ISO27001.md) | ISO/IEC 27001:2022 Annex A → controls, with a Statement of Applicability |
| [GDPR.md](GDPR.md) | Roles, lawful bases, transfers, Art. 30 records |
| [CCPA.md](CCPA.md) | CCPA as amended by CPRA |
| [DSAR.md](DSAR.md) | Data subject requests, including the export endpoint |
| [PCI-DSS.md](PCI-DSS.md) | SAQ A scope |
| [INCIDENT-RESPONSE.md](INCIDENT-RESPONSE.md) | Severities, alert routes, breach notification |
| [DATA-RETENTION.md](DATA-RETENTION.md) | What is kept, for how long, and what deletes it |
| [NETWORK-SECURITY.md](NETWORK-SECURITY.md) | Exposure, segmentation, TLS |
| [ACCESS-CONTROL.md](ACCESS-CONTROL.md) | Identities, privileges, reviews |
| [VENDOR-MANAGEMENT.md](VENDOR-MANAGEMENT.md), [SUBPROCESSORS.md](SUBPROCESSORS.md) | Third parties |
| [BCP-DR.md](BCP-DR.md) | Backups, recovery, continuity of a one-person company |
| [CHANGE-MANAGEMENT.md](CHANGE-MANAGEMENT.md) | How changes reach production |
| [RISK-REGISTER.md](RISK-REGISTER.md) | Risks, owners, treatment |
| [POLICIES.md](POLICIES.md) | Information security, acceptable use, encryption, key management |
| [DPA-TEMPLATE.md](DPA-TEMPLATE.md) | Draft customer DPA |
| [OTHER-OBLIGATIONS.md](OTHER-OBLIGATIONS.md) | COPPA, DMCA, accessibility, EU DSA |
