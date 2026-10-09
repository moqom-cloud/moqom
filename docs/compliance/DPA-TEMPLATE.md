# Data Processing Addendum (DRAFT TEMPLATE)

**Draft. Not for signature.** We have contacted legal counsel to review this.

This Data Processing Addendum ("DPA") forms part of the agreement ("Agreement")
between **[Customer legal name]** ("Customer") and **Elliott Wave LLC**
("MOQOM") for the MOQOM real-time media service ("Service").

## 1. Definitions

Terms such as "personal data", "processing", "controller", "processor",
"data subject" and "personal data breach" have the meanings given in the GDPR
(Regulation (EU) 2016/679) and, where applicable, the UK GDPR. "Service
provider", "business", "sell" and "share" have the meanings in the California
Consumer Privacy Act as amended ("CCPA"). "Customer Personal Data" is personal
data MOQOM processes on Customer's behalf in providing the Service.

## 2. Roles and scope

2.1 Customer is the controller (or a processor on behalf of its own
controller) and MOQOM is the processor of Customer Personal Data.

2.2 MOQOM is an independent controller of Customer's account and billing
data, which its privacy notice covers, not this DPA.

2.3 Details of processing are in **Annex I**.

## 3. MOQOM's obligations

MOQOM will:

a) process Customer Personal Data only on Customer's documented instructions,
   which are the Agreement, this DPA and Customer's use of the Service's
   configuration and APIs, unless the law requires otherwise, in which case
   MOQOM will tell Customer first unless the law forbids it;
b) ensure that anyone authorised to process it is bound by confidentiality;
c) implement the technical and organisational measures in **Annex II**;
d) engage subprocessors only under section 5;
e) assist Customer with data subject requests (section 6), security, breach
   notification, data protection impact assessments and prior consultation,
   taking into account the nature of processing;
f) at Customer's choice, delete or return Customer Personal Data at the end of
   the Service, within 30 days, except where law requires retention;
g) make available the information needed to demonstrate compliance and allow
   for audits (section 8);
h) inform Customer if, in its opinion, an instruction infringes data
   protection law.

## 4. Media

The Service relays real-time audio and video and **does not record or store
media** unless Customer configures recording to a storage bucket Customer
controls. Where Customer's clients enable [SFrame](https://www.rfc-editor.org/rfc/rfc9605) end-to-end encryption, MOQOM
cannot access media content.

## 5. Subprocessors

5.1 Customer gives general authorisation for the subprocessors listed at
**[URL of SUBPROCESSORS page]**.

5.2 MOQOM will give at least **30 days'** notice of a new subprocessor by
**[email / the page's change feed]**. Customer may object on reasonable data
protection grounds within that period; if the parties cannot resolve it,
Customer may terminate the affected Service and receive a refund of prepaid,
unused credit.

5.3 MOQOM imposes data protection obligations on each subprocessor that are
no less protective than this DPA and remains liable for its subprocessors.

## 6. Data subject requests

MOQOM will promptly, and within 10 days, forward any request it receives
concerning Customer Personal Data to Customer and will not respond itself
except to direct the requester to Customer. The Service's admin API allows
Customer to retrieve, correct and delete participant data.

## 7. Personal data breaches

MOQOM will notify Customer **without undue delay, and in any event within 48
hours**, after becoming aware of a personal data breach affecting Customer
Personal Data, with the information in GDPR Art. 33(3) as it becomes
available, and will take reasonable steps to contain it.

## 8. Audits

8.1 On request, MOQOM will provide its most recent independent audit report
(once one exists), its security documentation, and answers to a reasonable
security questionnaire once a year.

8.2 Where these are insufficient to demonstrate compliance, Customer may
conduct an audit, on 30 days' notice, during business hours, at Customer's
cost, no more than once a year (or after a breach), under confidentiality.

## 9. International transfers

9.1 Customer Personal Data is processed in the United States.

9.2 For transfers from the EEA, the parties incorporate the **Standard
Contractual Clauses** (Commission Implementing Decision (EU) 2021/914),
**Module 2** (controller to processor) or **Module 3** (processor to processor)
as applicable, with: Clause 7 (docking) included; Clause 9 option 2 (general
authorisation, 30 days); Clause 11 optional language omitted; Clause 13 as per
Annex I; Clause 17 the law of **[Ireland]**; Clause 18 the courts of
**[Ireland]**. Annexes I–III of the SCCs are Annexes I–II and the subprocessor
list of this DPA.

9.3 For transfers from the UK, the **UK International Data Transfer
Addendum** (version B1.0) applies, with the Tables completed by reference to
this DPA. For Switzerland, the SCCs apply with the FDPIC as competent
authority and references to the GDPR read as the Swiss FADP.

## 10. California (CCPA)

To the extent MOQOM processes personal information subject to the CCPA, MOQOM
is a **service provider** and will not: sell or share it; retain, use or
disclose it for any purpose other than the business purposes in the Agreement,
or outside the direct business relationship; or combine it with personal
information from other sources except as the CCPA permits. MOQOM will comply
with the CCPA, provide the same level of privacy protection, notify Customer if
it can no longer meet these obligations, and allow Customer to take reasonable
steps to stop and remediate unauthorised use. MOQOM certifies that it
understands these restrictions.

## 11. Liability and precedence

Liability under this DPA is subject to the Agreement's limitations. If this
DPA conflicts with the Agreement, this DPA governs as to data protection; the
SCCs govern over both.

---

## Annex I: Details of processing

| | |
| --- | --- |
| **Data exporter** | Customer; contact: [name, email] |
| **Data importer** | Elliott Wave LLC, United States; contact: Elliott, elliott@moqom.cloud |
| **Data subjects** | Customer's end users who join rooms (participants), and Customer's staff who moderate them |
| **Categories of data** | Participant identity strings assigned by Customer; device identifiers; room names and metadata; IP addresses; presence and connection events; moderation actions; real-time audio and video in transit |
| **Special categories** | None intended. Media may incidentally reveal special-category data; it is relayed, not stored, and can be end-to-end encrypted |
| **Frequency** | Continuous, while the Service is used |
| **Nature and purpose** | Relaying real-time media; room and participant management; moderation; usage metering for billing |
| **Retention** | Media: not stored. IP addresses: 30 days after last seen. Departed participants: 400 days. Expired bans: 90 days. Others per MOQOM's retention schedule, or deletion on Customer's instruction |
| **Subprocessor transfers** | Google Cloud (hosting), for the duration of the Agreement |
| **Competent supervisory authority** | [Per Clause 13] |

## Annex II: Technical and organisational measures

Summarised from MOQOM's control catalog (`docs/compliance/README.md`):

- **Encryption**: [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) ([QUIC](https://www.rfc-editor.org/rfc/rfc9000)) and TLS ≥1.2 in transit; mutual TLS between
  MOQOM components; AES-256 at rest; optional [SFrame](https://www.rfc-editor.org/rfc/rfc9605) end-to-end media encryption.
- **Access control**: a single administrator with MFA; workload identities
  without long-lived keys; least-privilege service accounts; tenant-scoped,
  hashed API keys.
- **Network**: private nodes and database; only media, HTTPS and the join
  endpoint exposed; firewall configuration checked automatically every 12 hours.
- **Logging and monitoring**: administrative audit logs retained 400 days;
  data-access logs for secrets, database administration, IAM and storage;
  alerts on IAM, firewall, secret access and logging changes.
- **Availability**: daily database backups with point-in-time recovery; relays
  in two regions.
- **Data minimisation and retention**: no media storage; automated retention
  enforcement.
- **Vulnerability management**: automated dependency, container, secret and
  infrastructure scanning; fixes within 7/30/90 days by severity; a public
  vulnerability disclosure address.
- **Incident response**: documented plan; notification within 48 hours.
- **Independent assurance**: none yet. A SOC 2 examination is planned.
