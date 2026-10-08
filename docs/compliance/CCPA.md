# CCPA as amended by CPRA

Draft. We have contacted legal counsel to review this.

## Does it apply?

The CCPA applies to a for-profit business doing business in California that
meets any one threshold: gross annual revenue above the inflation-adjusted
figure (US$26,625,000 for 2025); buying, selling or sharing the personal
information of 100,000 or more consumers or households; or 50% or more of
revenue from selling or sharing personal information.

**Today Elliott Wave LLC meets none of them**, so the CCPA likely does not yet
apply as a "business". MOQOM still follows it, because:

- California tenants will contractually require **service provider** terms
  (§1798.140(ag)) whether or not MOQOM is itself a business.
- Following it costs little given the GDPR program.

The owner should re-check the thresholds every year (GOV-2).

## As a service provider (participant data)

The customer DPA ([DPA-TEMPLATE.md](DPA-TEMPLATE.md), California section)
commits MOQOM to:

- process personal information only for the business purposes in the contract;
- not sell or share it, nor retain, use or disclose it outside the direct
  business relationship, nor combine it with other data except as permitted;
- comply with the CCPA and give the same level of protection;
- let the business take reasonable steps to stop unauthorised use;
- notify the business if MOQOM can no longer meet its obligations.

## As a business (tenant accounts), if it applies

| Requirement | MOQOM |
| --- | --- |
| Notice at collection | Privacy notice on moqom.cloud (draft, marked DRAFT) |
| Categories collected (12 months) | Identifiers (name, email, IP address); commercial information (credit purchases); internet activity (API usage) |
| Sensitive personal information | None collected; API keys are stored hashed and are account credentials only |
| Sold or shared | None |
| Rights: know, delete, correct, opt out, limit, non-discrimination | [DSAR.md](DSAR.md); 45-day deadline |
| Retention disclosure | [DATA-RETENTION.md](DATA-RETENTION.md) |
| Reasonable security (§1798.150) | The control catalog |
| Cybersecurity audits and risk assessments (CPPA regulations, phased in from 2028) | Monitor; likely below thresholds |

## Other US state laws

Colorado, Connecticut, Virginia, Utah, Texas, Oregon and others have similar
laws with their own thresholds, mostly higher than MOQOM's current size.
The GDPR-grade program covers their substance. Review annually.
