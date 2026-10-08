# Subprocessors

Third parties that process personal data on MOQOM's behalf. Customers are
given **30 days' notice** before a new subprocessor processes their data
(see the DPA). This list is not yet published on moqom.cloud (VEN-1, Planned).

## Current

| Subprocessor | Purpose | Data | Location | Transfer mechanism | Assurance |
| --- | --- | --- | --- | --- | --- |
| Google LLC (Google Cloud) | Hosting: compute, database, storage, logging, load balancing | All service data, including participant data and media in transit | United States (us-west1, us-west2) | Google Cloud Data Processing Addendum with SCCs | SOC 2 Type II, ISO 27001/27017/27018, PCI DSS |
| Stripe, Inc. | Payments: checkout, saved payment methods, invoices | Tenant billing contact, card data (collected by Stripe directly) | United States and Stripe's processing locations | Stripe DPA with SCCs | PCI DSS Level 1, SOC 2 |
| GitHub, Inc. (Microsoft) | Source hosting and CI | Source code only. No customer personal data by policy | United States | GitHub DPA | SOC 2 Type II, ISO 27001 |
| Hover (Tucows Inc.) | Company email (moqom.cloud MX: hostedemail.com), domain registrar and DNS | Support, security and DSAR correspondence | Canada / United States | **To confirm**: Tucows' privacy terms; SCCs if available | To collect |

Email and DNS were confirmed from public DNS records on 2026-10-06 (MX
`mx.hover.com.cust.hostedemail.com`, NS `ns1/ns2.hover.com`). Hover's
assurance reports are the weakest in this list. If customer correspondence
grows, moving mail to a provider with a SOC 2 report is worth considering.

GitHub is listed for completeness. It is a vendor rather than a processor of
customer data, unless a customer puts personal data in an issue.

## Planned

| Subprocessor | Purpose | When |
| --- | --- | --- |
| Cloudflare, Inc. | DNS, CDN, WAF for the sites | Not in use; notify customers before enabling |
| HackerOne Inc. | Vulnerability disclosure program | Not in use; would process researcher data, not customer data |

## Not subprocessors

- Apple and Google app stores, for the SDKs' sample apps: no customer data.
- The tenant's own infrastructure, for example a bring-your-own recording bucket.
  That is the customer's processor, not MOQOM's.
