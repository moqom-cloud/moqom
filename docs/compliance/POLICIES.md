# Information security policies

Elliott Wave LLC. Applies to everyone with access to MOQOM systems; today that
is Elliott. Reviewed every 12 months, or sooner on a significant change.

**Status: draft, not yet approved.** Approval is a signed commit by the owner
with the line `Approved-by: Elliott` (GOV-1).

## 1. Information security policy

**Objectives**

1. Customer data is used only to provide the service, and kept only as long as [DATA-RETENTION.md](DATA-RETENTION.md) says.
2. Every control that can be automated is, and every control leaves evidence.
3. Security failures are detected within a day and acted on per [INCIDENT-RESPONSE.md](INCIDENT-RESPONSE.md).
4. Compliance status is described truthfully: aligned, planned or implemented, never "certified" without a certificate.

**Principles**: least privilege; defence in depth; secure by default; no
customer data in non-production systems; infrastructure as code; the record
(logs, evidence) is protected more strongly than the systems it records.

**Exceptions** must be written in the risk register with a reason, a
compensating control and an expiry date.

## 2. Data classification

| Class | Examples | Handling |
| --- | --- | --- |
| Restricted | Secrets, private keys, Stripe keys, API key plaintext at issue time, media keys | Secret Manager or Kubernetes Secrets only; never in git, logs, tickets or chat; rotate on suspicion |
| Confidential | Customer personal data, the ledger, audit logs, compliance reports, the patent assessment | Encrypted at rest and in transit; access by IAM only; retention per schedule |
| Internal | Source code of private repositories, design documents | GitHub with MFA |
| Public | Open-source SDKs, websites, published docs | No restriction |

## 3. Acceptable use

- Company systems are used for company work. Customer data is never copied to
  a personal device, personal cloud storage, or a third-party tool (including
  AI tools) not listed in SUBPROCESSORS.md.
- **Device**: full-disk encryption on; OS and browser auto-update on; screen
  lock after at most 5 minutes; firewall on; no unattended unlocked device.
- **Remote work**: no customer data over untrusted networks without TLS;
  administrative access only through Google-authenticated `gcloud` and
  `kubectl`, which are TLS by default.
- **Accounts**: unique, MFA (passkey or hardware key preferred), password
  manager; no shared logins.
- **Card data**: MOQOM never accepts card numbers by email, chat or phone.
  Delete any received and point the sender to Stripe Checkout.
- **Reporting**: anything suspicious goes to security@moqom.cloud and is
  treated as an incident until shown otherwise.

## 4. Encryption

| Where | Requirement | Implementation |
| --- | --- | --- |
| At rest | AES-256 or provider equivalent | Google default encryption on Cloud SQL, GCS, disks and Secret Manager. CMEK optional; GKE Secrets CMEK not enabled (GKE-005, INFO) |
| In transit, external | TLS 1.2 minimum, [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) preferred | [QUIC](https://www.rfc-editor.org/rfc/rfc9000) (TLS 1.3); HTTPS LB SSL policy MODERN, min 1.2 |
| In transit, internal | TLS, mutual where both ends are ours | RelayLink mTLS; Cloud SQL ENCRYPTED_ONLY via the connector |
| Media | End-to-end where the client enables it | SFrame ([RFC 9605](https://www.rfc-editor.org/rfc/rfc9605)) in the Swift SDK |
| Passwords and API keys at rest | One-way hash | API keys: SHA-256 of a 256-bit random secret (no salt needed at that entropy) |
| Prohibited | SSLv3, TLS 1.0/1.1, RC4, 3DES, MD5/SHA-1 for signatures, home-grown cryptography | Enforced by SSL policy (NET-004) and code review |

## 5. Key management

| Key | Where | Rotation | Who |
| --- | --- | --- | --- |
| Token signing keys (`MOQOM_CONTROL_SIGNING_KEYS`) | Secret Manager → Kubernetes Secret | 12 months, or on suspicion; multiple keys supported so rotation has no downtime | Owner |
| RelayLink CA, relay and admin certificates | Secret Manager | Certificates ≤ 12 months; CA ≤ 5 years | Owner (`tools/relay-certificate.sh`) |
| Relay TLS certificate ([QUIC](https://www.rfc-editor.org/rfc/rfc9000)) | Secret Manager | Before expiry | Owner |
| Relay bearer credentials | Secret Manager | 12 months, or on suspicion | Owner |
| Stripe restricted key, webhook secret | Secret Manager | 12 months, or on suspicion | Owner |
| Customer API keys | Hash in Cloud SQL | Customer-controlled; revocable | Customer |
| Google-managed keys | Google | Google | Google |

Rules: no user-managed service account keys (IAM-005); secrets have at most two
enabled versions (SEC-003), so rotation always finishes by disabling the old
one; the nightly check flags any secret whose newest version is older than
365 days (SEC-002); every secret read by an unexpected principal alerts. A
compromised key is a SEV-1 or SEV-2 incident.

## 6. Logging and monitoring

Admin Activity logs are always on. Data Access logs cover writes everywhere
and reads on Secret Manager, Cloud SQL Admin, IAM and Storage. Audit logs are
kept 400 days and their configuration is alerted on. See LOG-* in the catalog.

## 7. Vulnerability management

| Severity | Fix within |
| --- | --- |
| Critical | 7 days (24 hours if exploited in the wild) |
| High | 30 days |
| Medium | 90 days |
| Low | Next convenient release |

Measured from when the gate or a report first surfaced the issue. Exceptions
go in the risk register.

## 8. Secure development

See [CHANGE-MANAGEMENT.md](CHANGE-MANAGEMENT.md). Dependencies are pinned
(lock files, `go.sum`); new dependencies need a reason; licences are checked
by cargo deny; the engine's vendored quinn is verified by its own scripts.

## 9. Supplier security

See [VENDOR-MANAGEMENT.md](VENDOR-MANAGEMENT.md).

## 10. Compliance and enforcement

The nightly check measures the technical parts of these policies. A violation
is recorded in the risk register and corrected; repeated or wilful violation
by a future employee or contractor ends their access.
