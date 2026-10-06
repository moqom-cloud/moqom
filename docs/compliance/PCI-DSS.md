# PCI DSS: SAQ A

MOQOM sells prepaid credit through **Stripe Checkout** (a Stripe-hosted
payment page reached by redirect) and the **Stripe customer portal**. Card
data is entered on Stripe's pages and never reaches MOQOM's servers, logs or
database. That places MOQOM in **Self-Assessment Questionnaire A** under PCI
DSS v4.0.1. MOQOM has **not yet completed** an SAQ A or Attestation of
Compliance (PCI-2, Planned). Stripe is a PCI DSS Level 1 Service Provider.

## What MOQOM does hold

| Data | Card data? | Note |
| --- | --- | --- |
| Stripe customer ID (`cus_…`) | No | |
| Saved PaymentMethod ID (`pm_…`) for auto top-up | No | A token. Charges are created server-side by Stripe API calls; the PAN stays with Stripe |
| Checkout session and PaymentIntent IDs in ledger references | No | |
| Stripe secret key, webhook signing secret | No, but sensitive | Secret Manager → Kubernetes Secret mounted read-only; never logged |

## SAQ A eligibility

| Criterion | MOQOM |
| --- | --- |
| Card-not-present e-commerce only | Yes |
| All payment processing outsourced to a PCI DSS validated third party | Yes, Stripe |
| No electronic storage, processing or transmission of account data on MOQOM systems | Yes: redirect to Stripe Checkout |
| Payment page delivered entirely by the processor | Yes: Checkout is hosted at checkout.stripe.com |
| The merchant's page that redirects is protected from script-based attacks that could affect the e-commerce system (added to SAQ A in the January 2025 revision) | **Planned**: a Content-Security-Policy and Subresource Integrity on the signup and billing pages of moqom.cloud. The sites use no third-party scripts today |
| Merchant confirms the third party's compliance annually | Planned: download Stripe's AOC each year (VEN-2) |

## SAQ A requirements and how they are met

| Req. | Topic | MOQOM | Status |
| --- | --- | --- | --- |
| 2.2.2 | Vendor default accounts | No default accounts; managed services only | Implemented |
| 3.1.1 | Policies for account data | POLICIES.md: MOQOM does not accept card data by any channel; card numbers sent by email are deleted and the sender told to use Checkout | Planned (sign-off) |
| 6.3.1 / 6.3.3 | Vulnerabilities identified, patches applied | Security gate, Dependabot, patch targets | Planned (code ready) |
| 8.2.x / 8.3.x | Unique IDs, MFA on admin access to systems that can affect the payment page (the sites' bucket and DNS) | One owner; MFA | Planned (MFA not evidenced) |
| 9.4.x | Paper with card data | None exists | N/A |
| 11.3.2 | External vulnerability scans by an ASV, quarterly | Required for SAQ A only where the merchant's site redirects to the processor; **confirm with the acquirer (Stripe) whether ASV scans are expected** | Planned |
| 12.8.x | Third-party service provider management | Stripe listed in SUBPROCESSORS.md with its AOC | Planned |
| 12.10.1 | Incident response | INCIDENT-RESPONSE.md, "Payment compromise" runbook | Implemented (document) |

## Controls specific to the integration

- **Webhook authenticity** (PCI-3): every Stripe webhook is verified against
  the signing secret with a 5-minute timestamp tolerance; a failure is refused and
  logged as `stripe_webhook_signature_rejected`, which is counted by a log
  metric and alerts above three in five minutes. Verification: Implemented.
  Alert: Planned (code ready).
- **Idempotence**: a payment is credited under the ledger idempotency key
  `stripe-pi:<PaymentIntent ID>`, which the database holds unique, so a
  replayed webhook cannot mint credit twice.
- **Restricted key**: the stored Stripe key should be a restricted key (`rk_`)
  limited to customers, checkout sessions, payment intents and billing portal,
  as the dev Terraform comment says. As of 2026-10-06 the Stripe secrets exist
  but **have no versions** (SEC-001), so billing is off.

## Annual cycle

1. January: complete SAQ A and the AOC; store them in the evidence bucket under `pci/<year>/`.
2. Download Stripe's AOC.
3. Confirm the CSP on the payment-redirect pages still holds.
