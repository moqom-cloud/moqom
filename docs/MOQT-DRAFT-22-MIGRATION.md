# Moving from MoQ Transport draft-19 to draft-22

MOQOM speaks `draft-ietf-moq-transport-19` today. The working group's current draft is -22
(October 2026). This is the work to support -22, where it lands in the code, and how to ship it
without breaking deployed clients.

Section numbers refer to draft-22. Its changelog (Appendix A.1 to A.3) is the source for "what
changed".

## Shipping plan

Drafts don't interoperate, so the version is chosen by ALPN.

1. Make the Rust codec (`moqom-proto`) and the Swift codec (`MoqomWire`) speak both `moqt-19`
   and `moqt-22`, selected per session from the negotiated ALPN.
2. Relays offer both. The admission ticket, metering and the relay link (gRPC) don't change.
3. SDKs offer `moqt-22` first and `moqt-19` second.
4. Drop `moqt-19` once no supported SDK release needs it.

Where the ALPN is fixed to `moqt-19` today:

| File | What |
| --- | --- |
| `moqom-engine/moqom-transport/src/lib.rs` | `MOQT_DRAFT_19` and `Carrier::from_alpn` |
| `moqom-engine/moqom-client/src/tls.rs` | Client ALPN list |
| `moqom-swift/Sources/.../MemoryCarrier.swift`, `LoopbackTransport.swift` | Test carriers |
| `moqom-swift/Sources/.../DirectEndpoint.swift` | `static let alpn = "moqt-19"` (direct calls) |

Direct calls can move later than relay sessions, because both ends are MOQOM SDKs.

## Wire changes

The control message type codes are the same as in -19, plus one new message. Every item below
is missing from both codecs today (checked by grep on 2026-10-06).

### New

| Item | Draft-22 | Work |
| --- | --- | --- |
| `PUBLISH_STATE_NOTIFY` (message `0x22`) | §9.10 | New message: codec, relay forwarding, SDK event |
| Fill fetch streams | §3.4, §11.4 | Replace Joining FETCH. The subscriber asks for a fill with `FILL_PARAMETERS`, and the publisher opens a fetch stream bound to the subscription. |
| `FILL_PARAMETERS` (param `0x23`) | §9.20.15 | Length-prefixed parameter on SUBSCRIBE and REQUEST_UPDATE |
| `FILL_TIMEOUT` (param `0x0A`) | §9.20.5 | Check our codec; add it if missing |
| `LOCATION_FILTER` (param `0x21`) | §9.20.9 | Replaces the start and end fields. It has an explicit Location Filter Type (`0x00` to `0x05`) and also carries FETCH's range. |
| `INCLUDE_PROPERTIES` (param `0x35`) | §9.20.21 | uint8. Lets a subscriber ask for Object Properties to be sent or left out. |
| `End of Timed-Out Range` (status `0x20C`) | §11.4.1.2 | Object status a relay sends after dropping objects for delivery timeout |
| Fill and subscription scheduling | §3.4 | Relay scheduler: how fill objects share bandwidth with live objects |

### Changed

| Item | Draft-22 | Work |
| --- | --- | --- |
| Parameter placement | A.2, A.3 | PUBLISH carries subscription parameters. Changes go in REQUEST_UPDATE, not PUBLISH_OK. |
| `FORWARD` on REQUEST_UPDATE for SUBSCRIBE_TRACKS | §9.5, §9.18 | Rust relay: allow it. Swift has no SUBSCRIBE_TRACKS yet. |
| Paused subscriptions | §3.1.1, §7.2 | Rename "forward state" to paused, and check the relay's upstream pause rules against §7.2 |
| Type Flags bitfields | §11.2.1, §11.3.1 | An unknown bit is a PROTOCOL_VIOLATION. Check the subgroup and datagram decoders. |
| `OBJECT_DELIVERY_TIMEOUT` start point | §5.2, §9.20.4 | Starts at the last header byte. Check the relay's timer. |
| Relay sends an upstream FETCH | §3.2.3 | MUST; check that the relay never answers from cache alone where the draft forbids it |
| REDIRECT, Retry Interval 0 | §9.4.1 | Check the client retry logic |
| `moqt://` URI | §6.1 | Host resolution; the query component is not part of the identity |
| Error codes | §12 | Error codes are now collected in one place. Check every code we send against §12.2 to §12.5. |
| Max stream count | A.2 | Allow up to 2^64−1 |
| Track Alias reuse | §3.1.3 | Caution text; check that the relay doesn't reuse aliases early |
| Delivery Mode rename, per-request shorthand names | A.2 (-21) | Editorial; rename in docs and logs |
| Extensions declared in SETUP | A.1 (-22, #1921) | Check how our SETUP options declare extension support |

### Removed

| Item | Where we still have it |
| --- | --- |
| Joining FETCH | Rust: 4 files, including `INVALID_JOINING_REQUEST_ID` in `request.rs`. Swift: 7 files. |
| `SUBSCRIPTION_ENDED` | Rust: 1 file |
| `VERSION_NEGOTIATION_FAILED` | Check; remove if present |
| Requirement to retain Largest Location | Relay can drop this state |

### Security sections added in -20 to -22

These cover preventing impersonation, mTLS, the `moqt` URI, logging untrusted strings, and a
recommendation for end-to-end encryption. We already do most of this. Check that logs escape
peer-supplied track names, namespaces and implementation strings.

## Our own conformance gaps (independent of -22)

| Gap | Status |
| --- | --- |
| Swift sent subscriber priority as `0x0E` (that's the Track Property DEFAULT PUBLISHER PRIORITY), not Message Parameter `0x20` | Fixed (`MoqtSession.swift`) |
| Draft-22 closes the session on any unknown Message Parameter (§9.20). Our codecs must stop sending anything -22 doesn't define, and must close rather than skip. | To do |
| Token structure: use the draft's Alias Type and Token Type framing exactly | Check while touching AUTHORIZATION_TOKEN |
| Our own extensions must be negotiated in SETUP, per #1921, not assumed | To do with SETUP |
| `MOQT_IMPLEMENTATION` string: length limit and log escaping | Length checked in `moqom-relay/src/session.rs`; check escaping |
| Datagram forwarding parity with subgroup streams in the relay | Check with the -22 datagram tests |

## Tests

1. Byte-exact vectors for every -22 message and parameter, shared by the Rust and Swift test
   suites (both codecs decode each other's output).
2. A Rust relay with a Swift client on `moqt-22`, and the same pair on `moqt-19`. Both must pass
   in one build.
3. Interop with at least one non-MOQOM implementation from the working group's interop list.
4. Run the relay load generator (`moqom-loadgen`) on -22 to check that fill streams don't regress
   live latency.

## Estimate

Most of the work is fill fetch, which replaces Joining FETCH on both sides, and the
LOCATION_FILTER rewrite. Those touch subscribe, fetch, relay caching and both SDKs. Everything
else is local. Expect about two to three engineer-weeks for both codecs and the relay, plus
interop time.
