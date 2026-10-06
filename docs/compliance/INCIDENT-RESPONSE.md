# Incident response

On-call: **Elliott**, always. There is no rotation and no backup responder;
see [BCP-DR.md](BCP-DR.md#key-person-risk) for what that means. Alerts
reach elliott@moqom.cloud by email. Phone paging is Planned (INC-5).

## Severities

| Sev | Meaning | Examples | Respond within | Update customers |
| --- | --- | --- | --- | --- |
| SEV-1 | Confirmed or likely compromise, personal-data breach, or total outage | Unexpected IAM change by an unknown principal; logging or evidence settings changed; leaked Stripe key; all relays down | 1 hour, any time | Within 24 h, then daily |
| SEV-2 | Security control failing, or major degradation | CRITICAL compliance drift; unexpected secret read; webhook forgery pattern; one region down; backup failures | 4 hours, waking hours | If customer-visible |
| SEV-3 | Weakness without evidence of exploitation | HIGH drift; vulnerable dependency reachable; 5xx spike | 2 business days | No |
| SEV-4 | Hygiene | MEDIUM/LOW findings | Next planning cycle | No |

## Automated alert routes

All created by `deploy/modules/compliance` (Planned, code ready), routed to the
email channel `compliance_alert_emails` (default elliott@moqom.cloud).

| Alert policy | Source | Default severity |
| --- | --- | --- |
| `moqom-dev: logging-tamper` | Admin Activity: sink, bucket, exclusion or audit-config changes; evidence bucket updates | SEV-1 |
| `moqom-dev: iam-changes` | Admin Activity: SetIamPolicy, SA key and role changes | SEV-2 until traced to a commit |
| `moqom-dev: secret-access-unexpected` | Data Access: AccessSecretVersion by anyone but the relay SAs | SEV-2 |
| `moqom-dev: firewall-changes` | Admin Activity: firewall insert/patch/delete | SEV-3 (expected on deploys) |
| `moqom-dev: stripe-webhook-rejected` | Control plane `stripe_webhook_signature_rejected` >3 in 5 min | SEV-2 |
| `moqom-dev: control-errors` | Control plane ERROR logs >10 in 5 min | SEV-3 |
| `moqom-dev: load balancer 5xx` | HTTPS LB 5xx >25 in 5 min | SEV-3 |
| `moqom-dev: compliance-drift` | New finding ≥ HIGH in the nightly check | SEV-2 (CRITICAL) / SEV-3 (HIGH) |
| `moqom-dev: retention-failed` | `retention_sweep_failed` | SEV-3 |
| `moqom-dev: compliance check failed` | Cloud Run job failure, or no report for 23 h | SEV-3 |

Not yet alerted: relay and Cloud SQL capacity (the observability module's
alerts are off in dev), control-plane HTTP access anomalies (no access log).

## Process

1. **Detect**: an alert, a report to security@moqom.cloud, or a vendor notice.
2. **Triage**: assign a severity. Open a private incident note (date, time,
   what is known) and keep timestamps from here on.
3. **Contain**: revoke keys, disable a service account, scale the control
   plane to zero, block at the firewall or load balancer, rotate secrets
   (`deploy/tools/sync-secrets.sh`). Prefer actions that preserve evidence.
4. **Preserve evidence**: audit logs are already in the 400-day bucket. Copy
   anything else relevant (exports, screenshots) to the evidence bucket under
   `incidents/<date>-<slug>/`. Do not delete the attacker's artefacts until
   copied.
5. **Eradicate and recover**: fix the cause through a reviewed change. Restore
   from backup if data was altered ([BCP-DR.md](BCP-DR.md)).
6. **Notify**: see below.
7. **Review** within 5 business days for SEV-1/2: timeline, cause, what
   detected it, what would have detected it sooner, actions with dates. Add
   any new risk to the register.

## Personal-data breaches

A breach is a security incident leading to accidental or unlawful
destruction, loss, alteration, unauthorised disclosure of, or access to,
personal data.

| MOQOM's role | Who to notify | Deadline |
| --- | --- | --- |
| Controller (tenant accounts) | Lead supervisory authority, or each affected EU/UK authority (MOQOM has no EU establishment) | **72 hours** from becoming aware (GDPR Art. 33), unless unlikely to result in a risk |
| Controller | Affected tenants | Without undue delay where high risk (Art. 34) |
| Processor (participant data) | Each affected tenant (the controller) | **Without undue delay**; the DPA commits to 48 hours from awareness |
| California residents | Affected individuals; the Attorney General if over 500 residents | Most expedient time possible (Cal. Civ. Code §1798.82) |
| Other US states | Per state law | Varies |
| Payment compromise | Stripe | Immediately |

The notification states: nature of the breach, categories and approximate
numbers of people and records, likely consequences, measures taken, and a
contact (elliott@moqom.cloud). Partial information may be given in phases.

Every breach, notifiable or not, is recorded (Art. 33(5)) in the incident note.

## Runbooks

- **Leaked Stripe key**: roll the key in the Stripe dashboard; add a new
  version in Secret Manager; run `sync-secrets.sh`; restart the control plane;
  review Stripe's logs for the key's use; disable old secret versions.
- **Leaked GCP credential**: identify the principal from the audit log;
  disable it (`gcloud iam service-accounts disable`, or Google account
  recovery); review every Admin Activity entry since the leak.
- **Relay credential or TLS key leak**: rotate in Secret Manager, roll the
  relay instance groups, revoke the old credential in the control plane.
- **Abusive room content**: moderation API (kick, ban, mute); preserve the
  moderation log; for CSAM, report to NCMEC CyberTipline as required by 18
  U.S.C. §2258A, and do not copy the material.

## Contacts

| Who | How |
| --- | --- |
| Google Cloud support | Console support case (project moqom-503700) |
| Stripe | Dashboard support; security@stripe.com |
| GitHub | support.github.com |
| Supervisory authority | Chosen with counsel per affected country (e.g. the Irish DPC, the UK ICO) |
| Law enforcement | FBI IC3 (ic3.gov) |
| Counsel | Recorded privately |
