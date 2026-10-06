# Trust and safety: privacy and compliance

Draft. We have contacted legal counsel to review this. Not legal advice.

Covers the bot and synthetic-media detection in `moqom/go/internal/trust`,
described in [TRUST-AND-SAFETY.md](../TRUST-AND-SAFETY.md) (private). **Status:
Planned.** The code is built and tested on synthetic data and processes no
real personal data yet. Nothing below is operating. Every requirement marked
**Before launch** must be met before the detector sees production traffic,
including in shadow mode, because shadow mode still profiles.

## What changes

Until now [GDPR.md](GDPR.md) could say MOQOM does **no profiling**. Turning the
detector on ends that. It evaluates participants' behaviour to predict whether
they are automated, which is profiling under Art. 4(4) GDPR. Some decisions
that follow from it (exclusion from counts and billing, holding a tip) are
made by automated means.

## Data and minimisation

| Data | Form kept | Kept for |
| --- | --- | --- |
| Account and device identifiers | HMAC pseudonyms, tenant-scoped; device also platform-scoped | Profile: 30 days after last seen |
| IP address | ASN, network class, HMAC of /24 or /48, country. The address itself is never stored by this system | As above |
| Behaviour (joins, watch time, switches, mints, chat and tip *timing*) | Running statistics only | As above |
| Transport (RTT, loss, fingerprint pseudonym) | Running statistics | As above |
| Media | **None stored.** Object sizes and group timing only. Opt-in classifier sees sampled keyframes transiently, never E2EE streams | Not stored |
| Message text, tip amounts | **Not collected.** Timing and a coarse amount band | — |
| Assessments that led to an action, reviews, appeals | Audit log (score, reasons, model version, actor) | 400 days, with the moderation log |
| Cross-tenant bot confirmations | Platform pseudonym + tenant pseudonym, human-confirmed only | **To decide**: proposed 400 days or until retracted |

No special-category data is processed by design. The detector does not
recognise faces or voices. The optional classifier judges whether a frame
looks synthetic, not who is in it. That keeps it outside biometric
identification, but counsel should confirm for the specific vendor.

## Roles

Room participants' data is processed by MOQOM **as processor** for the tenant
([GDPR.md](GDPR.md)). Detection splits:

- **Per-tenant moderation** (scores, reviews and actions inside one tenant):
  processor, on the tenant's instructions. Tenants choose the policy, turn
  enforcement on, and own their moderators' decisions.
- **Billing integrity** (not charging tenants for bots) and **platform
  security**: MOQOM's own purposes, so MOQOM is **controller** for that
  processing.
- **Cross-tenant index**: MOQOM is **controller**. It combines data across
  controllers, and nobody else can be controller for it.

**Before launch:** update the [DPA template](DPA-TEMPLATE.md) to describe the
processing and MOQOM's controller purposes. Add Art. 30 records for both
roles. List any classifier vendor in [SUBPROCESSORS.md](SUBPROCESSORS.md),
with a DPA and transfer mechanism, before any tenant opts in.

## Lawful basis

**Legitimate interests (Art. 6(1)(f))**:

- preventing fraud (Recital 47): fake audiences, laundered tips, billing for
  non-humans;
- network and information security (Recital 49);
- for tenants, keeping their community and metrics honest.

Consent is not suitable: a bot operator would withhold it, and consent must
be as easy to withdraw as to give.

**Before launch:** a written legitimate-interests assessment covering the
purpose test, the necessity test (could less data do it? This is why the
schema is minimised), and the balancing test. The balancing test leans on:

- pseudonymisation;
- 30-day retention;
- no media, text or raw addresses;
- shadow mode by default;
- human review before any kick or ban, and of every publisher;
- a working appeal path.

Participants reasonably expect a live platform to fight bots and fraud.

## Article 22: automated decisions

Art. 22 restricts decisions **based solely on automated processing** that
produce **legal or similarly significant effects**.

| Action | Automated? | Significant effect on the subject? | Position |
| --- | --- | --- | --- |
| Log, shadow-flag | yes | no | Outside Art. 22 |
| Step-up challenge | yes | low: a short delay or task | Outside, if challenges are rare and passable (rate-limited to one an hour) |
| Throttle | yes | low to moderate | Keep short-lived; reviewable |
| Exclude a **viewer** from counts, billing, tips | yes | Low for the viewer, who still watches. **Can be significant for a streamer whose genuine viewers are wrongly excluded**, and for a viewer whose tip is held | Treat as potentially significant: tip holds go to a person; every automatic action is recorded as an appealable case (built); **Before launch:** streamers can see and contest exclusion totals in their room (not built) |
| Kick, ban | **no: human review first** (`HumanFrom = kick`) | yes | Human decides; the model only proposes |
| Any action on a **publisher** above shadow-flag | **no: always reviewed** | yes (income) | Human decides |

**Recommendation (built in as the default; tenants must not be able to turn it
off for bans):** a person reviews every kick and ban, and every action against
a publisher, before it takes effect. The reviewer must have authority and
competence to disagree, and must see the reasons, not just the score. Counsel
should confirm that this is "meaningful human involvement" (EDPB guidelines
on automated decision-making, WP251rev.01) and not a rubber stamp.
Reviewer-agreement rates should be monitored. A reviewer who agrees 100% of
the time is a warning sign.

**Four-eyes on appeals needs two people.** The compliance programme has one
([README](README.md), Roles). Until there is a second reviewer, appeals
cannot be decided under the four-eyes rule (the code refuses with
`ErrSameEyes`). Decide who the second reviewer is (a contractor under NDA,
or the tenant's own moderator for that tenant's cases) **before launch**.

UK: the Data (Use and Access) Act 2025 relaxes the UK Art. 22 restrictions
outside special-category data but keeps safeguards (information, the right to
contest, human intervention). The design above meets the stricter EU rule, so
nothing extra is needed. Counsel to confirm.

## DPIA

**Required before launch.** Of the EDPB criteria (WP248rev.01), the processing
meets several:

- evaluation or scoring;
- automated decision-making with possible significant effect;
- systematic monitoring of behaviour;
- large scale once deployed;
- matching datasets (the cross-tenant index);
- innovative technology (synthetic-media classification).

Two or more criteria mean a DPIA is presumed necessary, and Art. 35(3)(a)
applies directly if exclusion or tip holds are significant. The DPIA should
cover the risks below and record the mitigations already in the design:

| Risk | Mitigation in the design |
| --- | --- |
| False positive excludes a real viewer or harms a streamer | Shadow mode default; thresholds tested against human false-positive targets (synthetic harness ≤ 2% exclusion, ≤ 0.5% kick proposals); minimum distinct reasons; human review above exclude; appeals |
| Discrimination, e.g. people on VPNs, shared devices (families, internet cafés), mobile carriers with carrier-grade NAT, assistive tech that looks "regular" | VPN and shared device are weak reasons and cannot act alone; network prefix share capped in the baseline. **Before launch:** measure false positives by network class and platform on real labelled data, and re-weight |
| Function creep: scores reused for marketing, ranking, credit | Purpose limitation in the DPA; scores not exposed through any public API |
| Cross-tenant tracking | Platform pseudonyms only; k-anonymity (3+ other tenants); human-confirmed only; one weak bit disclosed; retraction on appeal |
| Inaccurate media classification (deepfake detectors are unreliable) | Never a sole reason; never automated above shadow; opt-in tenants only; never on E2EE |
| Insider misuse | Hash-chained audit log; access through the review queue only |
| Security of the profile store | Pseudonymised; keys in Secret Manager (`moqom-503700`); key rotation severs old data |

The existing "no profiling, DPIA not presumed mandatory" statement in
[GDPR.md](GDPR.md) holds only while the detector is off. It now points here.

## Transparency

Art. 13 and 14 need the privacy notice and the tenant-facing documentation to
say that profiling happens, for what purpose, on what basis, and what the
consequences are. They must also describe the right to object (Art. 21) and,
where Art. 22 safeguards apply, the right to human intervention and to contest
(Art. 22(3)).

**Wording describes outcomes, never mechanisms.** It must be honest without
being a manual for evasion. Proposed text, for counsel to review:

> **Keeping rooms genuine.** We use automated systems to detect fake accounts,
> automated viewers and fraudulent activity. Bot viewers are excluded from
> viewer counts and billing, and suspicious tips may be held for review.
> Automated systems may limit an account's activity; a person reviews any
> decision to remove or ban an account, and you can ask for a decision to be
> reviewed again by contacting the service you used. We keep the information
> used for this for up to 30 days, longer only where it supports a decision
> that was taken.

Tenant documentation (private to tenants under NDA, still outcome-level):
what the policy ladder does, that shadow mode is the default, how to review
and appeal, and that the subject is told only "removed under the platform's
policy" (`tenant_policy`).

**Tension to resolve with counsel:**

- **Access requests (Art. 15).** Assessments are personal data. Recital 63
  lets trade secrets limit what is disclosed, but not refuse everything.
  Proposed: give the fact and outcome of any assessment that led to an
  action, the categories of data used, and the right to contest. Withhold the
  exact score, weights and thresholds as trade secret and security
  information. Under Art. 22 cases the subject is owed "meaningful
  information about the logic involved", and the CJEU in *Dun & Bradstreet
  Austria* (C-203/22, 2025) read that as an explanation of the procedure and
  principles actually applied. The reasons list (plain-language reason
  codes) is the intended answer.
- **EU Digital Services Act**, where a tenant is an online platform:
  Art. 17 requires a *statement of reasons* for restrictions, including
  visibility restrictions and suspensions, that says whether automated means
  were used and gives the facts relied on. The generic `tenant_policy` code
  given to the subject may not be enough for a tenant subject to the DSA.
  MOQOM should give tenants a reason summary they can pass on (category plus
  "automated detection, reviewed by a person" where true) without exposing
  thresholds. Decide before any EU tenant enables enforcement.

## Retention

- Profiles: 30 days after last seen, enforced in code
  (`DetectorConfig.Retention`), matching the participant-IP rule.
- Audit entries: 400 days, with the moderation log.
- Cross-tenant confirmations: to decide.
- Pseudonym keys are rotated; rotation is also the bulk-erasure path.

**Before launch:** persistent storage gets sweep rules in
`internal/retention`, and rows in [DATA-RETENTION.md](DATA-RETENTION.md) move
from Planned to Implemented.

## CCPA / CPRA

The CCPA likely does not yet apply to Elliott Wave LLC as a "business"
([CCPA.md](CCPA.md)), and MOQOM follows it anyway:

- **Purpose.** Using personal information to detect security incidents and
  resist fraud is a permitted business purpose ("security and integrity"). It
  is not a sale or share, and no opt-out of sale is triggered.
- **Notice at collection** must list the category of use (security and fraud
  prevention), using the outcome wording above.
- **Automated decision-making technology** regulations (CPPA, finalised 2025):
  pre-use notice, opt-out and access duties attach to ADMT used for
  *significant decisions* (financial or lending, housing, education,
  employment, healthcare). Bot detection for a streaming room is not on that
  list. Holding a tip or a streamer's payout may come close to "financial"
  for MOTOM, so **counsel to assess when MOTOM integration is built**.
  Security and fraud uses have exceptions to the opt-out. Risk-assessment
  duties may apply to profiling at scale once the thresholds are met.
- **Sensitive personal information**: none processed (precise geolocation is
  not; country only).

## EU AI Act

- **Prohibited practices (Art. 5, applying since February 2025).** Bot
  detection is not social scoring as Art. 5(1)(c) describes it (evaluation of
  people over time based on social behaviour or personality, leading to
  detrimental treatment in unrelated contexts or disproportionate to the
  behaviour). Detection is about automation, not personality, and acts in the
  same kind of context. **The cross-tenant index is the part to check**:
  treatment in tenant B based on conduct in tenants C, D and E. It is
  limited to human-confirmed automation, gives one weak bit, and is
  retractable. Counsel to confirm it is proportionate.
- **High-risk (Annex III).** Not listed: it is not biometric identification or
  categorisation, employment, credit scoring (AI used to detect financial
  fraud is expressly carved out of the creditworthiness item), education, or
  essential services. The high-risk obligations, and their application
  timetable (which the EU has been revising), are not expected to apply.
  Re-check if the visual-prompt liveness challenge ever does face matching:
  that would be biometric.
- **Transparency (Art. 50).** Duties fall on providers and deployers of systems
  that *generate* synthetic content or are deepfakes. MOQOM detects; it does
  not generate. If MOQOM ever labels a stream as AI-generated to viewers, the
  label must be accurate, which the detector cannot guarantee (see the limits
  in TRUST-AND-SAFETY.md). Prefer labelling only on a valid C2PA declaration.
- **AI literacy (Art. 4)**: reviewers should be trained on what the scores mean
  and don't. One page and a record that it was read is enough at this size.

## Before launch checklist

1. DPIA completed and signed off; LIA written.
2. GDPR.md, DPA template, Art. 30 records and the privacy notice updated.
3. Sub-processor listed and DPA in place for any classifier vendor.
4. A second reviewer exists for appeals.
5. Human review before every kick, ban and publisher action, confirmed in
   each tenant's policy (not configurable away).
6. False-positive rates measured by network class and platform on real
   labelled data in shadow mode, before enforcement is turned on anywhere.
7. Access-request and DSA statement-of-reasons answers agreed.
8. Retention sweep rules for any persistent trust storage.
9. Reviewer training note.
10. Patent provisional filed before any public description
    ([INVENTION-DISCLOSURE-TRUST.md](../INVENTION-DISCLOSURE-TRUST.md)).
