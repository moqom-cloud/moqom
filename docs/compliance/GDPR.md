# GDPR (and UK GDPR)

Draft. We have contacted legal counsel to review this.

## Roles

| Data | MOQOM's role | Controller |
| --- | --- | --- |
| Tenant account: email, name, Stripe customer ID, API key hashes, ledger | **Controller** | Elliott Wave LLC |
| Room participants: identity strings, room metadata, IP addresses, presence, moderation actions | **Processor** | The tenant who created the room |
| Media (audio/video) | **Processor**, transient: relayed and not stored. With SFrame, MOQOM cannot read it | The tenant |
| Website visitors | Controller | Elliott Wave LLC. The sites use no third-party analytics, advertising trackers or font CDNs (per the site's draft privacy notice) |

Elliott Wave LLC is established in the United States with no EU or UK
establishment. Article 27 requires an EU representative (and the UK GDPR a UK
representative) for a non-EU controller or processor that offers services to
people in the EU, unless processing is occasional and low-risk. **Appointing
one is an open decision for the owner and counsel.**

A Data Protection Officer is not required: MOQOM's core activity is not
large-scale regular and systematic monitoring, nor large-scale special-category
processing. Elliott is the privacy contact (privacy@ or security@moqom.cloud).

## Lawful bases (controller processing)

| Purpose | Data | Basis (Art. 6(1)) |
| --- | --- | --- |
| Create and run the tenant account | Email, name, API keys | (b) contract |
| Take payment, keep accounts | Stripe customer ID, ledger | (b) contract; (c) legal obligation for tax records |
| Security, abuse prevention, fraud | IPs in logs, audit logs | (f) legitimate interests: keeping the service and its users safe |
| Service email (receipts, security notices) | Email | (b) contract |
| Marketing email | — | Not done. If started: (a) consent |

For processor activities the tenant determines the basis; MOQOM acts only on
documented instructions under the [DPA](DPA-TEMPLATE.md).

## International transfers

All infrastructure is in the United States (GCP us-west1 and us-west2). EU and
UK personal data is therefore transferred to the US.

- **To MOQOM**: the customer DPA incorporates the EU Standard Contractual
  Clauses (Commission Implementing Decision (EU) 2021/914) Module 2
  (controller→processor), and the UK International Data Transfer Addendum.
  Elliott Wave LLC is **not** certified under the EU-US Data Privacy Framework;
  certifying is an option that would simplify this. Status: Planned.
- **Onward to subprocessors**: Google Cloud and Stripe each offer SCCs in their
  own data processing terms. See [SUBPROCESSORS.md](SUBPROCESSORS.md).
- **Transfer impact assessment**: Planned. Relevant facts: media is not stored;
  with SFrame it is unreadable to MOQOM; stored personal data is minimal and IPs
  are anonymised after 30 days.

## Records of processing (Art. 30)

### As controller (Art. 30(1))

| Field | Entry |
| --- | --- |
| Controller | Elliott Wave LLC, contact elliott@moqom.cloud |
| Purposes | Tenant account administration; billing; security of the service |
| Data subjects | Tenant account holders (developers and businesses) |
| Categories of data | Name, email, Stripe customer ID, API key hashes (not keys), credit balance and ledger, IP addresses in logs |
| Recipients | Stripe (payments), Google Cloud (hosting) |
| Transfers | US; SCCs via vendor terms |
| Retention | See [DATA-RETENTION.md](DATA-RETENTION.md): account while active plus 400 days; ledger 7 years; logs 30–400 days |
| Security measures | [POLICIES.md](POLICIES.md); catalog in [README.md](README.md) |

### As processor (Art. 30(2))

| Field | Entry |
| --- | --- |
| Processor | Elliott Wave LLC |
| Controllers | Each tenant (listed in `billing_tenants`) |
| Categories of processing | Relaying real-time media; room and participant management; moderation (kick, ban, mute); usage metering |
| Categories of data | Participant identity strings chosen by the tenant, room names and metadata, IP addresses (anonymised after 30 days), presence state, moderation actions, media in transit (not stored) |
| Transfers | US; SCCs in the customer DPA |
| Security measures | As above; SFrame end-to-end encryption available in the Swift SDK |

## Data subject rights

See [DSAR.md](DSAR.md). For processor data, MOQOM forwards requests to the tenant
and assists within 10 days.

## DPIA

A Data Protection Impact Assessment is not presumed mandatory: no profiling,
no special-category data by design, no systematic monitoring. A tenant using
MOQOM for, say, telehealth may need its own DPIA, and MOQOM will assist (Art.
28(3)(f)). A short screening DPIA for the relay is Planned.

That holds only while bot and synthetic-media detection is off. Detection
profiles participants, and a DPIA is required before it processes production
traffic, even in shadow mode: see
[TRUST-AND-SAFETY-PRIVACY.md](TRUST-AND-SAFETY-PRIVACY.md). Status: Planned.

## Breach notification

[INCIDENT-RESPONSE.md](INCIDENT-RESPONSE.md#personal-data-breaches): authority
within 72 hours of awareness as controller (Art. 33); controllers without
undue delay as processor (Art. 33(2)); data subjects where high risk (Art. 34).
