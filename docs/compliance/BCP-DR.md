# Business continuity and disaster recovery

## What exists today (verified 2026-10-06)

| Component | Configuration | Source |
| --- | --- | --- |
| Cloud SQL `moqom-dev-pg` (Postgres 16, us-west1) | **ZONAL**; automated daily backups, **3 retained**; PITR on with **1 day** of transaction logs; deletion protection **off**; latest backup SUCCESSFUL | `envs/dev/main.tf` module `data`: `backup_retention_days = 3`, `transaction_log_retention_days = 1`, `deletion_protection = false` |
| Control plane | GKE Autopilot, **1 replica** (a correctness bound: the relay registry is in memory) | `module "control"` |
| Relays | Managed instance groups in us-west1 and us-west2 | `module "relay"` |
| Infrastructure | Terraform, state in `gs://moqom-tfstate-503700` (versioned) | `deploy/` |
| Secrets | Secret Manager; synced to the cluster by `tools/sync-secrets.sh` | |
| Media | Not stored, so there is nothing to restore | |

## Targets

| | Target | Today |
| --- | --- | --- |
| RPO (control-plane database) | 5 minutes | PITR gives seconds, but only within the last 1 day |
| RTO (control plane) | 4 hours | Untested |
| RTO (relays, one region lost) | Immediate: clients reconnect to the other region | Two regions exist; failover depends on DNS steering (module `edge_dns`, when `dns_domain` is set) |
| Backup retention | 7 daily backups, 7 days of PITR | 3 and 1: findings SQL-001 MEDIUM, SQL-002 LOW |

To meet the targets in dev, change `module "data"` to
`backup_retention_days = 7`, `transaction_log_retention_days = 7`. For prod,
use `availability_type = REGIONAL` and `deletion_protection = true`.

## Detection

- **Backup failures**: the nightly check reads the last 10 backup runs (rule
  SQL-003). A FAILED latest run, no successful run, or a newest success older
  than 36 hours is HIGH and alerts as drift. Planned (code ready).
- Cloud SQL CPU and disk alerts exist in the observability module but are off
  in dev (`alert_enabled = false`).

## Restore procedures

**Point-in-time restore** (data corruption, bad migration):

```sh
gcloud sql instances clone moqom-dev-pg moqom-dev-pg-restore \
  --project=moqom-503700 --point-in-time=2026-10-06T09:00:00Z
```

Verify the clone, then point the control plane at it
(`MOQOM_CONTROL_SQL_INSTANCE`) through Terraform, or copy the affected rows back.

**Backup restore** (instance lost):

```sh
gcloud sql backups list --instance=moqom-dev-pg --project=moqom-503700
gcloud sql backups restore <BACKUP_ID> --restore-instance=<target> \
  --backup-instance=moqom-dev-pg --project=moqom-503700
```

**Whole environment**: `terraform apply` in `deploy/envs/dev` against a new
project; restore the database from backup (backups survive only while the
instance exists, so an **export to GCS** is Planned for project-loss scenarios);
re-run `sync-secrets.sh`; rebuild images from git.

## Restore testing (AVL-4, Planned)

Quarterly: clone to a point in time, run `moqom-dbadmin` checks and the store
conformance tests against the clone, record the time taken and the result in
the evidence bucket under `dr/<date>/`, then delete the clone.

## Key-person risk

MOQOM has one person. If Elliott is unavailable, nobody can respond to
incidents, rotate secrets or pay vendors. Mitigations, all **Planned**:

1. A sealed break-glass record (password manager emergency access or a sealed
   letter with counsel) covering: Google Cloud owner recovery, GitHub org,
   Stripe, Hover (registrar, DNS, email), and where this documentation lives.
2. A named trusted contact who may suspend the service and notify customers.
3. Vendor billing on a card with auto-pay and alerts, so the service does not
   lapse for non-payment. The GitHub Actions outage since 28 July, caused by a
   failed account payment, is exactly this failure.
4. Customer contracts that disclose the single-operator status and the
   continuity plan.

## Dependencies

| Vendor outage | Effect | Response |
| --- | --- | --- |
| Google Cloud region | One relay region down; control plane down if us-west1 | Clients fail over to the other relay region; restore control plane in us-west2 from backup |
| Stripe | No new top-ups; existing credit keeps working | Wait; prepaid credit buffers it |
| GitHub | No CI or new images; production unaffected | Build locally with Cloud Build |
| Hover | DNS or email down | Production DNS for relays is Cloud DNS where delegated; email delays |
