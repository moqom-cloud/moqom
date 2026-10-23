# Where MOQOM goes beyond, or diverges from, draft-ietf-moq-transport-19

> **PRIVATE AND CONFIDENTIAL. Elliott Wave LLC. Do not publish, do not copy into
> any public repository, do not post to an IETF list.** This document names
> mechanisms that `PATENT-ASSESSMENT.md` routes to filing or trade secret, and
> it maps where they touch the wire. A public copy would be prior art against
> those filings (see that document, point 2) and could trigger IETF disclosure
> obligations under BCP 79 (point 4). Share only with counsel and people under
> NDA.

Owner: Elliott Wave LLC. Written October 2026 against the code on disk in
`moqom-engine`, `moqom-swift` and `moqom`.

The draft is an **IETF** Internet-Draft from the MoQ working group:
[draft-ietf-moq-transport-19](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/19/),
published 6 July 2026. Section numbers below are from that revision. The
working group has since published -22 (1 October 2026); this document has not
been diffed against -20 to -22, so re-check every section number and codepoint
before acting on it externally.

## Method

- Codepoints come from the two independent codecs:
  `moqom-engine/moqom-proto/src/control/{setup,params}.rs`,
  `moqom-engine/moqom-client/src/properties.rs`, and
  `moqom-swift/Sources/MoqomWire/Parameters.swift`,
  `moqom-swift/Sources/MoqomKit/Transport/ObjectProperties+Wire.swift`.
- Behaviour comes from `moqom-relay/src/{identity,possession,hops,session}.rs`
  and `MoqomKit/Transport/MoqtSession.swift`.
- "Built" means the code exists and is exercised by tests. "Planned" means
  `PATENT-ASSESSMENT.md` or `PLAN.md` describes it and no wire format exists yet.
- Route column cross-references the candidate table in `PATENT-ASSESSMENT.md`
  by row number. "IETF" means a plausible working-group contribution; "fix"
  means it is a conformance gap to close, not something to contribute.

## Summary

| # | Item | Draft-19 | MOQOM | Status | Route | Interop impact |
|---|---|---|---|---|---|---|
| X1 | Authorization token value encoding | §10.2.2: value is a Token structure (Alias Type, optional Alias, Token Type, Token Value) | Raw compact JWS bytes, no Token structure | Built | Fix | **High.** A conformant peer cannot parse MOQOM's token, and MOQOM cannot parse a conformant one |
| X2 | `AUTHORIZATION_PROOF` Setup Option, `0x4D51` | §15.4: no private range; implementations SHOULD request provisional registration. §10.3: unknown options MUST be ignored | Self-assigned codepoint carrying an ES256 compact JWS | Built | Row 4: **File** | Low at SETUP: ignored by others |
| X3 | `AUTHORIZATION_PROOF` Message Parameter, `0x4D51`, on `REQUEST_UPDATE` | §10.2: every Message Parameter MUST be defined by the version or negotiated via Setup Options; unknown ones MUST close the session with PROTOCOL_VIOLATION | Sent unconditionally on renewal; nothing in SETUP negotiates it | Built | Row 4: **File** | **High.** A conformant relay closes the session at the first renewal |
| X4 | `RELAY_HOPS` Message Parameter, `0x4D50`, on relay-to-relay `SUBSCRIBE` | Same §10.2 rule. §6.2: PUBLISH_NAMESPACE "does not protect against loops" | Hop budget of 3, spent per forward, clamped on receipt | Built | Not in patent table. IETF candidate | **High** for mixed fleets: a non-MOQOM parent relay closes the session. None between MOQOM relays |
| X5 | Object Properties `0x78`, `0x79`, `0x7A`, `0x7C` | §2.5, §15.8: `0x78`–`0x7F` reserved for application use, no registration, relays forward unchanged | Capture time (µs), decoder configuration, temporal layer, audio silence | Built | `0x7A` touches row 2. Otherwise publish | Low. Forwarded by any conformant relay; not understood by non-MOQOM players |
| X6 | SFrame per object | §13.4: E2EE is external; points to secure-objects | RFC 9605 header on the payload; AAD = group, object, priority, object properties | Built (Apple SDK, opt-in) | Row 13: **do not file** | Medium. Not wire-compatible with draft-ietf-moq-secure-objects-01 |
| X7 | Token format and claims | §13.3: names C4M (CAT) and Privacy Pass as the token schemes | Ed25519 over JSON claims, `typ: MOQT`, `cnf.jkt` | Built; CAT planned | Fix (align to C4M) | High until aligned |
| X8 | Possession proof semantics | Draft silent (§13.3.1 delegates replay protection to the token scheme). C4M-01 §3 defines per-action DPoP proofs | One proof per SETUP and per renewal, `typ: MOQT-POP`, ES256 only, single use | Built | Row 4: **File** | Medium. Different from C4M's DPoP profile even after X7 |
| X9 | Credential renewal applies to the session | §10.2.2: a token on a message authorizes "the operation carrying the parameter" | A `REQUEST_UPDATE` token renews the whole session's deadline; scope may not change | Built | Row 4 (renewal element) | Low. Uses the draft's own message; semantics stricter |
| X10 | Session ends at token expiry | Silent at session level; §6.2 notes cancellation for expired credentials | Relay closes a session whose credential lapsed | Built | None | None |
| X11 | In-session revocation | Silent | Control-plane ban closes the live QUIC session | Built | Row 5: **File** (dependent on 4) | None on the MOQT wire; the directive travels on MOQOM's own gRPC link |
| X12 | `MOQT_IMPLEMENTATION` content | §13.8: SHOULD send name and major version only | `moqom-relay/<full crate version>`, `moqom-swift/<full version>` | Built | Fix | None functionally; fingerprinting |
| X13 | Datagram forwarding at the relay | §4: relays MUST implement all messages; §9.7, §11.3 define datagram objects | Relay forwards subgroup streams only | Gap | Fix | Medium. Datagram publishers reach no one through a MOQOM relay |
| X14 | Demand feedback to publishers | No equivalent; §10.2.17 FORWARD and unsubscribe are the nearest mechanisms | Relay tells publisher which layers have no subscribers | Planned | Row 1: **File** | TBD: depends on the wire form chosen |
| X15 | Dependency / deadline descriptor | No equivalent | Codec-agnostic per-object descriptor a relay can shed on under E2EE | Planned | Row 2: **Decide first** | TBD: an Object Property, so forwardable by any relay |
| X16 | Lateness-based per-copy shedding | §8: delivery timeouts, reset with DELIVERY_TIMEOUT | Per-subscriber copy, shed on elapsed lateness, never substitutes a layer | Built | Row 3: **File** | None. Uses draft stream resets |
| X17 | Subscriber priority parameter code | §10.2.7, Table 13: `SUBSCRIBER_PRIORITY` is `0x20`; `0x0E` is not a Message Parameter (it is the `DEFAULT_PUBLISHER_PRIORITY` Track Property, §12.4) | Swift SDK sends subscriber priority as Message Parameter `0x0E` | Built (bug) | Fix | **High.** A conformant relay must close the session on the first SUBSCRIBE |

## Details

### X1. The authorization token is not wrapped in the draft's Token structure

**Draft.** §10.2.2 defines the value of AUTHORIZATION TOKEN as

```text
Token {
  Alias Type (vi64),         // DELETE 0x0, REGISTER 0x1, USE_ALIAS 0x2, USE_VALUE 0x3
  [Token Alias (vi64),]
  [Token Type (vi64),]       // IANA "MOQT Auth Token Type", §15.6; 0 = negotiated out of band
  [Token Value (..)]
}
```

and the Setup Option (§10.3.1.4) is "functionally equivalent". A Token that
cannot be decoded MUST close the session with `KEY_VALUE_FORMATTING_ERROR`.

**MOQOM.** The client writes the compact JWS directly as the option and
parameter value (`MoqtSession.swift`, the SETUP builder and
`renewCredential`). The relay splits the bytes on `.`
(`identity.rs`, `verify`) and also writes raw bytes when it dials upstream
(`session.rs`). The Swift and Rust codecs agree, which is why every
cross-language test passes. This is the "misreading the draft the same way in
both codecs" failure the READMEs warn about.

**Interop.** A conformant relay reads the first byte of a JWS (`e`, `0x65`) as
a varint Alias Type and fails the session. A conformant client sending
`USE_VALUE, type, value` to a MOQOM relay is refused as a malformed token.
moq-relay, for instance, documents a SETUP token "with Token Type 0"
([doc.moq.dev](https://doc.moq.dev/bin/relay/auth), accessed 6 Oct 2026).

**Action.** Encode `USE_VALUE (0x3)`, Token Type `0x0` (out-of-band), then the
JWS, on both sides, and accept that form on the relay. When C4M registers a
CAT token type, switch to it (X7). Small change, no IP implications, and it
should land before any interop event.

### X2. `AUTHORIZATION_PROOF` as a Setup Option, `0x4D51`

**Draft.** Unknown Setup Options MUST be ignored (§10.3, §15.4), so a private
option is safe at SETUP. But §15.4 also says "There is no reserved range for
private or application-specific use; implementations that need custom Setup
Options SHOULD request a provisional registration." `0x4D51` is outside the
greasing sequence `0x7f * N + 0x9D` (§14), which the code asserts at build time.

**MOQOM.** Odd type, so length-prefixed bytes; carries an ES256 compact JWS
proving the session holds the key named by the token's `cnf.jkt`. Defined in
`setup.rs` (`PRIVATE_USE`) and `Parameters.swift`.

**Route.** Row 4, **File**. Requesting a provisional registration publishes
the option's name and purpose, and arguably its semantics. **Do not request it
until the row-4 application is on file.** Until then the SHOULD stays unmet,
which costs nothing technically.

**Prior art note for counsel.**
[draft-ietf-moq-c4m-01](https://datatracker.ietf.org/doc/draft-ietf-moq-c4m/)
§3 (18 June 2026) already describes CAT tokens with a `cnf`/`jkt` confirmation
claim and DPoP proofs that MOQT relays MUST verify against it. Row 4's
candidate novelty therefore has to sit in what C4M does not describe: binding
the proof to the relay's own certificate, one proof per session rather than per
action, renewal over an existing request stream, and the staged enforcement
rollout. The rating "expect narrow claims" stands and is now concrete.

### X3. `AUTHORIZATION_PROOF` as a Message Parameter on `REQUEST_UPDATE`

**Draft.** §10.2: "All Message Parameters MUST be defined in the negotiated
version of MOQT or negotiated via Setup Options. An endpoint that receives an
unknown Message Parameter MUST close the session with PROTOCOL_VIOLATION."
§10.2.1: a parameter in a message type its definition does not allow also
closes the connection.

**MOQOM.** `renewCredential` attaches `0x4D51` whenever the client holds a
device key, with no check that the relay advertised support. The relay does not
echo the option in its own SETUP.

**Interop.** Against any conformant non-MOQOM relay, the first token renewal
ends the session. At SETUP the same proof is harmless (X2), so this only shows
up minutes into a call.

**Action.** Have the relay send `0x4D51` (or a zero-length capability option)
in its SETUP, and have the client send the message parameter only when it saw
one. That is exactly "negotiated via Setup Options" and needs no registration.
This change also discloses nothing new.

### X4. `RELAY_HOPS`, `0x4D50`, on relay-to-relay `SUBSCRIBE`

**Draft.** Same §10.2 rule as X3. Separately, §6.2 says PUBLISH_NAMESPACE "is
not a full-fledged routing protocol and does not protect against loops", and
the draft defines no hop limit.

**MOQOM.** Even type, so a varint. Absent on a client's SUBSCRIBE, which
starts a fresh budget of 3 (`hops.rs`, `BUDGET`). Each relay spends one before
forwarding and clamps a larger value on receipt, so a cycle ends in a refusal.
Not negotiated.

**Interop.** None inside a MOQOM fleet. A MOQOM relay configured with a
non-MOQOM parent (moq-relay, a Cloudflare upstream, moxygen) will be
disconnected on its first forwarded SUBSCRIBE.

**Route.** Not a patent candidate (a TTL is old art), and not covered by any
row. A good **IETF contribution candidate**: it closes a gap the draft names
itself, it is small, and contributing it builds standing without touching
anything filed. Negotiate it via a Setup Option first (same fix as X3).

### X5. Object Properties in the application range

**Draft.** §2.5 and §15.8 reserve `0x78`–`0x7F` (one-byte) and
`0x3800`–`0x3FFF` (two-byte) for application use, never allocated by IANA.
Relays that do not understand them MUST forward them unchanged. The draft also
lists provisional registrations from
[draft-ietf-moq-loc](https://datatracker.ietf.org/doc/draft-ietf-moq-loc/):
`TIMESTAMP 0x06`, `TIMESCALE 0x08`, `VIDEO_FRAME_MARKING 0x0A`,
`AUDIO_LEVEL 0x0C`, `VIDEO_CONFIG 0x0D` (Table 15).

**MOQOM.**

| Code | Name | Type | Nearest LOC property |
|---|---|---|---|
| `0x78` | Capture time, microseconds | varint | `TIMESTAMP` (`0x06`) with `TIMESCALE` |
| `0x79` | Decoder configuration for the group | bytes | `VIDEO_CONFIG` (`0x0D`) |
| `0x7A` | Temporal layer index | varint | `VIDEO_FRAME_MARKING` (`0x0A`) |
| `0x7C` | Audio silence flag | varint | `AUDIO_LEVEL` (`0x0C`) |

Pinned by a test on each side (`properties.rs`, `ObjectProperties+Wire.swift`).
Four of the eight one-byte application codes remain (`0x7B`, `0x7D`–`0x7F`).

**Interop.** Forwarded and cached correctly by any conformant relay. A third-
party player sees none of this metadata, because it looks for LOC codes.

**Route.** Publishing these freely is fine for `0x78`, `0x79` and `0x7C`.
`0x7A` is a building block of the row-2 descriptor; do not extend it in public
into anything that describes dependencies or deadlines until row 2 is decided.
Consider emitting LOC properties alongside, or instead, once LOC stabilises:
that is free interop.

**Watch.** The application range is shared by every application, and the draft
warns that "applications consuming tracks from uncoordinated sources may
encounter different semantics for the same code points". A MOQOM viewer
subscribing to a non-MOQOM publisher could misread another app's `0x7A`.
Gate interpretation on the track's catalog identifying a MOQOM publisher.

### X6. SFrame per object, not the secure-objects format

**Draft.** §13.4: "The end-to-end media security is handled by mechanisms
external to this specification," naming
[draft-ietf-moq-secure-objects](https://datatracker.ietf.org/doc/draft-ietf-moq-secure-objects/).
Secure-objects-01 (6 July 2026) derives the nonce counter from Group ID and
Object ID rather than carrying an SFrame CTR, carries the Key ID as an
immutable property, and authenticates the Full Track Name, Group ID, Object
ID, publisher priority and the Immutable Properties (§12.7 of MOQT) as AAD. It
also defines an encrypted-properties list.

**MOQOM.** `SFrame.swift` implements RFC 9605 with the AES-GCM suites `0x0004`
and `0x0005` (default). `MediaProtection.swift` encrypts each object's payload
with a full SFrame header and binds group, object, priority and the object's
properties (all of them, in wire encoding) as AAD. Opt-in per room, shared key
supplied by the app.

**Interop.** Not decryptable by a secure-objects implementation even with the
right key. That is consistent with `OPEN-CORE.md` layer 2 (media keyed to the
network), but public material should say "SFrame (RFC 9605)" and not imply
secure-objects compatibility.

**Two hardening items found while mapping this:**

1. **Track name is not in the AAD.** Under one shared room key, a relay could
   move an object from one of a publisher's tracks to another at the same group
   and object number (say, between two layers), and it would still decrypt.
   Secure-objects closes this by authenticating the Full Track Name. Add it.
2. **All object properties are authenticated, including mutable ones.** The
   draft distinguishes mutable properties from Immutable Properties (`0x0B`,
   §12.7), and §2.5 lets a relay modify a property it supports when that
   property's rules allow. Any such rewrite on a third-party relay would make
   MOQOM objects fail authentication. Authenticating only the Immutable
   Properties container, and putting MOQOM's properties inside it, matches the
   draft's model.

**Route.** Row 13, **do not file**: MLS, sender keys, Zoom's E2EE and SFrame
are the prior art. Aligning with secure-objects is pure upside and good
standing in the working group.

### X7. Token format: JWS today, CAT planned

**Draft.** §13.3 names two token schemes for MOQT: Privacy Pass
([draft-ietf-moq-privacy-pass-auth-03](https://datatracker.ietf.org/doc/draft-ietf-moq-privacy-pass-auth/))
and C4M, which uses the Common Access Token, CTA-5007-B, a CWT over CBOR and
COSE with a `moqt` claim scoping actions to namespaces and track names.

**MOQOM.** Ed25519 over JSON claims in compact JWS, `alg` pinned to `EdDSA`,
`typ` pinned to `MOQT` (`identity.rs`, `token.go`). `identity.rs` says "the
full design target is CAT/CWT with COSE". Some Swift doc comments already call
the token "a CAT"; they should say "JWS, CAT planned" until it is one.

**Route.** Fix. Nothing to file in an encoding.

### X8. Possession-proof semantics versus C4M's DPoP profile

**C4M-01 §3.** Proofs are DPoP JWTs (`typ: dpop-proof+jwt`) created fresh for
each MOQT action, with an authorization context (`actx`: action, track
namespace, track name) per
[draft-nandakumar-moq-generic-dpop-proof](https://datatracker.ietf.org/doc/draft-nandakumar-moq-generic-dpop-proof/),
and with the freshness window and `jti` replay setting carried in a `catdpop`
claim
([C4M-01 §3.1](https://www.ietf.org/archive/id/draft-ietf-moq-c4m-01.txt),
accessed 6 Oct 2026).

**MOQOM.** One proof at SETUP and one per renewal, `typ: MOQT-POP`, ES256 only
because that is what the Secure Enclave does. The proof commits to its token by
hash and is accepted once (`possession.rs`). The module's own note explains why
per-request proofs add nothing once a session has pinned its principal.

**Interop.** Even after X7, a C4M relay expecting per-action DPoP proofs will
not accept MOQOM's session-scoped proof, and vice versa.

**Route.** Row 4, **File**. This divergence is where the claimable difference
lives, so it must not be argued on the MoQ list before filing. After filing,
proposing session-scoped proofs to C4M is possible only with a BCP 79
disclosure and a licensing declaration, as `PATENT-ASSESSMENT.md` point 4 says.

### X9. Renewal renews the session, not just the request

**Draft.** §10.2.2 lists `REQUEST_UPDATE` among the messages that may carry
AUTHORIZATION TOKEN, which "conveys information to authorize the sender to
perform the operation carrying the parameter".

**MOQOM.** A token arriving on any request stream renews the deadline of the
whole session. A renewal may move the deadline and nothing else; a token for a
different holder or scope is refused and the session continues under the old
one (`moqom-engine` README; `identity.rs`).

**Interop.** Uses the draft's message as written. Another relay would treat
the token as applying to that one request, which is weaker, not broken.

**Route.** The renewal element is part of row 4's combination. Do not present
it to the working group as a design until filed.

### X10 and X11. Expiry and revocation reach the live session

Draft-19 says nothing about what happens to an established session when a
credential expires or is withdrawn, beyond §6.2 noting that a subscriber can
cancel a PUBLISH_NAMESPACE on credential expiry. MOQOM closes the session on
expiry (X10) and on a control-plane ban (X11). Neither adds anything to the
MOQT wire: X11's directive travels on MOQOM's own gRPC RelayLink. X11 is row 5,
**File as a dependent claim of row 4**. X10 is unremarkable (moq-relay
documents the same behaviour) and needs no protection.

### X12. Implementation string

§13.8 asks for the implementation name and major version only, to limit
fingerprinting. `session.rs` sends `moqom-relay/` plus the full crate version,
and the Swift client sends `moqom-swift/` plus `moqomKitVersion`. Trim both to
the major version. Fix; trivial.

### X13. No datagram forwarding

§4 requires relays to implement all MOQT messages and the processing rules of
§9; datagram objects are defined in §11.3. `moqom-relay` carries subgroup
streams only (`BENCHMARKS-AND-SPECS.md`, "Datagram forwarding not yet
carried"). Not an extension; a conformance gap that interop testing will hit.

### X14. Demand feedback (planned)

Row 1, **File**, the strongest commercial candidate. Draft-19 has no message
that tells a publisher a layer has lost its audience; the nearest tools are
unsubscribe (closing the request stream), the `FORWARD` parameter (§10.2.17)
and subscriber priority (§10.2.7). Before filing, decide whether the wire form
is (a) only standard messages a MOQOM publisher interprets, which interops
with every relay, or (b) a new parameter or message, which must be negotiated
via a Setup Option (§10.2) or every other relay will close the session. (a) is
better for interop and for the system claim, because it shows the mechanism
lives in the relay-plus-client combination rather than in a codepoint.

### X15. Dependency and deadline descriptor (planned)

Row 2, **Decide first**: patent with a royalty-free IETF declaration, or
patent and keep proprietary. On the wire it would be an Object Property, so
any conformant relay forwards it unchanged (§2.5). Codepoint options: the
remaining one-byte application codes (X5), the two-byte application range
`0x3800`–`0x3FFF`, or a Specification Required registration
(`0x80`–`0x37FF`, §15.8) if it goes to the IETF. It must not go in the
Mandatory Track Property range (`0x4000`–`0x7FFF`, §2.5.1), because that makes
non-MOQOM relays refuse the track. **File before any draft is posted**, as the
assessment says, and before requesting any codepoint.

### X16. Lateness-based shedding

Row 3, **File**, server-side. On the wire it uses only the draft's stream
reset; the novelty is in when and per what. §8's delivery timeouts are the
prior art to distinguish. Confirm that the reset code the relay uses is the
one §11.4.3 and §15.11.4 specify; this review did not check it.

## Codepoints MOQOM uses outside the draft's registries

| Space | Code | Name | Parity / value | Negotiated? | Defined in |
|---|---|---|---|---|---|
| Setup Option | `0x4D51` | `AUTHORIZATION_PROOF` | odd, bytes (JWS) | Ignorable by design | `setup.rs`, `Parameters.swift` |
| Message Parameter | `0x4D51` | `AUTHORIZATION_PROOF` | odd, bytes (JWS) | **No** (X3) | `params.rs`, `Parameters.swift` |
| Message Parameter | `0x4D50` | `RELAY_HOPS` | even, varint | **No** (X4) | `params.rs` (Rust only) |
| Object Property | `0x78` | capture time, µs | even, varint | Not required (app range) | `properties.rs`, `ObjectProperties+Wire.swift` |
| Object Property | `0x79` | decoder configuration | odd, bytes | Not required | same |
| Object Property | `0x7A` | temporal layer | even, varint | Not required | same |
| Object Property | `0x7C` | silence | even, varint | Not required | same |

Spot checks against draft-19 that matched: SETUP `0x2F00`, setup options
`0x01`–`0x08`, message parameters `0x02` (delivery timeout), `0x03` and
`0x22` (group order). This was not a full audit of every message and error
code.

### X17. Subscriber priority sent under the wrong code

`MoqomKit/Transport/MoqtSession.swift` defines
`SubscribeParameter.subscriberPriority = 0x0E` and attaches it to SUBSCRIBE.
Draft-19 Table 13 assigns `SUBSCRIBER_PRIORITY` to `0x20` (§10.2.7); `0x0E`
is not in the Message Parameter registry at all, so §10.2 requires a
conformant receiver to close the session with PROTOCOL_VIOLATION. MOQOM's
relay carries subscribe parameters opaquely (per the comment beside the
constant), which is why this has never failed in testing. The code comment
says "per draft-19 §15", so this looks like a transcription slip, possibly
from the `DEFAULT_PUBLISHER_PRIORITY` Track Property, which is `0x0E`. Fix:
change to `0x20` and add a test that pins it against the registry.

## Things with no MOQT wire footprint

Listed so nobody looks for them here: row 6 (room state as a control track:
ordinary MOQT tracks and objects), row 7 (render-aware selection: ordinary
subscriptions), rows 9, 10 and 12 (client-only), row 14 (`moqom-cc`; §3.7 of
the draft is non-normative guidance and a congestion controller adds no
codepoints), and row 16 (QUIC-layer connection-ID steering, below MOQT).

## Recommended order

1. **Conformance fixes that disclose nothing**, before any interop event:
   X1 (Token structure), X17 (subscriber priority code), X3 and X4
   (negotiate via a SETUP capability), X12 (implementation string), and X6's
   two hardening items.
2. **Filings** for rows 3, 4 and 5 (built, and visible in the code), with
   counsel told about C4M-01 §3 as prior art for row 4.
3. **Then** request provisional registrations for `0x4D51` and `0x4D50`, and
   offer `RELAY_HOPS` (X4) to the working group as the first contribution: it
   is useful, small, and not on the patent table.
4. **Decide row 2** before X15 gets a codepoint, and before secure-objects
   alignment work brings the descriptor up in conversation.
5. Re-run this mapping against draft-22.
