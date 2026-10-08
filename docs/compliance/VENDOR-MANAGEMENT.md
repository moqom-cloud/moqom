# Vendor management

## Tiering

| Tier | Meaning | Vendors | Review |
| --- | --- | --- | --- |
| Critical | Holds customer data or can take the service down | Google Cloud, Stripe | Annually: current SOC 2 / ISO / PCI reports, DPA terms, incident history |
| Important | Holds code or company data | GitHub; Hover (registrar, DNS, email) | Annually: security page, MFA enforced, DPA if relevant |
| Low | No data | Everything else | On adoption |

## Before adopting a vendor that will touch customer data

1. Does it need customer personal data at all? If not, configure so it does not.
2. Get its SOC 2 Type II or ISO 27001 certificate; read the exceptions and the complementary user entity controls (CUECs).
3. Sign or accept its DPA, with SCCs if data leaves the EU/UK.
4. Add it to SUBPROCESSORS.md and give customers 30 days' notice.
5. Add it to the risk register.

## Shared responsibility: Google Cloud

| Google | MOQOM |
| --- | --- |
| Physical security, hardware, hypervisor, encryption at rest by default, network backbone, managed service patching (Cloud SQL, GKE control plane) | IAM, firewall rules, workload configuration, node and image updates on the REGULAR channel, application security, data retention, logging configuration, backups configuration |

## Shared responsibility: Stripe

| Stripe | MOQOM |
| --- | --- |
| Card data, PCI DSS for the payment page, fraud tooling | Keeping its key restricted and secret, verifying webhooks, not collecting card data elsewhere |

## Annual evidence to collect (VEN-2, Planned)

Store under `vendors/<year>/` in the evidence bucket:

- Google Cloud SOC 2 Type II and ISO 27001 certificate (Compliance Reports Manager).
- Stripe PCI DSS AOC and SOC 2 (Stripe's compliance page).
- GitHub SOC 2 Type II (GitHub's trust centre).
- Screenshots of MFA enforcement in each admin console.
