# Trust and safety: bots and synthetic media

> **Private.** This describes detection mechanisms in more detail than anything
> public may. Do not publish it, quote it in a public issue, or paraphrase it
> in marketing. Several mechanisms are patent candidates
> ([INVENTION-DISCLOSURE-TRUST.md](INVENTION-DISCLOSURE-TRUST.md)), and a public
> description before filing destroys novelty outside the US. It also hands
> the attacker the feature list.
>
> Public wording describes outcomes only, for example: "Bot viewers are
> excluded from viewer counts and billing." "Suspicious activity may be
> reviewed by a person." Never mention signals, thresholds, models or
> canaries.

Code: `moqom/go/internal/trust` (detection, policy, review, audit, the
MOTOM boundary), `moqom/go/internal/trust/sim` (adversarial harness),
`moqom/go/cmd/moqom-trust-eval` (prints the harness tables). Everything is
private Go. Nothing here is in `moqom/proto`, the SDKs or `moqom-engine`,
because Apache-2.0 code licenses our patents to every recipient
([PATENT-ASSESSMENT.md](PATENT-ASSESSMENT.md) §1). Status: **built and tested
against synthetic data; not wired into a running deployment.**

## What it is for

1. **Bot viewers** inflate a streamer's audience, which is fraud against
   advertisers, against other streamers on a tenant's leaderboard, and against
   the tenant, who pays egress for them.
2. **Bot streamers** loop a recorded file or a synthetic face as if live, to
   farm tips, game discovery or run scams.
3. **Coordinated rings** of both, plus tips that go round in a circle to
   launder money or game a payout ranking.
4. **Synthetic media**: a live feed that is AI-generated, or a deepfake of a
   real person.

## Architecture

```
relay (generic stats only) ──┐
control plane (joins, mints, ├─► Event ─► Detector ─► Assessment ─► Decide ─► Queue ─► Enforcer
  presence, attestation)     │   (privacy-     │  per participant   │ policy   │ review,   (moderation
client telemetry (claims) ───┘    minimised)   │  and per room      │ ladder   │ appeal,    API)
                                               │                    │          │ audit
                              rules + anomaly baseline + learner    │          ├─► Exclusions ─► counts, billing
                              + ring graph + bursts + media evidence│          └─► TipScorer ─► MOTOM (TipRiskSink)
```

- **Event** (`signal.go`) is the only input. It carries pseudonyms and coarse
  classes, never usernames, addresses, media or message text. A test fails if
  a field with such a name is added.
- **Detector** (`detector.go`) keeps a `Profile` of running summaries per
  participant per room, a `DeviceState` per device key, and per tenant a ring
  `Graph`, join `Bursts`, an anomaly `Baseline`, a `Learner` and Platt
  calibration. `Assess` returns a score in [0, 1], the reasons behind it, and
  the model version. `AssessRoom` rolls participants up to a room.
- **Decide** (`policy.go`) turns an assessment into a graded action under the
  tenant's `Policy`. **Queue** applies the automatic part, opens human reviews
  and appeals, and writes everything to a hash-chained `AuditLog`.
- **Exclusions** (`exclusion.go`) is the set of participants left out of counts
  and billing. It plugs into `server.Config.Audience` and
  `billing.Config.Usage`.
- **TipScorer** (`tips.go`) is the MOTOM boundary.

### Where signals come from, and why

Signals are preferred in this order: **relay-observed**, then
**control-plane-observed**, then **client-claimed**. A bot controls its own
client, so a client claim is evidence only when it contradicts something the
server saw (`telemetry_mismatch`). Never when it agrees.

The relay is `moqom-engine/moqom-relay` (BUSL-1.1). It should emit **generic
statistics only**: per-session RTT and loss, per-track object sizes and group
cadence, subscribe/unsubscribe, and a counter for a named track. That is
operationally useful and reveals no detection logic, so it stays safe if
moqom-engine is ever published. The detection happens in private code that
consumes those statistics. **Open decision:** whether the emitter lives in
the relay or in a private sidecar that reads the relay's existing metrics.

## Signals (the privacy-minimised schema)

| Signal | Source | Stored as | Raw form discarded |
|---|---|---|---|
| Account, device | control plane | HMAC pseudonym, tenant-scoped; device also platform-scoped for the cross-tenant index | yes |
| IP address | relay / control plane | ASN, network class (residential, mobile, hosting, VPN, Tor), HMAC of /24 or /48, country | immediately. `ClassifyAddress` is the only function that sees an address |
| Join, leave, presence, subscribe | relay / control plane | timestamps folded into running mean/variance | event-level detail after folding |
| Token mints and refreshes | control plane | per-device recent mint times (max 128) | — |
| Attestation | control plane | hardware-backed flag, monotonic counter, nonce pseudonym | the attestation itself |
| Transport | relay | RTT, RTT variance, loss, TLS/QUIC fingerprint pseudonym, headless hint | — |
| Publish statistics | relay | delta-object sizes, group cadence, recurrence. **Sizes only, never payload**; works under E2EE | — |
| Chat, tips | control plane / MOTOM | timing and amount band only | message text, exact amount |
| Canary hit | relay | a counter | — |
| Client claims | client | declared platform, hardware, headless; compared with server view | — |

Retention: a profile is forgotten 30 days after the participant was last
seen (`DetectorConfig.Retention`), matching the 30-day IP rule in
[DATA-RETENTION.md](compliance/DATA-RETENTION.md). Assessments that led to an
action live in the audit log with the moderation log (400 days). Pseudonym
keys rotate. Rotating cuts the link to old data, which is a deliberate erasure
path.

## Models

The score is a sum of log-odds, so every contribution is attributable and
every reason can be shown to a reviewer:

```
z = logit(prior 2%) + Σ matched rule weights + anomaly + media/liveness evidence
    + clamp(learned correction, ±3)
score = sigmoid(a·z + b)          # Platt, per tenant, once there are 30+ trusted labels per class
```

### 1. Rules: known patterns (`rules.go`)

| Reason | Weight | Pattern |
|---|---|---|
| `canary_subscribe` | 5.5 | subscribed to a track no real client selects (below) |
| `attestation_replay` | 5.0 | an attestation counter went backwards or a proof nonce was reused |
| `impossible_concurrency` | 3.0 | one device in ~3.5+ rooms at once |
| `looped_publish` | 3.0 | publisher's object sizes repeat (below) |
| `headless_client` | 2.5 | headless transport or client fingerprint |
| `token_farming` | 2.5 | ~3.5+ mints per session, or 30+ per device per hour |
| `ring_member` | 2.5 / 1.2 | in a coordinated ring via device or timing / via co-audience only |
| `impossible_travel`, `shared_device`, `regular_watch`, `chat_cadence` | 2.0 | as named |
| `hosting_network`, `exhaustive_subscribe`, `telemetry_mismatch`, `tip_cadence`, `known_elsewhere` | 1.5 | as named |
| `synthetic_cadence`, `join_burst`, `software_key`, `vpn` | 0.5–1.0 | weak alone, by design |

The thresholds marked "~" are **keyed** (below). No single weak reason can
reach throttle or above, because the policy also requires a minimum number of
distinct reasons.

### 2. Per-tenant online anomaly model (`anomaly.go`)

18 behavioural features per profile (session count, watch-time mean and
regularity, switch rate, tracks, mints per session, RTT, jitter, loss,
presence/chat/tip rates, hosting, hardware, countries). Two detectors over a
reservoir of each tenant's own population:

- **robust z-scores** (median and MAD, with a floor of half the standard
  deviation so a feature where most people are identical cannot divide by
  zero), on continuous features only;
- **an isolation forest** (64 trees × 128 samples), which catches unusual
  *combinations* that no single feature shows.

Both contribute only well above normal (isolation > 0.6, z > 6), up to 3.5
log-odds together. The baseline is the poisoning target, so:

- only profiles the detector currently scores below 10% are admitted, at most
  once a day each;
- one network prefix may hold at most 4% of the reservoir;
- a refit is compared with the last accepted one, and if the largest feature
  shift or the excess tail mass crosses the limit, **refits stop** until a
  person calls `AcceptDrift`.

### 3. Learner on moderator labels (`learn.go`)

Logistic regression, chosen so its weights can be explained to an auditor
and it trains without dependencies. It is **stacked**: it learns a
*correction* to the rules and anomaly score (trained with that score as an
offset), so it never re-counts evidence the rules already count, and its say
is capped at ±3 log-odds. Poisoning defences:

- trusted labelers (MOQOM reviewers, a tenant's designated moderators) count
  fully, everyone else at 0.1; a cap of 200 labels per labeler per day;
- **reputation**: a labeler who mostly contradicts a confident model loses
  75% of their weight;
- **blind audit**: a sample of each untrusted labeler's decisions is
  re-labelled by a trusted reviewer, and their weight follows agreement,
  retroactively to every label they ever gave;
- **holdout gate**: each retrain is scored on held-out trusted labels, pooled
  *and per trusted labeler*. A retrain that makes any of them worse is
  refused. A refused retrain is the alarm for a compromised trusted account.

### 4. Coordinated rings (`graph.go`)

Accounts are linked by any of:

- **shared device key**;
- **co-arrival**: joining within 2 s of each other in at least 3 distinct rooms;
- **co-audience**: IDF-weighted Jaccard ≥ 0.6 over *rare* rooms (any room
  watched by more than 10% of the tenant is ignored), with at least 3 rare
  rooms shared. This catches farms that stagger their timing and still have
  to show up for the same customers.

A ring is a connected component of 4 or more. Membership by co-audience
alone is a weaker reason (1.2) than device or timing (2.5), because real fans
of a botted streamer also share that streamer's room. **Join bursts** are
flagged against each room's own EWMA arrival rate with a Poisson tail test. A
go-live notification is also a burst, so burst membership is weak by design.

**Cross-tenant index** (`GlobalIndex`): a platform-scoped device pseudonym
confirmed as a bot by trusted reviewers in **3 or more other tenants** is a
weak reason (`known_elsewhere`) everywhere. The threshold is k-anonymity, so
one tenant cannot learn what one competitor saw. A human overturn retracts
the confirmation.

### 5. Room assessment (`room.go`)

The share of participants above the suspect line, the estimated inflation
share, the rings and bursts present, and the publisher's own assessment.
`view_inflation` and `suspect_publisher` are room-level reasons for the
tenant's moderators. `Attach` adds media or liveness evidence to a publisher.

### 6. Synthetic and replayed media (`media.go`, `replay.go`)

**What this can and cannot do.** Nobody can reliably tell a good real-time
deepfake from a camera by looking at it. Classifier accuracy published on
benchmarks drops sharply on generators the classifier was not trained on, on
compressed video, and against an attacker who tests against the classifier.
So MOQOM treats media evidence as **one input among many**: never the only
reason for an action, never a reason to kick on its own, and always a reason
for human review rather than automation. What we *can* do reliably:

- **Provenance (C2PA)**: when a stream carries a valid, trusted Content
  Credentials manifest, we know what its signer declared. If the declared
  IPTC `digitalSourceType` is synthetic (`trainedAlgorithmicMedia` and
  related), that is strong evidence (weight 4). A camera-capture declaration
  is recorded. **Absence of a manifest is not evidence of anything.** Almost
  nothing carries one today. `C2PADetector` takes a `C2PAValidator`; which
  library implements it is open.
- **Replay** (no classifier, works under E2EE): the relay sees each object's
  size. A file played on a loop produces the same size sequence again, which
  `ReplayIndex` finds by exact 8-gram shingles. Low-entropy runs (a static
  slide) are skipped. `LoopScore` finds periodicity in differenced delta-frame
  sizes, which catches short loops even when re-encoded. **Limit:** a long
  loop re-encoded on each pass defeats both (see harness).
- **External classifier hook** (`ExternalClassifier`): server-side, only for
  tenants that opted in, only on sampled keyframes (keyed random intervals,
  so a publisher cannot predict which frames are checked), never on E2EE
  streams (MOQOM cannot read them). Its key comes from the environment,
  loaded from Secret Manager in `moqom-503700`. A score ≥ 0.9 is a weight-1.5
  reason. **Open decision:** vendor, and the DPA and sub-processor listing it
  needs.
- **Liveness challenges**: the server asks a suspect publisher to do
  something a pre-recorded or offline-rendered stream cannot. Kinds:
  - `fresh_keyframe`: start a new group within 1.5 s; the relay sees group
    starts even under E2EE;
  - `track_toggle`: stop and resume the track inside a window;
  - `visual_prompt`: show a gesture or word; needs the classifier, so it is
    inconclusive without one.

  The kind is chosen at random. A challenge is defined as a JSON message in
  private code only (not in the public proto), and is limited to once an hour
  per device. A failure is a weight-2 reason. A real-time deepfake pipeline
  *can* pass the first two, which is why they are evidence and not proof.

## Adversarial hardening

### Threat model

| Attacker | Capability | Goal |
|---|---|---|
| Bot-farm operator | headless clients, proxies (residential, mobile), many accounts, scripted timing; reads our public docs and SDKs | inflate views, farm tips, avoid detection |
| Phone farm | real handsets with hardware keys, real mobile IPs | as above, with genuine devices |
| Adaptive operator | watches which of its accounts get excluded and changes behaviour | find and stay under the boundary |
| Prober | tunes one account just under a threshold it has found | learn and exploit thresholds |
| Compromised moderator / malicious tenant | files labels, appeals | poison the learner to whitelist bots or ban rivals |
| Slow poisoner | admits many borderline-normal profiles | drag the anomaly baseline towards the farm |
| Synthetic streamer | loops a file, or runs a real-time face/voice model | pass as live |
| Insider (MOQOM staff) | access to scores and reasons | look up a participant; tamper with a decision |

### Mitigations

- **Server-observed first** (above). The client cannot lie about what the
  relay saw.
- **Secret, randomised thresholds and features** (`secret.go`): every
  threshold is jittered ±12% by an HMAC of (tenant, epoch, name), and 20% of
  anomaly features are dropped per tenant per epoch (except network class and
  hardware). Policy step boundaries are jittered ±0.3 log-odds the same way.
  A boundary found in one tenant or week is wrong in the next.
- **Coarse feedback**: the subject never sees a score, a reason or a model.
  Every action carries the moderation code `tenant_policy`, the same code a
  human moderator's action carries, and actions are delayed a random 0–3
  minutes, so an operator cannot tell which session or behaviour triggered
  it.
- **Poisoning-resistant training**: labeler weighting, caps, reputation,
  blind audit, a per-labeler holdout gate, baseline admission by low score
  only, a per-prefix share cap, and a drift alarm that stops refits.
- **Canary / honeypot track**: rooms advertise a `video/4320` rung that no
  decoder accepts. Render-aware SDK selection never subscribes to it, so a
  real MOQOM client never touches it. A scraper that subscribes to
  everything does. Weight 5.5.
- **Shadow mode** is the default for every tenant (`DefaultPolicy`): the
  detector proposes and logs, nothing is enforced except a shadow flag, until
  the tenant turns enforcement on. New rules ship in shadow too.
- **Audit**: every decision, review, appeal and lift is in a SHA-256 hash
  chain that `Verify` checks. Editing an entry breaks the chain.

### Adversarial test harness

`internal/trust/sim` simulates a tenant with 3,000 humans across 40 rooms for 5
days. Farms target 6 rooms:

- **naive**: 60 accounts, headless, on hosting networks;
- **proxy**: 60 accounts on residential proxies with jitter;
- **adaptive mimic**: 60 accounts that escalate every time accounts are caught;
  - level 1 drops canary hits;
  - level 2 spreads arrivals and fakes a fingerprint;
  - level 3 uses human-like rates and cover traffic;
- **phone farm**: 45 real handsets, 3 accounts each.

There are two reviewers. One works the queue with a 2% error rate. The other
does blind spot checks, which also feed calibration. In the poisoning
scenarios a compromised moderator files 400 inverted labels a day.

The tests in `sim_test.go` enforce these targets on two seeds:

- human exclusion false positives ≤ 2%;
- human kick proposals ≤ 0.5%;
- naive farm caught ≥ 95%;
- coordinated farms caught ≥ 60%;
- the adaptive farm is forced to escalate;
- the defended learner stays within 15 points of clean under poisoning, and the
  undefended one is ≥ 30 points worse;
- replayed publishers are flagged and cameras are not;
- keyed thresholds catch probers;
- the drift alarm protects the baseline;
- the whole run is deterministic.

#### Results

> **SYNTHETIC.** Simulated populations; behaviour distributions are our
> assumptions. These numbers measure the detector against the harness, not
> against real traffic, and must never be quoted publicly. Mean over 5 seeds,
> from `go run ./cmd/moqom-trust-eval -seeds 5`.

Baseline (defended learner, no poisoning). Rows are the share caught:

| Day | Humans excluded | Humans proposed for kick | Naive | Proxy | Adaptive mimic (escalation level) | Phone farm, 3 accounts/handset | Bot share in target rooms, true / estimated |
|---|---|---|---|---|---|---|---|
| 1 | 0.0% | 0.0% | 100% | 50.0% | 50.0% (1.0) | 0.0% | 67.4% / 35.9% |
| 2 | 0.3% | 0.0% | 100% | 82.7% | 37.0% (1.8) | 92.0% | 55.1% / 39.6% |
| 3 | 0.3% | 0.0% | 100% | 93.0% | 35.0% (2.6) | 96.0% | 47.2% / 35.7% |
| 4 | 0.2% | 0.0% | 100% | 97.7% | 34.3% (2.8) | 85.8% | 42.1% / 29.8% |
| 5 | 0.2% | 0.0% | 100% | 90.0% | 2.0% (2.8) | 80.0% | 38.7% / 21.6% |

Day 5 under other conditions:

| Scenario | Humans excluded | Proxy | Phone farm | Adaptive mimic |
|---|---|---|---|---|
| Compromised moderator, defended learner | 0.4% | 100% | 82.2% | 5.7% |
| Compromised moderator, **undefended** learner | 0.1% | 38.3% | 29.3% | 0.0% |
| Phone farm with **one handset per account** | 0.3% | 100% | 2.2% | 2.7% |

Publishers (200 synthetic streams each):

| Stream | Flagged as replay |
|---|---|
| Live camera | 0.0% |
| Static slide, live | 0.5% |
| 90 s file loop, not re-encoded | 100% |
| 90 s file loop, re-encoded each pass | **0.0%** |
| 4 s loop, re-encoded | 100% |

Probing: an attacker sitting 0.05 log-odds under the boundary it found in
epoch 0 is caught 0% of the time later with fixed thresholds and **45.8%**
with keyed thresholds. To stay safe across 30 epochs × 40 tenants it must sit
0.58 log-odds lower, which is a lot of behaviour given up.

Drift: a slow poisoning campaign raised the alarm at step 20 of 40. The
farm's real behaviour scored a largest robust z of 6.6 before the campaign,
5.6 after it with the alarm honoured, and 3.7 (below the contribution
threshold of 6) with every alarm overridden. The alarm is what keeps the
farm visible.

#### What the harness says we cannot do

- **An operator who watches outcomes and adapts wins over time.** The adaptive
  mimic is caught at 50% on day 1 and about 2–6% by day 5, once it reaches
  human-like rates with cover traffic. Detection then rests on the canary,
  rings and economics (it has to keep paying for the farm), and on human
  review of the rooms it inflates. This is why counts exclusion and tip holds
  matter more than bans.
- **A phone farm with one real handset per account** is nearly invisible
  (≈2%). Hardware attestation proves a real device, not a real person. The
  remaining lever is the cost of a handset per account.
- **Re-encoded long loops** defeat size-based replay detection. Liveness
  challenges and the classifier are the next layer, and both are evidence,
  not proof.
- **Estimated bot share undercounts** the true share by roughly a third to a
  half in target rooms, so counts after exclusion are still inflated, just
  less.
- Results vary between seeds (individual seeds range a few points either
  side of the means), and every behaviour distribution is an assumption.

## Enforcement

### Graded actions (`policy.go`)

| Action | Default score threshold (jittered) | Min distinct reasons | Effect |
|---|---|---|---|
| log | 0.25 | 1 | audit only |
| shadow-flag | 0.50 | 1 | visible to the tenant's moderators, nothing else |
| step-up challenge | 0.70 | 1 | liveness challenge for publishers, re-attestation for viewers |
| throttle | 0.80 | 2 | lower rung, slower joins |
| exclude | 0.85 | 2 | out of viewer counts, billing, and tip eligibility |
| kick | 0.97 | 2 | **human review first** |
| ban | 0.995 | 3 | **human review first** |

Every threshold and the action ceiling are per tenant. Defaults:

- **Shadow mode is on.**
- `AutoCeiling = exclude`: nothing above exclude happens without a person, and
  the participant is excluded meanwhile.
- **Publishers** are never acted on automatically above shadow-flag, since
  wrongly stopping a stream is the most visible false positive, and are always
  reviewed.

### Review, appeal, audit

`Queue.Process` applies the automatic part and opens a review case if the
proposal needs one. `Resolve` records the reviewer's decision and applies it
through the `Enforcer`, which wraps the existing moderation API, so a trust
action is a normal moderation entry with `DecidedByModel` set. `Appeal`
opens an appeal against a decision. **Four-eyes**: the reviewer who made the
decision cannot decide the appeal (`ErrSameEyes`). An upheld appeal lifts the
action and the exclusion, and retracts any cross-tenant confirmation. Every
step is an `AuditEntry` with the actor, the model version and the reasons.

### Counts, billing, tips

- `Exclusions.Countable` is `server.Config.Audience`: excluded viewers leave
  the room's device, people and audience counts (they stay in the room;
  publishers always count).
- `Exclusions.Unbillable` is `billing.Config.Usage`: their usage records are
  withheld from charging and counted as `Unbilled`, with the reason logged.
  **Open decision:** whether tenants should still pay MOQOM's egress cost for
  bots they attract, or MOQOM absorbs it.
- `TipScorer` implements the boundary to MOTOM (`motom/` does not exist yet,
  so only the interface is defined). MOTOM calls `Score` with a
  `TipObservation` before settling, and receives a `TipVerdict`. A tip is held
  for review when the sender is excluded, the sender's band is at or above
  `HoldFrom`, or sender and recipient are in the same ring. `TipRiskSink` is
  what MOTOM implements to receive flags pushed from moderation. Held tips
  are a payout decision for a person, never an automatic reversal.

## Integration hooks

| Where | Hook | Status |
|---|---|---|
| Relay | emit generic per-session and per-track statistics, a counter for the canary track, and group starts for challenges | **Not built.** Decide relay vs sidecar |
| Control plane | feed joins, leaves, mints, attestation into `Detector.Observe` | Not wired |
| `server.Config.Audience` | `Exclusions` | Built and tested |
| `billing.Config.Usage` | `Exclusions` | Built and tested |
| Moderation API | `Enforcer` implementation over the existing kick/ban/mute RPCs, with `DecidedByModel` | Interface only |
| MOTOM | `TipScorer.Score`, `TipRiskSink` | Interface only |
| Admin console | review and appeal queue, room inflation view | Not built |
| Storage | profiles, assessments, audit log | In memory. **Open decision** (Postgres tables with sweep rules) |

## Open decisions

1. Licence of `internal/trust`: BUSL-1.1 like the rest of `moqom` (current
   header), or proprietary with no conversion date, since detection logic loses
   value when public.
2. Where the relay-side statistics emitter lives, given moqom-engine may go
   public.
3. Whether tenants are billed MOQOM's egress for excluded bots.
4. A "verified viewer" count (hardware-attested devices only) beside the
   total: honest and hard to fake, but it rewards platforms with attestation.
5. Classifier vendor, DPA and sub-processor entry; or none.
6. C2PA validator library.
7. Production co-audience: MinHash/LSH instead of the exact inverted index,
   and ageing out old co-arrival pairs.
8. Persistent storage and retention rules for profiles and the audit log.
9. Filing before any public mention (see the disclosure).
