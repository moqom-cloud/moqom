# Invention disclosure: trust and safety

> **CONFIDENTIAL — never publish. Attorney work product once shared with
> counsel.** This document describes inventions in more detail than the code
> does. A public copy, or any public description of these mechanisms (a blog
> post, a conference talk, a public repository, an IETF draft, a marketing
> page, a tweet), made before a filing is on record, is prior art against our
> own applications. Outside the US that destroys novelty outright; the US
> grace period is one year from our own disclosure (35 U.S.C. §102(b)(1)) and
> is not a plan. **Recommendation: file a US provisional application covering
> candidates 1–6 before any public mention of trust and safety beyond outcome
> wording**, then decide on PCT within 12 months.
>
> An engineering triage written for a patent attorney to start from, not legal
> advice and not a freedom-to-operate opinion. Prior art below is what an
> engineer knows of; **nobody has run a professional search**. Bot detection,
> fraud scoring and deepfake detection are crowded fields with large
> portfolios (ad-fraud vendors, streaming platforms, payment processors,
> CAPTCHA and bot-management vendors). Expect most generic claims to fail.
> What may survive is what depends on MOQOM's specific architecture: a MoQ
> relay that sees object sizes and group boundaries but not media, render-aware
> layer selection, and hardware-attested device keys bound to tokens.

Owner of record: **Elliott Wave LLC**. Inventor: Elliott (sole). Code:
`moqom/go/internal/trust` (BUSL-1.1, private), conceived and reduced to
practice in 2026. Evidence of reduction to practice: the adversarial harness
in `internal/trust/sim` and its synthetic results in
[TRUST-AND-SAFETY.md](TRUST-AND-SAFETY.md).

**Where the claims must read.** Per [PATENT-ASSESSMENT.md](PATENT-ASSESSMENT.md)
§1, anything implemented in the Apache-2.0 SDKs or the Apache half of
moqom-engine licenses our patents to every recipient. Every candidate here is
drafted to read on **server-side** behaviour (relay, control plane, detection
service), or on a relay-plus-client system where the inventive step is
server-side. The canary (candidate 2) relies on client behaviour that is
already public in the SDKs, so its claim must be on the server's use of it.

**Patent or trade secret.** Detection mechanisms are only partly detectable
from outside, which weakens enforcement. For each candidate the "Route" says
whether filing beats keeping it secret.

## Summary

| # | Candidate | Novelty | Detectability of infringement | Route |
|---|---|---|---|---|
| 1 | Relay-side detection of replayed or looped live media from encrypted object sizes and group cadence | **Medium** | Medium (observable behaviour: a looped stream gets flagged) | **File** |
| 2 | Canary layer that render-aware clients never select, used as a bot signal at the relay | **Medium** | Low–Medium | **File** (system claim with render-aware selection) |
| 3 | Relay-verified liveness challenge: server-timed keyframe or track-toggle request, verified from group boundaries under E2EE | **Medium** | Medium | **File** with 1 |
| 4 | Co-audience ring detection over rare rooms with IDF weighting and link-type-weighted ring evidence | Low–Medium | Low | File as dependent / trade secret |
| 5 | Attested-device concurrency and attestation-counter replay as bot evidence in a token-gated media network | Low–Medium | Low | Dependent claim of PATENT-ASSESSMENT #4 |
| 6 | Cross-tenant k-anonymous pseudonymous bot index | Low–Medium | Low | File if it becomes a product feature |
| 7 | Keyed per-tenant, per-epoch randomisation of thresholds, feature masks and action timing | Low | Very low | **Trade secret** |
| 8 | Offset-trained residual learner with audit-weighted labelers and a per-labeler holdout gate | Low | Very low | **Trade secret** |
| 9 | Drift alarm on max median shift and tail-mass excess that halts baseline refits | Low | Very low | **Trade secret** |

## 1. Replay detection from encrypted object sizes at a MoQ relay

**Problem.** A "live" stream may be a recorded file on a loop, used to farm
tips or occupy discovery. With end-to-end encryption (SFrame) the relay
cannot decode media, so content-based duplicate detection is impossible where
the stream actually flows.

**How it works.** For each published track the relay records the size of each
*delta* object (non-keyframe) and the group cadence. It never looks at the
payload.

- A shingle index (`ReplayIndex`) stores hashes of exact 8-object size
  sequences over a sliding horizon. The recurrence ratio, meaning the share of
  recent shingles seen before, is near 1 for a byte-identical loop and near 0
  for a camera.
- Low-entropy runs (size range under 64 bytes, e.g. a static slide or black
  screen) are excluded so that live but static content is not flagged.
- A periodicity score (`LoopScore`) is the peak autocorrelation of
  *first-differenced* delta sizes at lags ≥ 8. Differencing removes slow
  bitrate trends and GOP structure, so short loops are found even through
  re-encoding.
- Either score above threshold is evidence (log-odds weight) that feeds a
  multi-signal risk score, never a sole basis for action.

**Prior art (honest).**

- *Video fingerprinting*: perceptual hashes, YouTube Content ID, audio
  fingerprinting (Shazam-style). These need decoded content.
- *Traffic-analysis fingerprinting of encrypted video*: the academic line on
  identifying which Netflix or YouTube title is playing from the burst sizes
  of encrypted DASH/HLS segments (e.g. Schuster et al., "Beauty and the
  Burst", USENIX Security 2017; Reed & Kranch 2017). This is **the closest
  art**: it shows that encrypted segment sizes fingerprint content. It
  identifies known titles for a passive eavesdropper; it does not detect
  self-recurrence of a live publisher at a cooperating relay.
- *Loop detection in surveillance video* (frozen or looped CCTV feeds) on
  decoded frames.
- *Liveness / replay detection in face anti-spoofing* on decoded frames.

**What may be novel here.** Detecting that a *live publication* repeats
*itself*, at the *media relay*, from per-object sizes of an **end-to-end
encrypted** MoQ track, combined with entropy gating and differenced-period
analysis, as an input to an enforcement pipeline. The MoQ object model
(object = one frame, group = GOP) gives frame-level granularity that segment
fingerprinting does not have.

**Draft claim directions.**

- A method at a media relay forwarding encrypted objects of a live track,
  comprising: recording a sequence of object sizes for non-keyframe objects;
  computing a recurrence measure of fixed-length size subsequences within the
  same track; excluding subsequences whose size range is below a threshold;
  and generating a replay indication when the recurrence measure exceeds a
  threshold, without decrypting any object.
- Dependent: periodicity of first-differenced sizes; combination of both
  scores; use of the indication to issue a liveness challenge (candidate 3) or
  to adjust a risk score.
- System: relay + detection service + moderation action.

**Limits to tell counsel.** Our own harness shows re-encoding a long loop on
each pass defeats both measures (0% flagged). A competitor's detector would
be visible only as behaviour.

**Novelty: Medium.** **Route: File.**

## 2. Canary layer exploiting render-aware layer selection

**Problem.** Scraper bots and view-inflation clients often subscribe to
everything a room offers, or to the highest rung available. A real viewer's
client does not.

**How it works.**

- The room advertises an additional track (e.g. a `video/4320` rung) that no
  decoder in a real MOQOM client accepts. MOQOM's render-aware selection
  subscribes only to what a tile can display.
- The relay counts subscriptions to the canary and forwards nothing useful on
  it.
- A subscription is strong bot evidence (weight 5.5).
- The canary's name and parameters can be rotated per room and epoch, so a
  farm cannot simply blocklist one name.

**Prior art.**

- *Honeypots and honeytokens* in security generally: honey links invisible to
  humans in web pages to trap crawlers, honey fields in forms.
- *Ad-fraud invisible pixels and hidden ad slots* to detect non-human
  impressions.
- *CDN bot-management "trap" URLs.*
- Render-aware / viewport-aware ABR and layer selection (simulcast in WebRTC
  SFUs, "subscribe to what you render" in SDKs such as LiveKit's adaptive
  stream).

**What may be novel here.** Pairing render-aware layer selection, which
guarantees that genuine clients never select a given layer, with a media
relay that advertises such a layer as a honeytoken *in the media catalog* of
a live session and treats subscription to it as bot evidence. Honeytokens in
web pages are old; a honeytoken layer in a media catalog whose safety rests on
the client's selection algorithm may not be.

**Draft claim directions.** A system comprising a relay that publishes, in a
live session's catalog, at least one layer having parameters outside what any
conforming receiver will select under a render-aware selection rule; and a
detection service that, upon a subscription to that layer, increases a bot
risk score of the subscriber. Dependent: rotation of canary parameters per
session or epoch; the layer carries no decodable media.

**Exposure note.** Render-aware selection lives in the Apache SDKs; the claim
must be on the relay and detector, not on the client never selecting it.
**Do not document the canary in any SDK, README or catalog spec.**

**Novelty: Medium.** **Route: File** (system claim). Keep the specific
parameters secret.

## 3. Relay-verified liveness challenge from group boundaries

**Problem.** Proving that a publisher is live and responsive without
decoding the media, and under E2EE.

**How it works.** The detection service sends a randomly chosen challenge to
the publisher's client:

- start a new group (keyframe) within 1.5 s; or
- stop and resume the track within a window; or
- show a visual prompt, judged by a classifier where permitted.

The relay, which sees group starts and track state but not content, verifies
the first two from timing alone. The challenge kind is chosen at random,
challenges are rate-limited per device, and a failure is evidence, not proof.
A file-replay tool that does not implement the protocol fails. A real-time
synthetic pipeline can pass, which the disclosure must state.

**Prior art.**

- *Challenge-response liveness in biometrics* (blink, turn your head, read
  digits) on decoded video.
- *Video conferencing "request keyframe" (PLI/FIR in RTP)*: a receiver asking
  for a keyframe is decades old. **The mechanism is standard; only its use as
  a liveness test is new.**
- *Remote attestation and proof-of-presence protocols.*

**What may be novel here.** Using a keyframe or track-state request whose
timing is verified at a relay that cannot decrypt the media, as a liveness
proof for a live publisher, chosen at random among kinds and combined with
size-based replay detection (candidate 1) as the trigger.

**Draft claim directions.** A method comprising: detecting a replay
indication for a live track at a relay (candidate 1); sending the publisher a
challenge selected at random from a set including a group-start request and
a track-state toggle; observing at the relay, without decryption, whether a
group start or toggle occurred within a deadline; and adjusting a risk score
accordingly.

**Novelty: Medium** (as a combination with 1; Low alone). **Route: File
together with 1.**

## 4. Co-audience ring detection over rare rooms

**Problem.** Bot farms stagger their arrival times to defeat co-arrival
detection, but still have to attend the same customers' rooms.

**How it works.** Accounts are linked when the IDF-weighted Jaccard
similarity of the *rare* rooms they attended is ≥ 0.6 with at least 3 rare
rooms shared. Rooms attended by more than 10% of a tenant's audience are
ignored. Rings are connected components (union-find) over device, co-arrival
and co-audience links. Ring membership is weighted by **link type**:
co-audience-only membership counts for less, because real fans of a botted
streamer also share that room.

**Prior art.** Heavy. Co-visitation and co-engagement graphs for fake-account
and fake-engagement detection:

- CopyCatch (Facebook, WWW 2013): lockstep page likes;
- SynchroTrap (CCS 2014): synchronised actions;
- the FRAUDAR and dense-subgraph line;
- review-spam group detection;
- ad-click fraud rings.

TF-IDF/Jaccard similarity is textbook. LSH for co-visitation is standard.

**What may be novel here.** Little in the algorithm. Possibly the
combination of rare-room filtering with link-type-weighted evidence feeding a
per-participant log-odds score in a live media service, and its use to hold
tips between ring members (candidate 4 + MOTOM). Probably not independently
patentable.

**Draft claim directions.** Only as a dependent of a system claim:
"…wherein the coordination indication comprises membership in a component
linked by attendance similarity over sessions whose audience share is below a
threshold, weighted less than membership by device or timing linkage."

**Novelty: Low–Medium.** **Route: Trade secret**, or dependent claim only.

## 5. Attested-device concurrency and counter replay

**Problem.** Farms clone one real device's credential across many headless
clients, or run one device in many rooms at once.

**How it works.** MOQOM tokens are bound to a hardware-backed device key with
a single-use possession proof (PATENT-ASSESSMENT #4). The control plane
tracks, per device key:

- concurrent sessions;
- monotonic attestation counters;
- possession-proof nonces.

A counter that fails to advance, or a reused nonce, is strong evidence of a
captured and replayed attestation. Concurrency above a keyed threshold is
evidence of one device serving many viewers.

**Prior art.**

- Apple App Attest and Google Play Integrity both document counter checks and
  replay protection; FIDO/WebAuthn signature counters exist specifically to
  detect cloned authenticators.
- Concurrent-stream limits per account are universal in streaming (Netflix
  screens).

**What may be novel here.** Very little standalone. As a dependent claim of
the token/possession-proof invention, feeding a bot-risk score rather than a
hard rejection, it adds claim depth.

**Novelty: Low–Medium.** **Route: dependent claim of PATENT-ASSESSMENT #4.**

## 6. Cross-tenant k-anonymous pseudonymous bot index

**Problem.** A farm moves between tenants. Tenants must not see each
other's users, and a tenant must not be able to learn what one competitor
observed.

**How it works.** Devices have a platform-scoped HMAC pseudonym, separate
from the tenant-scoped one. A confirmation is recorded only when a
**trusted human reviewer** labels a device a bot. A tenant sees only a single
weak bit, "known elsewhere", and only when **3 or more other tenants** have
confirmed it. Its own confirmations never count towards it. Human overturns
retract.

**Prior art.**

- Shared fraud consortia and blocklists (payment-fraud networks, email
  reputation, Spamhaus, MaxMind minFraud), and device-reputation services.
- k-anonymity (Sweeney 2002) and threshold disclosure.
- Private set intersection for threat-intel sharing.

**What may be novel here.** Possibly the specific combination: per-tenant and
platform pseudonym scopes in a multi-tenant media platform, a disclosure rule
excluding the asking tenant's own contributions, human-confirmed-only
membership with retraction on appeal, and output limited to one evidence bit
in a log-odds score. A narrow claim at best.

**Novelty: Low–Medium.** **Route: file only if it becomes a marketed
feature**; otherwise trade secret.

## 7. Keyed randomisation of thresholds, features and timing

**Problem.** An adaptive attacker probes to find decision boundaries.

**How it works.** Every rule threshold is jittered ±12% by an HMAC of
(tenant, epoch, name). 20% of anomaly features are masked per tenant per
epoch. Policy step boundaries are jittered ±0.3 log-odds. Enforcement is
delayed a random 0–3 minutes and carries a generic code. Harness (synthetic):
a prober 0.05 log-odds under a found boundary is caught 45.8% of the time with
keyed thresholds and 0% with fixed ones.

**Prior art.**

- Randomised defences against adversarial ML (randomised smoothing,
  moving-target defence).
- Randomised thresholds in fraud systems to resist probing; delayed bans
  (game anti-cheat ban waves are the classic example).

**Novelty: Low.** Infringement is undetectable from outside. **Route: Trade
secret.** Do not file.

## 8. Residual learner with audit-weighted labelers and per-labeler holdout gate

**How it works.**

- Logistic regression trained with the rule-and-anomaly log-odds as a fixed
  offset, so it learns only a bounded correction.
- Labeler weight comes from trust, a daily cap, contradiction of a confident
  model, and blind-audit agreement rescaled as 2·accuracy − 1, applied
  retroactively.
- A retrain is refused if it worsens held-out trusted labels pooled **or for
  any single trusted labeler**, which stops a compromised trusted account from
  outvoting reviewers by volume.

**Prior art.**

- Stacking and offset/residual models (GLM offsets).
- Learning from crowds with annotator reliability (Dawid–Skene 1979; Raykar
  et al. 2010).
- Data-poisoning defences (RONI, "reject on negative impact", Nelson et al.
  2008, is close to the holdout gate).
- Influence-based filtering.

**Novelty: Low.** **Route: Trade secret.**

## 9. Drift alarm halting baseline refits

**How it works.** A refit compares new robust statistics with the last
accepted ones. The score is the larger of the largest per-feature median
shift (in robust-scale units) and the excess tail mass beyond the old 95th
percentile. Above 1.0, refits stop until a person accepts. Harness
(synthetic): the alarm fires at step 20 of 40. With it honoured the farm's
largest z stays at 5.6; overridden it falls to 3.7, below the contribution
threshold.

**Prior art.** Concept-drift detection (DDM, ADWIN, Page–Hinkley, KS tests,
PSI in credit scoring), and human-in-the-loop model gating.

**Novelty: Low.** **Route: Trade secret.**

## Not candidates

- C2PA verification and external deepfake classifiers: standard integrations.
- Graded enforcement ladders, review queues, four-eyes appeals, hash-chained
  audit logs: well known, and business-method risk under *Alice*.
- Excluding flagged bots from view counts and billing: industry practice
  (MRC invalid-traffic guidelines). An outcome, not a mechanism.

## Before filing

1. Counsel to run a professional search on 1–3, starting with the
   encrypted-traffic fingerprinting literature for 1 and honeytoken and
   bot-trap patents for 2.
2. Decide the trade-secret list (7–9, the specific canary parameters, rule
   weights) and keep it out of every application. A patent publishes what it
   claims.
3. Confirm that no Apache-2.0 repository implements anything claimed, and
   that the relay-side statistics emitter, if it lands in moqom-engine, emits
   generic statistics only (no replay scoring, no canary semantics).
4. Inventor assignment to Elliott Wave LLC on record (PATENT-ASSESSMENT §5).
5. **No public mention** (docs, site, talks, SDK comments, IETF) until the
   provisional is filed. Outcome wording only, even then.
