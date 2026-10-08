# Access control

## Human access

One person, Elliott, holds every human account:

| System | Identity | Privilege | MFA |
| --- | --- | --- | --- |
| Google Cloud (moqom-503700) | elliott@moqom.cloud | `roles/owner` (only owner; IAM-002 enforces) | Required; **evidence Planned** (IAM-5) |
| GitHub (moqom-cloud org) | Elliott's account | Org owner | Required; evidence Planned |
| Stripe | Elliott | Owner | Required; evidence Planned |
| Hover: registrar, DNS and mailbox for moqom.cloud | Elliott | Owner | Required; evidence Planned. Whoever controls this account can receive password resets for every other account here |

Policy: phishing-resistant MFA (passkey or hardware key) on all of the above;
no shared accounts; the owner's daily Google identity is used for admin work
through `gcloud` with short-lived tokens. A break-glass account is Planned
(see BCP-DR key-person risk).

## Machine identities

| Identity | Used by | Grants |
| --- | --- | --- |
| `moqom-dev-control@` (GSA via Workload Identity from KSA `moqom/moqom-control`) | Control plane, retention CronJob | Cloud SQL IAM login |
| Relay SA (one per region) | Relay instance groups | Secret accessor on relay TLS and credential secrets; recording bucket (unused) |
| `moqom-dev-compliance@` | Compliance Cloud Run job | `roles/viewer`, `roles/iam.securityReviewer`; create and read on the evidence bucket. Cannot change what it inspects |
| `moqom-dev-compliance-cron@` | Cloud Scheduler | `roles/run.invoker` on the compliance job only |
| `449257903768-compute@developer` (default compute SA) | **Cloud Build's default service account** in this project (`gcloud builds get-default-service-account`), and the default for any VM created without one | **`roles/editor`: finding IAM-003 HIGH.** Removing it as-is would break image builds. Fix: create a dedicated `moqom-dev-build@` SA with `roles/artifactregistry.writer`, `roles/logging.logWriter` and `roles/storage.objectAdmin` on the `_cloudbuild` bucket, plus evidence create; pass `--service-account` to builds; then remove `roles/editor` from the compute SA |
| Federation pool `moqom-dev-external` | External workload identity | Per the federation module |

Rules: no user-managed service account keys (IAM-005; none exist); every SA
and grant is in Terraform; no basic roles on SAs (IAM-003).

## Customer-facing access

- **API keys**: `mqk_<id>_<secret>`; SHA-256 hash stored, plaintext shown
  once; kinds `backend` (account and billing) and `relay` (usage reports only);
  revocable; revoked rows kept 400 days then deleted.
- **Tenant isolation**: every table is keyed by tenant and every service
  method checks the key's tenant. Tested.
- **Room credentials**: short-lived tokens minted against device accounts,
  whose grants cap what any token can do.
- **Moderation**: every kick, ban and mute goes to `moderation_log`.

## Reviews

| Review | Cadence | Input | Status |
| --- | --- | --- | --- |
| IAM bindings | Quarterly | `evidence.iam_policy` in the latest compliance report | Planned |
| GitHub org members, deploy keys, Actions secrets | Quarterly | GitHub settings | Planned |
| Stripe users and API keys | Quarterly | Stripe dashboard | Planned |
| Secret versions and age | Nightly | SEC-001..003 | Planned (code ready) |

Record each review as a dated line in RISK-REGISTER.md's review log.

## Contractors and auditors (when there are any)

Grant read-only, time-boxed roles (IAM conditions with an expiry); NDA first;
remove at the end of the engagement and confirm removal in the next report.
