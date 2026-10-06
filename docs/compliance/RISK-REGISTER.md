# Risk register

Likelihood and impact are 1 (low) to 5 (high); score = L × I. Treatment is
Mitigate, Accept, Transfer or Avoid. Owner of every risk: Elliott. Initial
assessment 2026-10-06; review quarterly (GOV-3).

| ID | Risk | L | I | Score | Treatment | Action | Due | Status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| R-01 | SSH/RDP open to the internet on the default VPC | 3 | 4 | 12 | Mitigate | Delete default-allow-ssh/rdp/icmp and the default network | 2026-10-13 | Open |
| R-02 | Known exploitable dependency vulnerabilities (gRPC authorization bypass GO-2026-4762, gRPC DoS, pgx SQL injection GO-2026-5004, x/net) in the control plane | 3 | 5 | 15 | Mitigate | Upgrade grpc ≥1.83.1, pgx ≥5.9.2, x/net ≥0.55, x/text ≥0.39, otel ≥1.41; bump the Go toolchain and Dockerfile base off 1.23.1 (unsupported) | 2026-10-13 | Open |
| R-03 | Default compute SA holds roles/editor, and it is Cloud Build's identity, so any build step can change almost anything | 2 | 4 | 8 | Mitigate | Dedicated build SA with narrow roles (ACCESS-CONTROL.md), then remove editor | 2026-10-20 | Open |
| R-04 | No Data Access audit logs, no alerting: an intrusion would go unnoticed | 3 | 4 | 12 | Mitigate | Apply `deploy/modules/compliance` | 2026-10-13 | Open (code ready) |
| R-05 | Key-person dependency: one person holds every role and credential | 3 | 5 | 15 | Mitigate | Break-glass record, trusted contact (BCP-DR.md) | 2026-11-30 | Open |
| R-06 | Vendor account lapses for non-payment (already happened to GitHub Actions) | 4 | 3 | 12 | Mitigate | Fix GitHub billing; auto-pay and alerts on all vendors | 2026-10-13 | Open |
| R-07 | Control plane single replica, database ZONAL: zone loss is an outage | 2 | 4 | 8 | Accept for dev; Mitigate for prod | REGIONAL database and a shared relay registry before paying tenants depend on it | Before GA | Accepted (dev) |
| R-08 | Thin backup window (3 backups, 1 day PITR) | 2 | 4 | 8 | Mitigate | 7 and 7 in `module "data"` | 2026-10-20 | Open |
| R-09 | GKE API endpoint reachable from the internet | 2 | 4 | 8 | Mitigate | Authorized networks or DNS endpoint | 2026-10-31 | Open |
| R-10 | No separate production environment | 3 | 3 | 9 | Mitigate | `envs/prod` in its own project | Before GA | Open |
| R-11 | Abuse of rooms (illegal content, harassment) creating legal exposure | 2 | 4 | 8 | Mitigate | Moderation API exists; abuse reporting channel and NCMEC procedure (OTHER-OBLIGATIONS.md) | 2026-11-30 | Partial |
| R-12 | Stripe key compromise allows refunds or data access | 1 | 4 | 4 | Mitigate | Restricted key; secret-read alert; webhook verification | With billing launch | Partial |
| R-13 | Forged webhooks mint credit | 1 | 3 | 3 | Mitigate | Signature verification, idempotency, alert | — | Mitigated (alert code ready) |
| R-14 | Participant IPs kept indefinitely | 3 | 2 | 6 | Mitigate | Deploy the retention CronJob | 2026-10-20 | Open (code ready) |
| R-15 | Misrepresenting compliance status to customers | 2 | 4 | 8 | Avoid | Honesty rule in README.md; no "compliant/certified" claims | — | Mitigated |
| R-16 | Container image vulnerabilities undetected (registry scanning off) | 3 | 3 | 9 | Mitigate | Security gate with Trivy; enable Artifact Registry scanning | 2026-10-31 | Open |
| R-17 | No WAF/DDoS layer in front of the join endpoint | 2 | 3 | 6 | Accept for now | Revisit at first paying tenant | — | Accepted |
| R-18 | EU users served without an Art. 27 representative or DPF certification | 2 | 3 | 6 | Mitigate | Decide with counsel | 2026-12-31 | Open |
| R-19 | Security workflows on public repos do not run (GitHub hosted runners blocked) | 4 | 2 | 8 | Mitigate | Fix GitHub billing | 2026-10-13 | Open |

## Review log

| Date | Reviewer | Notes |
| --- | --- | --- |
| 2026-10-06 | Elliott (drafted with automation) | Initial register from the first compliance check. Pending owner review |
