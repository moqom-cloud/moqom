# Data retention

What MOQOM stores, for how long, and what deletes it. "Enforced by" names the
machine. Where it says "manual", nothing deletes it automatically yet, and
that is a gap.

The sweep is `moqom/go/cmd/moqom-retention` (package `internal/retention`),
deployed as the GKE CronJob `moqom-dev-retention` at 03:41 UTC by
`deploy/modules/retention`. **Status: Planned (code ready, tested; not
deployed).** It works in batches of 5,000 rows, logs one `retention_rule` event
per rule and a `retention_sweep` summary, exits non-zero and logs
`retention_sweep_failed` on any error (which alerts), and supports `-dry-run`.
Periods are flags, so a change of policy is a change to the CronJob's args,
reviewed like any other.

## Schedule

| Data | Where | Kept for | Enforced by | Basis |
| --- | --- | --- | --- | --- |
| Media (audio/video) | Relay memory only | Not stored. Forwarded, then gone | Architecture: no code writes media to storage | Minimisation |
| Participant IP address | `participants.remote_addr` | 30 days after last seen, then blanked | Sweep rule `participant_addresses` | IP bans are issued from it while a room is live |
| Departed participant row | `participants` (presence `left`) | 400 days after last seen | Sweep rule `departed_participants` | Abuse reports and appeals |
| Expired ban (may hold an IP prefix) | `bans` | 90 days after expiry | Sweep rule `expired_bans` | Repeat-offender context |
| Expired mute | `mutes` | 30 days after expiry | Sweep rule `expired_mutes` | — |
| Trust and safety profile (pseudonymised behaviour statistics, no addresses or media) | Detector memory (**Planned**: not deployed; no persistent store yet) | 30 days after last seen | Code: `DetectorConfig.Retention` in `internal/trust`; a sweep rule is needed once persisted | Fraud and abuse prevention; see [TRUST-AND-SAFETY-PRIVACY.md](TRUST-AND-SAFETY-PRIVACY.md) |
| Trust and safety audit log (decisions, reviews, appeals) | **Planned**: not deployed | 400 days, with the moderation log | **Manual (Planned)** | Evidence behind enforcement decisions and appeals |
| Cross-tenant bot confirmations (platform pseudonyms) | **Planned**: not deployed | **To decide** (proposed 400 days or until retracted) | — | Fraud prevention across tenants (MOQOM as controller) |
| Revoked API key (name, hash, dates) | `billing_keys` | 400 days after revocation | Sweep rule `revoked_api_keys` | Access reviews over an audit period |
| Expired, never-revoked API key (name, hash, allowlist, dates) | `billing_keys` | 400 days after expiry | Sweep rule `expired_api_keys` | Access reviews over an audit period |
| API key use, per minute (key ID, tenant, operation, source IP, outcome, count) | `billing_key_use` | 400 days after the minute's last use | Sweep rule `api_key_use` | Security monitoring and the tenant's own access review; no secrets |
| Completed provisioning job | `billing_jobs` | 400 days after completion | Sweep rule `finished_billing_jobs` | Troubleshooting |
| **Billing ledger** | `billing_ledger` | **7 years. Never deleted or edited** | Append-only by design; the sweep refuses to touch it (`Protected`, tested) | Tax and accounting records (US federal and state record-keeping; EU/UK equivalents where they apply) |
| Tenant account | `billing_tenants` | While active, then until the ledger it anchors expires | Manual. Erasure request: email and name are replaced, ID and ledger kept ([DSAR.md](DSAR.md)) | Contract; legal obligation |
| Moderation log | `moderation_log` | 400 days | **Manual (Planned to automate)**; protected from the sweep until a period is agreed with tenants, since it is the evidence behind their enforcement decisions | Tenant's legitimate interest |
| Rooms, room events, grant overrides, watchdogs | `rooms`, `events`, `grant_overrides`, `watchdogs` | Life of the room, then 400 days | **Manual (Planned)**: no sweep rule yet | — |
| Control-plane logs (may include IPs and tenant IDs) | `_Default` (30 days); audit log bucket (400 days) once applied | 30 / 400 days | Log bucket retention | Security (legitimate interests) |
| Relay logs | `moqom-dev-relay` log bucket | 7 days | Log bucket retention | Operations, abuse |
| Admin Activity audit logs | `_Required` | 400 days, locked | Google | Security, audit |
| Data Access audit logs | Audit log bucket | 400 days | Log bucket retention (Planned, code ready) | Security, audit |
| Compliance reports, SBOMs, scan output | Evidence bucket | 400 days undeletable, deleted at 3 years | Retention policy and lifecycle (Planned, code ready) | Audit evidence |
| Database backups | Cloud SQL | 3 backups and 1 day of PITR (target 7 and 7) | Cloud SQL | Recovery |
| Card details, invoices | Stripe | Stripe's policy | Stripe | MOQOM never receives card data |

## Things that are not retention

- **Deletion from backups**: deleted rows remain in Cloud SQL backups until the
  backups roll off (currently 3 days, target 7). Erasure is complete once that
  window passes. State this in DSAR replies.
- **Legal hold**: to suspend the sweep, scale the CronJob to suspended
  (`kubectl -n moqom patch cronjob moqom-dev-retention -p '{"spec":{"suspend":true}}'`)
  and record why in the risk register. The evidence bucket's retention policy
  cannot be suspended or shortened once locked.

## Changing a period

1. Edit this table and `retention.DefaultPolicy()` (or the CronJob args) in the same commit.
2. Run `moqom-retention -dry-run` against the database and attach the counts to the commit.
3. Apply.
