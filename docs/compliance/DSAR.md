# Data subject requests

Covers GDPR Arts. 15–22 and CCPA/CPRA rights to know, delete, correct and
opt out. Draft. We have contacted legal counsel to review this.

## Who is asking

| Requester | MOQOM's role | What happens |
| --- | --- | --- |
| A tenant (account holder) about their own account | Controller | MOQOM answers directly |
| A participant in a tenant's rooms | Processor | MOQOM tells the requester which tenant controls the data (if it can tell), forwards the request to that tenant within **10 days**, and helps the tenant answer. MOQOM does not answer a processor request on its own initiative |
| Anyone about website visits | Controller | The sites set no tracking; server logs only |

## Deadlines

| Law | Deadline | Extension |
| --- | --- | --- |
| GDPR / UK GDPR | 1 month from receipt | +2 months for complex requests, with notice inside the first month |
| CCPA / CPRA | Acknowledge within 10 business days; answer within 45 calendar days | +45 days, with notice |

## Channels

- Email to privacy@moqom.cloud (until it exists: security@moqom.cloud).
- For tenants, the self-service export below.

## Verifying identity

- **Tenant**: a request made with a valid backend API key is verified by the
  key. An email request must come from the account email, and MOQOM confirms by
  replying to that address before acting.
- **Participant**: MOQOM holds only the identity string the tenant assigned, so
  it generally cannot verify a participant. That is one reason the request goes
  to the tenant.

## Access and portability: `GET /v1/account/export`

Status: **Planned (code ready)**. Implemented in
`moqom/go/internal/billing/export.go`, tested by `TestAccountExport`, and
deployed with the next control-plane image.

```
GET /v1/account/export
Authorization: Bearer mqk_<id>_<secret>      (a backend key; relay keys are refused)

200 OK
Content-Type: application/json
Content-Disposition: attachment; filename="moqom-account-export.json"

{
  "generated_at": "2026-10-06T12:00:00Z",
  "tenant":   { "id", "name", "email", "status", "has_card", "auto_top_up", "created_at" },
  "balance":  { "micros", "display" },
  "api_keys": [ { "id", "name", "kind", "created_at", "revoked_at" } ],
  "ledger":   [ { "kind", "amount", "capped", "reference", "detail", "at" } ],
  "held_elsewhere": [ { "holder", "what", "how_to_get_it" } ]
}

401 Unauthorized   missing, invalid, revoked or relay key
```

- Machine-readable JSON, which satisfies Art. 20 portability.
- Leaves out **key hashes** (a credential verifier, not personal data) and
  **MOQOM's internal cost** of usage (not the tenant's data).
- `held_elsewhere` points to Stripe (card details, invoices; through the
  billing portal) and to the tenant's own room data (through the admin API).
- Every export logs a `dsar_export` event with tenant and key ID, kept 400 days
  in the audit log bucket. That log entry is the record that the request was
  answered.

## Erasure

Status: **Manual (Planned to automate)**. Runbook:

1. Verify the requester (above).
2. Revoke all keys (`DELETE /v1/keys/{id}`, or in the database).
3. In `billing_tenants`, replace `email` with `erased+<tenant-id>@invalid` and
   `name` with `erased`. **Keep the tenant ID and the ledger**: financial
   records have a 7-year legal retention, which GDPR Art. 17(3)(b) and CCPA
   §1798.105(d) both allow.
4. Delete the customer in Stripe, or ask Stripe to, subject to Stripe's own
   legal retention.
5. Delete the tenant's rooms and their participants, bans, mutes and events
   once any open abuse case is closed.
6. Reply, stating that backups roll off within the backup window (see
   [DATA-RETENTION.md](DATA-RETENTION.md)).
7. Record the request, dates and actions in the DSAR log (a private sheet: date
   received, requester, type, verified how, date answered).

## Correction

There is no self-service endpoint for changing a tenant's name or email yet
(Planned). Corrections are made by email request, verified as above, and
applied in `billing_tenants`. Ledger entries are never edited; a correction is
a new entry.

## CCPA specifics

MOQOM does not sell or share personal information for cross-context
behavioural advertising, so there is no opt-out link to offer. If that ever
changes, honour Global Privacy Control signals. See [CCPA.md](CCPA.md).
