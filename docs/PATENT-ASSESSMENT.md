# Patent assessment

> **Confidential — attorney work product once shared with counsel.** This
> document must never be published, and it is the reason `moqom` stays
> private. It describes inventions in more detail than the code does, and a
> public copy would be prior art against the applications it discusses.
>
> It is an engineering triage written for a patent attorney to start from, not
> legal advice and not a freedom-to-operate opinion. Novelty ratings are an
> engineer's reading of the prior art the plan already cites; nobody has run a
> professional search.

Owner of record: **Elliott Wave LLC**. Status: **patent applications pending**.

## The five things that matter more than any single claim

### 1. Publishing an SDK under Apache-2.0 licenses our own patents

Section 3 of Apache-2.0 grants every recipient a perpetual, royalty-free patent
license to claims *necessarily infringed* by the contribution as published.
Elliott Wave LLC is the contributor of everything in `moqom-swift`, `moqom-go`,
`moqom-rust` and the Apache half of `moqom-engine`. The moment those go public,
any pending claim that reads on what they do is licensed to every user of that
code and its derivatives.

What survives:

- **Claims on things the Apache code does not do.** Relay-side, control-plane
  and director methods live in BUSL-1.1 code. BUSL has no express patent grant,
  though an implied license to licensees is arguable — counsel should decide
  whether the BUSL Additional Use Grant needs a sentence on patents either way.
- **Claims against independent implementations.** A competitor who writes their
  own relay, or their own client, from our published design gets no Section 3
  license, because they did not use our code.
- **Defensive termination.** Anyone who sues over a patent reading on the SDK
  loses their license to it.

So: **draft system and server-side claims deliberately**, so the commercially
valuable claims read on the relay, director, or a relay-plus-client
combination, rather than on behaviour a lone client exhibits.

### 2. Publication before filing forfeits most of the world

The US gives a one-year grace period after the inventor's own disclosure
(35 U.S.C. §102(b)(1)). The European Patent Convention, China, Korea and most
other jurisdictions apply absolute novelty: a public GitHub repository, a blog
post, or an IETF Internet-Draft dated before the priority date destroys the
claim there.

**Rule: nothing describing an invention becomes public until a filing covering
it is on record.** That applies to:

- flipping any repository to public — `git log -p` publishes every past
  version, not just `HEAD`;
- READMEs, which in this project describe mechanisms in unusual depth;
- `docs/PLAN.md`, which is the most complete description of all of them;
- IETF drafts and mailing-list posts (see point 4).

Before each repository goes public, counsel should check its full history
against the filed claims, not just the current tree.

### 3. Patent or trade secret, per invention — not both

A patent publishes the invention; a trade secret depends on it never being
published. `LICENSING.md` already chose trade secret for `moqom-cc`, shipped as a
BUSL prebuilt binary. That choice is incompatible with claiming it in a filing,
because the application will itself be published at 18 months. Each candidate
below says which route fits.

### 4. IETF participation creates disclosure obligations

The plan proposes contributing the dependency/deadline descriptor to the MoQ
working group, and interop work puts us on the list. Under BCP 79 (RFC 8179),
a participant who knows of a patent or *pending application* they own that
covers a contribution must disclose it to the IETF. Disclosing is not optional,
and a licensing declaration (royalty-free, FRAND, or none) will be expected. A
royalty-bearing patent on an IETF-track mechanism will also cost adoption. Pick
the posture **before** the first contribution: the usual choice for a
standards play is a royalty-free declaration with defensive suspension.

### 5. Ownership has to be documented, not asserted

- **Assignment.** Copyright and inventions vest in the individual author or
  inventor. Elliott Wave LLC owns them only through a signed written assignment
  from Elliott, covering work back to the first commit. Commits exist under two
  addresses, `elliott@moqom.cloud` and a personal Gmail; the assignment should
  cover both. Patent applications should be filed by, or assigned to, the LLC
  and recorded at the USPTO.
- **Inventorship with AI assistance.** If code or designs were produced with
  AI tools, USPTO guidance (February 2024, following *Thaler v. Vidal*) requires
  a natural person to have made a *significant contribution* to the conception
  of each claim. US Copyright Office guidance likewise protects only the human
  authorship in AI-assisted work. Neither is a problem if Elliott conceived the
  inventions and directed the work, but counsel will ask, and contemporaneous
  evidence of conception (design notes, the plan's history, commit messages)
  is what answers it. Keep it.
- **Third-party code is not ours.** `moqom-rans` is a port of Microsoft's
  MIT-licensed `msrtc_rans`, and `vendor/quinn` is upstream's. Nothing in either
  can be claimed, and "100% owned" should be stated as "100% owned except
  identified third-party components" — which is what the `NOTICE` files now say.
- **Marking.** "Patent pending" may only be applied to something an
  application actually covers (35 U.S.C. §292). The `NOTICE` files say Elliott
  Wave LLC *has* applications pending, which is accurate; do not mark a product
  as covered until counsel maps the claims to it.

## Candidate inventions

Rated on three axes. **Novelty** is the engineer's read against prior art the
plan already knows about. **Detectability** is whether infringement by a
competitor could be shown from outside, which decides whether a patent is
enforceable in practice. **Exposure** is where the mechanism runs, which decides
whether Apache-2.0 Section 3 licenses it away.

| # | Mechanism | Built? | Novelty | Detectability | Runs in | Route |
|---|---|---|---|---|---|---|
| 1 | Aggregate-demand loop: relay tells publisher which layers have no subscribers, publisher stops encoding them, with pinned layers for recorders | Pending | Medium | High | Relay + client | **File** — system claim |
| 2 | Codec-agnostic dependency and deadline descriptor for MoQ objects, enabling precise shedding without bitstream parsing | Pending | Medium–High | High | Relay + client | **Decide first**: patent or IETF royalty-free |
| 3 | Shedding a subscriber's subgroup on elapsed lateness rather than queue depth, per-copy, without the relay choosing a lower rung | Built | Medium | Medium | Relay | **File** — server-side |
| 4 | Token possession proof bound to the relay's own certificate, single-use, from a Secure Enclave key named by `cnf.jkt`, renewable in-session via `REQUEST_UPDATE` | Built | Medium | Medium | Relay + client | **File** — combination claim |
| 5 | In-session revocation: control-plane ban propagated over the relay link closes the open QUIC session, not just future tokens | Built | Low–Medium | High | Relay + control plane | File only as a dependent claim of 4 |
| 6 | Room state as a sequence-numbered control track of absolute values with periodic snapshots, so reconnection recovers grants, mutes and stage exactly | Built (client) | Medium | Medium | Client + relay | File as system claim |
| 7 | Render-aware layer selection: subscribing to what each tile can display, combined with relay deduplication so per-viewer choice does not multiply upstream cost | Built (client) | Low–Medium | High | Client | Weak alone — fold into 1 |
| 8 | Cohost attribution signed into the client token by the tenant (`via`), first-writer-wins across reconnects | Built | Low–Medium | Low | Control plane | Business-method risk (*Alice*); probably not |
| 9 | Shared AR coordinate space anchored privately per device, so placement in a participant's room never crosses the wire, replicated as an idempotent log | In progress | Medium | Medium | Client | **File** before the sample goes public |
| 10 | Adaptive playout delay targeted at display and lip-sync needs rather than the last request, with bounded give-back | Built (client) | Medium | Low | Client | Trade secret; licensed away if public |
| 11 | Capture-clock offset anchoring that does not subtract a camera delivery from measured latency | Built | Low | Low | Measurement | Publish freely — defensive value only |
| 12 | Music detection as a pipeline change: bypass voice processing, switch encoder hint, trigger rights handling, all on-device inside the E2EE boundary | Pending | Medium | Medium | Client | File if it ships as a differentiator |
| 13 | E2EE scoped by group size: MLS for small rooms, sender keys above, one SFrame layer under both, with on-device inference inside the boundary | Pending | Low | High | Client + control plane | Do not file — MLS, Signal sender keys, Zoom E2EE and SFrame are the prior art |
| 14 | `moqom-cc` media-aware congestion control: delivery deadlines, audio/base/enhancement value hierarchy, ECN, latency objective | Pending | Medium | **Very low** | Client (binary) | **Trade secret** — already decided |
| 15 | Watchdogs as a third room entity class excluded from roster, counts and moderation | Pending | Low | High | Control plane | Do not file |
| 16 | `SO_REUSEPORT` + eBPF steering on a shard index in the QUIC connection ID | In progress | Low | Low | Relay | Do not file — QUIC-LB, Katran, Cloudflare |
| 17 | Trust and safety: relay-side replay detection from encrypted object sizes, canary layer exploiting render-aware selection, relay-verified liveness challenges, and six weaker candidates. Detailed in [INVENTION-DISCLOSURE-TRUST.md](INVENTION-DISCLOSURE-TRUST.md) | Built (private, synthetic-tested) | Medium (top three) | Low–Medium | Relay + control plane | **File provisional** on the top three before any public mention; rest trade secret |
| 18 | Role change on a live session: an in-session renewal (4) may change the grants of a session whose principal and room are fixed, only with a fresh possession proof and only with a token minted no earlier than the one it replaces; the control plane can narrow but never widen a session over the relay link (5), held as a per-person ceiling that caps later renewals and reconnections; open announcements, watches and subscriptions re-check against the new grants and are withdrawn individually while the session and playback continue; mutes are keyed to the person so they survive a move between queue and stage | Built | Low–Medium | High | Relay + control plane + client | Fold into 4 and 5 as dependent claims; describe only the outcome ("no handoff, the token's grants change") publicly |

### The ones worth an attorney's time first

**1 — Aggregate-demand encoding loop.** The strongest commercial candidate,
because it is visible from outside (a publisher's uplink drops when a layer
loses its last viewer) and it requires owning both ends — the plan says so
itself. Prior art to distinguish: LiveKit's *Dynacast*, which pauses simulcast
layers no subscriber wants, and WebRTC's REMB/TMMBR. The distinguishing
elements are likely to be MoQ-specific: demand derived from relay
subscription state across a relay mesh, the pinned-layer exception for
watchdogs, and keyframe-on-resume coordinated with the relay's group cache.
Claim the system, not the client.

**2 — Dependency/deadline descriptor.** Potentially the most novel, and the
one with a fork in the road. AV1's RTP Dependency Descriptor and SVC
scalability structures are the prior art; the claimable difference is a
codec-agnostic descriptor in MoQ object metadata that a relay uses to shed
under end-to-end encryption, where it cannot parse the bitstream. The plan
wants it to be an IETF contribution for standing. That and a royalty-bearing
patent are incompatible. Either file and declare royalty-free (standing,
defensive value, no revenue), or file and keep it proprietary (revenue,
no standing, and competitors will design around it). **File before the first
draft is posted either way.**

**3 — Lateness-based per-copy shedding.** Server-side, so Apache Section 3 does
not touch it, and it is already built. Prior art is the draft's own delivery
timeout and generic active queue management (CoDel uses sojourn time, which
is close). The claim needs the combination: per-subscriber copy task, subgroup
reset with a drop notification on the control stream, and the rule that the
relay never substitutes a lower layer on the subscriber's behalf.

**4 — Certificate-bound, single-use possession proofs.** Prior art is dense:
DPoP (RFC 9449), mTLS-bound tokens (RFC 8705), TLS channel binding (RFC 9266),
Token Binding. The candidate novelty is the specific combination for a media
relay: binding to the relay's certificate so a proof captured at one relay is
useless at another, plus renewal over an existing MOQT request stream without
reconnecting, plus the staged `observed`/`required` rollout. Expect narrow
claims. Still worth filing, because it is built and security is a selling point.

### Do not file

**13, 15, 16** are known techniques recombined, and filing them costs money and
invites an obviousness rejection that narrows everything else in the same
application. **11** is better published: a public description is prior art that
stops anyone else claiming it. **14** stays a trade secret, as decided — but a
trade secret is only protected if it is treated as one, which means access
control on the source, NDAs for anyone who sees it, and never describing its
internals in a document that leaves the company, including a patent
application.

## Prior art to search before filing

Named so an attorney's search starts in the right place:

- LiveKit — Dynacast, adaptive stream, simulcast layer pausing
- Zoom — E2EE whitepaper (2020–2021), sender keys, AI-feature trade-off
- Agora — kick semantics, which the plan cites
- IETF MoQ WG — `draft-ietf-moq-transport`, `draft-ietf-moq-secure-objects`,
  `draft-jennings-moq-mocha-mls-keying`, and all individual drafts on priority
  and dependency metadata
- AV1 RTP Dependency Descriptor; RFC 9605 (SFrame); RFC 9420 (MLS)
- RFC 9449 (DPoP), RFC 8705, RFC 9266, Token Binding (RFC 8471–8473)
- QUIC-LB (`draft-ietf-quic-load-balancers`), Katran, Cloudflare's
  `SO_REUSEPORT` eBPF writeups
- CoDel / FQ-CoDel (RFC 8289, RFC 8290) for lateness-based shedding
- Meta's `moxygen` and `moq-rs` commit history, since they implement the same
  drafts

## Checklist before any repository goes public

- [ ] Signed IP assignment, Elliott → Elliott Wave LLC, recorded
- [ ] Counsel has the candidate table and has decided file / secret / publish
      for each row
- [ ] Applications on file for every row marked **File** whose mechanism
      appears anywhere in the repository's history
- [ ] Decision recorded on the IETF posture for candidate 2
- [ ] Counsel has reviewed whether BUSL-1.1's Additional Use Grant needs a
      patent sentence
- [ ] Repository history reviewed against the claims, not just `HEAD`
- [ ] `docs/PLAN.md` confirmed absent from every public repository
