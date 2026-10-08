# Other obligations

Short notes. We have contacted legal counsel to review this.

## COPPA (children under 13, US)

MOQOM is infrastructure sold to developers, not a service directed to
children, and it does not knowingly collect personal information from
children itself. But a tenant can build a child-directed app on MOQOM, and
participants' IPs, identity strings and live audio/video are "personal
information" under COPPA (16 CFR Part 312, as amended in 2025).

- The terms require tenants whose apps are directed to children, or who know
  a user is under 13, to obtain verifiable parental consent and to tell MOQOM.
  **Planned**: add this to the terms of service and the DPA.
- MOQOM acts only as the tenant's service provider for such data and applies
  the same retention schedule (IPs 30 days).
- The tenant account itself is for adults (18+, or the age of majority).
- GDPR Art. 8 and the UK Age Appropriate Design Code are analogous for EU/UK
  children; same approach.

## DMCA (US copyright)

MOQOM relays live media and stores none, so most of what DMCA §512(c) covers
(storage at the direction of a user) does not arise. The §512(a) conduit safe
harbour is the closer fit for relaying, and needs no agent. The sites and
any future user-hosted content would need a designated agent.

- **Planned**: register a DMCA designated agent with the US Copyright Office
  (online, renewed every 3 years) and publish the agent's contact on
  moqom.cloud; adopt and publish a repeat-infringer policy (required for every
  §512 safe harbour).
- Notices go to the agent address; MOQOM forwards them to the tenant whose
  room is involved and can end the room through the moderation API.

## Accessibility

Target: **WCAG 2.2 Level AA** for moqom.cloud, moqom.dev and the elliottwave
sites. Relevant laws: ADA Title III (US case law); the European Accessibility
Act (applies from 28 June 2025 to certain consumer-facing services,
including e-commerce; MOQOM's B2B signup may fall outside it, but meeting
the standard is cheaper than arguing scope).

- **Planned**: an automated accessibility check (axe-core or pa11y) in the
  sites' build, failing on serious violations; a manual keyboard and
  screen-reader pass before each launch; an accessibility statement page.
- The SDKs should expose captions or transcript tracks so tenants can build
  accessible apps. That is a product roadmap item, not a compliance control.

## EU Digital Services Act

MOQOM is an intermediary service. Relaying live media is closest to **"mere
conduit"** (Art. 4). The obligations on all intermediaries still apply:

| Article | Obligation | Status |
| --- | --- | --- |
| Art. 11 | Single point of contact for EU authorities, published | Planned: name security@moqom.cloud (or a dsa@ address) on the site, with languages |
| Art. 13 | Legal representative in the EU, if no EU establishment and services offered in the EU | Planned: decide with counsel; can be combined with the GDPR Art. 27 representative |
| Art. 14 | Terms describe content restrictions and moderation | Planned: terms update |
| Art. 15 | Annual transparency report on moderation | Exempt while a micro or small enterprise (Art. 15(2)) |

Hosting-service obligations (notice and action, Art. 16; statement of
reasons, Art. 17) apply only if MOQOM stores content, for example if
recording is ever enabled.

## Lawful intercept and law-enforcement requests

MOQOM cannot produce media it does not store, and cannot decrypt SFrame media.
Requests for stored data (account, logs) need valid legal process. Record each
request; notify the affected tenant unless prohibited. A published policy is
Planned.

## Export controls

The SDKs use standard cryptography (TLS, SFrame) and are published as open
source. Publicly available encryption source code is generally outside the US
EAR (§734.3(b)(3), §742.15(b)). The private relay and any binary distribution
may need classification (likely 5D002 with a mass-market or License Exception
ENC basis). Counsel to confirm.
